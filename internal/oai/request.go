// Package oai translates between the Anthropic Messages API and OpenAI-compatible
// chat completions (DESIGN §22.2), so that a vLLM, Ollama, LM Studio or llama.cpp
// server can stand behind the router (claude traffic) and behind internal/messages
// (chat, the concierge, the classifier). It works on JSON bytes and imports neither of
// its two users.
package oai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/kinginu/mado/internal/files"
)

// ChatURL is the chat-completions endpoint under an API root such as
// http://127.0.0.1:8000/v1.
func ChatURL(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/chat/completions"
}

// SetAuth sets Authorization: Bearer when there is a key (local servers have none).
func SetAuth(h http.Header, key string) {
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
}

// anthropic-side request, loosely typed: only what the translation reads.
type aRequest struct {
	Model         string          `json:"model"`
	MaxTokens     *int            `json:"max_tokens"`
	System        json.RawMessage `json:"system"`
	Messages      []aMessage      `json:"messages"`
	Tools         []aTool         `json:"tools"`
	ToolChoice    *aToolChoice    `json:"tool_choice"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	StopSequences []string        `json:"stop_sequences"`
	Metadata      *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
	Stream bool `json:"stream"`
}

type aMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or blocks
}

type aBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Source    *aSource        `json:"source"`
	Title     string          `json:"title"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result: string or blocks
	IsError   bool            `json:"is_error"`
}

type aSource struct {
	Type      string          `json:"type"` // base64 | url | text | content
	MediaType string          `json:"media_type"`
	Data      string          `json:"data"`
	URL       string          `json:"url"`
	Content   json.RawMessage `json:"content"`
}

type aTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type aToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
}

// OpenAI-side request.
type oRequest struct {
	Model             string          `json:"model"`
	Messages          []oMessage      `json:"messages"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	Stop              []string        `json:"stop,omitempty"`
	User              string          `json:"user,omitempty"`
	Tools             []oTool         `json:"tools,omitempty"`
	ToolChoice        any             `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	StreamOptions     *oStreamOptions `json:"stream_options,omitempty"`
}

type oStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oMessage struct {
	Role       string      `json:"role"`
	Content    any         `json:"content"` // string, []oPart, or nil (assistant with tool calls)
	ToolCalls  []oToolCall `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
}

type oPart struct {
	Type     string     `json:"type"` // text | image_url
	Text     string     `json:"text,omitempty"`
	ImageURL *oImageURL `json:"image_url,omitempty"`
}

type oImageURL struct {
	URL string `json:"url"`
}

type oToolCall struct {
	ID       string    `json:"id"`
	Type     string    `json:"type"`
	Function oFunction `json:"function"`
}

type oFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oTool struct {
	Type     string  `json:"type"`
	Function oToolFn `json:"function"`
}

type oToolFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ImageOmitted replaces an image inside a tool result (chat completions has no image
// parts in tool messages).
const ImageOmitted = "[image omitted]"

// TranslateRequest turns an Anthropic Messages request body into a chat-completions
// body. model, when not empty, replaces the body's model (the router's model swap).
// Fields chat completions has no counterpart for (cache_control, thinking,
// context_management, top_k, ...) are dropped.
func TranslateRequest(ctx context.Context, body []byte, model string) ([]byte, error) {
	var a aRequest
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("request is not a Messages API body: %v", err)
	}
	o := oRequest{Model: a.Model, MaxTokens: a.MaxTokens, Temperature: a.Temperature, TopP: a.TopP,
		Stop: a.StopSequences, Stream: a.Stream}
	if model != "" {
		o.Model = model
	}
	if a.Metadata != nil {
		o.User = a.Metadata.UserID
	}
	if a.Stream {
		o.StreamOptions = &oStreamOptions{IncludeUsage: true}
	}
	if sys := systemText(a.System); sys != "" {
		o.Messages = append(o.Messages, oMessage{Role: "system", Content: sys})
	}
	for _, m := range a.Messages {
		ms, err := translateMessage(ctx, m)
		if err != nil {
			return nil, err
		}
		o.Messages = append(o.Messages, ms...)
	}
	for _, t := range a.Tools {
		if len(t.InputSchema) == 0 || t.Name == "" {
			continue // an Anthropic-defined server tool; nothing to call
		}
		o.Tools = append(o.Tools, oTool{Type: "function", Function: oToolFn{Name: t.Name, Description: t.Description,
			Parameters: t.InputSchema}})
	}
	if tc := a.ToolChoice; tc != nil && len(o.Tools) > 0 {
		switch tc.Type {
		case "auto":
			o.ToolChoice = "auto"
		case "any":
			o.ToolChoice = "required"
		case "none":
			o.ToolChoice = "none"
		case "tool":
			o.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
		}
		if tc.DisableParallelToolUse {
			f := false
			o.ParallelToolCalls = &f
		}
	}
	return marshal(o)
}

// systemText joins a string system or its text blocks.
func systemText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var bs []aBlock
	if json.Unmarshal(raw, &bs) != nil {
		return ""
	}
	var parts []string
	for _, b := range bs {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func blocksOf(raw json.RawMessage) ([]aBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []aBlock{{Type: "text", Text: s}}, nil
	}
	var bs []aBlock
	if err := json.Unmarshal(raw, &bs); err != nil {
		return nil, fmt.Errorf("message content is neither a string nor blocks")
	}
	return bs, nil
}

func translateMessage(ctx context.Context, m aMessage) ([]oMessage, error) {
	bs, err := blocksOf(m.Content)
	if err != nil {
		return nil, err
	}
	if m.Role == "assistant" {
		var text []string
		var calls []oToolCall
		for _, b := range bs {
			switch b.Type {
			case "text":
				if b.Text != "" {
					text = append(text, b.Text)
				}
			case "tool_use":
				args := compact(b.Input)
				calls = append(calls, oToolCall{ID: b.ID, Type: "function", Function: oFunction{Name: b.Name, Arguments: args}})
			}
			// thinking and redacted_thinking are dropped on the way up
		}
		om := oMessage{Role: "assistant", ToolCalls: calls}
		if len(text) > 0 || len(calls) == 0 {
			om.Content = strings.Join(text, "\n\n")
		}
		return []oMessage{om}, nil
	}
	// user: tool results become tool messages first, the rest one user message after them
	var out []oMessage
	var parts []oPart
	hasImage := false
	for _, b := range bs {
		switch b.Type {
		case "tool_result":
			out = append(out, oMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: toolResultText(ctx, b)})
		case "text":
			if b.Text != "" {
				parts = append(parts, oPart{Type: "text", Text: b.Text})
			}
		case "image":
			if u := imageURL(b.Source); u != "" {
				parts = append(parts, oPart{Type: "image_url", ImageURL: &oImageURL{URL: u}})
				hasImage = true
			}
		case "document":
			if t := documentText(ctx, b); t != "" {
				parts = append(parts, oPart{Type: "text", Text: t})
			}
		}
	}
	if len(parts) > 0 || len(out) == 0 {
		if hasImage {
			out = append(out, oMessage{Role: "user", Content: parts})
		} else {
			texts := make([]string, 0, len(parts))
			for _, p := range parts {
				texts = append(texts, p.Text)
			}
			out = append(out, oMessage{Role: "user", Content: strings.Join(texts, "\n\n")})
		}
	}
	return out, nil
}

func imageURL(s *aSource) string {
	if s == nil {
		return ""
	}
	switch s.Type {
	case "base64":
		return "data:" + s.MediaType + ";base64," + s.Data
	case "url":
		return s.URL
	}
	return ""
}

// toolResultText flattens a tool_result's content; images become ImageOmitted.
// is_error results get an "Error: " prefix, the only trace of the flag that chat
// completions can carry.
func toolResultText(ctx context.Context, b aBlock) string {
	bs, err := blocksOf(b.Content)
	var parts []string
	if err == nil {
		for _, c := range bs {
			switch c.Type {
			case "text":
				parts = append(parts, c.Text)
			case "image":
				parts = append(parts, ImageOmitted)
			case "document":
				if t := documentText(ctx, c); t != "" {
					parts = append(parts, t)
				}
			}
		}
	}
	s := strings.Join(parts, "\n")
	if b.IsError {
		s = "Error: " + s
	}
	return s
}

// documentText is a document block's text: a text or content source as is, a base64
// PDF through files.PDFText, anything else a placeholder.
func documentText(ctx context.Context, b aBlock) string {
	var text string
	if s := b.Source; s != nil {
		switch {
		case s.Type == "text":
			text = s.Data
		case s.Type == "content":
			if bs, err := blocksOf(s.Content); err == nil {
				var parts []string
				for _, c := range bs {
					if c.Type == "text" {
						parts = append(parts, c.Text)
					}
				}
				text = strings.Join(parts, "\n")
			}
		case s.Type == "base64" && s.MediaType == "application/pdf":
			text = pdfText(ctx, s.Data)
		case s.Type == "url":
			text = "[document: " + s.URL + "]"
		}
	}
	if text == "" {
		text = "[document omitted]"
	}
	if b.Title != "" {
		return fmt.Sprintf("<document name=%q>\n%s\n</document>", b.Title, text)
	}
	return text
}

func pdfText(ctx context.Context, b64 string) string {
	data, err := decodeBase64(b64)
	if err != nil {
		return ""
	}
	f, err := os.CreateTemp("", "mado-oai-*.pdf")
	if err != nil {
		return ""
	}
	defer os.Remove(f.Name())
	_, werr := f.Write(data)
	if cerr := f.Close(); werr != nil || cerr != nil {
		return ""
	}
	t, err := files.PDFText(ctx, f.Name())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(t)
}

// EstimateTokens is the count_tokens answer for an openai upstream: bytes/4 of the
// translated request (DESIGN §22.2), or of the body itself when it does not translate.
func EstimateTokens(ctx context.Context, body []byte) int {
	n := len(body)
	if out, err := TranslateRequest(ctx, body, ""); err == nil {
		n = len(out)
	}
	return max(n/4, 1)
}

// marshal is json.Marshal without HTML escaping: the prompt reaches the model as
// written.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// compact is a tool_use input as a compact JSON string.
func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil || b.Len() == 0 || b.String() == "null" {
		return "{}"
	}
	return b.String()
}

func decodeBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
}
