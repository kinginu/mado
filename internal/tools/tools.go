// Package tools is the hub's tool registry (DESIGN §17.7): web_search, fetch_url and
// generate_image, each behind a small backend interface. Chat sessions offer the
// enabled tools to the model; code sessions reach web_search and fetch_url through
// the worker's mado MCP server, which relays the call to the hub. Keys and backends
// never leave the hub.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/kinginu/mado/internal/messages"
)

// Tool names.
const (
	WebSearch     = "web_search"
	FetchURL      = "fetch_url"
	GenerateImage = "generate_image"
)

// CodeTools are the tools code sessions may call through the MCP relay.
var CodeTools = []string{WebSearch, FetchURL}

// Config is [chat.tools].
type Config struct {
	// Fetch offers fetch_url (default true; it needs no backend).
	Fetch *bool `toml:"fetch"`
	// FetchMaxChars truncates fetched text (default 20000).
	FetchMaxChars int `toml:"fetch_max_chars"`
	// AllowPrivate lets fetch_url reach loopback, link-local, RFC 1918, CGNAT and ULA
	// addresses. Off by default.
	AllowPrivate bool         `toml:"allow_private"`
	Search       SearchConfig `toml:"search"`
	Image        ImageConfig  `toml:"image"`
}

// SearchConfig is [chat.tools.search]. An empty kind means no web_search.
type SearchConfig struct {
	Kind    string `toml:"kind"`     // searxng | brave
	BaseURL string `toml:"base_url"` // searxng
	KeyRef  string `toml:"key_ref"`  // brave
	// AllowPrivate lets the searxng connection reach a private address (a SearXNG on
	// the LAN or the tailnet).
	AllowPrivate bool `toml:"allow_private"`
}

// ImageConfig is [chat.tools.image]. kind "none" (the default) means no
// generate_image.
type ImageConfig struct {
	Kind    string `toml:"kind"` // none | openai
	BaseURL string `toml:"base_url"`
	Model   string `toml:"model"`
	KeyRef  string `toml:"key_ref"`
	Size    string `toml:"size"` // the default size; empty = the model's default
}

const DefaultFetchMaxChars = 20000

// FetchEnabled reports the fetch setting with its default.
func (c Config) FetchEnabled() bool { return c.Fetch == nil || *c.Fetch }

// Validate checks the config and fills the defaults. keys are the secrets file's
// [keys]; every key_ref must name one.
func (c *Config) Validate(keys map[string]string) error {
	if c.FetchMaxChars == 0 {
		c.FetchMaxChars = DefaultFetchMaxChars
	}
	if c.FetchMaxChars < 1000 || c.FetchMaxChars > 1_000_000 {
		return errors.New("[chat.tools] fetch_max_chars must be 1000..1000000")
	}
	needKey := func(section, ref string) error {
		if ref == "" {
			return fmt.Errorf("[chat.tools.%s] needs key_ref", section)
		}
		if keys != nil {
			if _, ok := keys[ref]; !ok {
				return fmt.Errorf("[chat.tools.%s] key_ref %q not in secrets file", section, ref)
			}
		}
		return nil
	}
	s := &c.Search
	switch s.Kind {
	case "":
	case "searxng":
		if err := checkBase(s.BaseURL); err != nil {
			return fmt.Errorf("[chat.tools.search] base_url: %w", err)
		}
	case "brave":
		if err := needKey("search", s.KeyRef); err != nil {
			return err
		}
	default:
		return fmt.Errorf("[chat.tools.search] kind must be searxng or brave")
	}
	im := &c.Image
	switch im.Kind {
	case "", "none":
		im.Kind = "none"
	case "openai":
		if im.BaseURL == "" {
			im.BaseURL = "https://api.openai.com"
		}
		if err := checkBase(im.BaseURL); err != nil {
			return fmt.Errorf("[chat.tools.image] base_url: %w", err)
		}
		if im.Model == "" {
			im.Model = "gpt-image-1"
		}
		if err := needKey("image", im.KeyRef); err != nil {
			return err
		}
		if im.Size != "" && !slices.Contains(imageSizes(im.Model), im.Size) {
			return fmt.Errorf("[chat.tools.image] size must be one of %s", strings.Join(imageSizes(im.Model), ", "))
		}
	default:
		return fmt.Errorf("[chat.tools.image] kind must be none or openai")
	}
	return nil
}

func checkBase(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return errors.New("must be http(s)://host[:port][/path]")
	}
	return nil
}

// Result is what a tool produced. Text goes back to the model; Image, when set, is
// stored by the caller and shown to the user.
type Result struct {
	Text  string
	Image *Image
	// Event, when set, is stored as its own event right after the tool results (the
	// concierge's plan card, DESIGN §20.3).
	Event json.RawMessage
}

// Image is a generated picture.
type Image struct {
	Data   []byte
	Mime   string
	Model  string
	Prompt string
	// Usage is the backend's token count (gpt-image models report one), priced
	// through [[prices]] like a chat response.
	Usage *messages.Usage
}

// Searcher is a web search backend.
type Searcher interface {
	Search(ctx context.Context, query string) ([]SearchResult, error)
}

// SearchResult is one hit.
type SearchResult struct {
	Title, URL, Snippet string
}

// Imager is an image generation backend.
type Imager interface {
	Generate(ctx context.Context, prompt, size string) (*Image, error)
	Sizes() []string
}

// Registry holds the enabled tools.
type Registry struct {
	Search  Searcher
	Fetcher *Fetcher
	Imager  Imager
}

// New builds the registry from a validated config.
func New(c Config, keys map[string]string) *Registry {
	r := &Registry{}
	switch c.Search.Kind {
	case "searxng":
		r.Search = &SearXNG{BaseURL: c.Search.BaseURL, Guard: Guard{AllowPrivate: c.Search.AllowPrivate}}
	case "brave":
		r.Search = &Brave{Key: keys[c.Search.KeyRef]}
	}
	if c.FetchEnabled() {
		r.Fetcher = &Fetcher{Guard: Guard{AllowPrivate: c.AllowPrivate}, MaxChars: c.FetchMaxChars}
	}
	if c.Image.Kind == "openai" {
		r.Imager = &OpenAIImages{BaseURL: c.Image.BaseURL, Model: c.Image.Model, Key: keys[c.Image.KeyRef], Size: c.Image.Size}
	}
	return r
}

// Names lists the enabled tools in a fixed order.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	var out []string
	if r.Search != nil {
		out = append(out, WebSearch)
	}
	if r.Fetcher != nil {
		out = append(out, FetchURL)
	}
	if r.Imager != nil {
		out = append(out, GenerateImage)
	}
	return out
}

// Has reports whether a tool is enabled.
func (r *Registry) Has(name string) bool { return slices.Contains(r.Names(), name) }

// CodeNames are the enabled tools a code session may call.
func (r *Registry) CodeNames() []string {
	var out []string
	for _, n := range r.Names() {
		if slices.Contains(CodeTools, n) {
			out = append(out, n)
		}
	}
	return out
}

// Defs are the enabled tools as custom Messages API tools (no Anthropic `type`).
func (r *Registry) Defs() []messages.Tool {
	var out []messages.Tool
	for _, n := range r.Names() {
		var sizes []string
		if n == GenerateImage {
			sizes = r.Imager.Sizes()
		}
		out = append(out, Definition(n, sizes))
	}
	return out
}

// Definition is the tool definition of one tool. sizes is generate_image's size enum.
func Definition(name string, sizes []string) messages.Tool {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	var desc string
	var props map[string]any
	required := []string{}
	switch name {
	case WebSearch:
		desc = "Search the web. Returns up to 8 results, each with a title, URL and snippet. " +
			"Use it for recent events or facts you are not sure of; read a promising page with fetch_url."
		props = map[string]any{"query": str("The search query, in the words a person would type into a search engine.")}
		required = []string{"query"}
	case FetchURL:
		desc = "Fetch a web page (http or https) and return its title and readable text. Long pages are truncated."
		props = map[string]any{"url": str("The absolute URL to fetch.")}
		required = []string{"url"}
	case GenerateImage:
		desc = "Generate an image from a text description. The image is shown to the user directly; " +
			"do not try to describe or link it afterwards beyond a short remark."
		size := str("Image size in pixels, width x height. Omit for the default.")
		if len(sizes) > 0 {
			size["enum"] = sizes
		}
		props = map[string]any{"prompt": str("A detailed description of the image to generate."), "size": size}
		required = []string{"prompt"}
	}
	b, _ := json.Marshal(map[string]any{"type": "object", "properties": props, "required": required})
	return messages.Tool{Name: name, Description: desc, InputSchema: b}
}

// Error is a tool failure reported to the model as an is_error tool_result.
type Error string

func (e Error) Error() string { return string(e) }

// Run executes one call. Every failure is an error whose text is safe to show to
// the model and the user (no keys).
func (r *Registry) Run(ctx context.Context, name string, input json.RawMessage) (Result, error) {
	var in struct {
		Query  string `json:"query"`
		URL    string `json:"url"`
		Prompt string `json:"prompt"`
		Size   string `json:"size"`
	}
	if len(input) > 0 && json.Unmarshal(input, &in) != nil {
		return Result{}, Error("the tool input is not a JSON object")
	}
	if !r.Has(name) {
		return Result{}, Error("unknown tool " + name)
	}
	switch name {
	case WebSearch:
		q := strings.TrimSpace(in.Query)
		if q == "" || len(q) > 1000 {
			return Result{}, Error("query must be 1..1000 bytes")
		}
		res, err := r.Search.Search(ctx, q)
		if err != nil {
			return Result{}, err
		}
		return Result{Text: FormatResults(q, res)}, nil
	case FetchURL:
		if len(in.URL) > 4000 {
			return Result{}, Error("URL too long")
		}
		text, err := r.Fetcher.Fetch(ctx, strings.TrimSpace(in.URL))
		if err != nil {
			return Result{}, err
		}
		return Result{Text: text}, nil
	case GenerateImage:
		p := strings.TrimSpace(in.Prompt)
		if p == "" || len(p) > 32000 {
			return Result{}, Error("prompt must be 1..32000 bytes")
		}
		if in.Size != "" && !slices.Contains(r.Imager.Sizes(), in.Size) {
			return Result{}, Error("size must be one of " + strings.Join(r.Imager.Sizes(), ", "))
		}
		img, err := r.Imager.Generate(ctx, p, in.Size)
		if err != nil {
			return Result{}, err
		}
		img.Prompt = p
		return Result{Image: img}, nil
	}
	return Result{}, Error("unknown tool " + name)
}

// MaxResults is how many search hits go to the model.
const MaxResults = 8

// FormatResults is the tool_result text of a search: a numbered list, one hit per
// block (title, URL, snippet), readable as is in the transcript.
func FormatResults(query string, res []SearchResult) string {
	if len(res) == 0 {
		return "No results for " + query + "."
	}
	var b strings.Builder
	for i, r := range res {
		if i >= MaxResults {
			break
		}
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, oneLine(r.Title, 200), r.URL)
		if s := oneLine(r.Snippet, 500); s != "" {
			fmt.Fprintf(&b, "   %s\n", s)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}
