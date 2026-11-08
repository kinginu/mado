package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/usage"
)

func aiandPair() proto.Profile {
	return proto.Profile{Name: "aiand-pair", Kind: proto.KindRouter, Main: "aiand-glm53",
		Aliases: map[string]string{"opus": "aiand-dsv4pro", "sonnet": "aiand-dsv4flash", "haiku": "aiand-dsv4flash"},
		Upstreams: map[string]proto.Upstream{
			"aiand-glm53":     {BaseURL: "http://up:1", Model: "zai-org/GLM-5.3", KeyRef: "k"},
			"aiand-dsv4pro":   {BaseURL: "http://up:1", Model: "deepseek-ai/deepseek-v4-pro", KeyRef: "k"},
			"aiand-dsv4flash": {BaseURL: "http://up:1", Model: "deepseek-ai/deepseek-v4-flash", KeyRef: "k"},
		}}
}

func TestMainChoices(t *testing.T) {
	p := aiandPair()
	cs := mainChoices(p)
	// The main comes first even though aiand-dsv4flash sorts before it.
	if strings.Join(cs, ",") != "aiand-glm53,aiand-dsv4flash,aiand-dsv4pro" {
		t.Fatalf("choices %v", cs)
	}
	for in, want := range map[string]string{"": "aiand-glm53", "aiand-dsv4flash": "aiand-dsv4flash"} {
		if got, err := pickMain(p, in); err != nil || got != want {
			t.Errorf("pickMain(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"opus", "fable", "zai-org/GLM-5.3", "oc-a"} {
		if _, err := pickMain(p, bad); err == nil {
			t.Errorf("pickMain(%q) accepted", bad)
		}
	}
	sub := proto.Profile{Name: "subscription", Kind: proto.KindSubscription}
	if got, _ := pickMain(sub, ""); got != "fable" {
		t.Errorf("subscription default %q", got)
	}
	for _, bad := range []string{"aiand-glm53", "opusplan", "claude-opus-5-5", "default"} {
		if _, err := pickMain(sub, bad); err == nil {
			t.Errorf("subscription accepted %q", bad)
		}
	}
}

// What the new-session form needs comes from GET /api/profiles and /api/machines;
// POST /api/sessions validates the profile, main model and mode (DESIGN §15).
func TestNewSessionChoicesAndValidation(t *testing.T) {
	x := newM2(t)
	if err := x.th.store.PutProfile(aiandPair()); err != nil {
		t.Fatal(err)
	}
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	do := func(method, path string, body, v any) int {
		return apiDo(t, x.c, x.th.srv.URL, csrf, method, path, body, v)
	}
	var ps struct {
		Profiles []struct {
			Name, Kind, Main, Enforce string
			Choices                   []string
			Aliases, Roles            []aliasRow
			RolesOn                   bool `json:"roles_on"`
		}
	}
	do("GET", "/api/profiles", nil, &ps)
	var pair *struct {
		Name, Kind, Main, Enforce string
		Choices                   []string
		Aliases, Roles            []aliasRow
		RolesOn                   bool `json:"roles_on"`
	}
	for i := range ps.Profiles {
		if ps.Profiles[i].Name == "aiand-pair" {
			pair = &ps.Profiles[i]
		}
	}
	if pair == nil {
		t.Fatalf("profiles %+v", ps)
	}
	// The main is listed first even though aiand-dsv4flash sorts before it.
	if pair.Main != "aiand-glm53" || strings.Join(pair.Choices, ",") != "aiand-glm53,aiand-dsv4flash,aiand-dsv4pro" {
		t.Errorf("main %q choices %v", pair.Main, pair.Choices)
	}
	// Read-only alias table and role split.
	has := func(rows []aliasRow, want aliasRow) bool { return slices.Contains(rows, want) }
	if !has(pair.Aliases, aliasRow{"main", "aiand-glm53", "aiand-glm53", "zai-org/GLM-5.3"}) ||
		!has(pair.Aliases, aliasRow{"opus", "opus", "aiand-dsv4pro", "deepseek-ai/deepseek-v4-pro"}) ||
		!has(pair.Roles, aliasRow{"implementer", "opus", "aiand-dsv4pro", "deepseek-ai/deepseek-v4-pro"}) ||
		!has(pair.Roles, aliasRow{"simple", "sonnet", "aiand-dsv4flash", "deepseek-ai/deepseek-v4-flash"}) {
		t.Errorf("alias tables %+v %+v", pair.Aliases, pair.Roles)
	}
	if !pair.RolesOn || pair.Enforce != "edits" {
		t.Errorf("roles on %v enforce %q", pair.RolesOn, pair.Enforce)
	}
	// Permission modes: default first, then acceptEdits and (since M5) auto; never the others.
	var ms struct {
		PermissionModes []string `json:"permission_modes"`
	}
	do("GET", "/api/machines", nil, &ms)
	if strings.Join(ms.PermissionModes, ",") != "default,acceptEdits,auto" {
		t.Errorf("modes %v", ms.PermissionModes)
	}

	cases := []struct {
		profile, main, mode string
		wantMain, wantMode  string // "" = refused
	}{
		{"aiand-pair", "", "", "aiand-glm53", "acceptEdits"}, // no mode: auto (M22), mado auto on a router
		{"aiand-pair", "aiand-dsv4flash", "acceptEdits", "aiand-dsv4flash", "acceptEdits"},
		{"subscription", "", "default", "fable", "default"},
		{"subscription", "sonnet", "", "sonnet", "auto"},         // no mode: claude's own auto (M22)
		{"aiand-pair", "oc-a", "", "", ""},                       // another profile's upstream
		{"aiand-pair", "opus", "", "", ""},                       // an alias, not a main
		{"subscription", "", "auto", "fable", "auto"},            // back in M5
		{"aiand-pair", "", "auto", "aiand-glm53", "acceptEdits"}, // mado auto (M9)
		{"subscription", "", "dontAsk", "", ""},
		{"subscription", "", "bypassPermissions", "", ""},
		{"subscription", "", "plan", "", ""},
	}
	for _, c := range cases {
		var out struct{ ID, Error string }
		st := do("POST", "/api/sessions", map[string]any{"kind": "code", "machine": x.mID, "cwd": "/proj/a", "profile": c.profile,
			"main_model": c.main, "permission_mode": c.mode}, &out)
		if c.wantMain == "" {
			if st != 400 || out.Error == "" {
				t.Errorf("%+v accepted: %d", c, st)
			}
			continue
		}
		if st != 201 {
			t.Errorf("%+v refused: %d %s", c, st, out.Error)
			continue
		}
		start := x.fw.next(proto.TStartSession).(*proto.StartSession)
		if start.SessionID != out.ID || start.MainModel != c.wantMain || start.PermissionMode != c.wantMode || start.Profile.Name != c.profile {
			t.Errorf("%+v: start frame main=%q mode=%q", c, start.MainModel, start.PermissionMode)
		}
	}
}

// A session that was started with dontAsk before M4 resumes in default mode (auto is
// allowed again since M5 and resumes as auto).
func TestResumeDropsLegacyPermissionMode(t *testing.T) {
	x := newM2(t)
	sid := proto.NewID()
	x.th.store.CreateSession(Session{ID: sid, MachineID: x.mID, OwnerSub: "local", CWD: "/proj/a", ProfileName: "subscription",
		MainModel: "sonnet", Title: "t", PermissionMode: "dontAsk"})
	x.th.store.UpdateSessionState(x.mID, &proto.SessionState{SessionID: sid, State: proto.StateIdle, ClaudeSessionID: "33333333-3333-4333-8333-333333333333"})
	x.fw.send(&proto.Sessions{})
	st := x.fw.next(proto.TStartSession).(*proto.StartSession)
	if st.SessionID != sid || st.PermissionMode != "default" || st.ResumeClaudeSessionID == "" {
		t.Fatalf("resume frame %+v", st)
	}
}

func feedSpool(t *testing.T, x *m2, sid, path string) int {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	n := 0
	for sc.Scan() {
		var e struct {
			Seq int64           `json:"seq"`
			Raw json.RawMessage `json:"raw"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		x.fw.send(&proto.Event{SessionID: sid, Seq: e.Seq, Raw: e.Raw})
		n++
	}
	// Wait until the hub stored them all (it records usage rows too, which takes time
	// under -race).
	for deadline := time.Now().Add(10 * time.Second); x.th.store.MaxSeq(sid) < int64(n) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	return n
}

func TestModelsPanelAndListColumn(t *testing.T) {
	x := newM2(t)
	sid := x.session(t, "r", proto.StateIdle, "")
	// Build the cache before the events arrive, so the live path is the one tested.
	if s := x.th.h.sessionModels(sid); len(s.Rows) != 0 {
		t.Fatal("empty session has rows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", x.th.srv.URL+"/s/"+sid+"/events?after=0", nil)
	resp, err := x.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	gotModels := make(chan usage.Stats, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		var ev string
		var data []string
		for sc.Scan() {
			l := sc.Text()
			switch {
			case strings.HasPrefix(l, "event: "):
				ev, data = strings.TrimPrefix(l, "event: "), nil
			case strings.HasPrefix(l, "data: "):
				data = append(data, strings.TrimPrefix(l, "data: "))
			case l == "" && ev == "models":
				var st usage.Stats
				if json.Unmarshal([]byte(strings.Join(data, "\n")), &st) == nil && st.Short() == "glm-5.3 ×15, deepseek-v4-flash ×14 (sub)" {
					gotModels <- st
					return
				}
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	feedSpool(t, x, sid, "../usage/testdata/agent_split.spool")
	select {
	case st := <-gotModels:
		if st.AgentCalls != 1 {
			t.Errorf("models SSE: %+v", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no models SSE update")
	}
	var detail struct{ Models usage.Stats }
	apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/sessions/"+sid, nil, &detail)
	if !slices.ContainsFunc(detail.Models.Rows, func(r usage.Row) bool {
		return r.Subagent && r.Model == "deepseek-ai/deepseek-v4-flash" && r.Events == 14
	}) {
		t.Errorf("session models panel: %+v", detail.Models)
	}
	// A fresh cache (hub restart) computes the same from the store.
	x.th.h.models.mu.Lock()
	x.th.h.models.accs = nil
	x.th.h.models.mu.Unlock()
	var list struct{ Sessions []apiSessionView }
	apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/sessions", nil, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].Models != "glm-5.3 ×15, deepseek-v4-flash ×14 (sub)" {
		t.Errorf("session list lacks the models column: %+v", list.Sessions)
	}
}

// The noisy system events of a real session do not become standalone blocks; the
// blocks carry what the SPA groups by (DESIGN §15).
func TestSessionBlocksNoise(t *testing.T) {
	x := newM2(t)
	sid := x.session(t, "r", proto.StateIdle, "")
	feedSpool(t, x, sid, "../usage/testdata/agent_split.spool")
	time.Sleep(300 * time.Millisecond)
	var bl struct{ Blocks []apiBlock }
	apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/sessions/"+sid+"/blocks", nil, &bl)
	const agent = "chatcmpl-tool-b44932abeb324aac_09scznd"
	var thinking, status, subText, noise int
	var agentCall bool
	for _, b := range bl.Blocks {
		switch {
		case b.Kind == "raw" && (strings.Contains(b.Label, "system/thinking_tokens") || strings.Contains(b.Label, "system/task_")):
			t.Errorf("raw block %q", b.Label)
		case b.Kind == "thinking_tokens":
			thinking++
		case b.Kind == "status" && b.Parent == agent:
			status++
		case b.Kind == "tool_use" && b.ToolID == agent:
			agentCall = b.Label == "Agent" && b.Agent == "Explore" && b.Model == "sonnet"
		case b.Kind == "assistant" && b.Subagent && b.Parent == agent && b.Model == "deepseek-ai/deepseek-v4-flash":
			subText++
		}
		if b.Kind == "system" && b.Noise {
			noise++
		}
	}
	if thinking != 179 || status == 0 || !agentCall || subText == 0 || noise == 0 {
		t.Errorf("thinking %d status %d agent call %v subagent text %d noise %d", thinking, status, agentCall, subText, noise)
	}
}

func TestRolesConfigAndEditor(t *testing.T) {
	dir := t.TempDir()
	write := func(roles string) string {
		p := dir + "/hub.toml"
		os.WriteFile(p, []byte(`base_url = "https://hub.example"
[[profiles]]
name = "aiand-pair"
kind = "router"
main = "a"
[profiles.upstreams.a]
base_url = "http://h:1"
model = "o/m"
`+roles), 0o600)
		return p
	}
	c, err := LoadConfig(write("[profiles.roles]\nimplementer = \"sonnet\"\nprompt = \"Delegate.\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := c.Profiles[0].toProto()
	if p.Roles == nil || p.Roles.Implementer != "sonnet" || p.Roles.Prompt != "Delegate." || p.Roles.Enabled != nil {
		t.Fatalf("roles %+v", p.Roles)
	}
	if _, err := LoadConfig(write("[profiles.roles]\nresearcher = \"a\"\n")); err == nil {
		t.Error("router name accepted as a role alias")
	}
	if c, err := LoadConfig(write("[profiles.roles]\nenforce = \"all\"\n")); err != nil {
		t.Fatal(err)
	} else if p, _ := c.Profiles[0].toProto(); p.Roles.Enforce != "all" {
		t.Errorf("enforce %+v", p.Roles)
	}
	if _, err := LoadConfig(write("[profiles.roles]\nenforce = \"bash\"\n")); err == nil {
		t.Error("bad enforce accepted")
	}
	if _, err := LoadConfig(write("[profiles.roles]\nmodel = \"opus\"\n")); err == nil {
		t.Error("unknown roles key accepted")
	}
	sec := &Secrets{Keys: map[string]string{}}
	sp, err := ParseProfile([]byte(`{"name":"sub2","kind":"subscription","roles":{"enabled":false}}`), sec)
	if err != nil || sp.Roles == nil || *sp.Roles.Enabled {
		t.Fatalf("subscription roles: %+v %v", sp.Roles, err)
	}
	if ep, err := ParseProfile([]byte(`{"name":"x","kind":"subscription","roles":{"enforce":"off"}}`), sec); err != nil || ep.Roles.Enforce != "off" {
		t.Errorf("editor enforce: %+v %v", ep.Roles, err)
	} else if d := docOf(ep); d.Roles == nil || d.Roles.Enforce != "off" {
		t.Errorf("editor does not round-trip enforce: %+v", d.Roles)
	}
	if _, err := ParseProfile([]byte(`{"name":"x","kind":"subscription","roles":{"enforce":"yes"}}`), sec); err == nil {
		t.Error("bad enforce accepted in the editor")
	}
	if _, err := ParseProfile([]byte(`{"name":"x","kind":"subscription","roles":{"simple":"gpt"}}`), sec); err == nil {
		t.Error("bad alias accepted in the editor")
	}
	if _, err := ParseProfile([]byte(`{"name":"x","kind":"subscription","roles":{"tools":["Bash"]}}`), sec); err == nil {
		t.Error("unknown roles field accepted in the editor")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	c, err := LoadConfig("../../deploy/hub.toml.example")
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Profiles[0].toProto()
	if err != nil || p.Roles == nil || p.Roles.Simple != "sonnet" {
		t.Fatalf("example profile %+v %v", p.Roles, err)
	}
}
