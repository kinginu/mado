package hub

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/proto"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		err  bool
	}{
		{`/new box /proj/a`, []string{"/new", "box", "/proj/a"}, false},
		{"  /new\tbox   /proj/a  r ", []string{"/new", "box", "/proj/a", "r"}, false},
		{`/new box "/proj/my app"`, []string{"/new", "box", "/proj/my app"}, false},
		{`/new box "/proj/my app" r`, []string{"/new", "box", "/proj/my app", "r"}, false},
		{`/new "box" /proj/"a b"`, []string{"/new", "box", "/proj/a b"}, false},
		{`/new box "/p/say \"hi\""`, []string{"/new", "box", `/p/say "hi"`}, false},
		{`/new box "/p/back\\slash"`, []string{"/new", "box", `/p/back\slash`}, false},
		{`/new box /p/back\slash`, []string{"/new", "box", `/p/back\slash`}, false}, // outside quotes \ is literal
		{`/new box ""`, []string{"/new", "box", ""}, false},
		{`/new box "/proj/a`, nil, true},
	}
	for _, c := range cases {
		got, err := splitCommand(c.in)
		if (err != nil) != c.err || !slices.Equal(got, c.want) {
			t.Errorf("splitCommand(%q) = %q, %v; want %q, err=%v", c.in, got, err, c.want, c.err)
		}
	}
	for _, c := range []struct {
		in            string
		cmd, ok       bool
		project, prof string
	}{
		{`/new box "/proj/my app"`, true, true, "/proj/my app", "subscription"},
		{`/new box "/proj/my app" r`, true, true, "/proj/my app", "r"},
		{`/new box "/proj/my app`, true, false, "", ""},
		{`/new box "/a" r extra`, true, false, "", ""},
		{`/new "bo x" /a`, true, false, "", ""},
		{`/newer box /a`, false, true, "", ""},
		{`hello /new`, false, true, "", ""},
	} {
		req, isCmd, err := parseNewCommand(c.in)
		if isCmd != c.cmd || (err == nil) != c.ok || (c.ok && c.cmd && (req.Project != c.project || req.Profile != c.prof)) {
			t.Errorf("parseNewCommand(%q) = %+v, %v, %v", c.in, req, isCmd, err)
		}
	}
}

func TestNewCommandQuotedProjectAndSessionLink(t *testing.T) {
	x := newM2(t)
	x.fw.send(&proto.Hello{Machine: "box", Projects: []string{"/proj/a", "/proj/my app"}})
	time.Sleep(100 * time.Millisecond)
	sid := x.session(t, "subscription", proto.StateIdle, "")
	// Unquoted, the path splits into too many tokens or misses the allowlist.
	if st, out := x.send(t, sid, `/new box /proj/my app`); st != http.StatusBadRequest || out["id"] != "" {
		t.Fatalf("unquoted spaced path accepted: %d %v", st, out)
	}
	// Prefix of a real project is not a match: exact only.
	if _, out := x.send(t, sid, `/new box "/proj/my"`); !strings.Contains(out["error"], "allowlist") {
		t.Fatalf("prefix project: %v", out)
	}
	_, out := x.send(t, sid, `/new box "/proj/my app"`)
	st := x.fw.next(proto.TStartSession).(*proto.StartSession)
	if st.CWD != "/proj/my app" || out["id"] != st.SessionID {
		t.Fatalf("quoted /new: %+v %v", st, out)
	}
}
