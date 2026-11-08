package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	searchTimeout = 15 * time.Second
	maxSearchBody = 2 << 20
	userAgent     = "mado/1 (+https://github.com/kinginu/mado)"
)

// SearXNG queries a SearXNG instance's JSON API (`search.formats` must include json).
type SearXNG struct {
	BaseURL string
	Guard   Guard
}

func (s *SearXNG) Search(ctx context.Context, q string) ([]SearchResult, error) {
	u, err := url.Parse(strings.TrimRight(s.BaseURL, "/") + "/search")
	if err != nil {
		return nil, Error("search backend misconfigured")
	}
	if err := s.Guard.CheckURL(u); err != nil {
		return nil, Error("search backend: " + err.Error())
	}
	u.RawQuery = url.Values{"q": {q}, "format": {"json"}}.Encode()
	var body struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := getJSON(ctx, s.Guard.Client(searchTimeout), u.String(), nil, &body); err != nil {
		return nil, err
	}
	var out []SearchResult
	for _, r := range body.Results {
		if r.URL == "" || len(out) >= MaxResults {
			continue
		}
		out = append(out, SearchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}

// BraveURL is the Brave Search web endpoint.
const BraveURL = "https://api.search.brave.com/res/v1/web/search"

// Brave queries the Brave Search API.
type Brave struct {
	Key     string
	BaseURL string // default BraveURL (tests)
	Client  *http.Client
}

func (b *Brave) Search(ctx context.Context, q string) ([]SearchResult, error) {
	base := b.BaseURL
	if base == "" {
		base = BraveURL
	}
	c := b.Client
	if c == nil {
		c = &http.Client{Timeout: searchTimeout}
	}
	u := base + "?" + url.Values{"q": {q}, "count": {fmt.Sprint(MaxResults)}}.Encode()
	var body struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := getJSON(ctx, c, u, http.Header{"X-Subscription-Token": {b.Key}}, &body); err != nil {
		return nil, err
	}
	var out []SearchResult
	for _, r := range body.Web.Results {
		if r.URL == "" || len(out) >= MaxResults {
			continue
		}
		out = append(out, SearchResult{Title: stripTags(r.Title), URL: r.URL, Snippet: stripTags(r.Description)})
	}
	return out, nil
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

// stripTags removes Brave's <strong> highlighting and decodes entities.
func stripTags(s string) string { return html.UnescapeString(tagRe.ReplaceAllString(s, "")) }

func getJSON(ctx context.Context, c *http.Client, u string, h http.Header, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Error("search backend misconfigured")
	}
	for k, vs := range h {
		req.Header[k] = vs
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return Error("search failed: " + transportError(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSearchBody))
	if err != nil {
		return Error("search failed: could not read the answer")
	}
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("search failed: HTTP %d", resp.StatusCode)
		if resp.StatusCode == http.StatusForbidden {
			msg += " (for SearXNG, enable the json format in settings.yml; for Brave, check the key)"
		}
		return Error(msg)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return Error("search failed: the answer is not JSON")
	}
	return nil
}

// transportError shortens a client error to its cause (the URL is already known).
func transportError(err error) string {
	if errors.Is(err, ErrBlocked) {
		return ErrBlocked.Error()
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}
