package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kinginu/mado/internal/proto"
)

// M12: the concierge (DESIGN §20).

func toolStream(id, toolID, name string, input any) string {
	var b strings.Builder
	ev := func(t, d string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", t, d) }
	ev("message_start", `{"type":"message_start","message":{"id":"`+id+`","type":"message","role":"assistant","model":"zai-org/glm-5.3","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}`)
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"`+toolID+`","name":"`+name+`","input":{}}}`)
	in, _ := json.Marshal(input)
	j, _ := json.Marshal(string(in))
	ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+string(j)+`}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":10,"output_tokens":5}}`)
	ev("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

type conciergeHub struct {
	*chatHub
	fw  *fakeWorker
	mID string
}

// newConciergeHub is a chat hub with a router profile, a fake worker "box" (roots
// /proj, capability fs) and a scripted model.
func newConciergeHub(t *testing.T) *conciergeHub {
	x := newChatHub(t)
	if err := x.store.PutProfile(proto.Profile{Name: "r", Kind: proto.KindRouter, Main: "oc-a",
		Upstreams: map[string]proto.Upstream{"oc-a": {BaseURL: "http://up:1", Model: "org/A", KeyRef: "aiand"}}}); err != nil {
		t.Fatal(err)
	}
	code, _ := x.store.CreateEnrollCode("box", time.Minute)
	tok, _ := x.store.Enroll("box", code)
	wsURL := "ws" + strings.TrimPrefix(x.srv.URL, "http") + "/ws/worker"
	wc, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wc.CloseNow() })
	fw := &fakeWorker{t: t, c: wc, ch: make(chan proto.Frame, 64)}
	go func() {
		for {
			_, b, err := wc.Read(context.Background())
			if err != nil {
				return
			}
			if f, err := proto.Decode(b); err == nil {
				fw.ch <- f
			}
		}
	}()
	fw.send(&proto.Hello{Machine: "box", Capabilities: []string{proto.CapFS}, Projects: []string{"/proj", "/proj/a"}, Roots: []string{"/proj"}})
	time.Sleep(100 * time.Millisecond)
	ms, _ := x.store.Machines()
	return &conciergeHub{chatHub: x, fw: fw, mID: ms[0].ID}
}

func (x *conciergeHub) script(bodies ...string) {
	x.llm.mu.Lock()
	x.llm.script = append(x.llm.script, bodies...)
	x.llm.mu.Unlock()
}

func (x *conciergeHub) newConcierge(t *testing.T) string {
	t.Helper()
	var out struct{ ID string }
	if st := x.do(t, "POST", "/api/sessions", map[string]any{"kind": "chat", "concierge": true}, &out); st != 201 {
		t.Fatalf("create concierge: %d", st)
	}
	return out.ID
}

// planBlocks are the plan blocks of a session as GET /blocks returns them.
func (x *conciergeHub) planBlocks(t *testing.T, sid string) []map[string]any {
	var r struct {
		Blocks []map[string]any `json:"blocks"`
	}
	x.getJSON(t, "/api/sessions/"+sid+"/blocks?before=0", &r)
	var out []map[string]any
	for _, b := range r.Blocks {
		if b["kind"] == "plan" {
			out = append(out, b)
		}
	}
	return out
}

func (x *conciergeHub) waitPlan(t *testing.T, id string, states ...string) *planRecord {
	t.Helper()
	for i := 0; i < 200; i++ {
		p, err := x.store.Plan(id)
		if err == nil {
			for _, s := range states {
				if p.State == s {
					return p
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	p, _ := x.store.Plan(id)
	t.Fatalf("plan %s never reached %v: %+v", id, states, p)
	return nil
}

func (x *conciergeHub) waitRequests(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < 250 && x.llm.count() < n; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if x.llm.count() < n {
		t.Fatalf("model requests %d, want %d", x.llm.count(), n)
	}
}

// lastUserText is the text of the last user message of a model request.
func lastUserText(req map[string]any) string {
	msgs, _ := req["messages"].([]any)
	if len(msgs) == 0 {
		return ""
	}
	m := msgs[len(msgs)-1].(map[string]any)
	b, _ := json.Marshal(m["content"])
	return string(b)
}

func TestConciergePlanFlow(t *testing.T) {
	x := newConciergeHub(t)
	// A code session on profile r makes r the last used profile.
	if err := x.store.CreateSession(Session{ID: proto.NewID(), MachineID: x.mID, OwnerSub: "local", CWD: "/proj/a",
		ProfileName: "r", MainModel: "oc-a", Title: "old", State: proto.StateExited}); err != nil {
		t.Fatal(err)
	}
	sid := x.newConcierge(t)
	var d struct {
		Session apiSessionView `json:"session"`
		Tools   []string       `json:"tools"`
	}
	x.getJSON(t, "/api/sessions/"+sid, &d)
	if !d.Session.Concierge || d.Session.Kind != KindChat || len(d.Tools) != 6 {
		t.Fatalf("concierge view %+v %v", d.Session, d.Tools)
	}

	plan := map[string]any{"summary": "new project", "prepare": []any{
		map[string]any{"machine": "box", "action": "mkdir", "path": "/proj/new"},
		map[string]any{"machine": "box", "action": "git_init", "path": "/proj/new"}},
		"sessions": []any{map[string]any{"machine": "box", "cwd": "/proj/new", "title": "new: design", "first_prompt": "Discuss the design first."}}}
	x.script(
		toolStream("m1", "t1", "list_machines", map[string]any{}),
		toolStream("m2", "t2", "inspect_dir", map[string]any{"machine": "box", "path": "/proj/a"}),
		toolStream("m3", "t3", "propose", map[string]any{"plan": plan}),
		chatStream("m4", "Here is the plan."))
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/send", map[string]any{"text": "a new project on box"}, nil); st != 202 {
		t.Fatalf("send %d", st)
	}
	in := x.fw.next(proto.TFSInspect).(*proto.FSInspect)
	if in.Kind != proto.InspectDir || in.Path != "/proj/a" {
		t.Fatalf("inspect %+v", in)
	}
	x.fw.send(&proto.FSResult{RequestID: in.RequestID, OK: true, Data: json.RawMessage(`{"path":"/proj/a","entries":[{"name":"go.mod"}]}`)})
	x.h.runner.Wait(sid)
	x.waitRequests(t, 4)
	r0 := x.llm.req(0)
	if sys, _ := r0["system"].(string); !strings.Contains(sys, "mado's concierge") {
		t.Errorf("system %q", sys)
	}
	var names []string
	for _, tl := range r0["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "list_machines,list_projects,inspect_dir,list_profiles,propose,session_status" {
		t.Errorf("tools %v", names)
	}
	if u := lastUserText(x.llm.req(1)); !strings.Contains(u, `\"name\":\"box\"`) || !strings.Contains(u, "/proj") {
		t.Errorf("list_machines result %s", u)
	}
	if u := lastUserText(x.llm.req(2)); !strings.Contains(u, "go.mod") {
		t.Errorf("inspect result %s", u)
	}
	if u := lastUserText(x.llm.req(3)); !strings.Contains(u, "plan_id: ") {
		t.Errorf("propose result %s", u)
	}
	pbs := x.planBlocks(t, sid)
	if len(pbs) != 1 {
		t.Fatalf("plan blocks %v", pbs)
	}
	pv := pbs[0]["plan"].(map[string]any)
	planID := pv["id"].(string)
	ps := pv["plan"].(map[string]any)["sessions"].([]any)[0].(map[string]any)
	if pv["state"] != PlanProposed || ps["profile"] != "r" || ps["mode"] != "auto" || len(pv["steps"].([]any)) != 3 {
		t.Fatalf("card %v", pv)
	}

	// Approve: mkdir succeeds, git_init fails; the session is not started.
	x.script(chatStream("m5", "The git init failed; here is a corrected plan."))
	var dv map[string]any
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/plans/"+planID, map[string]any{"decision": "approve"}, &dv); st != 200 || dv["state"] != PlanRunning {
		t.Fatalf("approve %d %v", st, dv)
	}
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/plans/"+planID, map[string]any{"decision": "approve"}, nil); st != 409 {
		t.Errorf("second approve %d", st)
	}
	pr := x.fw.next(proto.TFSPrepare).(*proto.FSPrepare)
	if pr.Action != proto.ActionMkdir || pr.Path != "/proj/new" {
		t.Fatalf("prepare %+v", pr)
	}
	x.fw.send(&proto.FSResult{RequestID: pr.RequestID, OK: true, Data: json.RawMessage(`{"output":"created"}`)})
	pr = x.fw.next(proto.TFSPrepare).(*proto.FSPrepare)
	if pr.Action != proto.ActionGitInit {
		t.Fatalf("prepare 2 %+v", pr)
	}
	x.fw.send(&proto.FSResult{RequestID: pr.RequestID, Error: "git: exit status 1: boom"})
	p := x.waitPlan(t, planID, PlanFailed)
	if p.Steps[0].State != stepDone || p.Steps[1].State != stepFailed || p.Steps[2].State != stepSkipped ||
		!strings.Contains(p.Error, "boom") || p.Steps[1].Output != "git: exit status 1: boom" {
		t.Fatalf("failed plan %+v", p)
	}
	x.waitRequests(t, 5)
	x.h.runner.Wait(sid)
	if u := lastUserText(x.llm.req(4)); !strings.Contains(u, "failed at step 2 of 3") || !strings.Contains(u, "boom") ||
		!strings.Contains(u, "not run:") {
		t.Errorf("the model did not get the error: %s", u)
	}
	x.fw.quiet(200 * time.Millisecond) // no start_session after a failure

	// A corrected plan runs to the end and starts the session.
	plan["prepare"] = []any{map[string]any{"machine": "box", "action": "mkdir", "path": "/proj/new"}}
	x.script(toolStream("m6", "t6", "propose", map[string]any{"plan": plan}), chatStream("m7", "Approve it."))
	x.do(t, "POST", "/api/sessions/"+sid+"/send", map[string]any{"text": "try again without git"}, nil)
	x.h.runner.Wait(sid)
	pbs = x.planBlocks(t, sid)
	if len(pbs) != 2 {
		t.Fatalf("plan blocks %d", len(pbs))
	}
	if pbs[0]["plan"].(map[string]any)["state"] != PlanFailed {
		t.Errorf("first card %v", pbs[0]["plan"])
	}
	planID = pbs[1]["plan"].(map[string]any)["id"].(string)
	x.script(chatStream("m8", "Started: [new: design](/s/x)."))
	x.do(t, "POST", "/api/sessions/"+sid+"/plans/"+planID, map[string]any{"decision": "approve"}, nil)
	pr = x.fw.next(proto.TFSPrepare).(*proto.FSPrepare)
	x.fw.send(&proto.FSResult{RequestID: pr.RequestID, OK: true, Data: json.RawMessage(`{"output":"already exists"}`)})
	st := x.fw.next(proto.TStartSession).(*proto.StartSession)
	if st.CWD != "/proj/new" || st.Profile.Name != "r" || st.PermissionMode != "acceptEdits" || st.Profile.Upstreams["oc-a"].Key != "sk-test" {
		t.Fatalf("start %+v", st)
	}
	x.fw.send(&proto.Ack{Ref: st.ID, OK: true})
	um := x.fw.next(proto.TUserMessage).(*proto.UserMessage)
	if um.SessionID != st.SessionID || um.Text != "Discuss the design first." {
		t.Fatalf("first prompt %+v", um)
	}
	p = x.waitPlan(t, planID, PlanDone)
	if p.Steps[1].SessionID != st.SessionID {
		t.Fatalf("done plan %+v", p)
	}
	news, _ := x.store.Session(st.SessionID)
	if news.Title != "new: design" || news.PermissionMode != "auto" || news.ProfileName != "r" || news.OwnerSub != "local" {
		t.Errorf("new session %+v", news)
	}
	if sp := x.h.store.Spawned(sid); len(sp) != 1 || sp[0] != st.SessionID {
		t.Errorf("spawned %v", sp)
	}
	x.waitRequests(t, 8)
	x.h.runner.Wait(sid)
	if u := lastUserText(x.llm.req(7)); !strings.Contains(u, "was approved and ran") || !strings.Contains(u, "/s/"+st.SessionID) {
		t.Errorf("done note %s", u)
	}
	// The note shows as mado's notice, not as the user's message.
	var r struct {
		Blocks []map[string]any `json:"blocks"`
	}
	x.getJSON(t, "/api/sessions/"+sid+"/blocks?before=0", &r)
	notes := 0
	for _, b := range r.Blocks {
		if b["kind"] == "note" {
			notes++
		}
		if b["kind"] == "user" && strings.Contains(b["text"].(string), "was approved") {
			t.Errorf("note rendered as the user's: %v", b)
		}
	}
	if notes != 2 {
		t.Errorf("notes %d", notes)
	}

	// session_status: own sessions only; empty id = the spawned ones.
	res, err := x.h.sessionStatus(sid, "local", "")
	if err != nil || !strings.Contains(res.Text, "new: design") || !strings.Contains(res.Text, "/proj/new") {
		t.Errorf("status %v %v", res.Text, err)
	}
	if _, err := x.h.sessionStatus(sid, "someone-else", st.SessionID); err == nil || err.Error() != "no such session" {
		t.Errorf("foreign status %v", err)
	}
	if res, _ := x.h.sessionStatus(sid, "someone-else", ""); strings.Contains(res.Text, "new: design") {
		t.Errorf("foreign spawned listed: %s", res.Text)
	}
	var dd map[string]any
	x.getJSON(t, "/api/sessions/"+sid, &dd)
	if sp, _ := dd["spawned"].([]any); len(sp) != 1 {
		t.Errorf("panel spawned %v", dd["spawned"])
	}
}

func TestConciergePlanValidation(t *testing.T) {
	x := newConciergeHub(t)
	ok := func() Plan {
		return Plan{Summary: "s", Prepare: []PrepareStep{{Machine: "box", Action: "mkdir", Path: "/proj/x"}},
			Sessions: []PlanSession{{Machine: "box", CWD: "/proj/x", Title: "x", FirstPrompt: "hi"}}}
	}
	p := ok()
	if err := x.h.validatePlan("local", &p); err != nil {
		t.Fatal(err)
	}
	if p.Sessions[0].Profile != "subscription" || p.Sessions[0].Mode != "auto" {
		t.Errorf("defaults without a last profile: %+v", p.Sessions[0])
	}
	cases := []struct {
		name string
		edit func(*Plan)
		want string
	}{
		{"outside", func(p *Plan) { p.Prepare[0].Path = "/etc/x" }, "inside a root"},
		{"root prefix", func(p *Plan) { p.Prepare[0].Path = "/project" }, "inside a root"},
		{"dotdot", func(p *Plan) { p.Prepare[0].Path = "/proj/../etc" }, "inside a root"},
		{"relative", func(p *Plan) { p.Prepare[0].Path = "proj/x" }, "inside a root"},
		{"trailing slash", func(p *Plan) { p.Prepare[0].Path = "/proj/x/" }, "inside a root"},
		{"action", func(p *Plan) { p.Prepare[0].Action = "rm" }, "action must be"},
		{"mkdir extra", func(p *Plan) { p.Prepare[0].URL = "https://x/y" }, "takes only"},
		{"http url", func(p *Plan) {
			p.Prepare[0] = PrepareStep{Machine: "box", Action: "clone", Path: "/proj/c", URL: "http://github.com/o/r"}
		}, "url must"},
		{"file url", func(p *Plan) {
			p.Prepare[0] = PrepareStep{Machine: "box", Action: "clone", Path: "/proj/c", URL: "file:///etc"}
		}, "url must"},
		{"creds", func(p *Plan) {
			p.Prepare[0] = PrepareStep{Machine: "box", Action: "clone", Path: "/proj/c", URL: "https://u:tok@github.com/o/r"}
		}, "url must"},
		{"option url", func(p *Plan) {
			p.Prepare[0] = PrepareStep{Machine: "box", Action: "clone", Path: "/proj/c", URL: "-uhttps://x/y"}
		}, "url must"},
		{"bad branch", func(p *Plan) {
			p.Prepare[0] = PrepareStep{Machine: "box", Action: "clone", Path: "/proj/c", URL: "https://github.com/o/r.git", Branch: "-b"}
		}, "branch"},
		{"worktree no branch", func(p *Plan) {
			p.Prepare[0] = PrepareStep{Machine: "box", Action: "worktree", Path: "/proj/w", Repo: "/proj/a"}
		}, "needs branch"},
		{"worktree repo outside", func(p *Plan) {
			p.Prepare[0] = PrepareStep{Machine: "box", Action: "worktree", Path: "/proj/w", Repo: "/srv/r", Branch: "b"}
		}, "repo"},
		{"worktree base", func(p *Plan) {
			p.Prepare[0] = PrepareStep{Machine: "box", Action: "worktree", Path: "/proj/w", Repo: "/proj/a", Branch: "b", Base: "a..b"}
		}, "base"},
		{"unknown machine", func(p *Plan) { p.Prepare[0].Machine = "nope" }, "unknown machine"},
		{"session cwd", func(p *Plan) { p.Sessions[0].CWD = "/tmp" }, "cwd"},
		{"session mode", func(p *Plan) { p.Sessions[0].Mode = "bypassPermissions" }, "mode"},
		{"session profile", func(p *Plan) { p.Sessions[0].Profile = "nope" }, "unknown profile"},
		{"session model", func(p *Plan) { p.Sessions[0].Profile, p.Sessions[0].MainModel = "r", "gpt" }, "main_model"},
		{"empty", func(p *Plan) { p.Prepare, p.Sessions = nil, nil }, "nothing to do"},
		{"summary", func(p *Plan) { p.Summary = " " }, "summary"},
		{"too many", func(p *Plan) {
			for i := 0; i < 20; i++ {
				p.Prepare = append(p.Prepare, p.Prepare[0])
			}
		}, "at most"},
	}
	for _, c := range cases {
		p := ok()
		c.edit(&p)
		if err := x.h.validatePlan("local", &p); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	for _, good := range []PrepareStep{
		{Machine: "box", Action: "clone", Path: "/proj/c", URL: "https://github.com/o/r.git", Branch: "main"},
		{Machine: "box", Action: "clone", Path: "/proj/c", URL: "git@github.com:o/r.git"},
		{Machine: "box", Action: "clone", Path: "/proj/c", URL: "ssh://git@host:2222/o/r.git"},
		{Machine: "box", Action: "worktree", Path: "/proj/wt-foo", Repo: "/proj/a", Branch: "feature/foo", Base: "main"},
		{Machine: "box", Action: "git_init", Path: "/proj"},
	} {
		p := ok()
		p.Prepare[0] = good
		if err := x.h.validatePlan("local", &p); err != nil {
			t.Errorf("%+v: %v", good, err)
		}
	}
	// The shape is strict; a plan sent as a JSON string is accepted.
	if _, err := decodePlan(json.RawMessage(`{"summary":"s","prepare":[{"machine":"box","action":"mkdir","path":"/proj/x","mode":"0777"}],"sessions":[]}`)); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Errorf("unknown field: %v", err)
	}
	if p, err := decodePlan(json.RawMessage(`"{\"summary\":\"s\",\"prepare\":[],\"sessions\":[]}"`)); err != nil || p.Summary != "s" {
		t.Errorf("string plan: %v", err)
	}
	if _, err := decodePlan(json.RawMessage(`[1]`)); err == nil {
		t.Error("array accepted")
	}
	// An offline machine, and a worker without the fs capability.
	x.fw.c.CloseNow()
	time.Sleep(100 * time.Millisecond)
	p = ok()
	if err := x.h.validatePlan("local", &p); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Errorf("offline: %v", err)
	}
}

func TestConciergeOldWorker(t *testing.T) {
	x := newConciergeHub(t)
	x.fw.send(&proto.Hello{Machine: "box", Projects: []string{"/proj", "/proj/a", "/srv"}}) // no fs, no roots
	time.Sleep(100 * time.Millisecond)
	wi, _ := x.h.workerInfoByName("box")
	if wi.fs || strings.Join(wi.roots, ",") != "/proj,/srv" {
		t.Fatalf("old worker %+v", wi)
	}
	p := Plan{Summary: "s", Prepare: []PrepareStep{{Machine: "box", Action: "mkdir", Path: "/proj/x"}}}
	if err := x.h.validatePlan("local", &p); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Errorf("old worker prepare: %v", err)
	}
	// Sessions alone still work on an old worker.
	p = Plan{Summary: "s", Sessions: []PlanSession{{Machine: "box", CWD: "/srv/x", Title: "x"}}}
	if err := x.h.validatePlan("local", &p); err != nil {
		t.Errorf("old worker session: %v", err)
	}
}

func TestConciergeDecisionAPI(t *testing.T) {
	x := newConciergeHub(t)
	sid := x.newConcierge(t)
	res, err := x.h.propose(sid, "local", json.RawMessage(`{"summary":"s","prepare":[{"machine":"box","action":"mkdir","path":"/proj/x"}],"sessions":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	planID := strings.TrimPrefix(strings.SplitN(res.Text, "\n", 2)[0], "plan_id: ")
	path := "/api/sessions/" + sid + "/plans/" + planID
	if st := apiDo(t, x.c, x.srv.URL, "", "POST", path, map[string]any{"decision": "reject"}, nil); st != 403 {
		t.Errorf("no csrf: %d", st)
	}
	if st := x.do(t, "POST", path, map[string]any{"decision": "maybe"}, nil); st != 400 {
		t.Errorf("bad decision: %d", st)
	}
	other := x.loginAs(t, "other", false)
	var me struct{ CSRF string }
	resp, _ := other.Get(x.srv.URL + "/api/me")
	json.NewDecoder(resp.Body).Decode(&me)
	resp.Body.Close()
	if st := apiDo(t, other, x.srv.URL, me.CSRF, "POST", path, map[string]any{"decision": "approve"}, nil); st != 404 {
		t.Errorf("other user: %d", st)
	}
	// The plan id under another session of the same user, and a plain chat session.
	sid2 := x.newConcierge(t)
	if st := x.do(t, "POST", "/api/sessions/"+sid2+"/plans/"+planID, map[string]any{"decision": "approve"}, nil); st != 404 {
		t.Errorf("wrong session: %d", st)
	}
	chatID := x.newChat(t, "glm-5.3", "")
	if _, err := x.h.propose(chatID, "local", json.RawMessage(`{"summary":"s","prepare":[{"machine":"box","action":"mkdir","path":"/proj/y"}],"sessions":[]}`)); err != nil {
		t.Fatal(err)
	}
	if ids := x.store.ProposedPlans(chatID); len(ids) != 1 || x.do(t, "POST", "/api/sessions/"+chatID+"/plans/"+ids[0], map[string]any{"decision": "approve"}, nil) != 404 {
		t.Errorf("plan of a non-concierge session decided")
	}
	n := x.llm.count()
	var v map[string]any
	if st := x.do(t, "POST", path, map[string]any{"decision": "reject"}, &v); st != 200 || v["state"] != PlanRejected {
		t.Fatalf("reject %d %v", st, v)
	}
	if st := x.do(t, "POST", path, map[string]any{"decision": "approve"}, nil); st != 409 {
		t.Errorf("approve after reject %d", st)
	}
	x.fw.quiet(200 * time.Millisecond) // nothing runs
	evs, _ := x.store.Events(sid, 0, 100)
	if len(evs) != 1 || !strings.Contains(string(evs[0].Raw), `"mado_note":"plan"`) || !strings.Contains(string(evs[0].Raw), "rejected plan "+planID) {
		t.Errorf("reject note %d %v", len(evs), evs)
	}
	if x.llm.count() != n {
		t.Error("a rejection ran a model turn")
	}
	// A newer proposal replaces a waiting one.
	r1, _ := x.h.propose(sid, "local", json.RawMessage(`{"summary":"a","prepare":[{"machine":"box","action":"mkdir","path":"/proj/a1"}],"sessions":[]}`))
	r2, _ := x.h.propose(sid, "local", json.RawMessage(`{"summary":"b","prepare":[{"machine":"box","action":"mkdir","path":"/proj/b1"}],"sessions":[]}`))
	id1 := strings.TrimPrefix(strings.SplitN(r1.Text, "\n", 2)[0], "plan_id: ")
	if p, _ := x.store.Plan(id1); p.State != PlanRejected || !strings.Contains(p.Error, "replaced") || len(r2.Event) == 0 {
		t.Errorf("replaced plan %+v", p)
	}
	// /new plan from any session starts a concierge.
	var created struct{ ID string }
	if st := x.do(t, "POST", "/api/sessions/"+chatID+"/send", map[string]any{"text": "/new plan"}, &created); st != 201 {
		t.Fatalf("/new plan %d", st)
	}
	if s, err := x.store.Session(created.ID); err != nil || !s.Concierge || s.Model != "glm-5.3" {
		t.Errorf("/new plan session %+v %v", s, err)
	}
}

func TestConciergeConfig(t *testing.T) {
	c := ConciergeConfig{Model: "bad name"}
	if err := c.applyDefaults(); err == nil {
		t.Error("bad model name accepted")
	}
	x := newChatHub(t)
	x.h.cfg.Concierge.Model = "glm-flash"
	if err := x.h.setupConcierge(); err != nil || x.h.conciergeModel() != "glm-flash" {
		t.Errorf("model %v %s", err, x.h.conciergeModel())
	}
	x.h.cfg.Concierge.Model = "nope"
	if err := x.h.setupConcierge(); err == nil || !strings.Contains(err.Error(), "[concierge] model") {
		t.Errorf("unknown model: %v", err)
	}
	// A plan left running by a stopped hub fails at the next start.
	rec := &planRecord{ID: proto.NewID(), SessionID: proto.NewID(), State: PlanRunning,
		Steps: []planStep{{Kind: "prepare", State: stepDone}, {Kind: "prepare", State: stepRunning}, {Kind: "session", State: stepPending}}}
	x.store.InsertPlan(rec)
	x.h.cfg.Concierge.Model = ""
	if err := x.h.setupConcierge(); err != nil {
		t.Fatal(err)
	}
	p, _ := x.store.Plan(rec.ID)
	if p.State != PlanFailed || p.Steps[1].State != stepFailed || p.Steps[2].State != stepSkipped {
		t.Errorf("reset plan %+v", p)
	}
}
