package hub

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/messages"
	"github.com/kinginu/mado/internal/policy"
	"github.com/kinginu/mado/internal/proto"
)

// The fixture's first question is a real one from a hub DB; the other two add a
// multiSelect question and one with previews (DESIGN §21.4).
func askFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/ask_user_question.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const (
	q1 = "このディレクトリには性質の異なる Handoff が2つあります。どちらを実装しますか？"
	q2 = "Which checks should run before each commit?"
	q3 = "Which layout should the settings page use?"
)

func TestAnswerQuestionValidation(t *testing.T) {
	in := askFixture(t)
	bad := []struct {
		name           string
		answers, notes map[string]string
		want           string
	}{
		{"none", nil, nil, "answers are required"},
		{"unknown", map[string]string{"What?": "x"}, nil, "not one of the questions"},
		{"empty", map[string]string{q1: "  "}, nil, "is empty"},
		{"long", map[string]string{q1: strings.Repeat("x", maxAnswer+1)}, nil, "longer than 2000"},
		{"unknown note", map[string]string{q1: "x"}, map[string]string{"What?": "n"}, "not one of the questions"},
		{"long note", map[string]string{q1: "x"}, map[string]string{q1: strings.Repeat("n", maxAnswer+1)}, "longer than 2000"},
	}
	for _, c := range bad {
		if _, _, err := answerQuestion(in, c.answers, c.notes); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if _, _, err := answerQuestion(`{"command":"ls"}`, map[string]string{"x": "y"}, nil); err == nil {
		t.Error("an input without questions was answered")
	}
	// Exactly 2000 bytes is fine.
	if _, _, err := answerQuestion(in, map[string]string{q1: strings.Repeat("é", maxAnswer/2)}, nil); err != nil {
		t.Errorf("2000 bytes: %v", err)
	}
	upd, stored, err := answerQuestion(in, map[string]string{q1: "Usage 集計の高速化", q2: "go vet, gofmt", q3: " Two columns "},
		map[string]string{q1: "start with phase 1", q2: " "})
	if err != nil {
		t.Fatal(err)
	}
	var u struct {
		Questions   []askQuestion
		Answers     map[string]string
		Annotations map[string]map[string]string
	}
	if err := json.Unmarshal(upd, &u); err != nil {
		t.Fatal(err)
	}
	if len(u.Questions) != 3 || u.Answers[q3] != "Two columns" || u.Answers[q2] != "go vet, gofmt" {
		t.Errorf("updated input %s", upd)
	}
	if u.Annotations[q1]["notes"] != "start with phase 1" || u.Annotations[q1]["preview"] != "" ||
		!strings.HasPrefix(u.Annotations[q3]["preview"], "+-----------+") || u.Annotations[q2] != nil {
		t.Errorf("annotations %v", u.Annotations)
	}
	var sa storedAnswers
	if json.Unmarshal([]byte(stored), &sa) != nil || len(sa.Answers) != 3 || len(sa.Notes) != 1 {
		t.Errorf("stored %s", stored)
	}
}

func askReq(sid, rid, input string) *proto.ApprovalRequest {
	return &proto.ApprovalRequest{SessionID: sid, RequestID: rid, ToolName: ToolAskUserQuestion, Input: json.RawMessage(input)}
}

func TestQuestionAnsweredThroughTheAPI(t *testing.T) {
	x := newM2(t)
	sid := x.session(t, "r", proto.StateRunning, "55555555-5555-4555-8555-555555555555")
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	approve := func(body map[string]any, v any) int {
		return apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/sessions/"+sid+"/approve", body, v)
	}
	// An "allow for session" from before M13 does not answer a question.
	x.th.store.AddSessionRule(sid, ToolAskUserQuestion)
	x.fw.send(askReq(sid, "q1", askFixture(t)))
	x.fw.quiet(300 * time.Millisecond)
	if a, _ := x.th.store.Approval(sid, "q1"); a.State != "pending" {
		t.Fatalf("question stored %+v", a)
	}
	var e struct{ Error string }
	for _, c := range []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"request_id": "q1", "decision": "allow_session"}, "cannot be allowed for the session"},
		{map[string]any{"request_id": "q1", "decision": "allow"}, "answers are required"},
		{map[string]any{"request_id": "q1", "decision": "allow", "answers": map[string]string{"Nope?": "x"}}, "not one of the questions"},
		{map[string]any{"request_id": "q1", "decision": "allow", "answers": map[string]string{q1: ""}}, "is empty"},
		{map[string]any{"request_id": "q1", "decision": "deny", "answers": map[string]string{q1: "x"}}, "only for allowing"},
	} {
		if st := approve(c.body, &e); st != 400 || !strings.Contains(e.Error, c.want) {
			t.Errorf("%v: %d %s", c.body, st, e.Error)
		}
	}
	// Answers on another tool's approval: the wrong tool.
	x.fw.send(bashReq(sid, "b1", "ls"))
	time.Sleep(100 * time.Millisecond)
	if st := approve(map[string]any{"request_id": "b1", "decision": "allow", "answers": map[string]string{"x": "y"}}, &e); st != 400 ||
		!strings.Contains(e.Error, "only for allowing an AskUserQuestion") {
		t.Errorf("answers on Bash: %d %s", st, e.Error)
	}
	x.fw.quiet(100 * time.Millisecond)

	var dec apiApproval
	answers := map[string]string{q1: "Settings 画面の刷新 (Recommended)", q2: "go vet, UI tests", q3: "Stacked"}
	if st := approve(map[string]any{"request_id": "q1", "decision": "allow", "answers": answers,
		"notes": map[string]string{q2: "UI tests only on main"}}, &dec); st != 200 || dec.State != "decided" || dec.Answers[q2] != "go vet, UI tests" ||
		dec.Notes[q2] != "UI tests only on main" {
		t.Fatalf("answer: %d %+v", st, dec)
	}
	ar := x.fw.next(proto.TApprovalResponse).(*proto.ApprovalResponse)
	var u map[string]json.RawMessage
	if ar.RequestID != "q1" || ar.Decision != proto.DecisionAllow || json.Unmarshal(ar.UpdatedInput, &u) != nil {
		t.Fatalf("response %+v", ar)
	}
	if !strings.Contains(string(u["answers"]), `"go vet, UI tests"`) || !strings.Contains(string(u["annotations"]), `"notes":"UI tests only on main"`) ||
		!strings.Contains(string(u["annotations"]), `"preview":"Name\n`) || len(u["questions"]) == 0 {
		t.Errorf("updated input %s", ar.UpdatedInput)
	}
	if a, _ := x.th.store.Approval(sid, "q1"); !strings.Contains(a.Answers, "go vet, UI tests") || a.DecidedBy != "local" {
		t.Errorf("stored %+v", a)
	}

	// Dismiss: deny with mado's message.
	x.fw.send(askReq(sid, "q2", askFixture(t)))
	time.Sleep(100 * time.Millisecond)
	if st := approve(map[string]any{"request_id": "q2", "decision": "deny"}, &dec); st != 200 || dec.Decision != "deny" {
		t.Fatalf("dismiss: %d %+v", st, dec)
	}
	if ar := x.fw.next(proto.TApprovalResponse).(*proto.ApprovalResponse); ar.Message != dismissMessage || len(ar.UpdatedInput) != 0 {
		t.Errorf("dismiss response %+v", ar)
	}

	// After a reload both question cards are there, with their questions and answers;
	// the Bash approval is still pending.
	var bl struct{ Approvals []apiApproval }
	apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/sessions/"+sid+"/blocks", nil, &bl)
	got := map[string]apiApproval{}
	for _, a := range bl.Approvals {
		got[a.RequestID] = a
	}
	if len(bl.Approvals) != 3 || got["q1"].Answers[q3] != "Stacked" || got["q2"].Decision != "deny" || got["b1"].State != "pending" ||
		!strings.Contains(string(got["q1"].Questions), `"multiSelect":true`) || got["b1"].Questions != nil {
		t.Errorf("blocks approvals %+v", bl.Approvals)
	}
	if !strings.HasPrefix(got["q1"].Summary, "このディレクトリ") || !strings.HasSuffix(got["q1"].Summary, "(+2 more)") {
		t.Errorf("summary %q", got["q1"].Summary)
	}
}

func TestQuestionNeverAutoDecided(t *testing.T) {
	x := newM2(t)
	fc := newFakeClassifier(t)
	x.th.h.classifier = &policy.Classifier{Upstream: messages.Upstream{BaseURL: fc.srv.URL}, Model: "m", Timeout: 3 * time.Second}
	sid := x.autoSession(t, "r")
	x.th.store.AddSessionRule(sid, ToolAskUserQuestion)
	fc.queue(`{"decision":"allow","reason":"looks fine"}`)
	x.fw.send(askReq(sid, "q1", askFixture(t)))
	x.fw.quiet(300 * time.Millisecond)
	a, _ := x.th.store.Approval(sid, "q1")
	if a.State != "pending" || a.Auto != "ask" || a.Reason != reasonQuestion || fc.count() != 0 {
		t.Fatalf("auto handled a question: %+v, classifier calls %d", a, fc.count())
	}
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	var e struct{ Error string }
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/sessions/"+sid+"/approve",
		map[string]any{"request_id": "q1", "decision": "allow_session"}, &e); st != 400 {
		t.Errorf("allow_session in auto: %d %s", st, e.Error)
	}
	var dec apiApproval
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/sessions/"+sid+"/approve",
		map[string]any{"request_id": "q1", "decision": "allow", "answers": map[string]string{q1: "Usage 集計の高速化"}}, &dec); st != 200 ||
		dec.Auto != "ask" || dec.Answers[q1] == "" {
		t.Fatalf("answer in auto: %d %+v", st, dec)
	}
	if ar := x.fw.next(proto.TApprovalResponse).(*proto.ApprovalResponse); !strings.Contains(string(ar.UpdatedInput), "Usage") {
		t.Errorf("response %+v", ar)
	}
}
