package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/kinginu/mado/internal/chat"
)

// ParseChatModel validates a chat model from the editor: unknown fields (a "key")
// are rejected, the defaults applied, and key_ref must exist in the secrets file.
func ParseChatModel(b []byte, secrets *Secrets) (chat.Model, error) {
	var m chat.Model
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("invalid JSON: %v", err)
	}
	if dec.More() {
		return m, errors.New("invalid JSON: trailing data")
	}
	if err := m.Normalize(); err != nil {
		return m, err
	}
	if m.KeyRef != "" {
		if _, ok := secrets.Keys[m.KeyRef]; !ok {
			return m, fmt.Errorf("key_ref %q is not in the secrets file", m.KeyRef)
		}
	}
	return m, nil
}

// apiChatModelRow is a chat model on the editor page; the override fields are those
// of apiProfileRow (DESIGN §28.1). Priced reports a price row for Model (§28.2).
type apiChatModelRow struct {
	Name              string `json:"name"`
	Label             string `json:"label"`
	Model             string `json:"model"`
	BaseURL           string `json:"base_url"`
	KeyRef            string `json:"key_ref"`
	Source            string `json:"source"`
	Vision            bool   `json:"vision"`
	Default           bool   `json:"default"`
	Editable          bool   `json:"editable"`
	OverridesConfig   bool   `json:"overrides_config"`
	DiffersFromConfig bool   `json:"differs_from_config"`
	Priced            bool   `json:"priced"`
	JSON              string `json:"json"`
	ConfigJSON        string `json:"config_json"`
}

// chatModelDocJSON is the editor form of a stored chat model JSON ("" stays "").
func chatModelDocJSON(stored string) string {
	var m chat.Model
	if stored == "" || json.Unmarshal([]byte(stored), &m) != nil {
		return ""
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return string(b)
}

func (h *Hub) chatModelRows() []apiChatModelRow {
	ms, _ := h.store.ChatModels()
	states := h.store.overrideStates(ovChatModels)
	prices := h.priceTable()
	out := []apiChatModelRow{}
	for _, m := range ms {
		st := states[m.Name]
		b, _ := json.MarshalIndent(m, "", "  ")
		cj := chatModelDocJSON(st.ConfigJSON)
		_, priced := prices.Lookup(m.Model)
		out = append(out, apiChatModelRow{Name: m.Name, Label: m.Label, Model: m.Model, BaseURL: m.BaseURL, KeyRef: m.KeyRef,
			Source: st.Source, Vision: m.Vision, Default: m.Default, Editable: st.Source != SourceBuiltin,
			OverridesConfig: st.Overrides(), DiffersFromConfig: st.Overrides() && cj != string(b), Priced: priced,
			JSON: string(b), ConfigJSON: cj})
	}
	return out
}

const chatModelTemplate = `{
  "name": "my-model",
  "label": "My model",
  "base_url": "https://api.example.com",
  "model": "org/model",
  "key_ref": "",
  "context_window": 131072,
  "max_output": 32000,
  "vision": false
}`

// saveChatModel validates and stores a chat model from the editor.
func (h *Hub) saveChatModel(ws *WebSession, q editorSave) (string, error) {
	if len(q.JSON) > 16<<10 {
		return "", userError("model too large")
	}
	m, err := ParseChatModel([]byte(q.JSON), h.secrets)
	if err != nil {
		return "", userError(err.Error())
	}
	if q.Original != "" && q.Original != m.Name {
		return "", userError("renaming is not supported; create a new model instead")
	}
	// A hub.toml model may be edited too: the save is an override (DESIGN §28.1).
	if src := h.store.ChatModelSource(m.Name); q.Original == "" && src != "" {
		return "", userError("a chat model with this name already exists")
	}
	if m.Default {
		ms, _ := h.store.ChatModels()
		for _, o := range ms {
			if o.Default && o.Name != m.Name {
				return "", userError("model " + o.Name + " is already the default")
			}
		}
	}
	if err := h.store.PutChatModel(m, SourceUI); err != nil {
		return "", userError("could not save")
	}
	h.store.Audit(ws.User.Sub, "chat_model_save", m.Name)
	h.reloadClassifier() // the classifier reads its model and key once
	return m.Name, nil
}

// apiAdminRevertChatModel drops a chat model's override: the hub.toml version is
// live again, from the next turn.
func (h *Hub) apiAdminRevertChatModel(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	name := r.PathValue("name")
	if ok, err := h.store.RevertChatModel(name); err != nil || !ok {
		apiError(w, http.StatusUnprocessableEntity, "this chat model does not override hub.toml")
		return
	}
	h.store.Audit(ws.User.Sub, "chat_model_revert", name)
	h.reloadClassifier()
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (h *Hub) apiAdminSaveChatModel(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	var q editorSave
	if !decodeBody(w, r, &q) {
		return
	}
	name, err := h.saveChatModel(ws, q)
	if err != nil {
		apiError(w, http.StatusUnprocessableEntity, userMessage(err, "could not save"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}
