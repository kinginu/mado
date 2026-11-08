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

	"github.com/kinginu/mado/internal/inbox"
	"github.com/kinginu/mado/internal/proto"
)

// M16 (DESIGN §23.6): the claude digest executor on job sessions.

// joinWorker enrolls a machine and connects a fake worker that says hello.
func joinWorker(t *testing.T, x *inboxHub, hello *proto.Hello) *fakeWorker {
	t.Helper()
	code, _ := x.store.CreateEnrollCode(hello.Machine, time.Minute)
	tok, err := x.store.Enroll(hello.Machine, code)
	if err != nil {
		t.Fatal(err)
	}
	wsURL := "ws" + strings.TrimPrefix(x.srv.URL, "http") + "/ws/worker"
	wc, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}}})
	if err != nil {
		t.Fatal(err)
	}
	wc.SetReadLimit(64 << 20)
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
	fw.send(hello)
	time.Sleep(100 * time.Millisecond)
	return fw
}

// noStart fails when the worker got a start_session.
func noStart(t *testing.T, fw *fakeWorker, name string) {
	t.Helper()
	for {
		select {
		case f := <-fw.ch:
			if proto.HeaderOf(f).T == proto.TStartSession {
				t.Fatalf("%s got a start_session", name)
			}
		default:
			return
		}
	}
}

func newJobsHub(t *testing.T, mutate func(*Config)) *inboxHub {
	x := newInboxHub(t, func(c *Config) {
		c.Inbox.Digest.Executor = "claude"
		if mutate != nil {
			mutate(c)
		}
	})
	x.h.jobs.ackWait, x.h.jobs.backstop = 3*time.Second, 5*time.Second
	return x
}

func (x *inboxHub) twoItems(t *testing.T) []int64 {
	t.Helper()
	if st, out := x.ingest(t, "angel", ingestToken, `[{"text":"Please review PR #42","author":"carol"},{"text":"FYI: release out"}]`); st != 200 {
		t.Fatalf("ingest %d %v", st, out)
	}
	its := x.items(t, "")
	return []int64{its[1].ID, its[0].ID}
}

func TestDigestClaudeExecutor(t *testing.T) {
	x := newJobsHub(t, nil)
	// "alpha" sorts first but has no jobs; "core" has them.
	alpha := joinWorker(t, x, &proto.Hello{Machine: "alpha", Capabilities: []string{"subscription"}, Projects: []string{"/p"}, Roots: []string{"/p"},
		JobDir: "/home/a/jobs"})
	core := joinWorker(t, x, &proto.Hello{Machine: "core", Capabilities: []string{"subscription", proto.CapJobs}, Projects: []string{"/src"},
		Roots: []string{"/src"}, JobDir: "/home/u/.local/state/mado/jobs"})
	ids := x.twoItems(t)
	var started apiDigest
	if st := x.do(t, "POST", "/api/inbox/digests", map[string]any{}, &started); st != 202 || started.Executor != "claude" {
		t.Fatalf("start: %d %+v", st, started)
	}
	ss := core.next(proto.TStartSession).(*proto.StartSession)
	if ss.Job == nil || ss.Job.Kind != "digest" || ss.Job.MainModel != "sonnet" || !strings.Contains(ss.Job.System, "inbox summarizer") ||
		ss.CWD != "/home/u/.local/state/mado/jobs" || ss.Profile.Kind != proto.KindSubscription || ss.PermissionMode != "default" ||
		len(ss.Tools) != 1 || ss.Tools[0] != "propose_task" {
		t.Fatalf("start_session %+v job %+v", ss, ss.Job)
	}
	sid := ss.SessionID
	core.send(&proto.Ack{Ref: ss.ID, OK: true})
	um := core.next(proto.TUserMessage).(*proto.UserMessage)
	if um.SessionID != sid || !strings.Contains(um.Text, "<inbox-items>") || !strings.Contains(um.Text, "Please review PR #42") {
		t.Fatalf("user message %+v", um)
	}
	// While it runs, the digest links to the job session.
	var running apiDigest
	x.getJSON(t, fmt.Sprintf("/api/inbox/digests/%d", started.ID), &running)
	if running.SessionID != sid {
		t.Fatalf("running digest session %q, want %q", running.SessionID, sid)
	}
	ev := func(seq int64, v map[string]any) {
		b, _ := json.Marshal(v)
		core.send(&proto.Event{SessionID: sid, Seq: seq, Raw: b})
	}
	core.send(&proto.SessionState{SessionID: sid, State: proto.StateRunning, ClaudeSessionID: "0b5c3c1e-5a4f-4c8e-9a51-1c1f3f2c9d10"})
	ev(1, map[string]any{"type": "mado_spawn", "pid": "42"})
	ev(2, map[string]any{"type": "system", "subtype": "init", "model": "sonnet"})
	ev(3, map[string]any{"type": "assistant", "message": map[string]any{"id": "msg_1", "model": "claude-sonnet-4-5",
		"content": []any{map[string]any{"type": "tool_use", "id": "tu_1", "name": "mcp__mado__propose_task"}}, "usage": map[string]any{"input_tokens": 100}}})
	args, _ := json.Marshal(map[string]any{"title": "Review PR #42", "body": "carol asked", "item_ids": []int64{ids[0], 99999}})
	core.send(&proto.ToolCall{SessionID: sid, CallID: "c1", Name: "propose_task", Args: args})
	tr := core.next(proto.TToolResult).(*proto.ToolResult)
	if tr.CallID != "c1" || tr.IsError || !strings.Contains(tr.Content, "proposed") {
		t.Fatalf("tool result %+v", tr)
	}
	// propose_task from another machine is dropped; web tools are not run as a job tool.
	alpha.send(&proto.ToolCall{SessionID: sid, CallID: "c2", Name: "propose_task", Args: args})
	digest := fmt.Sprintf("## angel\n- carol asks for a review [#%d]", ids[0])
	ev(4, map[string]any{"type": "assistant", "message": map[string]any{"id": "msg_2", "model": "claude-sonnet-4-5",
		"content": []any{map[string]any{"type": "text", "text": digest}}, "usage": map[string]any{"input_tokens": 150}}})
	ev(5, map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": digest,
		"modelUsage": map[string]any{"claude-sonnet-4-5": map[string]any{"inputTokens": 250, "outputTokens": 80}}})
	stop := core.next(proto.TStopSession).(*proto.StopSession)
	if stop.SessionID != sid {
		t.Fatalf("stop %+v", stop)
	}
	code := 0
	core.send(&proto.SessionState{SessionID: sid, State: proto.StateExited, ExitCode: &code})

	d := waitDigestState(t, x, started.ID)
	if d.State != "done" || d.Executor != "claude" || d.Text != digest || d.SessionID != sid || d.Model != "sonnet" || d.Error != "" {
		t.Fatalf("digest %+v", d.Digest)
	}
	if read := x.items(t, "?state=read"); len(read) != 2 || read[0].DigestID != started.ID {
		t.Fatalf("read %+v", read)
	}
	var tasks struct{ Tasks []inbox.Task }
	x.getJSON(t, "/api/inbox/tasks?state=proposed", &tasks)
	if len(tasks.Tasks) != 1 || tasks.Tasks[0].Title != "Review PR #42" || len(tasks.Tasks[0].ItemIDs) != 1 || tasks.Tasks[0].DigestID != started.ID {
		t.Fatalf("tasks %+v", tasks.Tasks)
	}
	s, err := x.store.Session(sid)
	if err != nil || s.Kind != KindDigest || s.MachineName != "core" || s.IsChat() || s.ProfileName != "subscription" {
		t.Fatalf("session %+v %v", s, err)
	}
	var sv struct {
		Session apiSessionView
		Tools   []string
	}
	x.getJSON(t, "/api/sessions/"+sid, &sv)
	if sv.Session.Kind != "digest" || sv.Session.Machine != "core" || sv.Session.Resumable || len(sv.Tools) != 1 {
		t.Fatalf("session view %+v", sv)
	}
	rows, _ := x.store.UsageRows(sid)
	if len(rows) != 1 || rows[0].Kind != "digest" || rows[0].Output != 80 || rows[0].Provisional {
		t.Fatalf("usage rows %+v", rows)
	}
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/send", map[string]string{"text": "hi"}, nil); st != 400 {
		t.Fatalf("send to a digest job: %d", st)
	}
	// After the run the job tool is refused.
	core.send(&proto.ToolCall{SessionID: sid, CallID: "c3", Name: "propose_task", Args: args})
	if tr := core.next(proto.TToolResult).(*proto.ToolResult); tr.CallID != "c3" || !tr.IsError {
		t.Fatalf("late call %+v", tr)
	}
	noStart(t, alpha, "alpha")
	x.getJSON(t, "/api/inbox/tasks?state=proposed", &tasks)
	if len(tasks.Tasks) != 1 {
		t.Fatalf("tasks after foreign calls %+v", tasks.Tasks)
	}
}

// The usage limit: claude exits 2 without a result → the chat executor runs and
// the digest row says chat with the claude note.
func TestDigestClaudeFallback(t *testing.T) {
	x := newJobsHub(t, nil)
	core := joinWorker(t, x, &proto.Hello{Machine: "core", Capabilities: []string{proto.CapJobs}, Roots: []string{"/src"}, JobDir: "/j"})
	ids := x.twoItems(t)
	x.llm.mu.Lock()
	x.llm.script = []string{chatStream("msg_d", fmt.Sprintf("## angel\n- review [#%d]", ids[0]))}
	x.llm.mu.Unlock()
	var started apiDigest
	if st := x.do(t, "POST", "/api/inbox/digests", nil, &started); st != 202 {
		t.Fatalf("start %d", st)
	}
	ss := core.next(proto.TStartSession).(*proto.StartSession)
	core.send(&proto.Ack{Ref: ss.ID, OK: true})
	core.next(proto.TUserMessage)
	b, _ := json.Marshal(map[string]any{"type": "system", "subtype": "api_retry", "attempt": 1, "error_status": 429})
	core.send(&proto.Event{SessionID: ss.SessionID, Seq: 1, Raw: b})
	code := 2
	core.send(&proto.SessionState{SessionID: ss.SessionID, State: proto.StateExited, ExitCode: &code})
	d := waitDigestState(t, x, started.ID)
	if d.State != "done" || d.Executor != "chat" || !strings.Contains(d.Error, "code 2") || !strings.Contains(d.Error, "claude executor") ||
		d.SessionID == "" || d.SessionID == ss.SessionID || d.Model != "zai-org/glm-5.3" {
		t.Fatalf("digest %+v", d.Digest)
	}
	if read := x.items(t, "?state=read"); len(read) != 2 {
		t.Fatalf("read %d", len(read))
	}
	if s, _ := x.store.Session(ss.SessionID); s.State != proto.StateExited {
		t.Fatalf("job session %+v", s)
	}

	// is_error in the result also falls back; so does a refused start.
	x.ingest(t, "angel", ingestToken, `{"text":"third"}`)
	x.llm.mu.Lock()
	x.llm.script = []string{chatStream("msg_e", "## angel\n- third")}
	x.llm.mu.Unlock()
	x.do(t, "POST", "/api/inbox/digests", nil, &started)
	ss = core.next(proto.TStartSession).(*proto.StartSession)
	core.send(&proto.Ack{Ref: ss.ID, OK: true})
	core.next(proto.TUserMessage)
	b, _ = json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": "Claude AI usage limit reached"})
	core.send(&proto.Event{SessionID: ss.SessionID, Seq: 1, Raw: b})
	core.next(proto.TStopSession)
	d = waitDigestState(t, x, started.ID)
	if d.Executor != "chat" || !strings.Contains(d.Error, "usage limit reached") {
		t.Fatalf("is_error digest %+v", d.Digest)
	}

	x.ingest(t, "angel", ingestToken, `{"text":"fourth"}`)
	x.llm.mu.Lock()
	x.llm.script = []string{chatStream("msg_f", "## angel\n- fourth")}
	x.llm.mu.Unlock()
	x.do(t, "POST", "/api/inbox/digests", nil, &started)
	ss = core.next(proto.TStartSession).(*proto.StartSession)
	core.send(&proto.Ack{Ref: ss.ID, OK: false, Error: "job: cwd must be the worker's job_dir"})
	d = waitDigestState(t, x, started.ID)
	if d.Executor != "chat" || !strings.Contains(d.Error, "refused the job") {
		t.Fatalf("refused digest %+v", d.Digest)
	}
}

// No worker with jobs (or the pinned one lacks them): never sent, chat at once.
func TestDigestClaudeNoJobsWorker(t *testing.T) {
	for _, pin := range []string{"", "alpha"} {
		t.Run("pin="+pin, func(t *testing.T) {
			x := newJobsHub(t, func(c *Config) { c.Inbox.Digest.Machine = pin })
			alpha := joinWorker(t, x, &proto.Hello{Machine: "alpha", Capabilities: []string{"subscription"}, Roots: []string{"/p"}, JobDir: "/j"})
			var core *fakeWorker
			if pin != "" {
				core = joinWorker(t, x, &proto.Hello{Machine: "core", Capabilities: []string{proto.CapJobs}, Roots: []string{"/p"}, JobDir: "/j"})
			}
			x.twoItems(t)
			x.llm.mu.Lock()
			x.llm.script = []string{chatStream("msg_d", "## angel\n- things")}
			x.llm.mu.Unlock()
			var started apiDigest
			x.do(t, "POST", "/api/inbox/digests", nil, &started)
			d := waitDigestState(t, x, started.ID)
			want := "no online worker has the jobs capability"
			if pin != "" {
				want = `pinned machine "alpha"`
			}
			if d.State != "done" || d.Executor != "chat" || !strings.Contains(d.Error, want) {
				t.Fatalf("digest %+v", d.Digest)
			}
			noStart(t, alpha, "alpha")
			if core != nil {
				noStart(t, core, "core (not pinned)")
			}
		})
	}
}

// executor = "chat" never touches workers; main_model is validated.
func TestDigestExecutorConfig(t *testing.T) {
	x := newInboxHub(t, nil)
	if _, ok := x.h.digestExecutor().(chatDigestExecutor); !ok {
		t.Fatal("executor = chat is not the chat executor")
	}
	y := newJobsHub(t, nil)
	if e := y.h.digestExecutor(); e.Name() != "claude" {
		t.Fatalf("executor %s", e.Name())
	}
	for _, m := range []string{"oc-strong", "--model", "gpt-5"} {
		c := InboxConfig{Digest: InboxDigestConfig{MainModel: m}}
		if err := c.applyDefaults(); err == nil {
			t.Errorf("main_model %q accepted", m)
		}
	}
	for _, m := range []string{"", "haiku", "fable", "claude-sonnet-4-5"} {
		c := InboxConfig{Digest: InboxDigestConfig{MainModel: m}}
		if err := c.applyDefaults(); err != nil {
			t.Errorf("main_model %q: %v", m, err)
		}
	}
}
