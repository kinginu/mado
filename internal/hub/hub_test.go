package hub

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kinginu/mado/internal/proto"
)

func TestHashToken(t *testing.T) {
	if got := HashToken("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal(got)
	}
	a, b := RandomToken(32), RandomToken(32)
	if len(a) != 43 || a == b || strings.ContainsAny(a, "+/=") {
		t.Fatalf("tokens %q %q", a, b)
	}
}

func TestLocalLoginAllowed(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:1", "127.1.2.3:8740", "[::1]:1", "localhost:1"} {
		if err := LocalLoginAllowed(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:1", ":8740", "[::]:1", "100.64.0.1:1", "100.101.102.103:8740",
		"[fd7a:115c:a1e0::1]:1", "192.168.1.2:1", "example.com:1", "127.0.0.1"} {
		if LocalLoginAllowed(bad) == nil {
			t.Errorf("%s allowed", bad)
		}
	}
}

func TestAllowlist(t *testing.T) {
	a := AllowConfig{Subs: []string{"s1"}, Emails: []string{"Me@Example.org"}}
	if !a.Allowed(Identity{Sub: "s1"}) || !a.Allowed(Identity{Sub: "x", Email: "me@example.org", EmailVerified: true}) {
		t.Error("allowed identities refused")
	}
	if a.Allowed(Identity{Sub: "x", Email: "me@example.org"}) || a.Allowed(Identity{Sub: "x"}) {
		t.Error("unverified email or unknown sub accepted")
	}
}

// --- fake IdP with discovery, JWKS and a PKCE-checking token endpoint.

type fakeIdP struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	mu    sync.Mutex
	codes map[string]fakeGrant
}

type fakeGrant struct {
	challenge, nonce, sub, email string
	groups                       []string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	f := &fakeIdP{key: k, codes: map[string]fakeGrant{}}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		g, ok := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || b64(sum[:]) != g.challenge {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": f.sign(map[string]any{"iss": f.srv.URL, "sub": g.sub, "aud": "mado-test", "exp": time.Now().Add(time.Hour).Unix(),
				"iat": time.Now().Unix(), "nonce": g.nonce, "email": g.email, "email_verified": true, "groups": g.groups})})
	})
	return f
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (f *fakeIdP) sign(claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	c, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	return in + "." + b64(sig)
}

type testHub struct {
	h     *Hub
	srv   *httptest.Server
	store *Store
}

func newTestHub(t *testing.T, issuer string, local bool) *testHub {
	t.Helper()
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	cfg := &Config{BaseURL: srv.URL, Listen: "127.0.0.1:0",
		OIDC:  OIDCConfig{Issuer: issuer, ClientID: "mado-test", RedirectURL: srv.URL + "/callback", Scopes: []string{"openid", "email"}},
		Allow: AllowConfig{Emails: []string{"me@example.org"}}}
	store, err := OpenStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h, err := New(cfg, store, &Secrets{Keys: map[string]string{}}, Options{LocalLogin: local})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	handler = h.Handler()
	return &testHub{h: h, srv: srv, store: store}
}

func browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestOIDCLoginFlow(t *testing.T) {
	idp := newFakeIdP(t)
	th := newTestHub(t, idp.srv.URL, false)

	login := func(c *http.Client, sub, email string) (*http.Response, url.Values) {
		resp, err := c.Get(th.srv.URL + "/login")
		if err != nil || resp.StatusCode != http.StatusFound {
			t.Fatalf("login: %v %v", err, resp.Status)
		}
		loc, _ := url.Parse(resp.Header.Get("Location"))
		q := loc.Query()
		if !strings.HasPrefix(loc.String(), idp.srv.URL+"/authorize") || q.Get("code_challenge_method") != "S256" ||
			q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" || q.Get("client_id") != "mado-test" {
			t.Fatalf("authorize url %s", loc)
		}
		code := RandomToken(8)
		idp.mu.Lock()
		idp.codes[code] = fakeGrant{q.Get("code_challenge"), q.Get("nonce"), sub, email, nil}
		idp.mu.Unlock()
		return nil, url.Values{"code": {code}, "state": {q.Get("state")}}
	}

	// Allowed user.
	c := browser()
	_, cb := login(c, "u1", "me@example.org")
	resp, _ := c.Get(th.srv.URL + "/callback?" + cb.Encode())
	if resp.StatusCode != http.StatusSeeOther {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("callback %s: %s", resp.Status, b)
	}
	resp, _ = c.Get(th.srv.URL + "/")
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `<script type="module" src="/ui/`+uiVersion+`/app.js">`) {
		t.Fatalf("index: %s %s", resp.Status, b)
	}
	// Replaying the same state fails (single use).
	resp, _ = c.Get(th.srv.URL + "/callback?" + cb.Encode())
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("replay: %s", resp.Status)
	}

	// State mismatch.
	c2 := browser()
	_, cb2 := login(c2, "u1", "me@example.org")
	cb2.Set("state", "forged")
	if resp, _ := c2.Get(th.srv.URL + "/callback?" + cb2.Encode()); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("forged state: %s", resp.Status)
	}

	// Not on the allowlist.
	c3 := browser()
	_, cb3 := login(c3, "u2", "stranger@example.org")
	if resp, _ := c3.Get(th.srv.URL + "/callback?" + cb3.Encode()); resp.StatusCode != http.StatusForbidden {
		t.Errorf("stranger: %s", resp.Status)
	}
	if resp, _ := c3.Get(th.srv.URL + "/"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("stranger got a session: %s", resp.Status)
	}

	// Local login is off.
	if resp, _ := browser().Post(th.srv.URL+"/local-login", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("local-login reachable: %s", resp.Status)
	}
}

var reCSRF = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func csrfOf(t *testing.T, c *http.Client, u string) string {
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	m := reCSRF.FindSubmatch(b)
	if m == nil {
		t.Fatalf("no csrf in %s", b)
	}
	return string(m[1])
}

// apiCSRF is the X-CSRF-Token value of a logged-in client (GET /api/me).
func apiCSRF(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	var me struct{ CSRF string }
	resp, err := c.Get(base + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if json.NewDecoder(resp.Body).Decode(&me) != nil || me.CSRF == "" {
		t.Fatalf("/api/me: %s", resp.Status)
	}
	return me.CSRF
}

// apiDo sends a JSON API request as the SPA does (same Origin, X-CSRF-Token) and
// decodes the answer into v when given. It returns the status code.
func apiDo(t *testing.T, c *http.Client, base, csrf, method, path string, body, v any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", base)
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if v != nil {
		if err := json.Unmarshal(b, v); err != nil && len(b) > 0 {
			t.Fatalf("%s %s: %s", method, path, b)
		}
	}
	return resp.StatusCode
}

type fakeWorker struct {
	t  *testing.T
	c  *websocket.Conn
	ch chan proto.Frame
}

func (w *fakeWorker) send(f proto.Frame) {
	b, _ := proto.Encode(f)
	if err := w.c.Write(context.Background(), websocket.MessageText, b); err != nil {
		w.t.Fatal(err)
	}
}

func (w *fakeWorker) next(want string) proto.Frame {
	w.t.Helper()
	for {
		select {
		case f := <-w.ch:
			if proto.HeaderOf(f).T == want {
				return f
			}
		case <-time.After(5 * time.Second):
			w.t.Fatalf("worker: timeout waiting for %s", want)
		}
	}
}

func TestEndToEndLocalLoginWorkerSSE(t *testing.T) {
	th := newTestHub(t, "", true)
	c := browser()
	if resp, _ := c.Post(th.srv.URL+"/local-login", "", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("local login: %s", resp.Status)
	}

	// Enroll a machine.
	code, _ := th.store.CreateEnrollCode("box", time.Minute)
	resp, _ := http.Post(th.srv.URL+"/enroll", "application/json", strings.NewReader(`{"name":"box","code":"`+code+`"}`))
	var tok struct{ Token string }
	json.NewDecoder(resp.Body).Decode(&tok)
	if tok.Token == "" {
		t.Fatalf("enroll: %s", resp.Status)
	}
	if resp, _ := http.Post(th.srv.URL+"/enroll", "application/json", strings.NewReader(`{"name":"box","code":"`+code+`"}`)); resp.StatusCode != 403 {
		t.Errorf("code reused: %s", resp.Status)
	}

	wsURL := "ws" + strings.TrimPrefix(th.srv.URL, "http") + "/ws/worker"
	if _, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer wrong"}}}); err == nil {
		t.Fatal("bad machine token accepted")
	}
	wc, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + tok.Token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer wc.CloseNow()
	fw := &fakeWorker{t: t, c: wc, ch: make(chan proto.Frame, 32)}
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
	fw.send(&proto.Hello{Machine: "box", Version: "t", ClaudeVersion: "2.1.302", Projects: []string{"/proj/a"}})
	time.Sleep(100 * time.Millisecond)
	ms, _ := th.store.Machines()

	// Create a session through the API.
	csrf := apiCSRF(t, c, th.srv.URL)
	do := func(method, path string, body, v any) int {
		return apiDo(t, c, th.srv.URL, csrf, method, path, body, v)
	}
	create := map[string]any{"kind": "code", "machine": ms[0].ID, "cwd": "/proj/a", "profile": "subscription", "main_model": "opus", "permission_mode": "default"}
	var e struct{ Error string }
	if st := do("POST", "/api/sessions", map[string]any{"kind": "code", "machine": ms[0].ID, "cwd": "/etc", "profile": "subscription"}, &e); st != 400 || !strings.Contains(e.Error, "allowlist") {
		t.Errorf("non-allowlisted cwd accepted: %d %s", st, e.Error)
	}
	if st := apiDo(t, c, th.srv.URL, "", "POST", "/api/sessions", create, nil); st != 403 {
		t.Errorf("missing csrf: %d", st)
	}
	var created struct{ ID string }
	if st := do("POST", "/api/sessions", create, &created); st != 201 || created.ID == "" {
		t.Fatalf("create: %d", st)
	}
	sid := created.ID
	st := fw.next(proto.TStartSession).(*proto.StartSession)
	if st.SessionID != sid || st.CWD != "/proj/a" || st.Profile.Kind != proto.KindSubscription || st.MainModel != "opus" {
		t.Fatalf("start frame: %+v", st)
	}
	fw.send(&proto.Ack{Ref: st.ID, OK: true})

	// SSE.
	req, _ := http.NewRequest("GET", th.srv.URL+"/s/"+sid+"/events?after=0", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req = req.WithContext(ctx)
	sresp, err := c.Do(req)
	if err != nil || sresp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("sse: %v %v", err, sresp.Header)
	}
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(sresp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	waitLine := func(sub string) {
		t.Helper()
		for {
			select {
			case l := <-lines:
				if strings.Contains(l, sub) {
					return
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("sse: no line containing %q", sub)
			}
		}
	}
	fixture, _ := os.ReadFile("../render/testdata/write_tool.jsonl")
	fl := strings.Split(strings.TrimSpace(string(fixture)), "\n")
	fw.send(&proto.SessionState{SessionID: sid, State: proto.StateRunning, ClaudeSessionID: "ef4d08a3-0fb3-45bf-927f-28034415014c"})
	waitLine("event: state")
	fw.send(&proto.Event{SessionID: sid, Seq: 1, Raw: json.RawMessage(fl[1])})
	waitLine("id: 1")
	waitLine(`"kind":"tool_use","label":"Write"`)
	fw.send(&proto.Event{SessionID: sid, Seq: 1, Raw: json.RawMessage(fl[1])}) // duplicate after resync: ignored
	fw.send(&proto.Event{SessionID: sid, Seq: 2, Raw: json.RawMessage(fl[4])})
	waitLine("id: 2")
	waitLine(`\u003cp\u003edone\u003c/p\u003e`)

	// Approval round trip with "allow for session".
	fw.send(&proto.ApprovalRequest{SessionID: sid, RequestID: "r1", ToolName: "Write", Input: json.RawMessage(`{"file_path":"/proj/a/x"}`)})
	waitLine(`"request_id":"r1","tool_name":"Write"`)
	var decided apiApproval
	if st := do("POST", "/api/sessions/"+sid+"/approve", map[string]string{"request_id": "r1", "decision": "allow_session"}, &decided); st != 200 || decided.State != "decided" {
		t.Fatalf("approve: %d %+v", st, decided)
	}
	ar := fw.next(proto.TApprovalResponse).(*proto.ApprovalResponse)
	if ar.RequestID != "r1" || ar.Decision != proto.DecisionAllowSession {
		t.Fatalf("approval response %+v", ar)
	}
	if st := do("POST", "/api/sessions/"+sid+"/approve", map[string]string{"request_id": "r1", "decision": "deny"}, nil); st != http.StatusConflict {
		t.Errorf("double decision: %d", st)
	}
	fw.send(&proto.ApprovalRequest{SessionID: sid, RequestID: "r2", ToolName: "Write", Input: json.RawMessage(`{"file_path":"/proj/a/y"}`)})
	if ar := fw.next(proto.TApprovalResponse).(*proto.ApprovalResponse); ar.RequestID != "r2" || ar.Decision != proto.DecisionAllow {
		t.Fatalf("remembered approval: %+v", ar)
	}
	waitLine(`"request_id":"r2"`) // the remembered decision is shown too
	// A different tool is not covered by the rule.
	fw.send(&proto.ApprovalRequest{SessionID: sid, RequestID: "r3", ToolName: "Bash", Input: json.RawMessage(`{"command":"ls"}`)})
	waitLine(`"tool_name":"Bash","summary":"ls"`)

	// Send a message.
	if st := do("POST", "/api/sessions/"+sid+"/send", map[string]string{"text": "reply with ok"}, nil); st != 202 {
		t.Fatalf("send: %d", st)
	}
	if um := fw.next(proto.TUserMessage).(*proto.UserMessage); um.Text != "reply with ok" || um.SessionID != sid {
		t.Fatalf("user message %+v", um)
	}
	// Cross-origin POST is refused even with the token.
	r2, _ := http.NewRequest("POST", th.srv.URL+"/api/sessions/"+sid+"/send", strings.NewReader(`{"text":"x"}`))
	r2.Header.Set("X-CSRF-Token", csrf)
	r2.Header.Set("Origin", "https://evil.example")
	if resp, _ := c.Do(r2); resp.StatusCode != 403 {
		t.Errorf("cross-origin post: %s", resp.Status)
	}

	// The blocks API returns the stored events; another user cannot see the session.
	var bl struct {
		Blocks    []apiBlock
		Last      int64
		Approvals []apiApproval
	}
	if st := do("GET", "/api/sessions/"+sid+"/blocks", nil, &bl); st != 200 || bl.Last != 2 || len(bl.Blocks) == 0 ||
		bl.Blocks[len(bl.Blocks)-1].Text != "done" || len(bl.Approvals) != 1 || bl.Approvals[0].RequestID != "r3" {
		t.Errorf("blocks: %d %+v", st, bl)
	}
	if st := status(t, c, th.srv.URL+"/s/"+sid); st != 200 {
		t.Errorf("shell for /s/{id}: %d", st)
	}
	th.store.UpsertUser(User{Sub: "other"})
	other, _ := th.store.CreateWebSession("other", time.Hour)
	for _, p := range []string{"/api/sessions/" + sid, "/api/sessions/" + sid + "/blocks", "/s/" + sid + "/events"} {
		r3, _ := http.NewRequest("GET", th.srv.URL+p, nil)
		r3.AddCookie(&http.Cookie{Name: cookieSession, Value: other})
		if resp, _ := http.DefaultClient.Do(r3); resp.StatusCode != 404 {
			t.Errorf("other user sees %s: %s", p, resp.Status)
		}
	}

	// A start failure reported by the worker marks the session.
	fw.send(&proto.Ack{Ref: "nope", OK: false, Error: "x"})
}
