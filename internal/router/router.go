// Package router is a pass-through proxy for the Anthropic Messages API.
//
// Each session registers a random bearer token and its profile. A request must carry
// exactly that bearer. Its "model" must be one of the profile's upstream names (the
// names Claude Code was told to use through ANTHROPIC_DEFAULT_*_MODEL and --model); the
// router swaps in the upstream's model, replaces the credentials and streams the
// response back unchanged.
package router

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kinginu/mado/internal/messages"
	"github.com/kinginu/mado/internal/proto"
)

const maxBody = 64 << 20

// Router implements http.Handler.
type Router struct {
	mu       sync.RWMutex
	sessions map[string]*route // token -> route
	client   *http.Client
	log      *slog.Logger
	// PendingWait is how long a request on a reserved token waits for its profile.
	PendingWait time.Duration
}

// route: upstreams is written once before ready is closed and never changed after.
type route struct {
	sessionID string
	ready     chan struct{}
	upstreams map[string]proto.Upstream
}

func New(log *slog.Logger) *Router {
	if log == nil {
		log = slog.Default()
	}
	tr := &http.Transport{
		Proxy:                 nil, // never route model traffic through an env proxy
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 50 * time.Minute, // long prefill on big local models
	}
	return &Router{sessions: map[string]*route{}, client: &http.Client{Transport: tr}, log: log, PendingWait: 60 * time.Second}
}

// Validate checks a router profile before it is registered.
func Validate(p proto.Profile) error {
	if p.Kind != proto.KindRouter {
		return fmt.Errorf("profile %q is not a router profile", p.Name)
	}
	if len(p.Upstreams) == 0 {
		return errors.New("profile has no upstreams")
	}
	for name, u := range p.Upstreams {
		if !ValidModelName(name) {
			return fmt.Errorf("upstream name %q is not allowed (use [A-Za-z0-9._-], no '/')", name)
		}
		pu, err := url.Parse(u.BaseURL)
		if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" || pu.User != nil {
			return fmt.Errorf("upstream %q: base_url must be http(s)://host[:port][/path]", name)
		}
		if u.Model == "" {
			return fmt.Errorf("upstream %q: empty model", name)
		}
		if !proto.ValidUpstreamKind(u.Kind) {
			return fmt.Errorf("upstream %q: kind must be anthropic or openai", name)
		}
		if u.AuthHeader != "" && u.AuthHeader != "bearer" && u.AuthHeader != "x-api-key" {
			return fmt.Errorf("upstream %q: auth_header must be bearer or x-api-key", name)
		}
		if u.Kind == proto.UpstreamOpenAI && u.AuthHeader == "x-api-key" {
			return fmt.Errorf("upstream %q: an openai upstream sends its key as Authorization: Bearer", name)
		}
		if _, _, err := proto.ParseTimeouts(u.TimeoutFirstByte, u.TimeoutTotal); err != nil {
			return fmt.Errorf("upstream %q: %v", name, err)
		}
		if u.ContextWindow != 0 && (u.ContextWindow < 1000 || u.ContextWindow > 10_000_000) {
			return fmt.Errorf("upstream %q: context_window must be 1000..10000000", name)
		}
		if u.MaxOutput != 0 && (u.MaxOutput < 1 || u.MaxOutput > 1_000_000) {
			return fmt.Errorf("upstream %q: max_output must be 1..1000000", name)
		}
	}
	if p.Main != "" {
		if _, ok := p.Upstreams[p.Main]; !ok {
			return fmt.Errorf("main %q is not an upstream", p.Main)
		}
	}
	for alias, target := range p.Aliases {
		switch alias {
		case "fable", "opus", "sonnet", "haiku":
		default:
			return fmt.Errorf("unknown alias %q", alias)
		}
		if _, ok := p.Upstreams[target]; !ok {
			return fmt.Errorf("alias %s -> %q is not an upstream", alias, target)
		}
	}
	return nil
}

// ValidModelName reports whether a router-side model name is safe for Claude Code
// (no '/', DESIGN §3.2) and for env vars.
func ValidModelName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// Register adds a session token, or fills in a reserved one. The profile must
// already be validated.
func (r *Router) Register(token, sessionID string, p proto.Profile) {
	ups := make(map[string]proto.Upstream, len(p.Upstreams))
	for k, v := range p.Upstreams {
		ups[k] = v
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.sessions[token]; old != nil && old.sessionID == sessionID && old.upstreams == nil {
		old.upstreams = ups
		close(old.ready)
		return
	}
	ready := make(chan struct{})
	close(ready)
	r.sessions[token] = &route{sessionID: sessionID, ready: ready, upstreams: ups}
}

// Reserve accepts a token whose profile is not known yet (a daemon restarted under a
// running claude). Requests on it wait up to PendingWait for Register.
func (r *Router) Reserve(token, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sessions[token]; ok {
		return
	}
	r.sessions[token] = &route{sessionID: sessionID, ready: make(chan struct{})}
}

// Registered reports whether a token has a profile (for tests and logs).
func (r *Router) Registered(token string) bool {
	r.mu.RLock()
	rt := r.sessions[token]
	r.mu.RUnlock()
	if rt == nil {
		return false
	}
	select {
	case <-rt.ready:
		return true
	default:
		return false
	}
}

// Unregister removes a session token.
func (r *Router) Unregister(token string) {
	r.mu.Lock()
	delete(r.sessions, token)
	r.mu.Unlock()
}

func (r *Router) lookup(req *http.Request) *route {
	auth := req.Header.Get("Authorization")
	const p = "Bearer "
	if !strings.HasPrefix(auth, p) {
		return nil
	}
	tok := auth[len(p):]
	r.mu.RLock()
	defer r.mu.RUnlock()
	for t, rt := range r.sessions {
		if subtle.ConstantTimeCompare([]byte(t), []byte(tok)) == 1 {
			return rt
		}
	}
	return nil
}

// apiError writes an Anthropic-shaped error body.
func apiError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": typ, "message": msg},
	})
}

// Hop-by-hop and credential headers never forwarded upstream.
var dropRequestHeaders = map[string]bool{
	"Authorization": true, "X-Api-Key": true, "Cookie": true, "Proxy-Authorization": true,
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true, "Host": true, "Content-Length": true,
	"Accept-Encoding": true, // let the transport negotiate; keeps SSE un-gzipped end to end
}

var dropResponseHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true, "Content-Length": true, "Set-Cookie": true,
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	if path != "/v1/messages" && path != "/v1/messages/count_tokens" {
		apiError(w, http.StatusNotFound, "not_found_error", "unknown path")
		return
	}
	if req.Method != http.MethodPost {
		apiError(w, http.StatusMethodNotAllowed, "invalid_request_error", "POST only")
		return
	}
	rt := r.lookup(req)
	if rt == nil {
		apiError(w, http.StatusUnauthorized, "authentication_error", "invalid bearer token")
		return
	}
	select {
	case <-rt.ready:
	default:
		t := time.NewTimer(r.PendingWait)
		select {
		case <-rt.ready:
			t.Stop()
		case <-t.C:
			apiError(w, http.StatusServiceUnavailable, "api_error", "session profile not yet restored from the hub")
			return
		case <-req.Context().Done():
			t.Stop()
			return
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBody))
	if err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body too large or unreadable")
		return
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "body is not a JSON object")
		return
	}
	var model string
	if err := json.Unmarshal(m["model"], &model); err != nil || model == "" {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "missing model")
		return
	}
	up, ok := rt.upstreams[model]
	if !ok {
		r.log.Warn("router: unknown model", "session", rt.sessionID, "model", model)
		apiError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("model %q is not in this session's profile", model))
		return
	}
	if up.Kind == proto.UpstreamOpenAI {
		r.serveOpenAI(w, req, rt, model, up, body)
		return
	}
	m["model"], _ = json.Marshal(up.Model)
	out, err := json.Marshal(m)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "api_error", "re-encode failed")
		return
	}
	target := strings.TrimRight(up.BaseURL, "/") + path
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	// Explicit timeouts only: an anthropic upstream without them behaves as before.
	fb, total, _ := proto.ParseTimeouts(up.TimeoutFirstByte, up.TimeoutTotal)
	ctx, cancel := withTotal(req.Context(), total)
	defer cancel()
	timer := startFirstByte(fb, cancel)
	ureq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(out))
	if err != nil {
		apiError(w, http.StatusBadGateway, "api_error", "bad upstream url")
		return
	}
	for k, vs := range req.Header {
		if dropRequestHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			ureq.Header.Add(k, v)
		}
	}
	ureq.Header.Set("Content-Type", "application/json")
	messages.SetAuth(ureq.Header, up.BaseURL, up.AuthHeader, up.Key)
	resp, err := r.client.Do(ureq)
	if err != nil {
		timer.stop()
		if timer.fired() && req.Context().Err() == nil {
			apiError(w, http.StatusGatewayTimeout, "api_error", fmt.Sprintf("upstream: no response within %s", fb))
			return
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		r.log.Warn("router: upstream error", "session", rt.sessionID, "model", model, "err", err)
		apiError(w, http.StatusBadGateway, "api_error", "upstream unreachable")
		return
	}
	defer resp.Body.Close()
	resp.Body = timer.body(resp.Body)
	if path == "/v1/messages/count_tokens" && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed) {
		apiError(w, http.StatusNotImplemented, "not_found_error", "upstream has no count_tokens")
		return
	}
	for k, vs := range resp.Header {
		if dropResponseHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	streamCopy(w, resp.Body)
}

// streamCopy copies and flushes after every read so SSE events pass through as they arrive.
func streamCopy(w http.ResponseWriter, body io.Reader) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// Server wraps the router in an http.Server bound to a loopback address.
type Server struct {
	*Router
	Addr string
	srv  *http.Server
	ln   net.Listener
}

// Listen binds addr, which must be a loopback address (DESIGN §9).
func Listen(addr string, r *Router) (*Server, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("router must listen on a loopback IP, got %q", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{Router: r, Addr: ln.Addr().String(), ln: ln,
		srv: &http.Server{Handler: r, ReadHeaderTimeout: 10 * time.Second}}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

func (s *Server) Close() error { return s.srv.Close() }

// URL is the base URL to give to ANTHROPIC_BASE_URL.
func (s *Server) URL() string { return "http://" + s.Addr }
