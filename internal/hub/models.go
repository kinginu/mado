package hub

import (
	"bytes"
	"sync"

	"github.com/kinginu/mado/internal/usage"
)

// modelCache keeps one usage accumulator per session (DESIGN §15). It is built from
// the stored events on first use and then fed every newly inserted event, so a gap
// that a resync fills later is counted too. Adding an event twice is harmless.
type modelCache struct {
	mu   sync.Mutex
	accs map[string]*usage.Acc
}

const maxCachedSessions = 4096

// usageRelevant is a cheap test before the JSON is parsed.
func usageRelevant(raw []byte) bool {
	return bytes.Contains(raw, []byte(`"type":"assistant"`)) || bytes.Contains(raw, []byte(`"type":"result"`)) ||
		bytes.Contains(raw, []byte(`"type":"mado_spawn"`))
}

func (h *Hub) sessionModels(sessionID string) usage.Stats {
	c := &h.models
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.accs[sessionID]; a != nil {
		return a.Stats()
	}
	a := usage.New()
	var after int64
	for {
		evs, err := h.store.UsageEvents(sessionID, after, 1000)
		if err != nil {
			return a.Stats() // not cached: try again next time
		}
		for _, e := range evs {
			a.Add(e.Raw)
			after = e.Seq
		}
		if len(evs) < 1000 {
			break
		}
	}
	if c.accs == nil || len(c.accs) >= maxCachedSessions {
		c.accs = map[string]*usage.Acc{}
	}
	c.accs[sessionID] = a
	return a.Stats()
}

// noteModelEvent feeds a newly stored event to a cached accumulator. It reports
// whether the event can change the session's model usage.
func (h *Hub) noteModelEvent(sessionID string, raw []byte) bool {
	if !usageRelevant(raw) {
		return false
	}
	c := &h.models
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.accs[sessionID]; a != nil {
		a.Add(raw)
	}
	return true
}
