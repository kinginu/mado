// Package chat runs plain chat sessions on the hub (DESIGN §17.3): no worker, no
// tmux, no claude process. A runner calls a Messages API upstream directly and stores
// every turn in the Claude Code stream-json shape, so the hub's renderer and usage
// accounting treat chat and code sessions alike.
package chat

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/kinginu/mado/internal/messages"
	"github.com/kinginu/mado/internal/proto"
)

// Model is one [[chat.models]] entry (DESIGN §17.1). Key is never stored: the hub
// resolves key_ref from its secrets file when it calls the upstream.
type Model struct {
	Name          string `json:"name"`
	Label         string `json:"label,omitempty"`
	BaseURL       string `json:"base_url"`
	Model         string `json:"model"`
	KeyRef        string `json:"key_ref,omitempty"`
	AuthHeader    string `json:"auth_header,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	MaxOutput     int    `json:"max_output,omitempty"`
	Vision        bool   `json:"vision,omitempty"`
	// Documents: native `document` (PDF) blocks. nil means the same as Vision (on
	// AI& a model that sees images also takes PDFs, which it renders as images).
	Documents *bool `json:"documents,omitempty"`
	Default   bool  `json:"default,omitempty"`
	// Kind is "" or "anthropic" (Messages API) or "openai" (chat completions,
	// DESIGN §22). The timeouts are Go durations; "" = 60s and 10m.
	Kind             string `json:"kind,omitempty"`
	TimeoutFirstByte string `json:"timeout_first_byte,omitempty"`
	TimeoutTotal     string `json:"timeout_total,omitempty"`
}

// Upstream is where the model's requests go; key is the resolved key_ref.
func (m Model) Upstream(key string) messages.Upstream {
	fb, total, _ := proto.ParseTimeouts(m.TimeoutFirstByte, m.TimeoutTotal) // checked by Normalize
	return messages.Upstream{BaseURL: m.BaseURL, Key: key, AuthHeader: m.AuthHeader, Kind: m.Kind,
		FirstByte: fb, Total: total}
}

// Defaults for omitted fields. A small max_tokens makes AI&'s reasoning models put
// their hidden reasoning into the answer, so the default output budget is large.
const (
	DefaultContextWindow = 131072
	DefaultMaxOutput     = 32000
)

// Normalize applies the defaults and checks the entry.
func (m *Model) Normalize() error {
	if !ValidModelName(m.Name) {
		return fmt.Errorf("chat model name %q: use 1-64 of [A-Za-z0-9._-]", m.Name)
	}
	if m.Label == "" {
		m.Label = m.Name
	}
	if len(m.Label) > 80 {
		return fmt.Errorf("chat model %s: label too long", m.Name)
	}
	pu, err := url.Parse(m.BaseURL)
	if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" || pu.User != nil {
		return fmt.Errorf("chat model %s: base_url must be http(s)://host[:port][/path]", m.Name)
	}
	if m.Model == "" || len(m.Model) > 200 {
		return fmt.Errorf("chat model %s: model is required", m.Name)
	}
	if m.AuthHeader != "" && m.AuthHeader != messages.AuthBearer && m.AuthHeader != messages.AuthAPIKey {
		return fmt.Errorf("chat model %s: auth_header must be bearer or x-api-key", m.Name)
	}
	if !proto.ValidUpstreamKind(m.Kind) {
		return fmt.Errorf("chat model %s: kind must be anthropic or openai", m.Name)
	}
	if _, _, err := proto.ParseTimeouts(m.TimeoutFirstByte, m.TimeoutTotal); err != nil {
		return fmt.Errorf("chat model %s: %v", m.Name, err)
	}
	if m.Kind == proto.UpstreamOpenAI {
		if m.AuthHeader == messages.AuthAPIKey {
			return fmt.Errorf("chat model %s: an openai model sends its key as Authorization: Bearer", m.Name)
		}
		// chat completions has no document parts: PDFs go as extracted text
		if m.Documents != nil && *m.Documents {
			return fmt.Errorf("chat model %s: documents = true needs an anthropic model", m.Name)
		}
		if m.Documents == nil {
			f := false
			m.Documents = &f
		}
	}
	if m.ContextWindow == 0 {
		m.ContextWindow = DefaultContextWindow
	}
	if m.MaxOutput == 0 {
		m.MaxOutput = DefaultMaxOutput
	}
	if m.ContextWindow < 1000 || m.ContextWindow > 10_000_000 {
		return fmt.Errorf("chat model %s: context_window must be 1000..10000000", m.Name)
	}
	if m.MaxOutput < 1 || m.MaxOutput > 1_000_000 {
		return fmt.Errorf("chat model %s: max_output must be 1..1000000", m.Name)
	}
	if m.MaxOutput+trimMargin >= m.ContextWindow {
		return fmt.Errorf("chat model %s: max_output must leave room in context_window", m.Name)
	}
	if m.Documents == nil {
		v := m.Vision
		m.Documents = &v
	}
	return nil
}

// NativeDocuments reports whether PDFs go to the model as document blocks.
func (m Model) NativeDocuments() bool {
	if m.Documents == nil {
		return m.Vision
	}
	return *m.Documents
}

// ValidModelName checks a [[chat.models]] name: 1-64 of [A-Za-z0-9._-].
func ValidModelName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// ValidateModels normalizes a list and checks names are unique and at most one is
// the default.
func ValidateModels(ms []Model) error {
	seen := map[string]bool{}
	defaults := 0
	for i := range ms {
		if err := ms[i].Normalize(); err != nil {
			return err
		}
		if seen[ms[i].Name] {
			return fmt.Errorf("chat model %s defined twice", ms[i].Name)
		}
		seen[ms[i].Name] = true
		if ms[i].Default {
			defaults++
		}
	}
	if defaults > 1 {
		return errors.New("more than one chat model has default = true")
	}
	return nil
}
