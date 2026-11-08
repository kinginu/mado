// Package messages is a small client for the Anthropic Messages API (DESIGN §17.2):
// the request and response types, the auth header rule shared with the router, and a
// streaming call that parses the SSE stream into typed events and assembles the final
// Response. It is written on net/http; there is no SDK.
package messages

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// Version is sent as anthropic-version on every request.
const Version = "2023-06-01"

// Request is the body of POST /v1/messages.
type Request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
	Tools     []Tool    `json:"tools,omitempty"`
	// ToolChoice forces a tool ({"type":"tool","name":...}); nil leaves it to the model.
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`
	Stream     bool        `json:"stream,omitempty"`
}

// ToolChoice is the request's tool_choice: auto, any, none, or tool with a name.
type ToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// Message is one conversation turn.
type Message struct {
	Role    string  `json:"role"` // user | assistant
	Content []Block `json:"content"`
}

// Block is a content block. One struct covers the types mado uses: text, image,
// document, tool_use, tool_result, and thinking (parsed from responses, never sent).
type Block struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// image, document
	Source *Source `json:"source,omitempty"`
	Title  string  `json:"title,omitempty"` // document
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// Source is the base64 payload of an image or document block.
type Source struct {
	Type      string `json:"type"` // base64
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// Tool is a custom tool definition (no Anthropic-defined `type`).
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Usage counts tokens. InputTokens excludes the cache-read and cache-write parts.
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// Add sums another response's counts into u.
func (u *Usage) Add(v Usage) {
	u.InputTokens += v.InputTokens
	u.OutputTokens += v.OutputTokens
	u.CacheCreationInputTokens += v.CacheCreationInputTokens
	u.CacheReadInputTokens += v.CacheReadInputTokens
}

// TotalInput is everything the model read: input plus both cache parts.
func (u Usage) TotalInput() int64 {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// Response is a complete (or, after an error, partial) assistant message.
type Response struct {
	ID           string  `json:"id"`
	Type         string  `json:"type,omitempty"`
	Role         string  `json:"role"`
	Model        string  `json:"model"`
	Content      []Block `json:"content"`
	StopReason   string  `json:"stop_reason,omitempty"`
	StopSequence string  `json:"stop_sequence,omitempty"`
	Usage        Usage   `json:"usage"`
}

// Text joins the response's text blocks.
func (r *Response) Text() string {
	var b strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// Auth header modes.
const (
	AuthBearer = "bearer"
	AuthAPIKey = "x-api-key"
)

// AuthHeader picks how the key is sent: the explicit mode when set, x-api-key for
// api.anthropic.com, bearer otherwise. The router uses the same rule.
func AuthHeader(baseURL, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if pu, err := url.Parse(baseURL); err == nil && strings.EqualFold(pu.Hostname(), "api.anthropic.com") {
		return AuthAPIKey
	}
	return AuthBearer
}

// SetAuth sets the credential header for key; an empty key sets nothing.
func SetAuth(h http.Header, baseURL, explicit, key string) {
	if key == "" {
		return
	}
	if AuthHeader(baseURL, explicit) == AuthAPIKey {
		h.Set("X-Api-Key", key)
	} else {
		h.Set("Authorization", "Bearer "+key)
	}
}
