package hub

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/proto"
)

// fakeClock records the timers; the test fires them.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{d: d, f: f}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.stopped
		t.stopped = true
		return was
	}
}

// live returns the timers not stopped or fired.
func (c *fakeClock) live() []*fakeTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*fakeTimer
	for _, t := range c.timers {
		if !t.stopped {
			out = append(out, t)
		}
	}
	return out
}

// fire runs the only live timer and waits for it.
func (c *fakeClock) fire(t *testing.T) *fakeTimer {
	t.Helper()
	ts := c.live()
	if len(ts) != 1 {
		t.Fatalf("%d live timers", len(ts))
	}
	c.mu.Lock()
	ts[0].stopped = true
	c.mu.Unlock()
	ts[0].f()
	return ts[0]
}

func newRLHub(t *testing.T) (*m2, *fakeClock) {
	x := newM2(t)
	fc := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	x.th.h.rl.clock = fc
	x.th.h.rl.jitter = func(time.Duration) time.Duration { return 0 }
	x.th.h.rl.idlePoll = 20 * time.Millisecond
	x.th.h.rl.idleWait = 5 * time.Second
	return x, fc
}

// Events as Claude Code 2.1.x sends them around a rejection.
func echoEv(text string) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "mado": "echo", "message": map[string]any{"role": "user",
		"content": []any{map[string]any{"type": "text", "text": text}}}})
	return string(b)
}

func rlEv(status string, resets int64) string {
	return fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"status":%q,"resetsAt":%d,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":1.01,"resetsAt":%d},"seven_day":{"utilization":0.53,"resetsAt":%d}}}}`,
		status, resets, resets, resets+86400*4)
}

const (
	initEv      = `{"type":"system","subtype":"init","session_id":"66666666-6666-4666-8666-666666666666"}`
	syntheticEv = `{"type":"assistant","message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"You've hit your session limit · resets 5:30pm (UTC)"}]}}`
	resultEv    = `{"type":"result","subtype":"success","is_error":true,"result":"You've hit your session limit"}`
	answerEv    = `{"type":"assistant","message":{"model":"claude-opus-4-8","role":"assistant","content":[{"type":"text","text":"working"}]}}`
)

// feed sends events after the session's last seq and waits until they are stored.
func (x *m2) feed(t *testing.T, sid string, raws ...string) {
	t.Helper()
	seq := x.th.store.MaxSeq(sid)
	for _, r := range raws {
		seq++
		x.fw.send(&proto.Event{SessionID: sid, Seq: seq, Raw: json.RawMessage(r)})
	}
	waitFor(t, func() bool { return x.th.store.MaxSeq(sid) == seq })
	time.Sleep(30 * time.Millisecond) // the event handler runs after the insert
}

func TestRateLimitResendsTheUsersMessages(t *testing.T) {
	x, fc := newRLHub(t)
	sid := x.session(t, "subscription", proto.StateIdle, "66666666-6666-4666-8666-666666666666")
	reset := fc.Now().Add(2 * time.Hour).Unix()
	// The message that hit the limit, then one typed while limited.
	x.feed(t, sid, `{"type":"mado_spawn","pid":"1"}`, echoEv("do X"), initEv, rlEv("rejected", reset), syntheticEv, resultEv)
	s, _ := x.th.store.Session(sid)
	if s.LimitedUntil != reset || s.LimitedType != "five_hour" || s.LimitedSeq != 1 || !strings.Contains(s.RateLimit, `"seven_day"`) {
		t.Fatalf("limited %+v", s)
	}
	x.feed(t, sid, echoEv("and Y"), initEv, rlEv("rejected", reset), syntheticEv, resultEv)
	// The banner's data in the session view.
	var d struct {
		Session struct {
			LimitedUntil int64           `json:"limited_until"`
			LimitedType  string          `json:"limited_type"`
			AutoResume   bool            `json:"auto_resume"`
			RateLimit    json.RawMessage `json:"rate_limit"`
		}
	}
	apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/sessions/"+sid, nil, &d)
	if d.Session.LimitedUntil != reset || d.Session.LimitedType != "five_hour" || !d.Session.AutoResume || !strings.Contains(string(d.Session.RateLimit), `"utilization":1.01`) {
		t.Fatalf("view %+v %s", d.Session, d.Session.RateLimit)
	}
	ts := fc.live()
	if len(ts) != 1 || ts[0].d != 2*time.Hour+90*time.Second {
		t.Fatalf("timers %+v", ts)
	}
	fc.set(time.Unix(reset, 0).Add(91 * time.Second))
	fc.fire(t)
	m := x.fw.next(proto.TUserMessage).(*proto.UserMessage)
	if m.SessionID != sid || m.Text != "do X\n\nand Y" || m.Note != "rate_limit_resume" || m.NoteText == "" {
		t.Fatalf("resume message %+v", m)
	}
	s, _ = x.th.store.Session(sid)
	if s.LimitedUntil != 0 || s.ResumeAttempts != 1 {
		t.Fatalf("after resume %+v", s)
	}
	// A turn without a rejection resets the counter.
	x.feed(t, sid, echoEv("do X\n\nand Y"), initEv, rlEv("allowed", reset+18000), answerEv, resultEv)
	if s, _ = x.th.store.Session(sid); s.ResumeAttempts != 0 || s.LimitedUntil != 0 {
		t.Fatalf("after a good turn %+v", s)
	}
	var audit struct {
		Rows []struct{ Action, Detail string }
	}
	apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/audit", nil, &audit)
	found := false
	for _, r := range audit.Rows {
		found = found || (r.Action == "session_auto_resume" && r.Detail == sid+" messages")
	}
	if !found {
		t.Errorf("audit %+v", audit.Rows)
	}
}

func TestRateLimitMidTurnContinues(t *testing.T) {
	x, fc := newRLHub(t)
	sid := x.session(t, "subscription", proto.StateIdle, "66666666-6666-4666-8666-666666666666")
	reset := fc.Now().Add(time.Hour).Unix()
	x.feed(t, sid, echoEv("refactor it"), initEv, answerEv, `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		rlEv("rejected", reset), syntheticEv, resultEv)
	if s, _ := x.th.store.Session(sid); s.LimitedSeq != 5 {
		t.Fatalf("mid-turn start %+v", s)
	}
	fc.set(time.Unix(reset, 0).Add(2 * time.Minute))
	fc.fire(t)
	if m := x.fw.next(proto.TUserMessage).(*proto.UserMessage); m.Text != continueText {
		t.Fatalf("continue text %q", m.Text)
	}
}

func TestRateLimitCapSwitchAndScope(t *testing.T) {
	x, fc := newRLHub(t)
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	reset := fc.Now().Add(time.Hour).Unix()
	after := time.Unix(reset, 0).Add(2 * time.Minute)

	// The cap: after 6 automatic attempts in a row, a note instead of a message.
	capped := x.session(t, "subscription", proto.StateIdle, "66666666-6666-4666-8666-666666666666")
	x.th.store.SetResumeAttempts(capped, 6)
	x.feed(t, capped, echoEv("x"), rlEv("rejected", reset), resultEv)
	fc.set(after)
	fc.fire(t)
	if n := x.fw.next(proto.TAppendNote).(*proto.AppendNote); n.SessionID != capped || n.Note != "rate_limit_gave_up" {
		t.Fatalf("cap note %+v", n)
	}
	if s, _ := x.th.store.Session(capped); s.LimitedUntil != 0 {
		t.Fatalf("cap left the session limited %+v", s)
	}

	// The switch off: the timer goes, and a late fire does nothing.
	fc.set(time.Unix(1_800_000_000, 0))
	off := x.session(t, "subscription", proto.StateIdle, "77777777-7777-4777-8777-777777777777")
	x.feed(t, off, echoEv("y"), rlEv("rejected", reset), resultEv)
	if len(fc.live()) != 1 {
		t.Fatal("no timer")
	}
	var out struct {
		Session struct {
			AutoResume bool `json:"auto_resume"`
		}
	}
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/sessions/"+off+"/auto_resume", map[string]bool{"on": false}, &out); st != 200 || out.Session.AutoResume {
		t.Fatalf("switch off %d %+v", st, out)
	}
	if len(fc.live()) != 0 {
		t.Fatal("timer left after the switch went off")
	}
	fc.set(after)
	x.th.h.autoResume(off)
	x.fw.quiet(200 * time.Millisecond)
	if s, _ := x.th.store.Session(off); s.LimitedUntil != 0 || s.AutoResume {
		t.Fatalf("switched off %+v", s)
	}
	// On again with a limit pending arms a timer.
	x.feed(t, off, echoEv("z"), rlEv("rejected", reset), resultEv)
	apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/sessions/"+off+"/auto_resume", map[string]bool{"on": true}, &out)
	if len(fc.live()) != 1 {
		t.Fatalf("switch on: %d timers", len(fc.live()))
	}
	// An allowed event after the rejection ends the limit and its timer.
	x.feed(t, off, rlEv("allowed", reset+18000))
	if s, _ := x.th.store.Session(off); s.LimitedUntil != 0 || len(fc.live()) != 0 {
		t.Fatalf("allowed %+v %d", s, len(fc.live()))
	}

	// Router sessions are never auto-resumed; chat sessions have no switch.
	router := x.session(t, "r", proto.StateIdle, "88888888-8888-4888-8888-888888888888")
	x.feed(t, router, echoEv("w"), rlEv("rejected", reset), resultEv)
	if s, _ := x.th.store.Session(router); s.LimitedUntil != 0 || s.RateLimit != "" || len(fc.live()) != 0 {
		t.Fatalf("router %+v", s)
	}
	chatID := proto.NewID()
	x.th.store.CreateSession(Session{ID: chatID, OwnerSub: "local", Kind: KindChat, Model: "m", Title: "t"})
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/sessions/"+chatID+"/auto_resume", map[string]bool{"on": true}, nil); st != 400 {
		t.Errorf("chat switch %d", st)
	}
}

// A session that stopped while limited is resumed, and the text goes to the new
// session once it is idle.
func TestRateLimitResumesAStoppedSession(t *testing.T) {
	x, fc := newRLHub(t)
	claudeID := "99999999-9999-4999-8999-999999999999"
	sid := proto.NewID()
	if err := x.th.store.CreateSession(Session{ID: sid, MachineID: x.mID, OwnerSub: "local", CWD: "/proj/a",
		ProfileName: "subscription", MainModel: "opus", Title: "t", PermissionMode: "auto"}); err != nil {
		t.Fatal(err)
	}
	x.th.store.UpdateSessionState(x.mID, &proto.SessionState{SessionID: sid, State: proto.StateIdle, ClaudeSessionID: claudeID})
	reset := fc.Now().Add(time.Hour).Unix()
	x.feed(t, sid, echoEv("ship it"), initEv, rlEv("rejected", reset), syntheticEv, resultEv)
	x.th.store.UpdateSessionState(x.mID, &proto.SessionState{SessionID: sid, State: proto.StateExited})
	fc.set(time.Unix(reset, 0).Add(2 * time.Minute))
	fc.fire(t)
	st := x.fw.next(proto.TStartSession).(*proto.StartSession)
	if st.ResumeClaudeSessionID != claudeID || st.SessionID == sid {
		t.Fatalf("resume start %+v", st)
	}
	x.fw.send(&proto.SessionState{SessionID: st.SessionID, State: proto.StateIdle, ClaudeSessionID: claudeID})
	m := x.fw.next(proto.TUserMessage).(*proto.UserMessage)
	if m.SessionID != st.SessionID || m.Text != "ship it" || m.Note != "rate_limit_resume" {
		t.Fatalf("message to the resumed session %+v", m)
	}
	old, _ := x.th.store.Session(sid)
	nw, _ := x.th.store.Session(st.SessionID)
	if old.ResumedBy != st.SessionID || old.LimitedUntil != 0 || nw.ResumeAttempts != 1 || nw.PermissionMode != old.PermissionMode {
		t.Fatalf("chain old %+v new %+v", old, nw)
	}
}

// On hub start every waiting session gets its timer; a reset long past fires soon.
func TestRateLimitArmedOnStart(t *testing.T) {
	x, fc := newRLHub(t)
	sid := x.session(t, "subscription", proto.StateIdle, "66666666-6666-4666-8666-666666666666")
	past := fc.Now().Add(-time.Hour).Unix()
	x.th.store.SetLimited(sid, past, "seven_day", 0)
	future := x.session(t, "subscription", proto.StateIdle, "77777777-7777-4777-8777-777777777777")
	x.th.store.SetLimited(future, fc.Now().Add(time.Hour).Unix(), "five_hour", 0)
	off := x.session(t, "subscription", proto.StateIdle, "88888888-8888-4888-8888-888888888888")
	x.th.store.SetLimited(off, past, "five_hour", 0)
	x.th.store.SetAutoResume(off, false)
	x.th.h.armRateLimits()
	ds := map[time.Duration]bool{}
	for _, tm := range fc.live() {
		ds[tm.d] = true
	}
	if len(fc.live()) != 2 || !ds[resumeSoon] || !ds[time.Hour+resumeDelay] {
		t.Fatalf("timers %v", ds)
	}
}
