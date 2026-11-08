package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/kinginu/mado/internal/oai"
	"github.com/kinginu/mado/internal/proto"
)

// withTotal bounds a request by d; 0 means no bound beyond the caller's context.
func withTotal(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

// fbTimer cancels a request when no body byte arrived within its duration.
type fbTimer struct {
	t     *time.Timer
	flag  atomic.Bool
	got   atomic.Bool
	limit time.Duration
}

func startFirstByte(d time.Duration, cancel context.CancelFunc) *fbTimer {
	f := &fbTimer{limit: d}
	if d > 0 {
		f.t = time.AfterFunc(d, func() {
			if !f.got.Load() {
				f.flag.Store(true)
				cancel()
			}
		})
	}
	return f
}

func (f *fbTimer) stop() {
	if f.t != nil {
		f.t.Stop()
	}
}

func (f *fbTimer) fired() bool { return f.flag.Load() }

func (f *fbTimer) body(rc io.ReadCloser) io.ReadCloser { return &fbBody{rc, f} }

type fbBody struct {
	io.ReadCloser
	f *fbTimer
}

func (b *fbBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && !b.f.got.Load() {
		b.f.got.Store(true)
		b.f.stop()
	}
	if err != nil && !b.f.got.Load() && b.f.fired() {
		return n, fmt.Errorf("upstream: no response within %s", b.f.limit)
	}
	return n, err
}

func (b *fbBody) Close() error { b.f.stop(); return b.ReadCloser.Close() }

// serveOpenAI answers one request from an OpenAI-compatible upstream (DESIGN §22.3):
// the Messages body is translated to chat completions, the answer back to Anthropic
// SSE or JSON. count_tokens is estimated here and never reaches the upstream.
func (r *Router) serveOpenAI(w http.ResponseWriter, req *http.Request, rt *route, model string, up proto.Upstream, body []byte) {
	if req.URL.Path == "/v1/messages/count_tokens" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": oai.EstimateTokens(req.Context(), body)})
		return
	}
	out, err := oai.TranslateRequest(req.Context(), body, up.Model)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var flags struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &flags)
	fb, total, _ := proto.ParseTimeouts(up.TimeoutFirstByte, up.TimeoutTotal)
	if fb == 0 {
		fb = proto.DefaultTimeoutFirstByte
	}
	if total == 0 {
		total = proto.DefaultTimeoutTotal
	}
	if !flags.Stream {
		fb = 0 // a non-streaming answer has no early bytes; only the total applies
	}
	ctx, cancel := context.WithTimeout(req.Context(), total)
	defer cancel()
	timer := startFirstByte(fb, cancel)
	defer timer.stop()
	ureq, err := http.NewRequestWithContext(ctx, http.MethodPost, oai.ChatURL(up.BaseURL), bytes.NewReader(out))
	if err != nil {
		apiError(w, http.StatusBadGateway, "api_error", "bad upstream url")
		return
	}
	ureq.Header.Set("Content-Type", "application/json")
	if flags.Stream {
		ureq.Header.Set("Accept", "text/event-stream")
	} else {
		ureq.Header.Set("Accept", "application/json")
	}
	oai.SetAuth(ureq.Header, up.Key)
	resp, err := r.client.Do(ureq)
	if err != nil {
		switch {
		case timer.fired() && req.Context().Err() == nil:
			apiError(w, http.StatusGatewayTimeout, "api_error", fmt.Sprintf("upstream: no response within %s", fb))
		case errors.Is(err, context.DeadlineExceeded) && req.Context().Err() == nil:
			apiError(w, http.StatusGatewayTimeout, "api_error", fmt.Sprintf("upstream: no answer within %s", total))
		case req.Context().Err() != nil:
		default:
			r.log.Warn("router: upstream error", "session", rt.sessionID, "model", model, "err", err)
			apiError(w, http.StatusBadGateway, "api_error", "upstream unreachable")
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(oai.ErrorBody(resp.StatusCode, b))
		return
	}
	if !flags.Stream {
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err != nil {
			apiError(w, http.StatusBadGateway, "api_error", "upstream answer broken")
			return
		}
		msg, err := oai.TranslateResponse(b, up.Model)
		if err != nil {
			apiError(w, http.StatusBadGateway, "api_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(msg)
		return
	}
	rc := http.NewResponseController(w)
	started := false
	emit := func(ev string, data []byte) error {
		if !started {
			if ev == "error" {
				// Nothing was sent yet: answer with an HTTP error, which Claude Code retries.
				status := http.StatusBadGateway
				if timer.fired() {
					status = http.StatusGatewayTimeout
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, err := w.Write(data)
				return err
			}
			started = true
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
		}
		if err := oai.WriteSSE(w, ev, data); err != nil {
			return err
		}
		return rc.Flush()
	}
	if err := oai.Stream(req.Context(), timer.body(resp.Body), up.Model, emit); err != nil && req.Context().Err() == nil {
		r.log.Warn("router: openai stream", "session", rt.sessionID, "model", model, "err", err)
	}
}
