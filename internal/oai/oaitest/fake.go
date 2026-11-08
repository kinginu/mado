// Package oaitest is a fake OpenAI-compatible chat-completions server for tests. It
// streams the way vLLM and Ollama do: `data: {...}` lines, usage only in a final chunk
// when stream_options.include_usage is set, then `data: [DONE]`.
package oaitest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Request is one request the server saw.
type Request struct {
	Path   string
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

// Reply is what the server answers. Status != 0 and != 200 sends ErrorBody with that
// status. Otherwise a streaming request gets Chunks (each one `data:` line) plus,
// when asked for, a usage chunk and [DONE]; a non-streaming one gets Message.
type Reply struct {
	Status    int
	ErrorBody string
	Chunks    []string
	Usage     string // the usage object, e.g. {"prompt_tokens":10,"completion_tokens":2}
	NoDone    bool   // end the stream without [DONE]
	Message   string // non-streaming body
	Hold      chan struct{}
}

// Server records requests and answers with Script(n, request), n counting from 1.
type Server struct {
	*httptest.Server
	Script func(n int, r Request) Reply
	mu     sync.Mutex
	reqs   []Request
}

func New(t *testing.T, script func(n int, r Request) Reply) *Server {
	t.Helper()
	s := &Server{Script: script}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Requests returns a copy of the requests so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	req := Request{Path: r.URL.Path, Header: r.Header.Clone(), Raw: raw}
	_ = json.Unmarshal(raw, &req.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	n := len(s.reqs)
	s.mu.Unlock()
	rep := s.Script(n, req)
	if rep.Status != 0 && rep.Status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.Status)
		io.WriteString(w, rep.ErrorBody)
		return
	}
	stream, _ := req.Body["stream"].(bool)
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, rep.Message)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush() // headers first, as vLLM does before the prefill
	}
	if rep.Hold != nil {
		select {
		case <-rep.Hold:
		case <-r.Context().Done():
			return
		}
	}
	for _, c := range rep.Chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
		if fl != nil {
			fl.Flush()
		}
	}
	opts, _ := req.Body["stream_options"].(map[string]any)
	if inc, _ := opts["include_usage"].(bool); inc && rep.Usage != "" {
		fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-fake\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":%s}\n\n", rep.Usage)
	}
	if !rep.NoDone {
		io.WriteString(w, "data: [DONE]\n\n")
	}
}

func chunk(delta string, finish string) string {
	f := "null"
	if finish != "" {
		f = `"` + finish + `"`
	}
	return `{"id":"chatcmpl-fake","object":"chat.completion.chunk","model":"served-model","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + f + `}]}`
}

// Role is the first chunk vLLM sends.
func Role() string { return chunk(`{"role":"assistant","content":""}`, "") }

// Text is a content delta.
func Text(s string) string {
	j, _ := json.Marshal(s)
	return chunk(`{"content":`+string(j)+`}`, "")
}

// Reasoning is a reasoning_content delta.
func Reasoning(s string) string {
	j, _ := json.Marshal(s)
	return chunk(`{"reasoning_content":`+string(j)+`}`, "")
}

// ToolStart opens tool call index with its id and name; ToolArgs adds fragments.
func ToolStart(index int, id, name string) string {
	return chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]}`, index, id, name), "")
}

func ToolArgs(index int, frag string) string {
	j, _ := json.Marshal(frag)
	return chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]}`, index, j), "")
}

// Finish is the finish_reason chunk.
func Finish(reason string) string { return chunk(`{}`, reason) }
