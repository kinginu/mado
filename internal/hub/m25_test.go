package hub

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/usage"
)

// setPrices seeds the price table as hub.toml would and drops the rates cache.
func setPrices(t *testing.T, h *Hub, ps usage.Prices) {
	t.Helper()
	if err := h.store.SeedPrices(ps); err != nil {
		t.Fatal(err)
	}
	h.invalidatePrices()
}

func routerProfile(name, model string) proto.Profile {
	return proto.Profile{Name: name, Kind: proto.KindRouter, Main: "oc-a",
		Upstreams: map[string]proto.Upstream{"oc-a": {BaseURL: "http://up:1", Model: model}}}
}

// The seeding rule of DESIGN §28.1, across restarts.
func TestOverrideSeedingRules(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model := func(name string) string {
		p, err := st.Profile(name)
		if err != nil {
			return "-"
		}
		return p.Upstreams["oc-a"].Model
	}
	state := func(name string) ovState { return st.overrideStates(ovProfiles)[name] }
	// Start 1: two config rows.
	if err := st.SeedProfiles([]proto.Profile{routerProfile("a", "m1"), routerProfile("b", "m1")}); err != nil {
		t.Fatal(err)
	}
	if s := state("a"); s.Source != SourceConfig || s.ConfigJSON == "" || s.Overrides() {
		t.Fatalf("config row %+v", s)
	}
	// The editor overrides a; a ui-only c is made.
	st.PutProfileFrom(routerProfile("a", "m-ui"), SourceUI)
	st.PutProfileFrom(routerProfile("c", "m-c"), SourceUI)
	if !state("a").Overrides() || state("c").Overrides() {
		t.Fatalf("override flags a %+v c %+v", state("a"), state("c"))
	}
	// Start 2: hub.toml changes a and b. The override survives, its config_json moves.
	if err := st.SeedProfiles([]proto.Profile{routerProfile("a", "m2"), routerProfile("b", "m2")}); err != nil {
		t.Fatal(err)
	}
	if model("a") != "m-ui" || model("b") != "m2" || !strings.Contains(state("a").ConfigJSON, `"m2"`) {
		t.Fatalf("after restart a=%s b=%s %+v", model("a"), model("b"), state("a"))
	}
	// Revert restores the current hub.toml version.
	if ok, err := st.RevertProfile("a"); !ok || err != nil || model("a") != "m2" || state("a").Source != SourceConfig {
		t.Fatalf("revert %v %v %s %+v", ok, err, model("a"), state("a"))
	}
	if ok, _ := st.RevertProfile("c"); ok {
		t.Error("a ui-only profile reverted")
	}
	// Start 3: a overridden again, then a and b leave hub.toml. b (config) goes, a
	// (ui) stays with an empty config_json, c is untouched, the builtin stays.
	st.PutProfileFrom(proto.Profile{Name: "subscription", Kind: proto.KindSubscription}, SourceBuiltin)
	st.PutProfileFrom(routerProfile("a", "m-ui2"), SourceUI)
	if err := st.SeedProfiles(nil); err != nil {
		t.Fatal(err)
	}
	if model("a") != "m-ui2" || model("b") != "-" || model("c") != "m-c" || state("a").ConfigJSON != "" || state("a").Source != SourceUI ||
		state("subscription").Source != SourceBuiltin {
		t.Fatalf("after removal a=%s b=%s c=%s %+v", model("a"), model("b"), model("c"), state("a"))
	}
	// A hub.toml entry that comes back under a ui name keeps the ui JSON.
	st.SeedProfiles([]proto.Profile{routerProfile("c", "m-toml")})
	if model("c") != "m-c" || !state("c").Overrides() {
		t.Fatalf("c %s %+v", model("c"), state("c"))
	}
	// Settings follow the same rule.
	st.SeedSetting(SettingClassifier, "x")
	st.PutSetting(SettingClassifier, "y")
	st.SeedSetting(SettingClassifier, "z")
	if r := st.Setting(SettingClassifier); r.Value != "y" || r.ConfigValue != "z" || r.Source != SourceUI {
		t.Fatalf("setting %+v", r)
	}
	if ok, _ := st.RevertSetting(SettingClassifier); !ok || st.Setting(SettingClassifier).Value != "z" {
		t.Fatalf("setting revert %+v", st.Setting(SettingClassifier))
	}
}

// A profile save overrides a hub.toml profile, reaches the live sessions on it, and
// the revert endpoint puts it back (DESIGN §28.1, §28.4).
func TestProfileOverrideAndRevert(t *testing.T) {
	x := newM2(t)
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	live := x.session(t, "r", proto.StateIdle, "11111111-1111-4111-8111-111111111111")
	js := `{"name":"r","kind":"router","main":"oc-a","upstreams":{"oc-a":{"base_url":"http://up:1","model":"org/B","key_ref":"k"}}}`
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/admin/profiles", map[string]string{"original": "r", "json": js}, nil); st != 200 {
		t.Fatalf("save %d", st)
	}
	waitRefresh := func(want string) {
		t.Helper()
		select {
		case f := <-x.fw.ch:
			m, ok := f.(*proto.StartSession)
			if !ok || !m.Refresh || m.SessionID != live || m.Profile.Upstreams["oc-a"].Model != want || m.Profile.Upstreams["oc-a"].Key != "the-key" {
				t.Fatalf("refresh frame %+v", f)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no refresh frame")
		}
	}
	waitRefresh("org/B")
	var list struct {
		Profiles []apiProfileRow `json:"profiles"`
	}
	apiDo(t, x.c, x.th.srv.URL, csrf, "GET", "/api/admin/profiles", nil, &list)
	var r apiProfileRow
	for _, p := range list.Profiles {
		if p.Name == "r" {
			r = p
		}
	}
	if r.Source != SourceUI || !r.Editable || !r.OverridesConfig || !r.DiffersFromConfig || !strings.Contains(r.ConfigJSON, "org/A") ||
		len(r.Upstreams) != 1 || r.Upstreams[0].Model != "org/B" || r.Upstreams[0].Priced {
		t.Fatalf("row %+v", r)
	}
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "DELETE", "/api/admin/profiles/r/override", nil, nil); st != 200 {
		t.Fatalf("revert %d", st)
	}
	waitRefresh("org/A")
	if p, _ := x.th.store.Profile("r"); p.Upstreams["oc-a"].Model != "org/A" || x.th.store.ProfileSource("r") != SourceConfig {
		t.Fatalf("reverted %+v", p)
	}
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "DELETE", "/api/admin/profiles/r/override", nil, nil); st != 422 {
		t.Errorf("second revert %d", st)
	}
	audit, _ := x.th.store.AuditAll(50)
	got := fmt.Sprint(audit)
	if !strings.Contains(got, "profile_save") || !strings.Contains(got, "profile_revert") {
		t.Errorf("audit %s", got)
	}
}

// Prices live in the store; rates() sees a save and a revert at once (§28.2).
func TestPricesStoreAndRatesCache(t *testing.T) {
	x := newChatHub(t)
	csrf := apiCSRF(t, x.c, x.srv.URL)
	if r := x.h.rates("zai-org/glm-5.3", false); r.Input != 0.6 {
		t.Fatalf("seeded rate %+v", r)
	}
	post := func(js, orig string) (int, string) {
		var out struct{ Name, Error string }
		st := apiDo(t, x.c, x.srv.URL, csrf, "POST", "/api/admin/prices", map[string]string{"json": js, "original": orig}, &out)
		return st, out.Name + out.Error
	}
	if st, msg := post(`{"model":"zai-org/glm-*","input":1,"output":2,"cache_read":0.5}`, "zai-org/glm-*"); st != 200 {
		t.Fatalf("override %d %s", st, msg)
	}
	if r := x.h.rates("zai-org/glm-5.3", false); r.Input != 1 || r.Output != 2 || r.CacheRead != 0.5 {
		t.Fatalf("rate after save %+v", r)
	}
	for _, bad := range []string{`{"model":"m","input":-1}`, `{"model":"","input":1}`, `{"model":"m","inputs":1}`} {
		if st, _ := post(bad, ""); st != 422 {
			t.Errorf("%s accepted: %d", bad, st)
		}
	}
	if st, msg := post(`{"model":"zai-org/glm-*","input":1}`, ""); st != 422 || !strings.Contains(msg, "already exists") {
		t.Errorf("duplicate add %d %s", st, msg)
	}
	if st, _ := post(`{"model":"org/new","input":3,"output":4}`, ""); st != 200 || x.h.rates("org/new", false).Output != 4 {
		t.Fatalf("add %d", st)
	}
	var list struct {
		Prices     []apiPriceRow     `json:"prices"`
		ChatModels []apiChatModelRow `json:"chat_models"`
		Currency   string            `json:"currency"`
	}
	apiDo(t, x.c, x.srv.URL, csrf, "GET", "/api/admin/profiles", nil, &list)
	if len(list.Prices) != 2 || list.Prices[0].Model != "org/new" || list.Prices[0].OverridesConfig || !list.Prices[1].OverridesConfig ||
		!list.Prices[1].DiffersFromConfig || !list.ChatModels[0].Priced || list.Currency == "" {
		t.Fatalf("list %+v %s", list.Prices, list.Currency)
	}
	q := "?model=" + url.QueryEscape("zai-org/glm-*")
	if st := apiDo(t, x.c, x.srv.URL, csrf, "DELETE", "/api/admin/prices"+q, nil, nil); st != 422 {
		t.Errorf("deleted a hub.toml price: %d", st)
	}
	if st := apiDo(t, x.c, x.srv.URL, csrf, "DELETE", "/api/admin/prices/override"+q, nil, nil); st != 200 || x.h.rates("zai-org/glm-5.3", false).Input != 0.6 {
		t.Fatalf("revert %d", st)
	}
	if st := apiDo(t, x.c, x.srv.URL, csrf, "DELETE", "/api/admin/prices?model=org/new", nil, nil); st != 200 || x.h.rates("org/new", false).Output != 0 {
		t.Fatalf("delete %d", st)
	}
	// Without a price the chat model row says so.
	apiDo(t, x.c, x.srv.URL, csrf, "POST", "/api/admin/prices", map[string]string{"json": `{"model":"zai-org/glm-*","input":1}`, "original": "zai-org/glm-*"}, nil)
	setPrices(t, x.h, nil) // hub.toml drops [[prices]]: the override stays
	if _, ok := x.h.priceTable().Lookup("zai-org/glm-5.3"); !ok {
		t.Error("the price override was dropped with hub.toml's entry")
	}
}

// The two settings: the classifier is rebuilt on save, a non-chat-model name is
// refused, the concierge reads its model per session (§28.3).
func TestSettingsClassifierAndConcierge(t *testing.T) {
	x := newChatHub(t)
	csrf := apiCSRF(t, x.c, x.srv.URL)
	if x.h.currentClassifier() != nil {
		t.Fatal("a classifier without [auto] classifier")
	}
	save := func(key, value string) int {
		return apiDo(t, x.c, x.srv.URL, csrf, "POST", "/api/admin/settings", map[string]string{"key": key, "value": value}, nil)
	}
	if st := save(SettingClassifier, "glm-flash"); st != 200 {
		t.Fatalf("save %d", st)
	}
	c := x.h.currentClassifier()
	if c == nil || c.Model != "zai-org/glm-5.3-flash" || c.Upstream.BaseURL != x.llm.srv.URL || c.Upstream.Key != "sk-test" {
		t.Fatalf("classifier %+v", c)
	}
	if st := save(SettingClassifier, "nope"); st != 422 || x.h.currentClassifier() != c {
		t.Errorf("unknown model: %d", st)
	}
	if st := save("auto.other", ""); st != 422 {
		t.Errorf("unknown key: %d", st)
	}
	// A chat model save rebuilds the classifier from the new JSON.
	js := fmt.Sprintf(`{"name":"glm-flash","base_url":%q,"model":"zai-org/glm-5.4-flash","key_ref":"aiand"}`, x.llm.srv.URL)
	if st := apiDo(t, x.c, x.srv.URL, csrf, "POST", "/api/admin/chat-models", map[string]string{"json": js, "original": "glm-flash"}, nil); st != 200 {
		t.Fatalf("chat model save %d", st)
	}
	if c := x.h.currentClassifier(); c == nil || c.Model != "zai-org/glm-5.4-flash" {
		t.Fatalf("classifier after model save %+v", c)
	}
	if v := x.h.autoView(&Session{})["classifier"]; v != "glm-flash" {
		t.Errorf("auto view %v", v)
	}
	if st := apiDo(t, x.c, x.srv.URL, csrf, "DELETE", "/api/admin/settings/auto.classifier/override", nil, nil); st != 200 || x.h.currentClassifier() != nil {
		t.Fatalf("revert %d", st)
	}
	if x.h.conciergeModel() != "glm-5.3" {
		t.Fatalf("concierge default %s", x.h.conciergeModel())
	}
	if st := save(SettingConciergeModel, "glm-flash"); st != 200 || x.h.conciergeModel() != "glm-flash" {
		t.Fatalf("concierge save %d %s", st, x.h.conciergeModel())
	}
	// A restart keeps the override; a classifier override naming a vanished model only
	// warns.
	x.h.cfg.Concierge.Model = "glm-5.3"
	if err := x.h.setupConcierge(); err != nil || x.h.conciergeModel() != "glm-flash" {
		t.Fatalf("restart %v %s", err, x.h.conciergeModel())
	}
	x.store.PutSetting(SettingClassifier, "gone")
	if err := x.h.setupAuto(); err != nil || x.h.currentClassifier() != nil {
		t.Fatalf("vanished classifier %v", err)
	}
	var list struct {
		Settings []apiSettingRow `json:"settings"`
	}
	apiDo(t, x.c, x.srv.URL, csrf, "GET", "/api/admin/profiles", nil, &list)
	if fmt.Sprint(list.Settings) != "[{auto.classifier gone  ui true} {concierge.model glm-flash glm-5.3 ui true}]" {
		t.Errorf("settings %v", list.Settings)
	}
}

// An AI&-shaped model list, written by hand from the fields AI& returns.
const aiandModels = `{"object":"list","data":[
 {"id":"deepseek-ai/deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash","pricing":{"input":0.3,"output":1.2,"cached_input":"0.03"},
  "deprecated_at":"2026-11-05T15:00:00.000Z","owned_by":"deepseek-ai"},
 {"id":"deepseek-ai/deepseek-v4.1-flash-1017","name":"DeepSeek V4.1 Flash 1017","pricing":{"input":0.3,"output":1.2,"cached_input":0.03,"currency":"USD"}},
 {"id":"zai-org/glm-5.3","context_length":131072},
 {"name":"no id"}, "junk"
]}`

// AI&'s real answer (testdata/aiand-models.json, captured unauthenticated): prices
// as top-level strings per million tokens, currency "usd", deprecation_date or null.
func TestNormaliseAIandSample(t *testing.T) {
	b, err := os.ReadFile("testdata/aiand-models.json")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := normaliseModels(b)
	if err != nil || len(ms) != 3 {
		t.Fatalf("%v %+v", err, ms)
	}
	by := map[string]upstreamModel{}
	for _, m := range ms {
		by[m.ID] = m
	}
	n := by["deepseek-ai/deepseek-v4.1-flash-1017"]
	if n.DeprecatedAt != "" || n.Input == nil || *n.Input != 0.3 || *n.Output != 0.6 || *n.CachedInput != 0.02 || n.Currency != "usd" ||
		n.Name != "deepseek-ai/DeepSeek-V4.1-Flash-1017" {
		t.Errorf("1017 %+v", n)
	}
	if o := by["deepseek-ai/deepseek-v4.1-flash"]; o.DeprecatedAt != "2026-11-05T15:00:00.000Z" {
		t.Errorf("v4.1-flash %+v", o)
	}
}

func TestNormaliseModels(t *testing.T) {
	ms, err := normaliseModels([]byte(aiandModels))
	if err != nil || len(ms) != 3 {
		t.Fatalf("%v %+v", err, ms)
	}
	a := ms[0]
	if a.Name != "DeepSeek V4.1 Flash" || a.DeprecatedAt != "2026-11-05T15:00:00.000Z" || *a.Input != 0.3 || *a.Output != 1.2 || *a.CachedInput != 0.03 {
		t.Errorf("entry 0 %+v", a)
	}
	if ms[1].DeprecatedAt != "" || ms[1].Currency != "USD" || ms[2].Input != nil || ms[2].Name != "" {
		t.Errorf("entries %+v %+v", ms[1], ms[2])
	}
	if ms, err := normaliseModels([]byte(`[{"id":"a"},{"id":"b"}]`)); err != nil || len(ms) != 2 {
		t.Errorf("bare array %v %v", ms, err)
	}
	if _, err := normaliseModels([]byte(`{"error":"x"}`)); err == nil {
		t.Error("an answer without a list was accepted")
	}
	if modelsURL("http://h:8000/v1/") != "http://h:8000/v1/models" || modelsURL("https://api.aiand.com") != "https://api.aiand.com/v1/models" {
		t.Error("models URL")
	}
}

func TestUpstreamModelsHandler(t *testing.T) {
	var hits atomic.Int32
	var auth atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		w.Write([]byte(aiandModels))
	}))
	defer up.Close()
	x := newM2(t)
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	p := routerProfile("ai", "deepseek-ai/deepseek-v4.1-flash")
	u := p.Upstreams["oc-a"]
	u.BaseURL, u.KeyRef = up.URL, "k"
	p.Upstreams["oc-a"] = u
	x.th.store.PutProfile(p)
	get := func(base, key string) (int, []upstreamModel) {
		var out struct{ Models []upstreamModel }
		st := apiDo(t, x.c, x.th.srv.URL, csrf, "GET", "/api/admin/upstream-models?base_url="+url.QueryEscape(base)+"&key_ref="+key, nil, &out)
		return st, out.Models
	}
	st, ms := get(up.URL, "k")
	if st != 200 || len(ms) != 3 || ms[0].DeprecatedAt == "" || auth.Load() != "Bearer the-key" {
		t.Fatalf("%d %+v %v", st, ms, auth.Load())
	}
	if st, _ := get(up.URL, "k"); st != 200 || hits.Load() != 1 {
		t.Errorf("not cached: %d hits", hits.Load())
	}
	// A pair no entry uses is not fetched (no key goes to an arbitrary URL).
	if st, _ := get("http://elsewhere.invalid", "k"); st != 404 {
		t.Errorf("unknown upstream %d", st)
	}
	if st, _ := get(up.URL, ""); st != 404 {
		t.Errorf("other key_ref %d", st)
	}
	// An upstream that fails yields an error, not an empty list.
	p2 := routerProfile("bad", "m")
	u2 := p2.Upstreams["oc-a"]
	u2.BaseURL = up.URL + "/broken"
	p2.Upstreams["oc-a"] = u2
	x.th.store.PutProfile(p2)
	if st, _ := get(up.URL+"/broken", ""); st != http.StatusBadGateway {
		t.Errorf("broken upstream %d", st)
	}
}
