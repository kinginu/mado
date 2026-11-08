package messages

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kinginu/mado/internal/oai"
	"github.com/kinginu/mado/internal/proto"
)

// Upstream is where a request goes. Key is the resolved secret; it is sent in a
// header and never logged or put into an error.
type Upstream struct {
	BaseURL    string
	Key        string
	AuthHeader string // "", bearer, x-api-key
	// Kind "openai" sends the request as chat completions through internal/oai and
	// translates the answer back (DESIGN §22.3); "" or "anthropic" is the Messages API.
	Kind string
	// FirstByte and Total override the client's timeouts when > 0.
	FirstByte time.Duration
	Total     time.Duration
}

// Event is one parsed stream event. Type is the SSE event type
// (message_start, content_block_start, content_block_delta, content_block_stop,
// message_delta, message_stop, error). The client adds no types of its own:
// message_stop carries the assembled Response, and a failure (an `error` event, a
// broken stream, a cancelled context) arrives as Type "error" with Err set and the
// partial Response assembled so far.
type Event struct {
	Type     string
	Index    int
	Block    *Block    // content_block_start
	Delta    *Delta    // content_block_delta, message_delta
	Usage    *Usage    // message_delta: the usage merged so far
	Response *Response // message_start (as sent), message_stop and error (assembled)
	Err      error     // error
}

// Delta is a content_block_delta or message_delta payload.
type Delta struct {
	Type         string `json:"type"` // text_delta, input_json_delta, thinking_delta, signature_delta
	Text         string `json:"text"`
	PartialJSON  string `json:"partial_json"`
	Thinking     string `json:"thinking"`
	Signature    string `json:"signature"`
	StopReason   string `json:"stop_reason"`
	StopSequence string `json:"stop_sequence"`
}

// APIError is an error answer from the upstream, either as an HTTP status or as a
// stream `error` event (Status 0).
type APIError struct {
	Status  int
	Type    string
	Message string
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("upstream")
	if e.Status != 0 {
		fmt.Fprintf(&b, " HTTP %d", e.Status)
	}
	if e.Type != "" {
		b.WriteString(" " + e.Type)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// IsContextOverflow reports an upstream 400 for a prompt longer than the context
// window. AI& and Anthropic both start the message with "prompt is too long"; a few
// servers say "context length" instead.
func IsContextOverflow(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
		return false
	}
	m := strings.ToLower(ae.Message)
	return strings.HasPrefix(m, "prompt is too long") || strings.Contains(m, "context length") ||
		strings.Contains(m, "context window")
}

// Client sends requests. The zero value is not usable; use NewClient.
type Client struct {
	HTTP *http.Client
	// FirstByte bounds the wait for the response headers of one attempt.
	FirstByte time.Duration
	// Total bounds the whole call, retries and streaming included.
	Total time.Duration
	// Backoff is the wait before each retry; len(Backoff)+1 attempts in all.
	Backoff []time.Duration
}

// NewClient has the DESIGN §22.1 defaults: 60 s to the first body byte, 10 min total, three
// attempts with 1 s and 2 s between them (a fourth wait of 4 s is kept for callers
// that raise the attempt count).
func NewClient() *Client {
	tr := &http.Transport{
		Proxy:               nil, // model traffic never goes through an env proxy
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &Client{HTTP: &http.Client{Transport: tr}, FirstByte: 60 * time.Second, Total: 10 * time.Minute,
		Backoff: []time.Duration{1 * time.Second, 2 * time.Second}}
}

const maxErrorBody = 64 << 10

// parseErrorBody reads both the Anthropic shape {"type":"error","error":{...}} and the
// OpenAI shape {"error":{...}} or {"message":...} some servers use for 404s.
func parseErrorBody(status int, b []byte) *APIError {
	e := &APIError{Status: status}
	var v struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
		Msg   string          `json:"message"`
	}
	if json.Unmarshal(b, &v) == nil {
		var inner struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		var s string
		switch {
		case json.Unmarshal(v.Error, &inner) == nil && (inner.Type != "" || inner.Message != ""):
			e.Type, e.Message = inner.Type, inner.Message
		case json.Unmarshal(v.Error, &s) == nil:
			e.Message = s
		default:
			e.Message = v.Msg
		}
	} else {
		e.Message = strings.TrimSpace(string(b))
	}
	if len(e.Message) > 2000 {
		e.Message = e.Message[:2000] + "…"
	}
	return e
}

func retryable(status int) bool { return status == http.StatusTooManyRequests || status >= 500 }

// retryableErr: 429 and 5xx answers, overload errors in the stream, and transport
// failures (the request may not have reached the model).
func retryableErr(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		if ae.Status != 0 {
			return retryable(ae.Status)
		}
		return ae.Type == "overloaded_error" || ae.Type == "api_error" || ae.Type == "rate_limit_error"
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// Stream sends req with stream:true and returns its events. A failure before the
// stream starts (after the retries) is returned as the error; anything later arrives
// as an `error` event. The channel is closed after message_stop or error.
//
// Retries: 429, 5xx and transport errors, and a stream that fails before its first
// content block, up to len(Backoff)+1 attempts. A stream that produced a block is
// never retried; events before the first block are held back so a retried attempt
// does not show twice.
func (c *Client) Stream(ctx context.Context, up Upstream, req Request) (<-chan Event, error) {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if up.Kind == proto.UpstreamOpenAI {
		if body, err = oai.TranslateRequest(ctx, body, ""); err != nil {
			return nil, err
		}
	}
	total, firstByte := c.Total, c.FirstByte
	if up.Total > 0 {
		total = up.Total
	}
	if up.FirstByte > 0 {
		firstByte = up.FirstByte
	}
	ctx, cancel := context.WithTimeout(ctx, total)
	out := make(chan Event, 64)
	first := make(chan error, 1)
	go func() {
		defer cancel()
		defer close(out)
		started := false // the caller has its channel
		var lastErr error
		for attempt := 0; ; attempt++ {
			if attempt > 0 {
				select {
				case <-time.After(c.Backoff[attempt-1]):
				case <-ctx.Done():
				}
				if ctx.Err() != nil {
					lastErr = ctx.Err()
					break
				}
			}
			resp, err := c.do(ctx, up, body, firstByte, req.Model)
			if err == nil {
				if !started {
					started = true
					first <- nil
				}
				var produced bool
				produced, err = parse(ctx, resp.Body, out)
				resp.Body.Close()
				if err == nil || produced {
					return // done, or parse reported the error itself
				}
			}
			lastErr = err
			if ctx.Err() != nil || !retryableErr(err) || attempt == len(c.Backoff) {
				break
			}
		}
		if !started {
			first <- lastErr
			return
		}
		out <- Event{Type: "error", Err: lastErr, Response: &Response{Role: "assistant"}}
	}()
	if err := <-first; err != nil {
		for range out {
		}
		return nil, err
	}
	return out, nil
}

func (c *Client) do(ctx context.Context, up Upstream, body []byte, firstByte time.Duration, model string) (*http.Response, error) {
	openai := up.Kind == proto.UpstreamOpenAI
	target := strings.TrimRight(up.BaseURL, "/") + "/v1/messages"
	if openai {
		target = oai.ChatURL(up.BaseURL)
	}
	actx, acancel := context.WithCancel(ctx)
	hreq, err := http.NewRequestWithContext(actx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		acancel()
		return nil, errors.New("bad upstream url")
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if openai {
		oai.SetAuth(hreq.Header, up.Key)
	} else {
		hreq.Header.Set("anthropic-version", Version)
		SetAuth(hreq.Header, up.BaseURL, up.AuthHeader, up.Key)
	}
	// The first-byte timer runs until the first byte of the body: an OpenAI-compatible
	// server sends its headers at once and prefills afterwards.
	var fired atomic.Bool
	t := time.AfterFunc(firstByte, func() { fired.Store(true); acancel() })
	noResponse := fmt.Errorf("upstream: no response within %s", firstByte)
	resp, err := c.HTTP.Do(hreq)
	if err != nil {
		t.Stop()
		acancel()
		if fired.Load() && ctx.Err() == nil {
			return nil, noResponse
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The url error names the host but never the key (it is in a header).
		return nil, fmt.Errorf("upstream unreachable: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Stop()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		resp.Body.Close()
		acancel()
		if openai {
			b = oai.ErrorBody(resp.StatusCode, b)
		}
		return nil, parseErrorBody(resp.StatusCode, b)
	}
	var rc io.ReadCloser = &firstByteBody{ReadCloser: resp.Body, timer: t, fired: &fired, err: noResponse}
	if openai {
		rc = translated(ctx, rc, model)
	}
	resp.Body = cancelOnClose{rc, acancel}
	return resp, nil
}

// firstByteBody stops the first-byte timer at the first byte read, and reports a
// fired timer as a no-response error rather than a cancellation.
type firstByteBody struct {
	io.ReadCloser
	timer *time.Timer
	fired *atomic.Bool
	err   error
	got   bool
}

func (f *firstByteBody) Read(p []byte) (int, error) {
	n, err := f.ReadCloser.Read(p)
	if n > 0 && !f.got {
		f.got = true
		f.timer.Stop()
	}
	if err != nil && !f.got && f.fired.Load() {
		return n, f.err
	}
	return n, err
}

func (f *firstByteBody) Close() error { f.timer.Stop(); return f.ReadCloser.Close() }

// translated turns a chat-completions SSE body into Anthropic SSE bytes; the message
// is named after the requested model, which usage and prices key on.
func translated(ctx context.Context, body io.ReadCloser, model string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		err := oai.Stream(ctx, body, model, func(ev string, data []byte) error { return oai.WriteSSE(pw, ev, data) })
		pw.CloseWithError(err)
	}()
	return pipeBody{pr, body}
}

type pipeBody struct {
	*io.PipeReader
	upstream io.ReadCloser
}

func (p pipeBody) Close() error {
	p.PipeReader.Close()
	return p.upstream.Close()
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error { err := c.ReadCloser.Close(); c.cancel(); return err }

// assembler builds the Response from the events.
type assembler struct {
	resp    Response
	partial map[int]*strings.Builder // input_json_delta per block index
	pos     map[int]int              // block index -> position in resp.Content
	// usage fields message_delta has carried so far (AI& sends a wrong input count in
	// message_start; the message_delta values win, message_start fills the gaps).
	deltaSet map[string]bool
}

func (a *assembler) finish() *Response {
	r := a.resp
	r.Content = append([]Block(nil), a.resp.Content...)
	for i, p := range a.pos {
		if sb := a.partial[i]; sb != nil && r.Content[p].Type == "tool_use" {
			if js := sb.String(); json.Valid([]byte(js)) {
				r.Content[p].Input = json.RawMessage(js)
			}
		}
		if r.Content[p].Type == "tool_use" && len(r.Content[p].Input) == 0 {
			r.Content[p].Input = json.RawMessage("{}")
		}
	}
	if r.Role == "" {
		r.Role = "assistant"
	}
	return &r
}

type rawUsage struct {
	Input      *int64 `json:"input_tokens"`
	Output     *int64 `json:"output_tokens"`
	CacheWrite *int64 `json:"cache_creation_input_tokens"`
	CacheRead  *int64 `json:"cache_read_input_tokens"`
}

func (a *assembler) mergeUsage(u *rawUsage, fromDelta bool) {
	if u == nil {
		return
	}
	set := func(name string, dst *int64, v *int64) {
		if v == nil {
			return
		}
		if fromDelta {
			*dst = *v
			a.deltaSet[name] = true
		} else if !a.deltaSet[name] {
			*dst = *v
		}
	}
	set("input", &a.resp.Usage.InputTokens, u.Input)
	set("output", &a.resp.Usage.OutputTokens, u.Output)
	set("cache_write", &a.resp.Usage.CacheCreationInputTokens, u.CacheWrite)
	set("cache_read", &a.resp.Usage.CacheReadInputTokens, u.CacheRead)
}

// parse reads one SSE stream. It reports whether a content block was produced. Events
// before the first block are held back; an error before it is returned without being
// sent, so the caller may retry.
func parse(ctx context.Context, r io.Reader, out chan<- Event) (produced bool, err error) {
	a := &assembler{partial: map[int]*strings.Builder{}, pos: map[int]int{}, deltaSet: map[string]bool{}}
	var held []Event
	send := func(e Event) bool {
		if !produced {
			held = append(held, e)
			return true
		}
		select {
		case out <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}
	fail := func(err error) (bool, error) {
		if !produced {
			return false, err
		}
		select {
		case out <- Event{Type: "error", Err: err, Response: a.finish()}:
		case <-time.After(5 * time.Second): // a consumer that left
		}
		return true, err
	}
	br := bufio.NewReaderSize(r, 64<<10)
	var data bytes.Buffer
	evType := ""
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 16<<20 {
			return fail(errors.New("upstream: SSE line too long"))
		}
		line = bytes.TrimRight(line, "\r\n")
		switch {
		case len(line) == 0 && (data.Len() > 0 || evType != ""):
			stop, perr := a.dispatch(evType, data.Bytes(), send, &produced, &held, out, ctx)
			data.Reset()
			evType = ""
			if perr != nil {
				return fail(perr)
			}
			if stop {
				return produced, nil
			}
		case bytes.HasPrefix(line, []byte("event:")):
			evType = string(bytes.TrimSpace(line[6:]))
		case bytes.HasPrefix(line, []byte("data:")):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(line[5:], []byte(" ")))
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			if data.Len() > 0 { // last event without a trailing blank line
				stop, perr := a.dispatch(evType, data.Bytes(), send, &produced, &held, out, ctx)
				if perr != nil {
					return fail(perr)
				}
				if stop {
					return produced, nil
				}
			}
			if rerr == io.EOF {
				return fail(errors.New("upstream: stream ended before message_stop"))
			}
			return fail(fmt.Errorf("upstream: stream broken: %w", rerr))
		}
	}
}

type wire struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	Message      json.RawMessage `json:"message"`
	ContentBlock *Block          `json:"content_block"`
	Delta        json.RawMessage `json:"delta"`
	Usage        *rawUsage       `json:"usage"`
	Error        json.RawMessage `json:"error"`
}

// dispatch handles one SSE event; stop is true after message_stop.
func (a *assembler) dispatch(evType string, data []byte, send func(Event) bool, produced *bool, held *[]Event, out chan<- Event, ctx context.Context) (stop bool, err error) {
	var w wire
	if len(data) == 0 || string(data) == "[DONE]" {
		return false, nil
	}
	if json.Unmarshal(data, &w) != nil {
		return false, fmt.Errorf("upstream: bad SSE data for %q", evType)
	}
	if w.Type == "" {
		w.Type = evType
	}
	ok := true
	switch w.Type {
	case "ping":
	case "message_start":
		var m struct {
			Response
			Usage *rawUsage `json:"usage"`
		}
		_ = json.Unmarshal(w.Message, &m)
		a.resp.ID, a.resp.Model, a.resp.Role, a.resp.Type = m.ID, m.Model, m.Role, m.Type
		a.mergeUsage(m.Usage, false)
		r := a.finish()
		ok = send(Event{Type: w.Type, Response: r})
	case "content_block_start":
		if w.ContentBlock == nil {
			return false, errors.New("upstream: content_block_start without a block")
		}
		b := *w.ContentBlock
		if b.Type == "tool_use" && string(b.Input) == "{}" {
			b.Input = nil // the input arrives as input_json_delta
		}
		a.pos[w.Index] = len(a.resp.Content)
		a.resp.Content = append(a.resp.Content, b)
		if !*produced {
			*produced = true
			for _, e := range *held {
				select {
				case out <- e:
				case <-ctx.Done():
					return false, ctx.Err()
				}
			}
			*held = nil
		}
		bc := b
		ok = send(Event{Type: w.Type, Index: w.Index, Block: &bc})
	case "content_block_delta":
		var d Delta
		_ = json.Unmarshal(w.Delta, &d)
		p, known := a.pos[w.Index]
		if !known {
			return false, fmt.Errorf("upstream: delta for unknown block %d", w.Index)
		}
		cb := &a.resp.Content[p]
		switch d.Type {
		case "text_delta":
			cb.Text += d.Text
		case "input_json_delta":
			if a.partial[w.Index] == nil {
				a.partial[w.Index] = &strings.Builder{}
			}
			a.partial[w.Index].WriteString(d.PartialJSON)
		case "thinking_delta":
			cb.Thinking += d.Thinking
		case "signature_delta":
			cb.Signature += d.Signature
		}
		ok = send(Event{Type: w.Type, Index: w.Index, Delta: &d})
	case "content_block_stop":
		ok = send(Event{Type: w.Type, Index: w.Index})
	case "message_delta":
		var d Delta
		_ = json.Unmarshal(w.Delta, &d)
		if d.StopReason != "" {
			a.resp.StopReason = d.StopReason
		}
		if d.StopSequence != "" {
			a.resp.StopSequence = d.StopSequence
		}
		a.mergeUsage(w.Usage, true)
		u := a.resp.Usage
		ok = send(Event{Type: w.Type, Delta: &d, Usage: &u})
	case "message_stop":
		if !*produced { // an empty answer is still an answer
			*produced = true
			for _, e := range *held {
				select {
				case out <- e:
				case <-ctx.Done():
					return false, ctx.Err()
				}
			}
			*held = nil
		}
		if !send(Event{Type: w.Type, Response: a.finish()}) {
			return false, ctx.Err()
		}
		return true, nil
	case "error":
		e := parseErrorBody(0, data)
		if e.Type == "" && e.Message == "" {
			e.Message = "stream error"
		}
		return false, e
	}
	if !ok {
		return false, ctx.Err()
	}
	return false, nil
}

// DefaultClient is used by the package-level Stream.
var DefaultClient = NewClient()

// Stream calls DefaultClient.Stream.
func Stream(ctx context.Context, up Upstream, req Request) (<-chan Event, error) {
	return DefaultClient.Stream(ctx, up, req)
}
