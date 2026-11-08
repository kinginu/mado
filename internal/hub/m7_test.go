package hub

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/proto"
)

func TestBudgetsConfig(t *testing.T) {
	dir := t.TempDir()
	load := func(extra string) (*Config, error) {
		p := filepath.Join(dir, "hub.toml")
		os.WriteFile(p, []byte("base_url = \"http://127.0.0.1:1\"\n"+extra), 0o600)
		return LoadConfig(p)
	}
	c, err := load("")
	if err != nil || c.Budgets.Currency != "USD" || c.Budgets.DefaultMonthly != nil {
		t.Fatalf("defaults %+v %v", c.Budgets, err)
	}
	c, err = load("[budgets]\ncurrency = \"jpy\"\ndefault_monthly = 3000\n")
	if err != nil || c.Budgets.Currency != "JPY" || c.Budgets.DefaultMonthly == nil || *c.Budgets.DefaultMonthly != 3000 {
		t.Fatalf("jpy %+v %v", c.Budgets, err)
	}
	for _, bad := range []string{"currency = \"yens\"", "currency = \"U1D\"", "default_monthly = -1", "default_monthly_usd = 10"} {
		if _, err := load("[budgets]\n" + bad + "\n"); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// §17.6: at or above the month's budget, chat sends, chat creation, router code
// sessions and messages into them are refused with the amount; subscription is exempt.
func TestBudgetsRefuseWork(t *testing.T) {
	x := newChatHub(t)
	one := 1.0
	x.h.cfg.Budgets = BudgetsConfig{Currency: "JPY", DefaultMonthly: &one}
	sid := x.newChat(t, "glm-5.3", "")
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/send", map[string]any{"text": "hi"}, nil); st != 202 {
		t.Fatalf("first send: %d", st)
	}
	waitIdle(t, x, sid)
	// One response of the fake upstream costs 0.6 + 0.1 + 1.1 = 1.8 >= 1.
	var me struct {
		Currency string
		Budget   budgetState
	}
	x.getJSON(t, "/api/me", &me)
	if me.Currency != "JPY" || !me.Budget.Over || me.Budget.Budget == nil || *me.Budget.Budget != 1 || me.Budget.MonthCost < 1.79 {
		t.Fatalf("me %+v", me)
	}
	var e struct{ Error string }
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/send", map[string]any{"text": "again"}, &e); st != 402 ||
		!strings.Contains(e.Error, "¥2 used of ¥1") {
		t.Fatalf("over budget send: %d %q", st, e.Error)
	}
	if st := x.do(t, "POST", "/api/sessions", map[string]any{"kind": "chat"}, &e); st != 402 || !strings.Contains(e.Error, "monthly budget") {
		t.Fatalf("over budget create: %d %q", st, e.Error)
	}
	if n := x.llm.count(); n != 1 {
		t.Fatalf("upstream called %d times", n)
	}
	// The user's own budget wins over the default.
	hundred := 100.0
	x.store.SetUserBudget("local", &hundred)
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/send", map[string]any{"text": "again"}, nil); st != 202 {
		t.Fatalf("send with own budget: %d", st)
	}
	waitIdle(t, x, sid)
	// Last month's usage does not count.
	x.store.SetUserBudget("local", &one)
	x.store.db.Exec(`UPDATE usage SET ts='2020-01-15T00:00:00Z'`)
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/send", map[string]any{"text": "new month"}, nil); st != 202 {
		t.Fatalf("send in a new month: %d", st)
	}
	waitIdle(t, x, sid)
}

func waitIdle(t *testing.T, x *chatHub, sid string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if !x.h.runner.Running(sid) {
			return
		}
	}
	t.Fatal("turn did not end")
}

func TestBudgetsRefuseRouterCodeWork(t *testing.T) {
	x := newM2(t)
	zero := 0.0
	x.th.h.cfg.Budgets = BudgetsConfig{Currency: "USD", DefaultMonthly: &zero}
	router := x.session(t, "r", proto.StateIdle, "")
	sub := x.session(t, "subscription", proto.StateIdle, "")
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	var e struct{ Error string }
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/sessions/"+router+"/send", map[string]string{"text": "hi"}, &e); st != 402 ||
		!strings.Contains(e.Error, "$0.00 used of $0.00") {
		t.Fatalf("router message: %d %q", st, e.Error)
	}
	if st, out := x.send(t, sub, "hi"); st != 202 {
		t.Fatalf("subscription message: %d %v", st, out)
	}
	x.fw.next(proto.TUserMessage)
	body := map[string]string{"kind": "code", "machine": "box", "cwd": "/proj/a", "profile": "r"}
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/sessions", body, &e); st != 402 {
		t.Fatalf("router session: %d %q", st, e.Error)
	}
	if st, _ := x.send(t, sub, "/new box /proj/b r"); st != 402 {
		t.Fatalf("/new on a router profile: %d", st)
	}
	body["profile"] = "subscription"
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/sessions", body, nil); st != 201 {
		t.Fatalf("subscription session: %d", st)
	}
	x.fw.next(proto.TStartSession)
}

func TestUsageAPIAndCSV(t *testing.T) {
	x := newChatHub(t)
	x.h.cfg.Budgets.Currency = "JPY"
	rec := func(sub, model, kind, id, ts string, in, out int64) {
		x.store.UpsertUser(User{Sub: sub, Email: sub + "@example.org", Name: strings.ToUpper(sub), IsAdmin: sub == "local"})
		if err := x.store.RecordUsage(UsageRow{TS: ts, SessionID: "s-" + sub, Kind: kind, OwnerSub: sub, Model: model, MessageID: id,
			Input: in, Output: out}, Rates{Input: 1, Output: 2}); err != nil {
			t.Fatal(err)
		}
	}
	this := monthOf(time.Now())
	ts := this + "-02T03:04:05Z"
	rec("local", "zai-org/glm-5.3", "chat", "a1", ts, 1000000, 1000000)
	rec("local", "zai-org/glm-5.3", "chat", "a2", ts, 1000000, 0)
	rec("local", "=cmd()", "code", "a3", ts, 0, 500000)
	rec("bob", "zai-org/glm-5.3", "chat", "b1", ts, 2000000, 0)
	rec("local", "zai-org/glm-5.3", "chat", "old", "2026-01-31T23:59:59.9Z", 1000000, 0)

	type usageResp struct {
		Month, User, Currency string
		Months                []string
		Models                []UsageGroup
		Total                 UsageTotals
		Users                 []UsageGroup
		People                []usagePerson
		Budget                *budgetState
	}
	var u usageResp
	if st := x.getJSON(t, "/api/usage", &u); st != 200 || u.Month != this || u.User != "" || u.Currency != "JPY" ||
		u.Total.Cost != 7 || len(u.Users) != 2 || u.Users[0].Sub != "local" || u.Users[0].Cost != 5 || len(u.People) != 2 ||
		len(u.Models) != 2 || u.Models[0].Model != "zai-org/glm-5.3" || u.Models[0].Responses != 3 {
		t.Fatalf("admin, everyone: %d %+v", st, u)
	}
	u = usageResp{}
	x.getJSON(t, "/api/usage?user=local&month=2026-01", &u)
	if u.Total.Cost != 1 || u.Users != nil || u.Budget != nil || len(u.Months) < 2 || u.Months[len(u.Months)-1] != "2026-01" {
		t.Fatalf("admin, local in January: %+v", u)
	}
	if st := x.getJSON(t, "/api/usage?month=2026-1", nil); st != 400 {
		t.Errorf("bad month: %d", st)
	}

	// A user sees only themselves, whatever ?user= says.
	bob := x.loginAs(t, "bob", false)
	get := func(c *http.Client, path string) (int, string) {
		resp, err := c.Get(x.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode, readAll(resp)
	}
	st, b := get(bob, "/api/usage")
	if st != 200 || !strings.Contains(b, `"user":"bob"`) || strings.Contains(b, `"people"`) || strings.Contains(b, "local@") ||
		!strings.Contains(b, `"budget":{"budget":null`) {
		t.Fatalf("bob: %d %s", st, b)
	}
	if st, _ := get(bob, "/api/usage?user=local"); st != 403 {
		t.Errorf("bob reads local: %d", st)
	}
	if st, _ := get(bob, "/api/usage.csv?user=local"); st != 403 {
		t.Errorf("bob exports local: %d", st)
	}
	st, b = get(bob, "/api/usage.csv")
	if st != 200 || strings.Count(b, "\n") != 2 || !strings.Contains(b, "bob@example.org") || strings.Contains(b, "local") {
		t.Fatalf("bob csv: %d %s", st, b)
	}

	// The admin export: every row of the month, formula cells defused, in the currency.
	resp, _ := x.c.Get(x.srv.URL + "/api/usage.csv?month=" + this)
	b = readAll(resp)
	if resp.Header.Get("Content-Disposition") != `attachment; filename="mado-usage-`+this+`.csv"` ||
		!strings.HasPrefix(b, "ts,user,email,kind,session_id,model,message_id,input,cache_read,cache_write,output,cost,currency\n") ||
		strings.Count(b, "\n") != 5 || !strings.Contains(b, ",'=cmd(),") || !strings.Contains(b, ",1,JPY\n") {
		t.Fatalf("admin csv: %v %s", resp.Header, b)
	}
}

func TestAdminUsersAPI(t *testing.T) {
	x := newChatHub(t)
	x.h.cfg.Budgets.Currency = "JPY"
	def := 1000.0
	x.h.cfg.Budgets.DefaultMonthly = &def
	bob := x.loginAs(t, "bob", false)
	x.store.RecordUsage(UsageRow{SessionID: "s", Kind: "chat", OwnerSub: "bob", Model: "m", MessageID: "b1", Input: 1000000}, Rates{Input: 1500})
	type usersResp struct {
		Users         []apiUser
		Currency      string
		DefaultBudget *float64 `json:"default_budget"`
	}
	var u usersResp
	if st := x.getJSON(t, "/api/admin/users", &u); st != 200 || len(u.Users) != 2 || u.Currency != "JPY" || *u.DefaultBudget != 1000 {
		t.Fatalf("users: %d %+v", st, u)
	}
	var b apiUser
	for _, v := range u.Users {
		if v.Sub == "bob" {
			b = v
		}
	}
	if !b.Over || b.MonthCost != 1500 || b.Own != nil || *b.Budget != 1000 || b.IsAdmin || b.LastLogin == "" {
		t.Fatalf("bob %+v", b)
	}
	if st := x.do(t, "PATCH", "/api/admin/users/bob", map[string]any{"monthly_budget": 2000}, &b); st != 200 || b.Over || *b.Own != 2000 {
		t.Fatalf("raise: %d %+v", st, b)
	}
	if own, _ := x.store.UserBudget("bob"); own == nil || *own != 2000 {
		t.Fatalf("stored %v", own)
	}
	if st := x.do(t, "PATCH", "/api/admin/users/bob", map[string]any{"monthly_budget": nil}, &b); st != 200 || !b.Over || b.Own != nil {
		t.Fatalf("back to default: %d %+v", st, b)
	}
	for _, bad := range []map[string]any{{"monthly_budget": -1}, {"monthly_budget": "5"}, {"is_admin": true}, {}} {
		if st := x.do(t, "PATCH", "/api/admin/users/bob", bad, nil); st != 400 {
			t.Errorf("%v: %d", bad, st)
		}
	}
	if st := x.do(t, "PATCH", "/api/admin/users/nobody", map[string]any{"monthly_budget": 1}, nil); st != 404 {
		t.Errorf("unknown user: %d", st)
	}
	// Not for users.
	bcsrf := apiCSRF(t, bob, x.srv.URL)
	if st := apiDo(t, bob, x.srv.URL, bcsrf, "GET", "/api/admin/users", nil, nil); st != 403 {
		t.Errorf("bob lists users: %d", st)
	}
	if st := apiDo(t, bob, x.srv.URL, bcsrf, "PATCH", "/api/admin/users/bob", map[string]any{"monthly_budget": 1e9}, nil); st != 403 {
		t.Errorf("bob raises his budget: %d", st)
	}
}

// M6 open issue: a decision taken while the machine is offline is not recorded.
func TestApprovalWhileOfflineStaysPending(t *testing.T) {
	x := newM2(t)
	code, _ := x.th.store.CreateEnrollCode("away", time.Minute)
	x.th.store.Enroll("away", code)
	var awayID string
	ms, _ := x.th.store.Machines()
	for _, m := range ms {
		if m.Name == "away" {
			awayID = m.ID
		}
	}
	sid := proto.NewID()
	x.th.store.CreateSession(Session{ID: sid, MachineID: awayID, OwnerSub: "local", CWD: "/p", ProfileName: "subscription",
		MainModel: "fable", State: proto.StateWaitingApproval})
	x.th.store.InsertApproval(Approval{SessionID: sid, RequestID: "r1", ToolName: "Bash", Input: `{"command":"ls"}`})
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	var e struct{ Error string }
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/sessions/"+sid+"/approve",
		map[string]string{"request_id": "r1", "decision": "allow_session"}, &e); st != 503 || !strings.Contains(e.Error, "offline") {
		t.Fatalf("offline approve: %d %q", st, e.Error)
	}
	if p, _ := x.th.store.PendingApprovals(sid); len(p) != 1 {
		t.Fatalf("approval no longer pending: %+v", p)
	}
	if x.th.store.HasSessionRule(sid, "Bash:ls") {
		t.Fatal("rule remembered for an undelivered decision")
	}
	var blocks struct{ Approvals []apiApproval }
	apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/sessions/"+sid+"/blocks", nil, &blocks)
	if len(blocks.Approvals) != 1 || blocks.Approvals[0].State != "pending" {
		t.Fatalf("card after reload: %+v", blocks)
	}
	// Reopening restores a claimed approval.
	x.th.store.DecideApproval(sid, "r1", "allow", "local")
	x.th.store.ReopenApproval(sid, "r1")
	if a, _ := x.th.store.Approval(sid, "r1"); a.State != "pending" || a.Decision != "" {
		t.Fatalf("reopened %+v", a)
	}
}

// M6 open issue: an upload that no message refers to is not an attachment of the
// session, and housekeeping deletes it after a day.
func TestUnsentUploadsHiddenAndCollected(t *testing.T) {
	x := newChatHub(t)
	sid := x.newChat(t, "", "")
	_, sent := x.upload(t, sid, "sent.txt", []byte("kept"))
	_, unsent := x.upload(t, sid, "unsent.txt", []byte("dropped"))
	sentID, unsentID := sent["id"].(string), unsent["id"].(string)
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/send", map[string]any{"text": "see file", "file_ids": []string{sentID}}, nil); st != 202 {
		t.Fatalf("send: %d", st)
	}
	waitIdle(t, x, sid)
	var d struct {
		Files []map[string]any
	}
	x.getJSON(t, "/api/sessions/"+sid, &d)
	if len(d.Files) != 1 || d.Files[0]["id"] != sentID {
		t.Fatalf("panel files: %+v", d.Files)
	}
	uf, _ := x.store.File(unsentID)
	blob, _ := x.h.blobs.Path(uf.SHA256)
	x.h.housekeep(time.Now()) // too young
	if _, err := x.store.File(unsentID); err != nil {
		t.Fatal("a fresh upload was collected")
	}
	x.h.housekeep(time.Now().Add(25 * time.Hour))
	if _, err := x.store.File(unsentID); err == nil {
		t.Fatal("unsent upload survived a day")
	}
	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Errorf("blob of the unsent upload: %v", err)
	}
	if _, err := x.store.File(sentID); err != nil {
		t.Fatal("a sent attachment was collected")
	}
	if st := x.getJSON(t, "/files/"+sentID, nil); st != 200 {
		t.Errorf("sent file: %d", st)
	}
}
