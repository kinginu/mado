package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/usage"
)

// rates looks up the price of an upstream model (DESIGN §17.6). Subscription
// sessions cost 0; an unknown model costs 0 and is logged once.
func (h *Hub) rates(model string, subscription bool) Rates {
	if subscription {
		return Rates{}
	}
	p, ok := h.priceTable().Lookup(model)
	if !ok {
		if _, seen := h.unpriced.LoadOrStore(model, true); !seen {
			h.log.Info("no [[prices]] entry; cost recorded as 0", "model", model)
		}
		return Rates{}
	}
	return Rates{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite}
}

// priceTable is the prices table (DESIGN §28.2), read from the store once and again
// after a save or revert.
func (h *Hub) priceTable() usage.Prices {
	h.pricesMu.Lock()
	defer h.pricesMu.Unlock()
	if h.prices == nil {
		ps, err := h.store.Prices()
		if err != nil {
			h.log.Error("read prices", "err", err)
			return usage.Prices{}
		}
		h.prices = ps
	}
	return h.prices
}

// invalidatePrices drops the cache; models without a price are logged once again.
func (h *Hub) invalidatePrices() {
	h.pricesMu.Lock()
	h.prices = nil
	h.pricesMu.Unlock()
	h.unpriced.Clear()
}

// recordCodeUsage keeps a code session's usage rows (§17.13). An assistant event's
// message.usage gives a provisional row per response: Claude Code copies it from
// message_start, so the output count is 0 and, behind an upstream that reports its
// real counts only in message_delta (AI&), the input is an estimate with no cache
// split. The turn's result carries the exact counts (modelUsage, cumulative per
// claude process and per requested model name, subagents included), so on a result
// the turn's provisional rows are replaced by one exact row per model.
func (h *Hub) recordCodeUsage(sessionID string, seq int64, raw []byte) {
	isAssistant := bytes.Contains(raw, []byte(`"type":"assistant"`)) && bytes.Contains(raw, []byte(`"usage"`))
	isResult := bytes.Contains(raw, []byte(`"type":"result"`)) && bytes.Contains(raw, []byte(`"modelUsage"`))
	if !isAssistant && !isResult {
		return
	}
	var e struct {
		Type    string `json:"type"`
		Message *struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				Input      int64 `json:"input_tokens"`
				CacheRead  int64 `json:"cache_read_input_tokens"`
				CacheWrite int64 `json:"cache_creation_input_tokens"`
				Output     int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
		ModelUsage map[string]modelTokens `json:"modelUsage"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	switch {
	case e.Type == "assistant" && e.Message != nil && e.Message.Usage != nil && e.Message.ID != "":
	case e.Type == "result" && len(e.ModelUsage) > 0:
	default:
		return
	}
	s, err := h.store.Session(sessionID)
	if err != nil || s.IsChat() {
		return
	}
	kind := KindCode
	if s.Kind == KindDigest {
		kind = KindDigest // a claude digest job (DESIGN §23.6)
	}
	var prof *proto.Profile
	if p, err := h.store.Profile(s.ProfileName); err == nil && p.Kind == proto.KindRouter {
		prof = p
	}
	if e.Type == "assistant" {
		u := e.Message.Usage
		row := UsageRow{SessionID: sessionID, Kind: kind, OwnerSub: s.OwnerSub, Model: e.Message.Model, MessageID: e.Message.ID,
			Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Output: u.Output, Seq: seq, Provisional: true}
		if err := h.store.RecordUsage(row, h.rates(e.Message.Model, prof == nil)); err != nil {
			h.log.Error("record usage", "session", sessionID, "err", err)
		}
		return
	}
	prev, typ, later, err := h.store.UsageBoundary(sessionID, seq)
	if err != nil {
		h.log.Error("record usage", "session", sessionID, "err", err)
		return
	}
	if later {
		return // the next result already counted this turn
	}
	base := map[string]modelTokens{}
	if typ == "result" {
		var p struct {
			ModelUsage map[string]modelTokens `json:"modelUsage"`
		}
		_ = json.Unmarshal(prev.Raw, &p)
		base = p.ModelUsage
		// Without a mado_spawn marker (an older shim) a smaller cumulative count
		// means a new claude process.
		for name, cur := range e.ModelUsage {
			if b, ok := base[name]; ok && cur.less(b) {
				base = map[string]modelTokens{}
				break
			}
		}
	}
	var rows []UsageRow
	var rates []Rates
	names := make([]string, 0, len(e.ModelUsage))
	for name := range e.ModelUsage {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		d := e.ModelUsage[name].minus(base[name])
		if d == (modelTokens{}) {
			continue
		}
		model := name
		if prof != nil {
			if up, ok := prof.Upstreams[name]; ok {
				model = up.Model
			}
		}
		rows = append(rows, UsageRow{SessionID: sessionID, Kind: kind, OwnerSub: s.OwnerSub, Model: model,
			MessageID: fmt.Sprintf("turn:%s:%d:%s", sessionID, seq, name), Seq: seq,
			Input: d.Input, CacheRead: d.CacheRead, CacheWrite: d.CacheWrite, Output: d.Output})
		rates = append(rates, h.rates(model, prof == nil))
	}
	if err := h.store.ReplaceTurnUsage(sessionID, prev.Seq, seq, rows, rates); err != nil {
		h.log.Error("record usage", "session", sessionID, "err", err)
	}
}

// modelTokens is one entry of result.modelUsage.
type modelTokens struct {
	Input      int64 `json:"inputTokens"`
	CacheRead  int64 `json:"cacheReadInputTokens"`
	CacheWrite int64 `json:"cacheCreationInputTokens"`
	Output     int64 `json:"outputTokens"`
}

func (a modelTokens) less(b modelTokens) bool {
	return a.Input < b.Input || a.CacheRead < b.CacheRead || a.CacheWrite < b.CacheWrite || a.Output < b.Output
}

func (a modelTokens) minus(b modelTokens) modelTokens {
	return modelTokens{a.Input - b.Input, a.CacheRead - b.CacheRead, a.CacheWrite - b.CacheWrite, a.Output - b.Output}
}

// publishEvent sends a newly stored event to the session's viewers: its blocks as
// JSON and, when it can change the model usage, the Models panel.
func (h *Hub) publishEvent(sessionID string, seq int64, raw []byte) {
	if data := h.eventJSON(seq, raw); data != "" {
		h.broker.publish(sessionID, sseMsg{event: "block", id: fmt.Sprint(seq), data: data})
	}
	if h.noteModelEvent(sessionID, raw) {
		j, _ := json.Marshal(h.sessionModels(sessionID))
		h.broker.publish(sessionID, sseMsg{event: "models", data: string(j)})
	}
}

func (h *Hub) publishState(sessionID, state string) {
	j, _ := json.Marshal(map[string]string{"state": state})
	h.broker.publish(sessionID, sseMsg{event: "state", data: string(j)})
}

// publishApproval sends an approval card (pending, or decided by a remembered rule).
func (h *Hub) publishApproval(sessionID string, a Approval) {
	j, _ := json.Marshal(h.approvalJSON(a))
	h.broker.publish(sessionID, sseMsg{event: "approval", data: string(j)})
}
