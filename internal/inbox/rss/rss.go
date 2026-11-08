// Package rss is the inbox kind "rss" (DESIGN §23.4): RSS, Atom and JSON Feed through
// gofeed, with conditional GET and the last 1000 item ids in the cursor. RSSHub routes
// (X accounts and anything else it proxies) are ordinary feeds here.
package rss

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/kinginu/mado/internal/inbox"
	"github.com/kinginu/mado/internal/tools"
)

func init() { inbox.Register(Kind{}) }

// Kind is the rss kind.
type Kind struct{}

// Config is the account config.
type Config struct {
	URL          string `json:"url"`
	AllowPrivate bool   `json:"allow_private"`
}

// Cursor is the account cursor.
type Cursor struct {
	ETag         string   `json:"etag,omitempty"`
	LastModified string   `json:"last_modified,omitempty"`
	Seen         []string `json:"seen,omitempty"` // newest first
}

const (
	maxSeen     = 1000
	maxFeedSize = 10 << 20
)

func (Kind) Name() string { return "rss" }

func (Kind) Template() json.RawMessage {
	return json.RawMessage(`{"url": "https://example.org/feed.xml", "allow_private": false}`)
}

func (Kind) Validate(config json.RawMessage, keyRef string, keys map[string]string) error {
	var c Config
	if err := inbox.DecodeStrict(config, &c); err != nil {
		return err
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return errors.New("config: url must be an http(s) URL without credentials")
	}
	if keyRef != "" {
		return errors.New("an rss account takes no key_ref")
	}
	return nil
}

func (Kind) New(a inbox.Account, key string, env inbox.Env) (inbox.Collector, error) {
	var c Config
	if err := inbox.DecodeStrict(a.Config, &c); err != nil {
		return nil, err
	}
	return &collector{cfg: c, env: env}, nil
}

type collector struct {
	cfg Config
	env inbox.Env
}

func (c *collector) Poll(ctx context.Context, raw json.RawMessage) ([]inbox.Item, json.RawMessage, bool, error) {
	var cur Cursor
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &cur)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.URL, nil)
	if err != nil {
		return nil, nil, false, err
	}
	req.Header.Set("User-Agent", "mado-inbox/1 (+https://github.com/kinginu/mado)")
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/feed+json, application/xml;q=0.9, */*;q=0.8")
	if cur.ETag != "" {
		req.Header.Set("If-None-Match", cur.ETag)
	}
	if cur.LastModified != "" {
		req.Header.Set("If-Modified-Since", cur.LastModified)
	}
	client := c.env.HTTP
	if client == nil {
		client = (tools.Guard{}).Client(time.Minute)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, false, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, nil, false, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, nil, false, fmt.Errorf("fetch: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedSize+1))
	if err != nil {
		return nil, nil, false, fmt.Errorf("fetch: %w", err)
	}
	if len(body) > maxFeedSize {
		return nil, nil, false, errors.New("the feed is larger than 10 MB")
	}
	feed, err := gofeed.NewParser().ParseString(string(body))
	if err != nil {
		return nil, nil, false, fmt.Errorf("parse: %w", err)
	}
	seen := map[string]bool{}
	for _, id := range cur.Seen {
		seen[id] = true
	}
	now := time.Now
	if c.env.Now != nil {
		now = c.env.Now
	}
	type entry struct {
		it  inbox.Item
		pos int
	}
	var fresh []entry
	var ids []string
	for i, fi := range feed.Items {
		if fi == nil {
			continue
		}
		id := extID(fi)
		ids = append(ids, id)
		if seen[id] {
			continue
		}
		seen[id] = true
		fresh = append(fresh, entry{toItem(fi, id, now), i})
	}
	// Oldest first: by the item's time, feed order (usually newest first) breaking ties.
	sort.SliceStable(fresh, func(i, j int) bool {
		if !fresh[i].it.TS.Equal(fresh[j].it.TS) {
			return fresh[i].it.TS.Before(fresh[j].it.TS)
		}
		return fresh[i].pos > fresh[j].pos
	})
	items := make([]inbox.Item, len(fresh))
	for i, e := range fresh {
		items[i] = e.it
	}
	next := Cursor{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
	// The ids in this response, then the older ones, at most maxSeen.
	keep := map[string]bool{}
	for _, id := range append(ids, cur.Seen...) {
		if !keep[id] && len(next.Seen) < maxSeen {
			keep[id] = true
			next.Seen = append(next.Seen, id)
		}
	}
	b, _ := json.Marshal(next)
	return items, b, false, nil
}

// extID: guid, else link, else sha256(title+published).
func extID(fi *gofeed.Item) string {
	if g := strings.TrimSpace(fi.GUID); g != "" {
		return g
	}
	if l := strings.TrimSpace(fi.Link); l != "" {
		return l
	}
	h := sha256.Sum256([]byte(fi.Title + "\x00" + fi.Published))
	return "sha256:" + hex.EncodeToString(h[:])
}

func toItem(fi *gofeed.Item, id string, now func() time.Time) inbox.Item {
	text := tools.HTMLText(fi.Description)
	if c := tools.HTMLText(fi.Content); len(c) > len(text) {
		text = c
	}
	title := firstLine(tools.HTMLText(fi.Title))
	if title == "" {
		title = firstLine(text)
	}
	author := ""
	for _, p := range fi.Authors {
		if p != nil && strings.TrimSpace(p.Name) != "" {
			author = strings.TrimSpace(p.Name)
			break
		}
	}
	if author == "" && fi.DublinCoreExt != nil && len(fi.DublinCoreExt.Creator) > 0 {
		author = fi.DublinCoreExt.Creator[0]
	}
	ts := now()
	if fi.PublishedParsed != nil {
		ts = *fi.PublishedParsed
	} else if fi.UpdatedParsed != nil {
		ts = *fi.UpdatedParsed
	}
	raw, _ := json.Marshal(fi)
	return inbox.Item{ExtID: id, Kind: "feed", Author: author, Title: title, Text: text, URL: fi.Link, TS: ts.UTC(), Raw: raw}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}
