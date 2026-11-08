// Package github is the inbox kind "github" (DESIGN §23.4): the user's GitHub
// notifications through the REST API, one item per thread update, with the subject and
// its latest comment fetched once per item when fetch_bodies is set. Read-only by
// construction: the only HTTP method used is GET, nothing is ever marked read.
package github

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
	"unicode/utf8"

	"github.com/kinginu/mado/internal/inbox"
	"github.com/kinginu/mado/internal/tools"
)

func init() { inbox.Register(Kind{}) }

// The REST API and web roots. Tests point apiBase at an httptest server. The token is
// only ever sent to URLs under apiBase.
var (
	apiBase = "https://api.github.com"
	webBase = "https://github.com"
)

const (
	apiVersion   = "2022-11-28"
	userAgent    = "mado-inbox/1 (+https://github.com/kinginu/mado)"
	maxSeen      = 2000
	maxBodyBytes = 4 << 10
	maxListBytes = 8 << 20
	maxObjBytes  = 2 << 20
	defaultDays  = 7
)

// Kind is the github kind.
type Kind struct{}

// Config is the account config.
type Config struct {
	ParticipatingOnly bool  `json:"participating_only"`
	FetchBodies       *bool `json:"fetch_bodies"`
	ReceivedEvents    bool  `json:"received_events"`
	InitialDays       int   `json:"initial_days"`
}

func (c Config) fetchBodies() bool { return c.FetchBodies == nil || *c.FetchBodies }

func (c Config) days() int {
	if c.InitialDays == 0 {
		return defaultDays
	}
	return c.InitialDays
}

// Cursor is the account cursor.
type Cursor struct {
	Since        string            `json:"since,omitempty"`         // max updated_at of the finished passes
	LastModified string            `json:"last_modified,omitempty"` // Last-Modified of the last pass's first page
	PollInterval int               `json:"poll_interval,omitempty"` // X-Poll-Interval, seconds
	PolledAt     string            `json:"polled_at,omitempty"`     // when the last pass started
	Seen         map[string]string `json:"seen,omitempty"`          // thread id → updated_at
	// A pass in progress (more pages): the next page and what the pass has seen so far.
	Next         string `json:"next,omitempty"`
	PassSince    string `json:"pass_since,omitempty"`
	PassModified string `json:"pass_modified,omitempty"`
}

func (Kind) Name() string { return "github" }

func (Kind) Template() json.RawMessage {
	return json.RawMessage(`{"participating_only": false, "fetch_bodies": true, "received_events": false, "initial_days": 7}`)
}

func (Kind) Validate(config json.RawMessage, keyRef string, keys map[string]string) error {
	var c Config
	if err := inbox.DecodeStrict(config, &c); err != nil {
		return err
	}
	if c.ReceivedEvents {
		return errors.New("config: received_events is not implemented in M19; set it to false")
	}
	if c.InitialDays < 0 || c.InitialDays > 365 {
		return errors.New("config: initial_days must be between 1 and 365 (0 = 7)")
	}
	if keyRef == "" {
		return errors.New("a github account needs a key_ref: the secrets-file key whose value is a fine-grained PAT with Notifications: read")
	}
	if keys != nil {
		if _, ok := keys[keyRef]; !ok {
			return fmt.Errorf("key_ref %q is not in the secrets file", keyRef)
		}
	}
	return nil
}

func (Kind) New(a inbox.Account, key string, env inbox.Env) (inbox.Collector, error) {
	var c Config
	if err := inbox.DecodeStrict(a.Config, &c); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("the github token (key_ref) is empty")
	}
	client := env.HTTP
	if client == nil {
		client = (tools.Guard{}).Client(time.Minute)
	}
	now := env.Now
	if now == nil {
		now = time.Now
	}
	return &collector{cfg: c, token: strings.TrimSpace(key), http: client, now: now}, nil
}

type collector struct {
	cfg   Config
	token string
	http  *http.Client
	now   func() time.Time
}

// notification is the part of a /notifications thread the collector reads; the whole
// object is kept in raw.
type notification struct {
	ID        string `json:"id"`
	Reason    string `json:"reason"`
	UpdatedAt string `json:"updated_at"`
	Subject   struct {
		Title            string `json:"title"`
		URL              string `json:"url"`
		LatestCommentURL string `json:"latest_comment_url"`
		Type             string `json:"type"`
	} `json:"subject"`
	Repository struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
}

// fetched is what the collector keeps of a subject or comment fetch.
type fetched struct {
	Title   string `json:"title,omitempty"`
	State   string `json:"state,omitempty"`
	Body    string `json:"body,omitempty"`
	Author  string `json:"author,omitempty"`
	HTMLURL string `json:"html_url,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (c *collector) Poll(ctx context.Context, raw json.RawMessage) ([]inbox.Item, json.RawMessage, bool, error) {
	var cur Cursor
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &cur)
	}
	now := c.now().UTC()
	first := cur.Next == ""
	var reqURL string
	if first {
		// Never poll sooner than GitHub's X-Poll-Interval, whatever interval_s says.
		if t, err := time.Parse(time.RFC3339, cur.PolledAt); err == nil && cur.PollInterval > 0 &&
			now.Before(t.Add(time.Duration(cur.PollInterval)*time.Second)) {
			return nil, nil, false, nil
		}
		since := cur.Since
		if since == "" {
			since = now.Add(-time.Duration(c.cfg.days()) * 24 * time.Hour).Format(time.RFC3339)
		}
		q := url.Values{}
		q.Set("all", "false")
		q.Set("participating", strconv.FormatBool(c.cfg.ParticipatingOnly))
		q.Set("per_page", "50")
		q.Set("since", since)
		reqURL = apiBase + "/notifications?" + q.Encode()
		cur.PolledAt = now.Format(time.RFC3339)
		cur.PassSince, cur.PassModified = "", ""
	} else {
		reqURL = cur.Next
		if !underAPI(reqURL) {
			cur.Next = ""
			return nil, mustJSON(cur), false, errors.New("the stored next-page URL is not under the GitHub API; starting over")
		}
	}
	hdr := http.Header{}
	if first && cur.LastModified != "" {
		hdr.Set("If-Modified-Since", cur.LastModified)
	}
	resp, body, err := c.get(ctx, reqURL, hdr, maxListBytes)
	if err != nil {
		return nil, nil, false, err
	}
	if n, err := strconv.Atoi(resp.Header.Get("X-Poll-Interval")); err == nil && n > 0 {
		cur.PollInterval = n
	}
	if resp.StatusCode == http.StatusNotModified {
		return nil, mustJSON(cur), false, nil
	}
	if first {
		cur.PassModified = resp.Header.Get("Last-Modified")
	}
	var threads []json.RawMessage
	if err := json.Unmarshal(body, &threads); err != nil {
		return nil, nil, false, fmt.Errorf("notifications: %w", err)
	}
	if cur.Seen == nil {
		cur.Seen = map[string]string{}
	}
	var items []inbox.Item
	for _, tr := range threads {
		var n notification
		if err := json.Unmarshal(tr, &n); err != nil || n.ID == "" || n.UpdatedAt == "" {
			continue
		}
		if n.UpdatedAt > cur.PassSince {
			cur.PassSince = n.UpdatedAt
		}
		if cur.Seen[n.ID] == n.UpdatedAt {
			continue
		}
		it, err := c.item(ctx, n, tr)
		if err != nil {
			// Rate limit or a dead connection in a body fetch: keep the old cursor, the
			// page is fetched again next time.
			return nil, nil, false, err
		}
		cur.Seen[n.ID] = n.UpdatedAt
		items = append(items, it)
	}
	// The API lists newest first; hand them over oldest first.
	sort.SliceStable(items, func(i, j int) bool { return items[i].TS.Before(items[j].TS) })
	next := nextLink(resp.Header.Get("Link"))
	more := next != "" && underAPI(next) && len(threads) > 0
	if more {
		cur.Next = next
	} else {
		cur.Next = ""
		if cur.PassSince > cur.Since {
			cur.Since = cur.PassSince
		}
		if cur.PassModified != "" {
			cur.LastModified = cur.PassModified
		}
		cur.PassSince, cur.PassModified = "", ""
	}
	trimSeen(cur.Seen)
	return items, mustJSON(cur), more, nil
}

// item builds the inbox item for one thread update, fetching bodies when configured.
func (c *collector) item(ctx context.Context, n notification, rawThread json.RawMessage) (inbox.Item, error) {
	ts, err := time.Parse(time.RFC3339, n.UpdatedAt)
	if err != nil {
		ts = c.now()
	}
	repo := n.Repository.FullName
	title := n.Subject.Title
	if repo != "" {
		title = repo + ": " + title
	}
	if n.Subject.Type != "" {
		title += " [" + n.Subject.Type + "]"
	}
	it := inbox.Item{
		ExtID:     n.ID + "@" + n.UpdatedAt,
		ThreadKey: n.ID,
		Kind:      n.Reason,
		Title:     title,
		Author:    n.Repository.Owner.Login,
		URL:       WebURL(n.Subject.URL, n.Subject.Type, n.Repository.HTMLURL),
		TS:        ts.UTC(),
	}
	if it.Kind == "" {
		it.Kind = "notification"
	}
	rawOut := map[string]any{"notification": rawThread}
	if !c.cfg.fetchBodies() {
		it.Text = fmt.Sprintf("%s in %s (reason: %s)", n.Subject.Type, repo, n.Reason)
		it.Raw = mustJSON(rawOut)
		return it, nil
	}
	var subj, com *fetched
	if n.Subject.URL != "" {
		subj, err = c.fetch(ctx, n.Subject.URL)
		if err != nil {
			return it, err
		}
		rawOut["subject"] = subj
		if subj.HTMLURL != "" && subj.Error == "" {
			it.URL = subj.HTMLURL
		}
		if subj.Author != "" {
			it.Author = subj.Author
		}
	}
	if lc := n.Subject.LatestCommentURL; lc != "" && lc != n.Subject.URL {
		com, err = c.fetch(ctx, lc)
		if err != nil {
			return it, err
		}
		rawOut["comment"] = com
		if com.Error == "" {
			if com.HTMLURL != "" {
				it.URL = com.HTMLURL
			}
			if com.Author != "" {
				it.Author = com.Author
			}
		}
	}
	var b strings.Builder
	if subj != nil && subj.Error == "" {
		if subj.State != "" {
			b.WriteString("state: " + subj.State)
		}
		if subj.Body != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(subj.Body)
		}
	} else {
		fmt.Fprintf(&b, "%s in %s (reason: %s)", n.Subject.Type, repo, n.Reason)
		if subj != nil {
			b.WriteString("\n(subject not fetched: " + subj.Error + ")")
		}
	}
	if com != nil && com.Error == "" && com.Body != "" {
		b.WriteString("\n\n--- latest comment by @" + com.Author + " ---\n" + com.Body)
	}
	it.Text = b.String()
	it.Raw = mustJSON(rawOut)
	return it, nil
}

// fetch GETs one subject or comment. HTTP errors other than the rate limit (a private
// repository the token cannot read, a deleted comment) are recorded, not returned.
func (c *collector) fetch(ctx context.Context, u string) (*fetched, error) {
	if !underAPI(u) {
		return &fetched{Error: "not a GitHub API URL"}, nil
	}
	resp, body, err := c.get(ctx, u, nil, maxObjBytes)
	if err != nil {
		var rl *rateLimitError
		if errors.As(err, &rl) || ctx.Err() != nil {
			return nil, err
		}
		return &fetched{Error: err.Error()}, nil
	}
	if resp.StatusCode == http.StatusNotModified {
		return &fetched{Error: "HTTP 304"}, nil
	}
	var o struct {
		Title   string `json:"title"`
		Name    string `json:"name"`
		State   string `json:"state"`
		Merged  bool   `json:"merged"`
		Draft   bool   `json:"draft"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &o); err != nil {
		return &fetched{Error: "not JSON"}, nil
	}
	f := &fetched{Title: o.Title, State: o.State, Body: o.Body, HTMLURL: o.HTMLURL, Author: o.User.Login}
	if f.Title == "" {
		f.Title = o.Name // releases
	}
	if f.Author == "" {
		f.Author = o.Author.Login // releases, commits
	}
	if f.Body == "" {
		f.Body = o.Commit.Message
	}
	if o.Merged {
		f.State = "merged"
	} else if o.Draft && f.State == "open" {
		f.State = "open (draft)"
	}
	f.Body = clip(f.Body, maxBodyBytes)
	if !strings.HasPrefix(f.HTMLURL, "https://") && !strings.HasPrefix(f.HTMLURL, "http://") {
		f.HTMLURL = ""
	}
	return f, nil
}

type rateLimitError struct{ msg string }

func (e *rateLimitError) Error() string { return e.msg }

// get performs one authenticated GET. 2xx and 304 come back; everything else is an
// error, the rate limit as a *rateLimitError naming the reset time.
func (c *collector) get(ctx context.Context, u string, hdr http.Header, limit int64) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("github: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return resp, nil, nil
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if resp.Header.Get("X-Ratelimit-Remaining") == "0" {
			reset := "unknown"
			if s, err := strconv.ParseInt(resp.Header.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
				reset = time.Unix(s, 0).UTC().Format(time.RFC3339)
			}
			return nil, nil, &rateLimitError{"github: rate limit exhausted (HTTP " + strconv.Itoa(resp.StatusCode) + "); resets at " + reset}
		}
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			return nil, nil, &rateLimitError{"github: secondary rate limit (HTTP " + strconv.Itoa(resp.StatusCode) + "); retry after " + ra + " s"}
		}
	}
	if resp.StatusCode/100 != 2 {
		msg := fmt.Sprintf("github: HTTP %d for %s", resp.StatusCode, req.URL.Path)
		if resp.StatusCode == http.StatusUnauthorized {
			msg += " (the token is invalid or expired)"
		}
		return nil, nil, errors.New(msg)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, fmt.Errorf("github: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, nil, fmt.Errorf("github: response for %s larger than %d bytes", req.URL.Path, limit)
	}
	return resp, body, nil
}

func underAPI(u string) bool { return strings.HasPrefix(u, apiBase+"/") }

var linkRe = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="([^"]+)"`)

// nextLink returns the rel="next" URL of a Link header.
func nextLink(h string) string {
	for _, m := range linkRe.FindAllStringSubmatch(h, -1) {
		for _, rel := range strings.Fields(m[2]) {
			if rel == "next" {
				return m[1]
			}
		}
	}
	return ""
}

// WebURL derives the github.com page of a notification subject from its API URL:
// issues stay issues, pulls/N → pull/N, commits/sha → commit/sha, a release → the
// releases page (the exact page needs a fetch), a discussion (no REST subject URL) →
// the discussions page, anything else → the repository.
func WebURL(apiURL, subjectType, repoHTML string) string {
	repoPage := func(o, r string) string { return webBase + "/" + o + "/" + r }
	var rest string
	for _, b := range []string{apiBase, "https://api.github.com"} {
		if strings.HasPrefix(apiURL, b+"/repos/") {
			rest = strings.TrimPrefix(apiURL, b+"/repos/")
			break
		}
	}
	if rest == "" {
		// No subject URL (discussions, check suites): derive from the repository.
		if o, r, ok := ownerRepo(repoHTML); ok {
			if subjectType == "Discussion" {
				return repoPage(o, r) + "/discussions"
			}
			return repoPage(o, r)
		}
		return ""
	}
	p := strings.Split(rest, "/")
	if len(p) < 2 || p[0] == "" || p[1] == "" {
		return ""
	}
	base := repoPage(p[0], p[1])
	if len(p) < 4 {
		if subjectType == "Discussion" {
			return base + "/discussions"
		}
		return base
	}
	switch p[2] {
	case "issues":
		if p[3] == "comments" {
			return base + "/issues"
		}
		return base + "/issues/" + p[3]
	case "pulls":
		return base + "/pull/" + p[3]
	case "commits":
		return base + "/commit/" + p[3]
	case "releases":
		return base + "/releases"
	case "discussions":
		return base + "/discussions/" + p[3]
	}
	if subjectType == "Discussion" {
		return base + "/discussions"
	}
	return base
}

// ownerRepo splits https://github.com/o/r.
func ownerRepo(u string) (string, string, bool) {
	for _, b := range []string{webBase, "https://github.com"} {
		if strings.HasPrefix(u, b+"/") {
			p := strings.Split(strings.TrimPrefix(u, b+"/"), "/")
			if len(p) >= 2 && p[0] != "" && p[1] != "" {
				return p[0], p[1], true
			}
		}
	}
	return "", "", false
}

// trimSeen keeps the maxSeen most recently updated threads.
func trimSeen(m map[string]string) {
	if len(m) <= maxSeen {
		return
	}
	type kv struct{ id, ts string }
	all := make([]kv, 0, len(m))
	for id, ts := range m {
		all = append(all, kv{id, ts})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].ts != all[j].ts {
			return all[i].ts > all[j].ts
		}
		return all[i].id > all[j].id
	})
	for _, e := range all[maxSeen:] {
		delete(m, e.id)
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "\n[…]"
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
