package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kinginu/mado/internal/messages"
)

// The upstream's model list (DESIGN §28.6): the editor asks the hub for
// <base_url>/v1/models of an upstream or chat model it shows, to mark a model id
// that is retiring or no longer listed. AI& answers without a key and adds a display
// name, top-level input_per_1m, output_per_1m and cached_input_per_1m (strings),
// currency "usd" and deprecation_date (or null); other servers may return only ids.

const (
	upstreamModelsTTL     = 10 * time.Minute
	upstreamModelsTimeout = 10 * time.Second
	upstreamModelsMax     = 4 << 20
)

// upstreamModel is one normalised entry; absent fields stay empty.
type upstreamModel struct {
	ID           string   `json:"id"`
	Name         string   `json:"name,omitempty"`
	DeprecatedAt string   `json:"deprecated_at,omitempty"`
	Input        *float64 `json:"input,omitempty"`
	Output       *float64 `json:"output,omitempty"`
	CachedInput  *float64 `json:"cached_input,omitempty"`
	Currency     string   `json:"currency,omitempty"`
}

type upstreamModelCache struct {
	mu sync.Mutex
	m  map[string]upstreamModelsEntry
}

type upstreamModelsEntry struct {
	at     time.Time
	models []upstreamModel
}

// modelsURL is the model list under a base URL: an OpenAI-style API root that ends
// in /v1 gets /models, an Anthropic-style root /v1/models.
func modelsURL(baseURL string) string {
	b := strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(b, "/v1") {
		return b + "/models"
	}
	return b + "/v1/models"
}

// normaliseModels reads a model list in whatever shape the server uses: a bare
// array, or an object with data, models or result. Entries without a string id are
// skipped; unknown fields are ignored.
func normaliseModels(b []byte) ([]upstreamModel, error) {
	var top any
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, fmt.Errorf("not JSON: %v", err)
	}
	var list []any
	switch v := top.(type) {
	case []any:
		list = v
	case map[string]any:
		for _, k := range []string{"data", "models", "result"} {
			if l, ok := v[k].([]any); ok {
				list = l
				break
			}
		}
		if list == nil {
			return nil, errors.New("no model list in the answer")
		}
	default:
		return nil, errors.New("no model list in the answer")
	}
	out := []upstreamModel{}
	for _, e := range list {
		obj, ok := e.(map[string]any)
		if !ok {
			continue
		}
		id, _ := obj["id"].(string)
		if id == "" {
			continue
		}
		m := upstreamModel{ID: id, Name: firstString(obj, "name", "display_name")}
		m.DeprecatedAt = firstString(obj, "deprecated_at", "deprecation_date", "deprecationDate", "deprecated_on",
			"retirement_date", "retires_at", "sunset_at", "end_of_life")
		if d, ok := obj["deprecation"].(map[string]any); ok && m.DeprecatedAt == "" {
			m.DeprecatedAt = firstString(d, "date", "at", "deprecated_at")
		}
		// Prices sit on the entry or in a nested pricing object.
		srcs := []map[string]any{obj}
		for _, k := range []string{"pricing", "price", "prices", "cost"} {
			if p, ok := obj[k].(map[string]any); ok {
				srcs = append(srcs, p)
			}
		}
		for _, src := range srcs {
			if m.Input == nil {
				m.Input = firstNumber(src, "input_per_1m", "input", "input_price")
			}
			if m.Output == nil {
				m.Output = firstNumber(src, "output_per_1m", "output", "output_price")
			}
			if m.CachedInput == nil {
				m.CachedInput = firstNumber(src, "cached_input_per_1m", "cached_input", "cache_read", "cached", "input_cached", "cached_input_price")
			}
			if m.Currency == "" {
				m.Currency = firstString(src, "currency")
			}
		}
		out = append(out, m)
	}
	return out, nil
}

func firstString(o map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := o[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func firstNumber(o map[string]any, keys ...string) *float64 {
	for _, k := range keys {
		switch v := o[k].(type) {
		case float64:
			return &v
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				return &f
			}
		}
	}
	return nil
}

// knownUpstream finds the auth header rule of a base URL and key_ref pair that a
// stored profile upstream or chat model uses. Only such pairs are fetched, so the
// endpoint cannot send a key to a URL no entry names.
func (h *Hub) knownUpstream(baseURL, keyRef string) (authHeader string, ok bool) {
	if ps, err := h.store.Profiles(); err == nil {
		for _, p := range ps {
			for _, u := range p.Upstreams {
				if u.BaseURL == baseURL && u.KeyRef == keyRef {
					return u.AuthHeader, true
				}
			}
		}
	}
	if ms, err := h.store.ChatModels(); err == nil {
		for _, m := range ms {
			if m.BaseURL == baseURL && m.KeyRef == keyRef {
				return m.AuthHeader, true
			}
		}
	}
	return "", false
}

// upstreamModels returns the normalised list, from the cache when it is fresh.
func (h *Hub) upstreamModels(ctx context.Context, baseURL, keyRef, authHeader string) ([]upstreamModel, error) {
	ck := baseURL + "\x00" + keyRef
	c := &h.upModels
	c.mu.Lock()
	if e, ok := c.m[ck]; ok && time.Since(e.at) < upstreamModelsTTL {
		c.mu.Unlock()
		return e.models, nil
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, upstreamModelsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL(baseURL), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if keyRef != "" {
		messages.SetAuth(req.Header, baseURL, authHeader, h.secrets.Keys[keyRef])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the upstream answered %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, upstreamModelsMax))
	if err != nil {
		return nil, err
	}
	ms, err := normaliseModels(b)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]upstreamModelsEntry{}
	}
	c.m[ck] = upstreamModelsEntry{at: time.Now(), models: ms}
	c.mu.Unlock()
	return ms, nil
}

// apiAdminUpstreamModels is GET /api/admin/upstream-models?base_url=&key_ref=.
func (h *Hub) apiAdminUpstreamModels(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	baseURL, keyRef := r.URL.Query().Get("base_url"), r.URL.Query().Get("key_ref")
	auth, ok := h.knownUpstream(baseURL, keyRef)
	if !ok {
		apiError(w, http.StatusNotFound, "no profile upstream or chat model uses this base_url and key_ref")
		return
	}
	ms, err := h.upstreamModels(r.Context(), baseURL, keyRef, auth)
	if err != nil {
		apiError(w, http.StatusBadGateway, "could not load the model list: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": ms})
}
