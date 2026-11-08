package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/kinginu/mado/internal/usage"
)

// The editor's Prices and Settings sections (DESIGN §28.2, §28.3).

// apiPriceRow is one price row; its numbers are [budgets] currency per million tokens.
type apiPriceRow struct {
	Model             string  `json:"model"`
	Input             float64 `json:"input"`
	Output            float64 `json:"output"`
	CacheRead         float64 `json:"cache_read"`
	CacheWrite        float64 `json:"cache_write"`
	Source            string  `json:"source"`
	Editable          bool    `json:"editable"`
	OverridesConfig   bool    `json:"overrides_config"`
	DiffersFromConfig bool    `json:"differs_from_config"`
	JSON              string  `json:"json"`
	ConfigJSON        string  `json:"config_json"`
}

func priceDocJSON(stored string) string {
	var p usage.Price
	if stored == "" || json.Unmarshal([]byte(stored), &p) != nil {
		return ""
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	return string(b)
}

func (h *Hub) priceRows() []apiPriceRow {
	states := h.store.overrideStates(ovPrices)
	out := []apiPriceRow{}
	for _, p := range h.priceTable() {
		st := states[p.Model]
		b, _ := json.MarshalIndent(p, "", "  ")
		cj := priceDocJSON(st.ConfigJSON)
		out = append(out, apiPriceRow{Model: p.Model, Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite,
			Source: st.Source, Editable: true, OverridesConfig: st.Overrides(), DiffersFromConfig: st.Overrides() && cj != string(b),
			JSON: string(b), ConfigJSON: cj})
	}
	return out
}

const priceTemplate = `{
  "model": "org/model",
  "input": 0,
  "output": 0,
  "cache_read": 0,
  "cache_write": 0
}`

// ParsePrice validates one [[prices]] entry from the editor (same field names and
// the same rules as hub.toml).
func ParsePrice(b []byte) (usage.Price, error) {
	var p usage.Price
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("invalid JSON: %v", err)
	}
	if dec.More() {
		return p, errors.New("invalid JSON: trailing data")
	}
	return p, usage.Prices{p}.Validate()
}

func (h *Hub) savePrice(ws *WebSession, q editorSave) (string, error) {
	if len(q.JSON) > 4<<10 {
		return "", userError("price too large")
	}
	p, err := ParsePrice([]byte(q.JSON))
	if err != nil {
		return "", userError(err.Error())
	}
	if q.Original != "" && q.Original != p.Model {
		return "", userError("renaming is not supported; add a new price instead")
	}
	if src := h.store.PriceSource(p.Model); q.Original == "" && src != "" {
		return "", userError("a price for this model already exists")
	}
	if err := h.store.PutPrice(p, SourceUI); err != nil {
		return "", userError("could not save")
	}
	h.invalidatePrices()
	h.store.Audit(ws.User.Sub, "price_save", p.Model)
	return p.Model, nil
}

func (h *Hub) apiAdminSavePrice(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	var q editorSave
	if !decodeBody(w, r, &q) {
		return
	}
	name, err := h.savePrice(ws, q)
	if err != nil {
		apiError(w, http.StatusUnprocessableEntity, userMessage(err, "could not save"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

// apiAdminRevertPrice (?model=) puts a price back to its [[prices]] version.
func (h *Hub) apiAdminRevertPrice(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	model := r.URL.Query().Get("model")
	if ok, err := h.store.RevertPrice(model); err != nil || !ok {
		apiError(w, http.StatusUnprocessableEntity, "this price does not override hub.toml")
		return
	}
	h.invalidatePrices()
	h.store.Audit(ws.User.Sub, "price_revert", model)
	writeJSON(w, http.StatusOK, map[string]string{"name": model})
}

// apiAdminDeletePrice (?model=) removes a price added in the editor.
func (h *Hub) apiAdminDeletePrice(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	model := r.URL.Query().Get("model")
	if ok, err := h.store.DeletePrice(model); err != nil || !ok {
		apiError(w, http.StatusUnprocessableEntity, "only a price added here can be deleted; a hub.toml price is reverted")
		return
	}
	h.invalidatePrices()
	h.store.Audit(ws.User.Sub, "price_delete", model)
	writeJSON(w, http.StatusOK, map[string]string{"name": model})
}

// apiSettingRow is one overridable setting; Value and ConfigValue are chat model
// names or "".
type apiSettingRow struct {
	Key             string `json:"key"`
	Value           string `json:"value"`
	ConfigValue     string `json:"config_value"`
	Source          string `json:"source"`
	OverridesConfig bool   `json:"overrides_config"`
}

func (h *Hub) settingRows() []apiSettingRow {
	out := []apiSettingRow{}
	for _, k := range settingKeys {
		st := h.store.Setting(k)
		out = append(out, apiSettingRow{k, st.Value, st.ConfigValue, st.Source, st.Source == SourceUI})
	}
	return out
}

// saveSetting stores a ui value: "" or a chat model name. The classifier is rebuilt
// at once; the concierge reads its model at the start of each session.
func (h *Hub) saveSetting(ws *WebSession, key, value string) error {
	if !slices.Contains(settingKeys, key) {
		return userError("unknown setting")
	}
	if value != "" {
		if _, err := h.store.ChatModel(value); err != nil {
			return userError(fmt.Sprintf("%q is not a chat model name", value))
		}
	}
	if err := h.store.PutSetting(key, value); err != nil {
		return userError("could not save")
	}
	if key == SettingClassifier {
		h.reloadClassifier()
	}
	h.store.Audit(ws.User.Sub, "setting_save", key+"="+value)
	return nil
}

func (h *Hub) apiAdminSaveSetting(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	var q struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	if err := h.saveSetting(ws, q.Key, q.Value); err != nil {
		apiError(w, http.StatusUnprocessableEntity, userMessage(err, "could not save"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": q.Key})
}

func (h *Hub) apiAdminRevertSetting(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	key := r.PathValue("key")
	if ok, err := h.store.RevertSetting(key); err != nil || !ok {
		apiError(w, http.StatusUnprocessableEntity, "this setting does not override hub.toml")
		return
	}
	if key == SettingClassifier {
		h.reloadClassifier()
	}
	h.store.Audit(ws.User.Sub, "setting_revert", key)
	writeJSON(w, http.StatusOK, map[string]string{"key": key})
}
