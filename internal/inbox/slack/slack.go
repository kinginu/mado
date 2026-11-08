// Package slack is the inbox kind "slack" (DESIGN §23.4): read-only collection of a
// workspace's channels, DMs and thread replies through the Slack Web API with a user
// token (xoxp-). Only read methods are called: auth.test, conversations.history,
// conversations.replies, conversations.info, conversations.list, users.info.
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kinginu/mado/internal/inbox"
	"github.com/kinginu/mado/internal/tools"
)

func init() { inbox.Register(Kind{}) }

// apiBase is the Web API root; tests point it at a fixture server.
var apiBase = "https://slack.com/api/"

// sleep waits for a Retry-After; tests replace it.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

const (
	historyLimit    = 200
	maxHistoryPages = 25               // per channel per round (5000 messages)
	maxReplyPages   = 10               // per thread
	maxCalls        = 40               // API calls per Poll; the rest is "more"
	maxRetryAfter   = 30 * time.Second // longer waits end the Poll
	threadWindow    = 7 * 24 * time.Hour
	authTTL         = 24 * time.Hour
	nameTTL         = 24 * time.Hour
	dmListTTL       = time.Hour
	maxBody         = 16 << 20
	maxInitialHours = 24 * 30
)

// Kind is the slack kind.
type Kind struct{}

// Config is the account config.
type Config struct {
	Channels     []string `json:"channels"`
	DMs          bool     `json:"dms"`
	Replies      *bool    `json:"replies"`       // default true
	InitialHours int      `json:"initial_hours"` // default 24
}

func (c Config) replies() bool { return c.Replies == nil || *c.Replies }

func (c Config) initial() time.Duration {
	if c.InitialHours <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(c.InitialHours) * time.Hour
}

var channelRe = regexp.MustCompile(`^[CGD][A-Z0-9]{8,}$`)

func (Kind) Name() string { return "slack" }

func (Kind) Template() json.RawMessage {
	return json.RawMessage(`{"channels": ["C0123456789"], "dms": true, "replies": true, "initial_hours": 24}`)
}

func (Kind) Validate(config json.RawMessage, keyRef string, keys map[string]string) error {
	var c Config
	if err := inbox.DecodeStrict(config, &c); err != nil {
		return err
	}
	if err := c.check(); err != nil {
		return err
	}
	if keyRef == "" {
		return errors.New("a slack account needs a key_ref: the secrets-file key whose value is a user token (xoxp-…)")
	}
	if keys != nil {
		v, ok := keys[keyRef]
		if !ok {
			return fmt.Errorf("key_ref %q is not in the secrets file", keyRef)
		}
		if err := checkToken(v); err != nil {
			return fmt.Errorf("key_ref %q: %w", keyRef, err)
		}
	}
	return nil
}

func (c Config) check() error {
	seen := map[string]bool{}
	for _, ch := range c.Channels {
		if !channelRe.MatchString(ch) {
			return fmt.Errorf("config: channel %q is not a Slack conversation id (C…, G… or D…; channel details → bottom)", ch)
		}
		if seen[ch] {
			return fmt.Errorf("config: channel %s listed twice", ch)
		}
		seen[ch] = true
	}
	if len(c.Channels) == 0 && !c.DMs {
		return errors.New("config: name at least one channel or set dms")
	}
	if c.InitialHours < 0 || c.InitialHours > maxInitialHours {
		return fmt.Errorf("config: initial_hours must be between 1 and %d", maxInitialHours)
	}
	return nil
}

func checkToken(v string) error {
	switch {
	case strings.HasPrefix(v, "xoxp-"):
		return nil
	case strings.HasPrefix(v, "xoxb-"):
		return errors.New("this is a bot token (xoxb-); the slack kind needs the app's user token (xoxp-)")
	default:
		return errors.New("not a Slack user token (xoxp-…)")
	}
}

func (Kind) New(a inbox.Account, key string, env inbox.Env) (inbox.Collector, error) {
	var c Config
	if err := inbox.DecodeStrict(a.Config, &c); err != nil {
		return nil, err
	}
	if err := c.check(); err != nil {
		return nil, err
	}
	if err := checkToken(key); err != nil {
		return nil, err
	}
	return &collector{cfg: c, token: key, env: env}, nil
}

type collector struct {
	cfg   Config
	token string
	env   inbox.Env
}

// cached is a resolved name with the Unix time it was fetched.
type cached struct {
	N  string `json:"n"`
	At int64  `json:"at"`
}

// dm is an open IM or MPIM from conversations.list.
type dm struct {
	User string `json:"user,omitempty"` // im: the other party
	Name string `json:"name,omitempty"` // mpim: mpdm-a--b--c-1
	MPIM bool   `json:"mpim,omitempty"`
}

// Cursor is the account cursor.
type Cursor struct {
	Team   string `json:"team,omitempty"`
	URL    string `json:"url,omitempty"` // https://<workspace>.slack.com
	UserID string `json:"user_id,omitempty"`
	AuthAt int64  `json:"auth_at,omitempty"`
	// Channels: the newest top-level message ts stored per conversation.
	Channels map[string]string `json:"channels,omitempty"`
	// Threads: "<channel>:<parent ts>" → the newest reply ts stored, for parents
	// younger than threadWindow.
	Threads map[string]string `json:"threads,omitempty"`
	Users   map[string]cached `json:"users,omitempty"`
	Names   map[string]cached `json:"names,omitempty"` // conversation id → title
	DMs     map[string]dm     `json:"dms,omitempty"`
	DMsAt   int64             `json:"dms_at,omitempty"`
	// Pending: conversations left in the current round (a Poll ran out of calls or
	// was rate limited).
	Pending []string `json:"pending,omitempty"`
	// Errors: per-conversation errors of the current round, reported when it ends.
	Errors []string `json:"errors,omitempty"`
}

var (
	errBudget  = errors.New("call budget spent")
	errLimited = errors.New("rate limited")
)

// apiError is an "ok": false answer.
type apiError struct{ method, code string }

func (e *apiError) Error() string { return "slack " + e.method + ": " + e.code }

// fatal: the token itself is unusable; nothing else will work either.
func (e *apiError) fatal() bool {
	switch e.code {
	case "invalid_auth", "not_authed", "account_inactive", "token_revoked", "token_expired", "no_permission", "org_login_required", "ekm_access_denied", "two_factor_setup_required":
		return true
	}
	return false
}

type poll struct {
	c          *collector
	ctx        context.Context
	cur        Cursor
	now        time.Time
	client     *http.Client
	calls      int
	slept      bool
	retryAfter time.Duration
	items      []inbox.Item
}

func (c *collector) Poll(ctx context.Context, raw json.RawMessage) ([]inbox.Item, json.RawMessage, bool, error) {
	p := &poll{c: c, ctx: ctx, client: c.env.HTTP}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &p.cur)
	}
	if c.env.Now != nil {
		p.now = c.env.Now()
	} else {
		p.now = time.Now()
	}
	if p.client == nil {
		p.client = (tools.Guard{}).Client(time.Minute)
	}
	p.init()
	err := p.run()
	return p.finish(err)
}

func (p *poll) init() {
	if p.cur.Channels == nil {
		p.cur.Channels = map[string]string{}
	}
	if p.cur.Threads == nil {
		p.cur.Threads = map[string]string{}
	}
	if p.cur.Users == nil {
		p.cur.Users = map[string]cached{}
	}
	if p.cur.Names == nil {
		p.cur.Names = map[string]cached{}
	}
	// Expire caches and forget threads that left the window.
	for k, v := range p.cur.Users {
		if p.stale(v.At, nameTTL) {
			delete(p.cur.Users, k)
		}
	}
	for k, v := range p.cur.Names {
		if p.stale(v.At, nameTTL) {
			delete(p.cur.Names, k)
		}
	}
	edge := tsFromTime(p.now.Add(-threadWindow))
	for k := range p.cur.Threads {
		if i := strings.LastIndexByte(k, ':'); i < 0 || tsLess(k[i+1:], edge) {
			delete(p.cur.Threads, k)
		}
	}
	if !p.c.cfg.DMs {
		p.cur.DMs, p.cur.DMsAt = nil, 0
	}
}

func (p *poll) stale(at int64, ttl time.Duration) bool {
	return at == 0 || p.now.Sub(time.Unix(at, 0)) >= ttl || p.now.Before(time.Unix(at, 0))
}

func (p *poll) run() error {
	if p.cur.URL == "" || p.stale(p.cur.AuthAt, authTTL) {
		var r struct {
			URL    string `json:"url"`
			Team   string `json:"team"`
			UserID string `json:"user_id"`
		}
		if err := p.call("auth.test", nil, &r); err != nil {
			return err
		}
		p.cur.URL = strings.TrimRight(r.URL, "/")
		p.cur.Team, p.cur.UserID, p.cur.AuthAt = r.Team, r.UserID, p.now.Unix()
	}
	if p.c.cfg.DMs && (p.cur.DMs == nil || p.stale(p.cur.DMsAt, dmListTTL)) {
		if err := p.listDMs(); err != nil {
			if !p.channelError("dms", err) {
				return err
			}
		}
	}
	if len(p.cur.Pending) == 0 {
		p.cur.Pending = p.conversations()
	}
	for len(p.cur.Pending) > 0 {
		ch := p.cur.Pending[0]
		if err := p.channel(ch); err != nil && !p.channelError(ch, err) {
			return err
		}
		p.cur.Pending = p.cur.Pending[1:]
	}
	return nil
}

// channelError records a per-conversation API error and says whether the round may
// go on.
func (p *poll) channelError(ch string, err error) bool {
	var ae *apiError
	if errors.As(err, &ae) && !ae.fatal() {
		p.cur.Errors = append(p.cur.Errors, ch+": "+err.Error())
		if p.c.env.Log != nil {
			p.c.env.Log.Warn("inbox slack: conversation skipped", "channel", ch, "err", err)
		}
		return true
	}
	return false
}

// conversations: the configured channels, then the open DMs (sorted), each once.
func (p *poll) conversations() []string {
	seen := map[string]bool{}
	var out []string
	for _, ch := range p.c.cfg.Channels {
		if !seen[ch] {
			seen[ch] = true
			out = append(out, ch)
		}
	}
	var dms []string
	for id := range p.cur.DMs {
		if !seen[id] {
			seen[id] = true
			dms = append(dms, id)
		}
	}
	sort.Strings(dms)
	return append(out, dms...)
}

func (p *poll) finish(err error) ([]inbox.Item, json.RawMessage, bool, error) {
	more := false
	switch {
	case err == nil:
	case errors.Is(err, errBudget):
		more, err = true, nil
	case errors.Is(err, errLimited):
		// A short wait was already spent in this Poll: let the scheduler call again
		// (it may wait once more). A long one: the next interval resumes the round.
		more = p.retryAfter <= maxRetryAfter
		if p.c.env.Log != nil {
			p.c.env.Log.Info("inbox slack: rate limited", "retry_after", p.retryAfter, "pending", len(p.cur.Pending))
		}
		err = nil
	}
	if err == nil && !more && len(p.cur.Errors) > 0 {
		err = errors.New(strings.Join(p.cur.Errors, "; "))
		p.cur.Errors = nil
	}
	sort.SliceStable(p.items, func(i, j int) bool { return p.items[i].TS.Before(p.items[j].TS) })
	if len(p.cur.Pending) == 0 {
		p.cur.Pending = nil
	}
	b, _ := json.Marshal(p.cur)
	return p.items, b, more, err
}

// call runs one read method. A 429 is waited out once per Poll when Retry-After is at
// most 30 s; otherwise errLimited.
func (p *poll) call(method string, params url.Values, out any) error {
	if p.calls >= maxCalls {
		return errBudget
	}
	p.calls++
	u := apiBase + method
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	for {
		req, err := http.NewRequestWithContext(p.ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+p.c.token)
		req.Header.Set("User-Agent", "mado-inbox/1 (+https://github.com/kinginu/mado)")
		resp, err := p.client.Do(req)
		if err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			ra := retryAfter(resp.Header.Get("Retry-After"))
			if !p.slept && ra <= maxRetryAfter {
				p.slept = true
				if err := sleep(p.ctx, ra); err != nil {
					return err
				}
				continue
			}
			p.retryAfter = ra
			return errLimited
		}
		if err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("slack %s: HTTP %d", method, resp.StatusCode)
		}
		if len(body) > maxBody {
			return fmt.Errorf("slack %s: response larger than 16 MB", method)
		}
		var env struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		if !env.OK {
			if env.Error == "" {
				env.Error = "unknown_error"
			}
			return &apiError{method, env.Error}
		}
		if out != nil {
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("slack %s: %w", method, err)
			}
		}
		return nil
	}
}

func retryAfter(h string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || n < 0 {
		return time.Second
	}
	return time.Duration(n) * time.Second
}

type page struct {
	HasMore          bool `json:"has_more"`
	ResponseMetadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

func (p page) next() string {
	if !p.HasMore && p.ResponseMetadata.NextCursor == "" {
		return ""
	}
	return p.ResponseMetadata.NextCursor
}

func (p *poll) listDMs() error {
	dms := map[string]dm{}
	cursor := ""
	for i := 0; i < 20; i++ {
		var r struct {
			page
			Channels []struct {
				ID            string `json:"id"`
				IsIM          bool   `json:"is_im"`
				IsMPIM        bool   `json:"is_mpim"`
				User          string `json:"user"`
				Name          string `json:"name"`
				IsUserDeleted bool   `json:"is_user_deleted"`
			} `json:"channels"`
		}
		q := url.Values{"types": {"im,mpim"}, "exclude_archived": {"true"}, "limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		if err := p.call("conversations.list", q, &r); err != nil {
			return err
		}
		for _, c := range r.Channels {
			switch {
			case c.IsIM && !c.IsUserDeleted:
				dms[c.ID] = dm{User: c.User}
			case c.IsMPIM:
				dms[c.ID] = dm{Name: c.Name, MPIM: true}
			}
		}
		if cursor = r.next(); cursor == "" {
			break
		}
	}
	p.cur.DMs, p.cur.DMsAt = dms, p.now.Unix()
	return nil
}

// message is the part of a Slack message object the collector reads.
type message struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Username    string `json:"username"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
	ReplyCount  int    `json:"reply_count"`
	LatestReply string `json:"latest_reply"`
	BotProfile  *struct {
		Name string `json:"name"`
	} `json:"bot_profile"`
	Files []struct {
		Name  string `json:"name"`
		Title string `json:"title"`
	} `json:"files"`
}

type thread struct{ parent, floor string }

// channel collects one conversation: history newer than its cursor, and, with
// replies, new replies of the threads whose parents are in the scanned pages.
func (p *poll) channel(ch string) error {
	title, err := p.title(ch)
	if err != nil {
		return err
	}
	old, hasOld := p.cur.Channels[ch]
	floor := old
	if !hasOld {
		floor = tsFromTime(p.now.Add(-p.c.cfg.initial()))
	}
	// With a cursor, the scan reaches back into the thread window so that parents
	// already stored still show their latest_reply; only the page that crosses the
	// cursor is read, so quiet channels see the whole window and busy ones the newest
	// 200 messages.
	scan := floor
	if p.c.cfg.replies() && hasOld {
		if w := tsFromTime(p.now.Add(-threadWindow)); tsLess(w, scan) {
			scan = w
		}
	}
	newest := floor
	var threads []thread
	cursor := ""
	for pg := 0; ; pg++ {
		var r struct {
			page
			Messages []json.RawMessage `json:"messages"`
		}
		q := url.Values{"channel": {ch}, "oldest": {scan}, "limit": {strconv.Itoa(historyLimit)}, "inclusive": {"false"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		if err := p.call("conversations.history", q, &r); err != nil {
			return err
		}
		crossed := false
		for _, raw := range r.Messages {
			var m message
			if json.Unmarshal(raw, &m) != nil || m.TS == "" {
				continue
			}
			if !tsLess(floor, m.TS) {
				crossed = true
			} else {
				if tsLess(newest, m.TS) {
					newest = m.TS
				}
				if err := p.add(ch, title, m, raw); err != nil {
					return err
				}
			}
			if p.c.cfg.replies() && m.ReplyCount > 0 && (m.ThreadTS == "" || m.ThreadTS == m.TS) {
				f := p.cur.Threads[ch+":"+m.TS]
				if f == "" {
					f = m.TS
				}
				if m.LatestReply == "" || tsLess(f, m.LatestReply) {
					threads = append(threads, thread{m.TS, f})
				}
			}
		}
		if cursor = r.next(); cursor == "" || crossed {
			break
		}
		if pg+1 >= maxHistoryPages {
			if p.c.env.Log != nil {
				p.c.env.Log.Warn("inbox slack: history page limit reached, older messages skipped", "channel", ch)
			}
			break
		}
	}
	// Oldest threads first so a budget stop keeps the earlier ones.
	sort.Slice(threads, func(i, j int) bool { return tsLess(threads[i].parent, threads[j].parent) })
	for _, t := range threads {
		if err := p.replies(ch, title, t); err != nil {
			return err
		}
	}
	p.cur.Channels[ch] = newest
	return nil
}

func (p *poll) replies(ch, title string, t thread) error {
	key := ch + ":" + t.parent
	seen := p.cur.Threads[key]
	cursor := ""
	for pg := 0; pg < maxReplyPages; pg++ {
		var r struct {
			page
			Messages []json.RawMessage `json:"messages"`
		}
		q := url.Values{"channel": {ch}, "ts": {t.parent}, "oldest": {t.floor}, "limit": {strconv.Itoa(historyLimit)}, "inclusive": {"false"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		if err := p.call("conversations.replies", q, &r); err != nil {
			return err
		}
		for _, raw := range r.Messages {
			var m message
			if json.Unmarshal(raw, &m) != nil || m.TS == "" || m.TS == t.parent || !tsLess(t.floor, m.TS) {
				continue
			}
			if m.ThreadTS == "" {
				m.ThreadTS = t.parent
			}
			if err := p.add(ch, title, m, raw); err != nil {
				return err
			}
			if seen == "" || tsLess(seen, m.TS) {
				seen = m.TS
			}
		}
		if cursor = r.next(); cursor == "" {
			break
		}
	}
	if seen != "" {
		p.cur.Threads[key] = seen
	}
	return nil
}

// skip: joins, leaves, edits, deletions and empty bot messages.
func skip(m message) bool {
	switch m.Subtype {
	case "channel_join", "channel_leave", "group_join", "group_leave", "message_changed", "message_deleted":
		return true
	case "bot_message":
		return strings.TrimSpace(m.Text) == ""
	}
	return m.Type != "" && m.Type != "message"
}

func (p *poll) add(ch, title string, m message, raw json.RawMessage) error {
	if skip(m) {
		return nil
	}
	author, err := p.author(m)
	if err != nil {
		return err
	}
	kind := "message"
	switch {
	case p.isDM(ch):
		kind = "dm"
	case m.ThreadTS != "" && m.ThreadTS != m.TS:
		kind = "reply"
	}
	root := m.TS
	if m.ThreadTS != "" {
		root = m.ThreadTS
	}
	// Mentioned users are resolved like authors (users.info, cached).
	for _, sm := range mentionRe.FindAllStringSubmatch(m.Text, 20) {
		if _, err := p.userName(sm[1]); err != nil {
			return err
		}
	}
	text := p.rewrite(m.Text)
	if strings.TrimSpace(text) == "" && len(m.Files) > 0 {
		names := make([]string, 0, len(m.Files))
		for _, f := range m.Files {
			names = append(names, f.Name)
		}
		text = "(files: " + strings.Join(names, ", ") + ")"
	}
	p.items = append(p.items, inbox.Item{
		ExtID:     ch + ":" + m.TS,
		Kind:      kind,
		Author:    author,
		Title:     title,
		Text:      text,
		URL:       p.permalink(ch, m),
		ThreadKey: ch + ":" + root,
		TS:        tsTime(m.TS),
		Raw:       slimRaw(ch, raw),
	})
	return nil
}

func (p *poll) isDM(ch string) bool {
	if strings.HasPrefix(ch, "D") {
		return true
	}
	d, ok := p.cur.DMs[ch]
	return ok && d.MPIM
}

func (p *poll) permalink(ch string, m message) string {
	if p.cur.URL == "" {
		return ""
	}
	u := p.cur.URL + "/archives/" + ch + "/p" + strings.ReplaceAll(m.TS, ".", "")
	if m.ThreadTS != "" && m.ThreadTS != m.TS {
		u += "?thread_ts=" + m.ThreadTS + "&cid=" + ch
	}
	return u
}

func (p *poll) author(m message) (string, error) {
	if m.User != "" {
		return p.userName(m.User)
	}
	switch {
	case m.Username != "":
		return m.Username, nil
	case m.BotProfile != nil && m.BotProfile.Name != "":
		return m.BotProfile.Name, nil
	}
	return m.BotID, nil
}

// userName resolves a user id through users.info (cached 24 h): display name, else
// real name, else the id.
func (p *poll) userName(id string) (string, error) {
	if c, ok := p.cur.Users[id]; ok {
		return c.N, nil
	}
	var r struct {
		User struct {
			Name     string `json:"name"`
			RealName string `json:"real_name"`
			Profile  struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	name := id
	if err := p.call("users.info", url.Values{"user": {id}}, &r); err != nil {
		var ae *apiError
		if !errors.As(err, &ae) || ae.fatal() {
			return "", err
		}
	} else {
		for _, s := range []string{r.User.Profile.DisplayName, r.User.Profile.RealName, r.User.RealName, r.User.Name} {
			if s = strings.TrimSpace(s); s != "" {
				name = s
				break
			}
		}
	}
	p.cur.Users[id] = cached{name, p.now.Unix()}
	return name, nil
}

// title: "#name" for channels (conversations.info, cached 24 h), "DM <name>" for DMs.
func (p *poll) title(ch string) (string, error) {
	if d, ok := p.cur.DMs[ch]; ok {
		if d.MPIM {
			return "DM " + mpimName(d.Name), nil
		}
		n, err := p.userName(d.User)
		if err != nil {
			return "", err
		}
		return "DM " + n, nil
	}
	if c, ok := p.cur.Names[ch]; ok {
		return c.N, nil
	}
	var r struct {
		Channel struct {
			Name   string `json:"name"`
			IsIM   bool   `json:"is_im"`
			IsMPIM bool   `json:"is_mpim"`
			User   string `json:"user"`
		} `json:"channel"`
	}
	if err := p.call("conversations.info", url.Values{"channel": {ch}}, &r); err != nil {
		return "", err
	}
	t := "#" + r.Channel.Name
	switch {
	case r.Channel.IsIM:
		n, err := p.userName(r.Channel.User)
		if err != nil {
			return "", err
		}
		t = "DM " + n
	case r.Channel.IsMPIM:
		t = "DM " + mpimName(r.Channel.Name)
	case r.Channel.Name == "":
		t = "#" + ch
	}
	p.cur.Names[ch] = cached{t, p.now.Unix()}
	return t, nil
}

// mpimName: "mpdm-alice--bob--carol-1" → "alice, bob, carol".
func mpimName(s string) string {
	s = strings.TrimPrefix(s, "mpdm-")
	if i := strings.LastIndexByte(s, '-'); i > 0 {
		if _, err := strconv.Atoi(s[i+1:]); err == nil {
			s = s[:i]
		}
	}
	return strings.ReplaceAll(s, "--", ", ")
}

var mentionRe = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)

var tokenRe = regexp.MustCompile(`<([^<>]*)>`)

// rewrite turns Slack mrkdwn references into plain text: <@U…> → @name (the
// cached name; else the label or id), <#C…|name> → #name, <!here> → @here, <url|label> →
// label (url), and decodes &lt; &gt; &amp;.
func (p *poll) rewrite(s string) string {
	s = tokenRe.ReplaceAllStringFunc(s, func(tok string) string {
		in := tok[1 : len(tok)-1]
		ref, label, hasLabel := strings.Cut(in, "|")
		switch {
		case strings.HasPrefix(ref, "@"):
			id := ref[1:]
			if c, ok := p.cur.Users[id]; ok {
				return "@" + c.N
			}
			if hasLabel && label != "" {
				return "@" + strings.TrimPrefix(label, "@")
			}
			return "@" + id
		case strings.HasPrefix(ref, "#"):
			id := ref[1:]
			if hasLabel && label != "" {
				return "#" + label
			}
			if c, ok := p.cur.Names[id]; ok {
				return c.N
			}
			return "#" + id
		case strings.HasPrefix(ref, "!"):
			if hasLabel && label != "" {
				return label
			}
			name, _, _ := strings.Cut(ref[1:], "^")
			return "@" + name
		}
		if !hasLabel || label == "" {
			return strings.TrimPrefix(ref, "mailto:")
		}
		if label == ref || "mailto:"+label == ref {
			return label
		}
		return label + " (" + ref + ")"
	})
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

// slimRaw keeps the message JSON with its channel, reducing files to names and
// titles and attachments to their titles and links.
func slimRaw(ch string, raw json.RawMessage) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	m["channel"] = ch
	if fs, ok := m["files"].([]any); ok {
		out := make([]any, 0, len(fs))
		for _, f := range fs {
			if fm, ok := f.(map[string]any); ok {
				out = append(out, pick(fm, "name", "title", "filetype", "size"))
			}
		}
		m["files"] = out
	}
	if as, ok := m["attachments"].([]any); ok {
		out := make([]any, 0, len(as))
		for _, a := range as {
			if am, ok := a.(map[string]any); ok {
				out = append(out, pick(am, "title", "title_link", "from_url", "fallback"))
			}
		}
		m["attachments"] = out
	}
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

func pick(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

// Slack timestamps are "<unix seconds>.<6 digits>", unique per conversation.

func tsParts(ts string) (int64, int64) {
	sec, frac, _ := strings.Cut(ts, ".")
	s, _ := strconv.ParseInt(sec, 10, 64)
	frac = (frac + "000000")[:6]
	f, _ := strconv.ParseInt(frac, 10, 64)
	return s, f
}

func tsLess(a, b string) bool {
	as, af := tsParts(a)
	bs, bf := tsParts(b)
	return as < bs || (as == bs && af < bf)
}

func tsTime(ts string) time.Time {
	s, f := tsParts(ts)
	return time.Unix(s, f*1000).UTC()
}

func tsFromTime(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}
