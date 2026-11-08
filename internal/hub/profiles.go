package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/roles"
	"github.com/kinginu/mado/internal/router"
)

// profileDoc is the editable JSON form of a profile (DESIGN §5). It has no key
// field: keys live only in the secrets file and are referenced by key_ref.
type profileDoc struct {
	Name      string                 `json:"name"`
	Kind      string                 `json:"kind"`
	Main      string                 `json:"main,omitempty"`
	Aliases   map[string]string      `json:"aliases,omitempty"`
	Upstreams map[string]upstreamDoc `json:"upstreams,omitempty"`
	Roles     *rolesDoc              `json:"roles,omitempty"`
}

type rolesDoc struct {
	Enabled     *bool  `json:"enabled,omitempty"`
	Implementer string `json:"implementer,omitempty"`
	Researcher  string `json:"researcher,omitempty"`
	Simple      string `json:"simple,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	Enforce     string `json:"enforce,omitempty"`
}

type upstreamDoc struct {
	BaseURL       string `json:"base_url"`
	Model         string `json:"model"`
	KeyRef        string `json:"key_ref,omitempty"`
	AuthHeader    string `json:"auth_header,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	MaxOutput     int    `json:"max_output,omitempty"`
	// DESIGN §22.1
	Kind             string `json:"kind,omitempty"`
	TimeoutFirstByte string `json:"timeout_first_byte,omitempty"`
	TimeoutTotal     string `json:"timeout_total,omitempty"`
}

func docOf(p proto.Profile) profileDoc {
	d := profileDoc{Name: p.Name, Kind: p.Kind, Main: p.Main, Aliases: p.Aliases}
	if r := p.Roles; r != nil {
		d.Roles = &rolesDoc{r.Enabled, r.Implementer, r.Researcher, r.Simple, r.Prompt, r.Enforce}
	}
	if len(p.Upstreams) > 0 {
		d.Upstreams = map[string]upstreamDoc{}
		for k, u := range p.Upstreams {
			d.Upstreams[k] = upstreamDoc{u.BaseURL, u.Model, u.KeyRef, u.AuthHeader, u.ContextWindow, u.MaxOutput,
				u.Kind, u.TimeoutFirstByte, u.TimeoutTotal}
		}
	}
	return d
}

// ParseProfile validates profile JSON from the editor: unknown fields (including
// "key") are rejected, names must be safe, router names may not contain '/', and
// every key_ref must exist in the secrets file.
func ParseProfile(b []byte, secrets *Secrets) (proto.Profile, error) {
	var d profileDoc
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return proto.Profile{}, fmt.Errorf("invalid JSON: %v", err)
	}
	if dec.More() {
		return proto.Profile{}, errors.New("invalid JSON: trailing data")
	}
	if !validName(d.Name) {
		return proto.Profile{}, errors.New("name: use 1-64 of [A-Za-z0-9_-]")
	}
	p := proto.Profile{Name: d.Name, Kind: d.Kind, Main: d.Main, Aliases: d.Aliases}
	if r := d.Roles; r != nil {
		p.Roles = &proto.Roles{Enabled: r.Enabled, Implementer: r.Implementer, Researcher: r.Researcher, Simple: r.Simple, Prompt: r.Prompt, Enforce: r.Enforce}
	}
	if err := roles.Validate(p.Roles); err != nil {
		return p, err
	}
	switch d.Kind {
	case proto.KindSubscription:
		if d.Main != "" || len(d.Aliases) > 0 || len(d.Upstreams) > 0 {
			return p, errors.New("a subscription profile has no main, aliases or upstreams (roles are allowed)")
		}
		return p, nil
	case proto.KindRouter:
	default:
		return p, errors.New(`kind must be "router" or "subscription"`)
	}
	p.Upstreams = map[string]proto.Upstream{}
	for k, u := range d.Upstreams {
		if u.KeyRef != "" {
			if _, ok := secrets.Keys[u.KeyRef]; !ok {
				return p, fmt.Errorf("upstream %q: key_ref %q is not in the secrets file", k, u.KeyRef)
			}
		}
		p.Upstreams[k] = proto.Upstream{BaseURL: u.BaseURL, Model: u.Model, KeyRef: u.KeyRef, AuthHeader: u.AuthHeader,
			ContextWindow: u.ContextWindow, MaxOutput: u.MaxOutput, Kind: u.Kind, TimeoutFirstByte: u.TimeoutFirstByte,
			TimeoutTotal: u.TimeoutTotal}
	}
	if p.Main == "" {
		return p, errors.New("main is required for a router profile")
	}
	if err := router.Validate(p); err != nil {
		return p, err
	}
	return p, nil
}

// apiProfileRow is a profile on the editor page (DESIGN §28.1). Every row but the
// builtin one is editable; OverridesConfig marks a ui override of a hub.toml entry,
// whose hub.toml version is ConfigJSON ("" for an entry not in hub.toml).
type apiProfileRow struct {
	Name              string           `json:"name"`
	Kind              string           `json:"kind"`
	Source            string           `json:"source"`
	Summary           string           `json:"summary"`
	Editable          bool             `json:"editable"`
	OverridesConfig   bool             `json:"overrides_config"`
	DiffersFromConfig bool             `json:"differs_from_config"`
	JSON              string           `json:"json"`
	ConfigJSON        string           `json:"config_json"`
	Upstreams         []apiUpstreamRow `json:"upstreams"`
}

// apiUpstreamRow is one upstream of a router profile: where its model id is checked
// against the price table and the upstream's model list (DESIGN §28.2, §28.6).
type apiUpstreamRow struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	KeyRef  string `json:"key_ref"`
	Priced  bool   `json:"priced"`
}

// profileDocJSON is the editor form of a stored profile JSON ("" stays "").
func profileDocJSON(stored string) string {
	if stored == "" {
		return ""
	}
	var p proto.Profile
	if json.Unmarshal([]byte(stored), &p) != nil {
		return ""
	}
	b, _ := json.MarshalIndent(docOf(p), "", "  ")
	return string(b)
}

func (h *Hub) profileRows() []apiProfileRow {
	ps, _ := h.store.Profiles()
	states := h.store.overrideStates(ovProfiles)
	prices := h.priceTable()
	rows := []apiProfileRow{}
	for _, p := range ps {
		st := states[p.Name]
		sum := "plain claude, normal login"
		ups := []apiUpstreamRow{}
		if p.Kind == proto.KindRouter {
			names := make([]string, 0, len(p.Upstreams))
			for n := range p.Upstreams {
				names = append(names, n)
			}
			slices.Sort(names)
			sum = "main " + p.Main + " · " + strings.Join(names, ", ")
			for _, n := range names {
				u := p.Upstreams[n]
				_, priced := prices.Lookup(u.Model)
				ups = append(ups, apiUpstreamRow{n, u.BaseURL, u.Model, u.KeyRef, priced})
			}
		}
		b, _ := json.MarshalIndent(docOf(p), "", "  ")
		cj := profileDocJSON(st.ConfigJSON)
		rows = append(rows, apiProfileRow{Name: p.Name, Kind: p.Kind, Source: st.Source, Summary: sum,
			Editable: st.Source != SourceBuiltin, OverridesConfig: st.Overrides(), DiffersFromConfig: st.Overrides() && cj != string(b),
			JSON: string(b), ConfigJSON: cj, Upstreams: ups})
	}
	return rows
}

const profileTemplate = `{
  "name": "my-profile",
  "kind": "router",
  "main": "oc-strong",
  "aliases": {"opus": "oc-strong", "sonnet": "oc-strong", "haiku": "oc-strong"},
  "upstreams": {
    "oc-strong": {"base_url": "http://host:8000", "model": "org/Model", "key_ref": "", "context_window": 131072, "max_output": 32000}
  },
  "roles": {"enabled": true, "implementer": "opus", "researcher": "opus", "simple": "sonnet", "prompt": "", "enforce": "edits"}
}`

func (h *Hub) keyNames() []string {
	out := []string{}
	for k := range h.secrets.Keys {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// apiAdminProfiles is the editor page: profiles, chat models and prices with their
// JSON, the two settings, the known key_refs and the templates for new entries.
func (h *Hub) apiAdminProfiles(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	writeJSON(w, http.StatusOK, map[string]any{"profiles": h.profileRows(), "chat_models": h.chatModelRows(),
		"prices": h.priceRows(), "settings": h.settingRows(), "currency": h.cfg.Budgets.Currency,
		"keys": h.keyNames(), "chat_enabled": h.cfg.Chat.Enabled,
		"profile_template": profileTemplate, "chat_model_template": chatModelTemplate, "price_template": priceTemplate})
}

// editorSave is the body of the profile and chat model saves: the JSON text and the
// name it was loaded under ("" for a new entry).
type editorSave struct {
	JSON     string `json:"json"`
	Original string `json:"original"`
}

// saveProfile validates and stores a profile from the editor; errors are userErrors.
func (h *Hub) saveProfile(ws *WebSession, q editorSave) (string, error) {
	if len(q.JSON) > 64<<10 {
		return "", userError("profile too large")
	}
	p, err := ParseProfile([]byte(q.JSON), h.secrets)
	if err != nil {
		return "", userError(err.Error())
	}
	if q.Original != "" && q.Original != p.Name {
		return "", userError("renaming is not supported; create a new profile instead")
	}
	// A hub.toml profile may be edited too: the save is an override (DESIGN §28.1).
	switch src := h.store.ProfileSource(p.Name); {
	case q.Original == "" && src != "":
		return "", userError("a profile with this name already exists")
	case src == SourceBuiltin:
		return "", userError("this profile is built into the hub and is read-only")
	}
	if err := h.store.PutProfileFrom(p, SourceUI); err != nil {
		return "", userError("could not save")
	}
	h.store.Audit(ws.User.Sub, "profile_save", p.Name)
	h.refreshProfileSessions(p.Name)
	return p.Name, nil
}

// apiAdminRevertProfile drops a profile's override: the hub.toml version is live again.
func (h *Hub) apiAdminRevertProfile(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	name := r.PathValue("name")
	if ok, err := h.store.RevertProfile(name); err != nil || !ok {
		apiError(w, http.StatusUnprocessableEntity, "this profile does not override hub.toml")
		return
	}
	h.store.Audit(ws.User.Sub, "profile_revert", name)
	h.refreshProfileSessions(name)
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

// refreshProfileSessions pushes a changed router profile to the live sessions that
// use it, as after a reconnect (Refresh): the worker re-registers the session's
// router token, so the next request resolves the new upstreams (DESIGN §28.4).
// Aliases are in claude's environment and change only with a Resume.
func (h *Hub) refreshProfileSessions(name string) {
	prof, err := h.store.Profile(name)
	if err != nil || prof.Kind != proto.KindRouter {
		return
	}
	resolved := h.resolveProfile(*prof)
	h.mu.Lock()
	wcs := make([]*workerConn, 0, len(h.workers))
	for _, wc := range h.workers {
		wcs = append(wcs, wc)
	}
	h.mu.Unlock()
	for _, wc := range wcs {
		ss, err := h.store.SessionsOnMachine(wc.machineID)
		if err != nil {
			continue
		}
		for _, s := range ss {
			if s.Kind != KindCode || s.ProfileName != name || !resumable[s.State] {
				continue
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_ = wc.send(ctx, &proto.StartSession{SessionID: s.ID, CWD: s.CWD, Profile: resolved, MainModel: s.MainModel, Refresh: true})
			}()
		}
	}
}

func (h *Hub) apiAdminSaveProfile(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	var q editorSave
	if !decodeBody(w, r, &q) {
		return
	}
	name, err := h.saveProfile(ws, q)
	if err != nil {
		apiError(w, http.StatusUnprocessableEntity, userMessage(err, "could not save"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}
