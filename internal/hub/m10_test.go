package hub

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// M10 (DESIGN §19.3): the local login.

func TestLocalLoginIdentity(t *testing.T) {
	t.Setenv("USER", "kei")
	th := newTestHub(t, "", true)
	c := browser()
	resp, err := c.Post(th.srv.URL+"/local-login", "", nil)
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("local login: %v %v", err, resp)
	}
	u, err := th.store.User("local")
	if err != nil || u.Name != "kei" || u.Email != "local@localhost" || !u.IsAdmin {
		t.Fatalf("user %+v %v", u, err)
	}
	rows, _ := th.store.AuditFor("local", 10)
	if len(rows) == 0 || rows[0].Action != "login" || rows[0].Detail != "local" {
		t.Fatalf("audit %+v", rows)
	}
	if resp, _ := c.Get(th.srv.URL + "/"); resp.StatusCode != 200 {
		t.Errorf("index after local login: %s", resp.Status)
	}
	if got := (&Hub{cfg: &Config{}}); got.localIdentity().Name != "kei" {
		t.Error("name from $USER")
	}
	t.Setenv("USER", "")
	if (&Hub{cfg: &Config{}}).localIdentity().Name != "Local user" {
		t.Error("fallback name")
	}
}

func TestLocalLoginRefusesProxiedRequests(t *testing.T) {
	th := newTestHub(t, "", true)
	for _, hdr := range []string{"X-Forwarded-For", "Tailscale-User-Login"} {
		req, _ := http.NewRequest("POST", th.srv.URL+"/local-login", nil)
		req.Header.Set(hdr, "100.64.0.7")
		resp, err := browser().Do(req)
		if err != nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %v %v", hdr, err, resp)
		}
	}
	// A request from another host (RemoteAddr not loopback) is refused too.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/local-login", nil)
	req.RemoteAddr = "100.64.0.7:5555"
	th.h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("remote addr: %d", rec.Code)
	}
	if n, _ := th.store.AuditFor("local", 10); len(n) != 0 {
		t.Errorf("refused requests logged in: %+v", n)
	}
}

func TestLoginPageVariants(t *testing.T) {
	page := func(th *testHub, path string) (int, string, string) {
		resp, err := browser().Get(th.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Location"), string(b)
	}
	// Local login, no issuer: one Continue button, no IdP.
	st, _, b := page(newTestHub(t, "", true), "/login")
	if st != 200 || !strings.Contains(b, `action="/local-login"`) || !strings.Contains(b, ">Continue<") || strings.Contains(b, "idp=1") {
		t.Errorf("local only: %d %s", st, b)
	}
	// Local login and an issuer: the IdP is offered as well, and reachable.
	idp := newFakeIdP(t)
	both := newTestHub(t, idp.srv.URL, true)
	if st, _, b := page(both, "/login"); st != 200 || !strings.Contains(b, ">Continue<") || !strings.Contains(b, `href="/login?idp=1"`) {
		t.Errorf("local + idp: %d %s", st, b)
	}
	if st, loc, _ := page(both, "/login?idp=1"); st != http.StatusFound || !strings.HasPrefix(loc, idp.srv.URL+"/authorize") {
		t.Errorf("idp link: %d %s", st, loc)
	}
	// Local login off: straight to the IdP, as before.
	if st, loc, _ := page(newTestHub(t, idp.srv.URL, false), "/login"); st != http.StatusFound || !strings.HasPrefix(loc, idp.srv.URL+"/authorize") {
		t.Errorf("idp only: %d %s", st, loc)
	}
}
