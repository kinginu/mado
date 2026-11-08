package hub

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Identity is what the IdP vouches for.
type Identity struct {
	Sub, Email, Name string
	EmailVerified    bool
	Groups           []string // the `groups` claim (Pocket ID: group names)
}

// Authenticator hides the IdP so the hub is testable without one.
type Authenticator interface {
	// AuthURL is where the browser goes; verifier is the PKCE code verifier.
	AuthURL(ctx context.Context, state, nonce, verifier string) (string, error)
	// Exchange redeems the code with the verifier and checks the ID token and nonce.
	Exchange(ctx context.Context, code, verifier, nonce string) (Identity, error)
}

// OIDCAuth is Authorization Code + PKCE (S256) against a discovered issuer.
type OIDCAuth struct {
	cfg OIDCConfig

	mu       sync.Mutex
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

func NewOIDCAuth(cfg OIDCConfig) *OIDCAuth { return &OIDCAuth{cfg: cfg} }

// init discovers lazily so the hub starts even while the IdP is down.
func (a *OIDCAuth) init(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.oauth != nil {
		return nil
	}
	if a.cfg.Issuer == "" || a.cfg.ClientID == "" {
		return errors.New("oidc issuer/client_id not configured")
	}
	p, err := oidc.NewProvider(ctx, a.cfg.Issuer)
	if err != nil {
		return fmt.Errorf("oidc discovery: %w", err)
	}
	secret := ""
	if a.cfg.ClientSecretFile != "" {
		b, err := os.ReadFile(a.cfg.ClientSecretFile)
		if err != nil {
			return fmt.Errorf("client secret file: %w", err)
		}
		secret = strings.TrimSpace(string(b))
	}
	a.oauth = &oauth2.Config{ClientID: a.cfg.ClientID, ClientSecret: secret, Endpoint: p.Endpoint(),
		RedirectURL: a.cfg.RedirectURL, Scopes: a.cfg.Scopes}
	a.verifier = p.Verifier(&oidc.Config{ClientID: a.cfg.ClientID})
	return nil
}

func (a *OIDCAuth) AuthURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	if err := a.init(ctx); err != nil {
		return "", err
	}
	return a.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), nil
}

func (a *OIDCAuth) Exchange(ctx context.Context, code, verifier, nonce string) (Identity, error) {
	if err := a.init(ctx); err != nil {
		return Identity{}, err
	}
	tok, err := a.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("token exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return Identity{}, errors.New("no id_token in token response")
	}
	idt, err := a.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("id_token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		return Identity{}, errors.New("id_token nonce mismatch")
	}
	var c struct {
		Email         string          `json:"email"`
		EmailVerified *bool           `json:"email_verified"`
		Name          string          `json:"name"`
		Preferred     string          `json:"preferred_username"`
		Groups        json.RawMessage `json:"groups"`
	}
	if err := idt.Claims(&c); err != nil {
		return Identity{}, err
	}
	name := c.Name
	if name == "" {
		name = c.Preferred
	}
	return Identity{Sub: idt.Subject, Email: c.Email, Name: name, EmailVerified: c.EmailVerified != nil && *c.EmailVerified,
		Groups: parseGroups(c.Groups)}, nil
}

// parseGroups accepts a JSON array of strings or a single string; anything else
// means no groups.
func parseGroups(raw json.RawMessage) []string {
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	return nil
}

// IsAdmin: membership in the configured admin group, recomputed at every login.
func (c OIDCConfig) IsAdmin(id Identity) bool {
	return c.AdminGroup != "" && slices.Contains(id.Groups, c.AdminGroup)
}

// Allowed applies the optional hub.toml allowlist. Who may log in at all is decided
// by the IdP (Pocket ID: the client's allowed user groups); an empty allowlist lets
// every such user in, a non-empty one narrows it. Email matches need email_verified.
func (c AllowConfig) Allowed(id Identity) bool {
	if len(c.Subs) == 0 && len(c.Emails) == 0 {
		return id.Sub != ""
	}
	if id.Sub != "" && slices.Contains(c.Subs, id.Sub) {
		return true
	}
	if id.Email != "" && id.EmailVerified {
		for _, e := range c.Emails {
			if strings.EqualFold(e, id.Email) {
				return true
			}
		}
	}
	return false
}

// LocalLoginAllowed is the gate of the local login (DESIGN §19.3): the hub must listen
// on a loopback address only, so nobody else on the network reaches it.
func LocalLoginAllowed(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen address %q: %w", listen, err)
	}
	if !loopbackHost(host) {
		return fmt.Errorf("local login refused: listen address %q is not loopback", listen)
	}
	return nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// localRequest: the request comes from this computer and was not proxied in (a request
// forwarded by tailscale serve or another proxy carries one of these headers).
func localRequest(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Tailscale-User-Login") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// localIdentity is the fixed user of the local login, an admin.
func (h *Hub) localIdentity() Identity {
	name := os.Getenv("USER")
	if name == "" {
		name = "Local user"
	}
	return Identity{Sub: "local", Email: "local@localhost", Name: name, EmailVerified: true,
		Groups: []string{h.cfg.OIDC.AdminGroup}}
}

const (
	cookieSession = "mado_session"
	cookieLogin   = "mado_login"
	sessionTTL    = 30 * 24 * time.Hour
)

func (h *Hub) cookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: h.secureCookies,
		SameSite: http.SameSiteLaxMode, MaxAge: maxAge}
}

func (h *Hub) handleLogin(w http.ResponseWriter, r *http.Request) {
	// With the local login on, the page offers it; the IdP is one click further and
	// only when an issuer is configured (DESIGN §19.3).
	if h.localLogin && (r.URL.Query().Get("idp") == "" || h.cfg.OIDC.Issuer == "") {
		h.render(w, r, "login.html", map[string]any{"Local": true, "IdP": h.cfg.OIDC.Issuer != "", "UI": uiVersion})
		return
	}
	state, nonce, verifier := RandomToken(24), RandomToken(24), oauth2.GenerateVerifier()
	u, err := h.auth.AuthURL(r.Context(), state, nonce, verifier)
	if err != nil {
		h.log.Error("login", "err", err)
		http.Error(w, "identity provider unavailable", http.StatusBadGateway)
		return
	}
	if err := h.store.PutLoginFlow(state, nonce, verifier); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, h.cookie(cookieLogin, state, 900))
	http.Redirect(w, r, u, http.StatusFound)
}

func (h *Hub) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(w, "login failed: "+e, http.StatusUnauthorized)
		return
	}
	state, code := q.Get("state"), q.Get("code")
	c, err := r.Cookie(cookieLogin)
	if err != nil || state == "" || code == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		http.Error(w, "bad login state", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, h.cookie(cookieLogin, "", -1))
	nonce, verifier, err := h.store.TakeLoginFlow(state)
	if err != nil {
		http.Error(w, "bad login state", http.StatusBadRequest)
		return
	}
	id, err := h.auth.Exchange(r.Context(), code, verifier, nonce)
	if err != nil {
		h.log.Warn("oidc exchange failed", "err", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	if !h.cfg.Allow.Allowed(id) {
		h.store.Audit(id.Sub, "login_denied", id.Email)
		http.Error(w, "this account is not allowed", http.StatusForbidden)
		return
	}
	h.finishLogin(w, r, id, "")
}

// finishLogin stores the user and starts a web session. method is "" for the IdP and
// "local" for the local login; the audit row says "login local" for the latter.
func (h *Hub) finishLogin(w http.ResponseWriter, r *http.Request, id Identity, method string) {
	admin := h.cfg.OIDC.IsAdmin(id)
	if err := h.store.UpsertUser(User{Sub: id.Sub, Email: id.Email, Name: id.Name, IsAdmin: admin}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if h.inbox != nil {
		h.inbox.store.ResolveOwners() // accounts configured by this user's e-mail (DESIGN §23.9)
	}
	sid, err := h.store.CreateWebSession(id.Sub, sessionTTL)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	detail := id.Email
	if admin {
		detail += " admin"
	}
	if method != "" {
		detail = method
	}
	h.store.Audit(id.Sub, "login", detail)
	http.SetCookie(w, h.cookie(cookieSession, sid, int(sessionTTL.Seconds())))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Hub) handleLocalLogin(w http.ResponseWriter, r *http.Request) {
	if !h.localLogin || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if !localRequest(r) {
		h.log.Warn("local login refused: not a direct loopback request", "remote", r.RemoteAddr)
		http.Error(w, "local login is only available on this computer", http.StatusForbidden)
		return
	}
	h.finishLogin(w, r, h.localIdentity(), "local")
}

func (h *Hub) handleLogout(w http.ResponseWriter, r *http.Request) {
	ws := h.webSession(r)
	if ws == nil || r.Method != http.MethodPost || !h.checkCSRF(r, ws) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(cookieSession); err == nil {
		h.store.DeleteWebSession(c.Value)
	}
	h.store.Audit(ws.User.Sub, "logout", "")
	http.SetCookie(w, h.cookie(cookieSession, "", -1))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Hub) webSession(r *http.Request) *WebSession {
	c, err := r.Cookie(cookieSession)
	if err != nil || c.Value == "" {
		return nil
	}
	ws, err := h.store.WebSession(c.Value)
	if err != nil {
		return nil
	}
	return ws
}

// requireUser wraps handlers that need a logged-in user.
func (h *Hub) requireUser(next func(http.ResponseWriter, *http.Request, *WebSession)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws := h.webSession(r)
		if ws == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && !h.checkCSRF(r, ws) {
			http.Error(w, "bad csrf token", http.StatusForbidden)
			return
		}
		next(w, r, ws)
	}
}

// checkCSRF needs the per-session token and, when the browser sends one, a same-origin Origin.
func (h *Hub) checkCSRF(r *http.Request, ws *WebSession) bool {
	if o := r.Header.Get("Origin"); o != "" {
		ou, err := url.Parse(o)
		bu, _ := url.Parse(h.cfg.BaseURL)
		if err != nil || !strings.EqualFold(ou.Host, bu.Host) || ou.Scheme != bu.Scheme {
			return false
		}
	}
	if err := r.ParseForm(); err != nil {
		return false
	}
	tok := r.PostForm.Get("csrf")
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(ws.CSRF)) == 1
}
