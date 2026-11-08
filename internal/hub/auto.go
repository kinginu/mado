package hub

import (
	"context"
	"fmt"
	"slices"

	"github.com/kinginu/mado/internal/messages"
	"github.com/kinginu/mado/internal/policy"
	"github.com/kinginu/mado/internal/proto"
)

// mado auto mode (DESIGN §18): for a router-profile session in permission mode auto,
// claude runs in acceptEdits and the hub answers the remaining approval requests
// itself: session rule, deny rules, allow rules, classifier, then the user.

// KindAuto is the usage kind of classifier calls (the session owner pays).
const KindAuto = "auto"

// recentBlocks is how many rendered blocks the classifier sees; recentEvents is how
// many events are read to find them.
const (
	recentBlocks = 20
	recentEvents = 80
)

// setupAuto compiles [auto] and seeds the auto.classifier setting (DESIGN §28.3).
// The classifier named by hub.toml must be a chat model; one set in the editor that
// no longer is one is logged and leaves auto mode without a classifier.
func (h *Hub) setupAuto() error {
	if err := h.cfg.Auto.applyDefaults(); err != nil {
		return err
	}
	p, err := policy.New(policy.Config{Allow: h.cfg.Auto.Allow, Deny: h.cfg.Auto.Deny})
	if err != nil {
		return err
	}
	h.policy = p
	if err := h.store.SeedSetting(SettingClassifier, h.cfg.Auto.Classifier); err != nil {
		return err
	}
	st := h.store.Setting(SettingClassifier)
	c, err := h.buildClassifier(st.Value)
	if err != nil {
		if st.Source != SourceUI {
			return fmt.Errorf("[auto] classifier %q is not a [[chat.models]] name", st.Value)
		}
		h.log.Warn("the classifier set in the editor is not a chat model; auto mode runs without one", "classifier", st.Value)
	}
	h.setClassifier(c)
	return nil
}

// buildClassifier makes the classifier of a chat model name ("" = none). The model
// and key are read here; a chat model save rebuilds it (reloadClassifier).
func (h *Hub) buildClassifier(name string) (*policy.Classifier, error) {
	if name == "" {
		return nil, nil
	}
	m, err := h.store.ChatModel(name)
	if err != nil {
		return nil, fmt.Errorf("classifier %q is not a chat model name", name)
	}
	key := ""
	if m.KeyRef != "" {
		key = h.secrets.Keys[m.KeyRef]
	}
	return &policy.Classifier{Upstream: messages.Upstream{BaseURL: m.BaseURL, Key: key, AuthHeader: m.AuthHeader, Kind: m.Kind},
		Model: m.Model}, nil
}

func (h *Hub) setClassifier(c *policy.Classifier) {
	h.classMu.Lock()
	h.classifier = c
	h.classMu.Unlock()
}

func (h *Hub) currentClassifier() *policy.Classifier {
	h.classMu.RLock()
	defer h.classMu.RUnlock()
	return h.classifier
}

// reloadClassifier rebuilds the classifier from the setting, after a chat model or
// the setting changed; the next classifier call uses it.
func (h *Hub) reloadClassifier() {
	name := h.store.Setting(SettingClassifier).Value
	c, err := h.buildClassifier(name)
	if err != nil {
		h.log.Warn("auto mode runs without a classifier", "err", err)
	}
	h.setClassifier(c)
}

// claudeMode is the --permission-mode claude gets for a session (DESIGN §18.1): auto
// on a router profile is mado auto, which runs claude in acceptEdits; a stored mode
// that is no longer offered (plan, dontAsk from before M4) starts as default.
func claudeMode(s *Session, prof *proto.Profile) string {
	mode := s.PermissionMode
	if !slices.Contains(permissionModes, mode) {
		return "default"
	}
	if mode == "auto" && prof != nil && prof.Kind == proto.KindRouter {
		return "acceptEdits"
	}
	return mode
}

// madoAuto reports a session that runs mado auto mode (whether or not the switch is on).
func (h *Hub) madoAuto(s *Session) bool {
	if s.IsChat() || s.PermissionMode != "auto" {
		return false
	}
	p, err := h.store.Profile(s.ProfileName)
	return err == nil && p.Kind == proto.KindRouter
}

// autoSwitchable reports a session whose right panel has the auto switch: a code
// session on a router profile in mado auto, or in default or acceptEdits, which the
// switch turns into mado auto mid-session (DESIGN §18.6).
func (h *Hub) autoSwitchable(s *Session) bool {
	if s.IsChat() || !slices.Contains([]string{"default", "acceptEdits", "auto"}, s.PermissionMode) {
		return false
	}
	p, err := h.store.Profile(s.ProfileName)
	return err == nil && p.Kind == proto.KindRouter
}

// autoOn: the hub decides this session's approvals by policy now.
func (h *Hub) autoOn(s *Session) bool { return s.AutoPolicy && h.madoAuto(s) }

// autoView is the session detail's "auto" object for the right panel.
func (h *Hub) autoView(s *Session) map[string]any {
	c := h.store.AutoCounts(s.ID)
	return map[string]any{"enabled": h.autoOn(s), "allowed": c.Allowed, "denied": c.Denied, "asked": c.Asked,
		"classifier": h.store.Setting(SettingClassifier).Value}
}

// autoApproval runs steps 2 to 5 of the policy for a request that is stored as pending
// and has no session rule. The classifier runs in its own goroutine, so the worker
// connection keeps reading meanwhile.
func (h *Hub) autoApproval(s *Session, a Approval) {
	roots := []string{}
	if m, err := h.store.Machine(s.MachineID); err == nil {
		roots = m.Projects
	}
	req := policy.Request{Tool: a.ToolName, Input: []byte(a.Input), CWD: s.CWD, Roots: roots}
	switch d, name := h.policy.Evaluate(req); d {
	case policy.Deny, policy.Allow:
		h.autoDecide(s, a, d, "rule:"+name, name)
		return
	}
	cl := h.currentClassifier()
	if cl == nil {
		h.autoAsk(s, a, "no rule decides this and no classifier is configured")
		return
	}
	if err := h.checkBudget(s.OwnerSub); err != nil {
		h.autoAsk(s, a, "the classifier is not used: "+err.Error())
		return
	}
	go func() {
		recent, last := "", ""
		if evs, err := h.store.LastEvents(s.ID, recentEvents); err == nil {
			recent, last = policy.RecentText(evs, recentBlocks)
		}
		res := cl.Decide(context.Background(), s.ID, policy.Input{Tool: a.ToolName, ToolInput: []byte(a.Input),
			CWD: s.CWD, Roots: roots, LastUser: last, Recent: recent})
		for _, c := range res.Calls {
			model := c.Model
			if model == "" {
				model = cl.Model
			}
			row := UsageRow{SessionID: s.ID, Kind: KindAuto, OwnerSub: s.OwnerSub, Model: model, MessageID: c.MessageID,
				Input: c.Usage.InputTokens, CacheRead: c.Usage.CacheReadInputTokens, CacheWrite: c.Usage.CacheCreationInputTokens,
				Output: c.Usage.OutputTokens}
			if err := h.store.RecordUsage(row, h.rates(model, false)); err != nil {
				h.log.Error("record usage", "session", s.ID, "err", err)
			}
		}
		// The user may have turned the switch off while the model thought.
		if cur, err := h.store.Session(s.ID); err != nil || !h.autoOn(cur) {
			h.autoAsk(s, a, "auto mode was switched off")
			return
		}
		switch res.Decision {
		case policy.Allow, policy.Deny:
			h.autoDecide(s, a, res.Decision, "classifier", res.Reason)
		default:
			h.autoAsk(s, a, "classifier: "+res.Reason)
		}
	}()
}

// autoDecide answers the request for the policy, records and publishes it. A request
// already answered (by a viewer, meanwhile) is left alone; one that cannot reach the
// machine goes to the user.
func (h *Hub) autoDecide(s *Session, a Approval, d policy.Decision, by, reason string) {
	decision, msg := proto.DecisionAllow, ""
	if d == policy.Deny {
		decision = proto.DecisionDeny
		label := reason
		if by == "classifier" {
			label = "classifier: " + reason
		}
		msg = policy.DenyMessage(label)
	}
	ok, err := h.store.DecideAuto(s.ID, a.RequestID, decision, by, string(d), reason)
	if err != nil || !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), codeToolTimeout)
	defer cancel()
	if err := h.sendToSession(ctx, s, &proto.ApprovalResponse{SessionID: s.ID, RequestID: a.RequestID, Decision: decision, Message: msg}); err != nil {
		h.store.ReopenApproval(s.ID, a.RequestID)
		h.autoAsk(s, a, "the decision ("+decision+", "+reason+") could not reach the machine")
		return
	}
	h.store.Audit("", "approval_auto", s.ID+" "+a.ToolName+" "+decision+" "+by)
	a.State, a.Decision, a.DecidedBy, a.Auto, a.Reason = "decided", decision, by, string(d), reason
	h.publishApproval(s.ID, a)
}

// autoAsk leaves the request to the user, with the policy's reason on the card.
func (h *Hub) autoAsk(s *Session, a Approval, reason string) {
	if !h.store.MarkAsked(s.ID, a.RequestID, reason) {
		return // answered meanwhile
	}
	h.store.Audit("", "approval_auto", s.ID+" "+a.ToolName+" ask")
	a.State, a.Auto, a.Reason = "pending", string(policy.Ask), reason
	h.publishApproval(s.ID, a)
}
