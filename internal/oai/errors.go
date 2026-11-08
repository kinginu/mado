package oai

import (
	"encoding/json"
	"net/http"
	"strings"
)

// APIError is an upstream error in Anthropic terms.
type APIError struct {
	Status  int
	Type    string
	Message string
}

// ErrorType is the Anthropic error type for an HTTP status.
func ErrorType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusServiceUnavailable || status == 529:
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// parseError reads the error shapes OpenAI-compatible servers use:
// {"error":{"message","type","code"}} (OpenAI, Ollama, llama.cpp), {"error":"..."},
// {"object":"error","message":...} (vLLM), {"detail":...} (FastAPI) or plain text.
// The type is always derived from the status: OpenAI types (BadRequestError,
// invalid_request_error, server_error) do not map onto Anthropic ones.
func parseError(status int, b []byte) APIError {
	e := APIError{Status: status, Type: ErrorType(status)}
	var v struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(b, &v) == nil {
		var inner struct {
			Message string `json:"message"`
		}
		var s string
		switch {
		case json.Unmarshal(v.Error, &inner) == nil && inner.Message != "":
			e.Message = inner.Message
		case json.Unmarshal(v.Error, &s) == nil && s != "":
			e.Message = s
		case v.Message != "":
			e.Message = v.Message
		case json.Unmarshal(v.Detail, &s) == nil && s != "":
			e.Message = s
		case len(v.Detail) > 0:
			e.Message = string(v.Detail)
		}
	} else {
		e.Message = strings.TrimSpace(string(b))
	}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	if len(e.Message) > 2000 {
		e.Message = e.Message[:2000] + "…"
	}
	// Claude Code compacts on "prompt is too long"; vLLM says "maximum context length".
	if status == http.StatusBadRequest && isOverflow(e.Message) {
		e.Message = "prompt is too long: " + e.Message
	}
	return e
}

func isOverflow(m string) bool {
	l := strings.ToLower(m)
	if strings.HasPrefix(l, "prompt is too long") {
		return false
	}
	return strings.Contains(l, "maximum context length") || strings.Contains(l, "context length") ||
		strings.Contains(l, "context window") || strings.Contains(l, "exceeds the available context size")
}

// ErrorBody turns a non-2xx chat-completions answer into an Anthropic error body;
// the caller keeps the status.
func ErrorBody(status int, b []byte) []byte {
	e := parseError(status, b)
	return AnthropicError(e.Type, e.Message)
}

// AnthropicError is {"type":"error","error":{"type":...,"message":...}}.
func AnthropicError(typ, msg string) []byte {
	out, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
	return out
}
