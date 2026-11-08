package router

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/proto"
)

type seen struct {
	path    string
	headers http.Header
	body    map[string]any
}

func upstream(t *testing.T, ch chan<- seen, handler func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &m)
		ch <- seen{path: r.URL.Path, headers: r.Header.Clone(), body: m}
		handler(w)
	}))
	t.Cleanup(s.Close)
	return s
}

func profile(strong, worker string) proto.Profile {
	return proto.Profile{
		Name: "glm-pair", Kind: proto.KindRouter, Main: "oc-strong",
		Aliases: map[string]string{"opus": "oc-worker", "sonnet": "oc-worker", "haiku": "oc-worker", "fable": "oc-strong"},
		Upstreams: map[string]proto.Upstream{
			"oc-strong": {BaseURL: strong, Model: "zai-org/GLM-5.1", Key: "key-strong"},
			"oc-worker": {BaseURL: worker, Model: "Qwen/Qwen3-Coder-480B", Key: "key-worker", AuthHeader: "x-api-key"},
		},
	}
}

func post(t *testing.T, url, token, body string, extra map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMappingAndHeaders(t *testing.T) {
	ch := make(chan seen, 4)
	ok := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"message"}`)
	}
	strong := upstream(t, ch, ok)
	worker := upstream(t, ch, ok)
	p := profile(strong.URL, worker.URL+"/prefix/")
	if err := Validate(p); err != nil {
		t.Fatal(err)
	}
	r := New(nil)
	r.Register("tok-1", "s1", p)
	front := httptest.NewServer(r)
	defer front.Close()

	// Main model, bearer upstream; inbound x-api-key must be stripped.
	resp := post(t, front.URL+"/v1/messages", "tok-1", `{"model":"oc-strong","max_tokens":5,"messages":[]}`,
		map[string]string{"X-Api-Key": "leak-me", "Cookie": "c=1", "anthropic-beta": "x"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	s := <-ch
	if s.body["model"] != "zai-org/GLM-5.1" || s.body["max_tokens"].(float64) != 5 {
		t.Errorf("body not mapped: %v", s.body)
	}
	if got := s.headers.Get("Authorization"); got != "Bearer key-strong" {
		t.Errorf("upstream auth = %q", got)
	}
	if s.headers.Get("X-Api-Key") != "" || s.headers.Get("Cookie") != "" {
		t.Errorf("inbound credentials forwarded: %v", s.headers)
	}
	if s.headers.Get("Anthropic-Version") != "2023-06-01" || s.headers.Get("Anthropic-Beta") != "x" {
		t.Errorf("anthropic headers not forwarded: %v", s.headers)
	}

	// Worker model, x-api-key upstream, base path kept; inbound bearer (our session token) stripped.
	resp = post(t, front.URL+"/v1/messages", "tok-1", `{"model":"oc-worker"}`, nil)
	resp.Body.Close()
	s = <-ch
	if s.path != "/prefix/v1/messages" || s.body["model"] != "Qwen/Qwen3-Coder-480B" {
		t.Errorf("worker mapping: %s %v", s.path, s.body)
	}
	if s.headers.Get("X-Api-Key") != "key-worker" || s.headers.Get("Authorization") != "" {
		t.Errorf("worker auth headers: %v", s.headers)
	}
}

func TestRejections(t *testing.T) {
	ch := make(chan seen, 4)
	up := upstream(t, ch, func(w http.ResponseWriter) {})
	r := New(nil)
	r.Register("tok-1", "s1", profile(up.URL, up.URL))
	front := httptest.NewServer(r)
	defer front.Close()

	cases := []struct {
		name, path, token, body string
		status                  int
	}{
		{"no token", "/v1/messages", "", `{"model":"oc-strong"}`, 401},
		{"wrong token", "/v1/messages", "tok-2", `{"model":"oc-strong"}`, 401},
		{"prefix token", "/v1/messages", "tok-", `{"model":"oc-strong"}`, 401},
		{"unknown model", "/v1/messages", "tok-1", `{"model":"claude-opus-5-5"}`, 404},
		{"upstream real name is not accepted", "/v1/messages", "tok-1", `{"model":"zai-org/GLM-5.1"}`, 404},
		{"alias name is not a model", "/v1/messages", "tok-1", `{"model":"opus"}`, 404},
		{"missing model", "/v1/messages", "tok-1", `{}`, 400},
		{"bad json", "/v1/messages", "tok-1", `[1]`, 400},
		{"bad path", "/v1/complete", "tok-1", `{"model":"oc-strong"}`, 404},
	}
	for _, c := range cases {
		resp := post(t, front.URL+c.path, c.token, c.body, nil)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Errorf("%s: status %d want %d (%s)", c.name, resp.StatusCode, c.status, b)
		}
		if !strings.Contains(string(b), `"type":"error"`) {
			t.Errorf("%s: not an Anthropic error body: %s", c.name, b)
		}
	}
	select {
	case s := <-ch:
		t.Fatalf("a rejected request reached upstream: %+v", s)
	default:
	}
	r.Unregister("tok-1")
	resp := post(t, front.URL+"/v1/messages", "tok-1", `{"model":"oc-strong"}`, nil)
	if resp.StatusCode != 401 {
		t.Errorf("after unregister: %d", resp.StatusCode)
	}
}

// The first SSE event must reach the client before the upstream finishes.
func TestStreamsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	ch := make(chan seen, 1)
	up := upstream(t, ch, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	r := New(nil)
	r.Register("t", "s", profile(up.URL, up.URL))
	front := httptest.NewServer(r)
	defer front.Close()

	resp := post(t, front.URL+"/v1/messages", "t", `{"model":"oc-strong","stream":true}`, nil)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	br := bufio.NewReader(resp.Body)
	got := make(chan string, 1)
	go func() { l, _ := br.ReadString('\n'); got <- l }()
	select {
	case l := <-got:
		if !strings.HasPrefix(l, "event: message_start") {
			t.Fatalf("first line %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first event was buffered")
	}
	close(release)
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "message_stop") {
		t.Fatalf("missing tail: %q", rest)
	}
}

func TestCountTokens501(t *testing.T) {
	ch := make(chan seen, 1)
	up := upstream(t, ch, func(w http.ResponseWriter) { w.WriteHeader(404) })
	r := New(nil)
	r.Register("t", "s", profile(up.URL, up.URL))
	front := httptest.NewServer(r)
	defer front.Close()
	resp := post(t, front.URL+"/v1/messages/count_tokens", "t", `{"model":"oc-strong"}`, nil)
	if resp.StatusCode != 501 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if s := <-ch; s.path != "/v1/messages/count_tokens" {
		t.Fatalf("path %s", s.path)
	}
}

func TestValidate(t *testing.T) {
	good := profile("http://a:1", "https://b")
	if err := Validate(good); err != nil {
		t.Fatal(err)
	}
	bad := []func(p *proto.Profile){
		func(p *proto.Profile) { p.Kind = proto.KindSubscription },
		func(p *proto.Profile) { p.Upstreams["oc/strong"] = p.Upstreams["oc-strong"] },
		func(p *proto.Profile) { p.Upstreams["x"] = proto.Upstream{BaseURL: "file:///etc", Model: "m"} },
		func(p *proto.Profile) { p.Upstreams["x"] = proto.Upstream{BaseURL: "http://u:p@h", Model: "m"} },
		func(p *proto.Profile) { p.Main = "nope" },
		func(p *proto.Profile) { p.Aliases["gpt"] = "oc-strong" },
		func(p *proto.Profile) { p.Aliases["opus"] = "missing" },
	}
	for i, f := range bad {
		p := profile("http://a:1", "https://b")
		f(&p)
		if Validate(p) == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if _, err := Listen("0.0.0.0:0", New(nil)); err == nil {
		t.Error("non-loopback listen accepted")
	}
	s, err := Listen("127.0.0.1:0", New(nil))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

// A restarted daemon reserves a running claude's token; requests wait for the hub's
// profile refresh instead of failing with 401.
func TestReservedTokenWaitsForRegister(t *testing.T) {
	ch := make(chan seen, 4)
	up := upstream(t, ch, func(w http.ResponseWriter) { fmt.Fprint(w, `{"ok":true}`) })
	r := New(nil)
	front := httptest.NewServer(r)
	defer front.Close()
	r.Reserve("tok", "s1")
	if r.Registered("tok") {
		t.Fatal("reserved token reported registered")
	}
	done := make(chan int, 1)
	go func() {
		resp := post(t, front.URL+"/v1/messages", "tok", `{"model":"oc-strong"}`, nil)
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	select {
	case code := <-done:
		t.Fatalf("answered %d before the profile arrived", code)
	case <-time.After(200 * time.Millisecond):
	}
	r.Register("tok", "s1", profile(up.URL, up.URL))
	if code := <-done; code != 200 {
		t.Fatalf("status %d", code)
	}
	if got := <-ch; got.headers.Get("Authorization") != "Bearer key-strong" {
		t.Fatalf("upstream auth %q", got.headers.Get("Authorization"))
	}
	if resp := post(t, front.URL+"/v1/messages", "other", `{"model":"oc-strong"}`, nil); resp.StatusCode != 401 {
		t.Fatalf("unknown token: %d", resp.StatusCode)
	}

	r.PendingWait = 100 * time.Millisecond
	r.Reserve("late", "s2")
	if resp := post(t, front.URL+"/v1/messages", "late", `{"model":"oc-strong"}`, nil); resp.StatusCode != 503 {
		t.Fatalf("no refresh: %d, want 503 (claude retries)", resp.StatusCode)
	}
}

func TestValidateModelMetadata(t *testing.T) {
	p := profile("http://a:1", "http://b:1")
	u := p.Upstreams["oc-strong"]
	u.ContextWindow, u.MaxOutput = 131072, 32000
	p.Upstreams["oc-strong"] = u
	if err := Validate(p); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][2]int{{10, 0}, {-1, 0}, {0, -5}, {0, 2_000_000}} {
		u.ContextWindow, u.MaxOutput = bad[0], bad[1]
		p.Upstreams["oc-strong"] = u
		if Validate(p) == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
