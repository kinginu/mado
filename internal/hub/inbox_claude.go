package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kinginu/mado/internal/inbox"
	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/tools"
)

// The claude digest executor (DESIGN §23.6): a job session on a worker with the
// jobs capability, i.e. one `claude -p` turn on the subscription with no built-in
// tools and propose_task relayed through the mado MCP server.

// Job timing defaults; tests shorten them through the Hub fields.
const (
	jobAckWait  = 60 * time.Second
	jobBackstop = 20 * time.Minute
)

// digestJob is one running job session, known to the frame handlers.
type digestJob struct {
	sessionID, machineID string
	tools                inbox.ToolSet
	ch                   chan jobNote // result event or exit, never blocks the sender
}

// jobNote is what the executor waits for: the result event, or the shim's exit.
type jobNote struct {
	result json.RawMessage
	exited bool
	code   *int
	err    string
}

type jobState struct {
	mu   sync.Mutex
	jobs map[string]*digestJob
	// ackWait and backstop default to jobAckWait and jobBackstop.
	ackWait, backstop time.Duration
}

func (j *jobState) get(sessionID string) *digestJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.jobs[sessionID]
}

func (j *jobState) add(d *digestJob) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.jobs == nil {
		j.jobs = map[string]*digestJob{}
	}
	j.jobs[d.sessionID] = d
}

func (j *jobState) remove(sessionID string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.jobs, sessionID)
}

func (d *digestJob) note(n jobNote) {
	select {
	case d.ch <- n:
	default:
	}
}

// jobEvent tells a running job about a stored event of its session.
func (h *Hub) jobEvent(sessionID string, raw []byte) {
	d := h.jobs.get(sessionID)
	if d == nil {
		return
	}
	var e struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Type == "result" {
		d.note(jobNote{result: append(json.RawMessage(nil), raw...)})
	}
}

// jobState tells a running job that its shim reported an exit.
func (h *Hub) jobExit(m *proto.SessionState) {
	if m.State != proto.StateExited && m.State != proto.StateDetached {
		return
	}
	if d := h.jobs.get(m.SessionID); d != nil {
		d.note(jobNote{exited: true, code: m.ExitCode, err: m.Error})
	}
}

// onJobToolCall runs propose_task for the running digest job of that session, and
// only when the call comes from the job's machine (the caller checked ownership).
func (h *Hub) onJobToolCall(ctx context.Context, wc *workerConn, m *proto.ToolCall) {
	res := &proto.ToolResult{SessionID: m.SessionID, CallID: m.CallID}
	d := h.jobs.get(m.SessionID)
	switch {
	case d == nil || d.machineID != wc.machineID || d.tools == nil:
		res.Content, res.IsError = "mado: "+m.Name+" is only available to a running digest", true
	default:
		tctx, cancel := context.WithTimeout(ctx, codeToolTimeout)
		out, err := d.tools.Run(tctx, m.Name, m.Args)
		cancel()
		switch {
		case err == nil:
			res.Content = out.Text
		case errors.As(err, new(tools.Error)):
			res.Content, res.IsError = "Error: "+err.Error(), true
		default:
			res.Content, res.IsError = "Error: the tool failed", true
		}
	}
	if err := wc.send(ctx, res); err != nil {
		h.log.Warn("tool result not delivered", "session", m.SessionID, "err", err)
	}
}

// jobMachine picks the worker for a job: [inbox.digest] machine when set, else the
// first online worker (by name) that advertised jobs.
func (h *Hub) jobMachine() (id, name, jobDir string, err error) {
	pin := h.cfg.Inbox.Digest.Machine
	ms, err := h.store.Machines()
	if err != nil {
		return "", "", "", err
	}
	slices.SortFunc(ms, func(a, b Machine) int { return strings.Compare(a.Name, b.Name) })
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range ms {
		if pin != "" && m.Name != pin {
			continue
		}
		wc := h.workers[m.ID]
		if wc == nil || !slices.Contains(wc.caps, proto.CapJobs) || wc.jobDir == "" {
			continue
		}
		return m.ID, m.Name, wc.jobDir, nil
	}
	if pin != "" {
		return "", "", "", fmt.Errorf("the pinned machine %q is not online with the jobs capability", pin)
	}
	return "", "", "", errors.New("no online worker has the jobs capability")
}

// claudeDigestExecutor runs a digest as a job session (DESIGN §23.6). Every failure
// before the digest text exists is an inbox.ErrFallback, so the FallbackExecutor
// runs the chat executor; only the run's own context ending is a plain error.
type claudeDigestExecutor struct{ h *Hub }

func (claudeDigestExecutor) Name() string { return ExecutorClaude }

func (e claudeDigestExecutor) Run(ctx context.Context, in inbox.DigestInput) (inbox.DigestResult, error) {
	h := e.h
	model := h.cfg.Inbox.Digest.MainModel
	if model == "" {
		model = "sonnet"
	}
	res := inbox.DigestResult{Executor: ExecutorClaude, Model: model}
	fallback := func(format string, a ...any) (inbox.DigestResult, error) {
		return res, inbox.ErrFallback{Note: fmt.Sprintf(format, a...)}
	}
	mid, mname, dir, err := h.jobMachine()
	if err != nil {
		h.log.Info("inbox: claude digest not possible", "digest", in.DigestID, "err", err)
		return fallback("%v", err)
	}
	wc := h.worker(mid)
	if wc == nil {
		return fallback("the machine %s went offline", mname)
	}
	s := Session{ID: proto.NewID(), MachineID: mid, OwnerSub: in.Owner, Kind: KindDigest, CWD: dir,
		ProfileName: proto.KindSubscription, MainModel: model, PermissionMode: "default",
		Title: "Digest " + time.Now().In(digestLoc(h)).Format("2006-01-02 15:04")}
	if err := h.store.CreateSession(s); err != nil {
		return fallback("could not create the job session: %v", err)
	}
	job := &digestJob{sessionID: s.ID, machineID: mid, tools: in.Tools, ch: make(chan jobNote, 8)}
	h.jobs.add(job)
	defer h.jobs.remove(s.ID)
	res.SessionID = s.ID
	if in.OnSession != nil {
		in.OnSession(s.ID)
	}
	h.store.Audit(in.Owner, "session_start", fmt.Sprintf("%s digest %s %s %s digest_id=%d", s.ID, mname, dir, model, in.DigestID))
	stop := func() {
		_ = wc.send(context.Background(), &proto.StopSession{SessionID: s.ID})
	}
	fail := func(note string) (inbox.DigestResult, error) {
		h.store.SetSessionError(s.ID, note)
		h.publishState(s.ID, proto.StateExited)
		res.Cost = h.store.SessionUsage(s.ID).Cost
		return fallback("%s", note)
	}

	ack := make(chan string, 1)
	frame := &proto.StartSession{Header: proto.Header{ID: proto.NewID()}, SessionID: s.ID, CWD: dir,
		Profile: proto.Profile{Name: proto.KindSubscription, Kind: proto.KindSubscription}, MainModel: model,
		PermissionMode: "default", Tools: []string{inbox.ToolProposeTask},
		Job: &proto.Job{Kind: proto.JobDigest, MainModel: model, System: in.System}}
	h.mu.Lock()
	h.starts[frame.ID] = s.ID
	h.acks[frame.ID] = ack
	h.mu.Unlock()
	if err := wc.send(ctx, frame); err != nil {
		h.mu.Lock()
		delete(h.starts, frame.ID)
		delete(h.acks, frame.ID)
		h.mu.Unlock()
		return fail("could not reach " + mname + ": " + err.Error())
	}
	ackWait := h.jobs.ackWait
	if ackWait == 0 {
		ackWait = jobAckWait
	}
	select {
	case msg := <-ack:
		if msg != "" {
			return fail(mname + " refused the job: " + msg)
		}
	case <-time.After(ackWait):
		h.mu.Lock()
		delete(h.starts, frame.ID)
		delete(h.acks, frame.ID)
		h.mu.Unlock()
		stop()
		return fail(fmt.Sprintf("%s did not acknowledge the job within %s", mname, ackWait))
	case <-ctx.Done():
		stop()
		return res, errors.New("the digest was interrupted or took too long")
	}
	if err := wc.send(ctx, &proto.UserMessage{SessionID: s.ID, Text: in.Message}); err != nil {
		stop()
		return fail("could not send the records to " + mname + ": " + err.Error())
	}
	backstop := h.jobs.backstop
	if backstop == 0 {
		backstop = jobBackstop
	}
	timer := time.NewTimer(backstop)
	defer timer.Stop()
	for {
		select {
		case n := <-job.ch:
			if n.exited {
				note := "claude ended without a result"
				if n.code != nil {
					note = fmt.Sprintf("claude exited with code %d without a result", *n.code)
					if *n.code == 2 {
						note += " (the subscription's usage limit?)"
					}
				}
				if n.err != "" {
					note += ": " + n.err
				}
				return fail(note)
			}
			stop() // one turn: end stdin so claude exits
			text, err := jobResult(n.result)
			res.Cost = h.store.SessionUsage(s.ID).Cost
			if err != nil {
				return fallback("%v", err)
			}
			res.Text = text
			return res, nil
		case <-timer.C:
			stop()
			return fail(fmt.Sprintf("no result within %s", backstop))
		case <-ctx.Done():
			stop()
			res.Cost = h.store.SessionUsage(s.ID).Cost
			return res, errors.New("the digest was interrupted or took too long")
		}
	}
}

// jobResult reads a job's result event: the digest is result.result; is_error, a
// subtype other than success and any api_error_status are failures.
func jobResult(raw json.RawMessage) (string, error) {
	var r struct {
		Subtype        string          `json:"subtype"`
		IsError        bool            `json:"is_error"`
		Result         string          `json:"result"`
		APIErrorStatus json.RawMessage `json:"api_error_status"`
		Errors         []string        `json:"errors"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", errors.New("unreadable result event")
	}
	apiErr := len(r.APIErrorStatus) > 0 && string(r.APIErrorStatus) != "null"
	if r.IsError || r.Subtype != "success" || apiErr {
		msg := strings.TrimSpace(r.Result)
		if msg == "" && len(r.Errors) > 0 {
			msg = strings.Join(r.Errors, "; ")
		}
		if apiErr {
			msg = strings.TrimSpace(msg + " (api_error_status " + string(r.APIErrorStatus) + ")")
		}
		return "", fmt.Errorf("claude ended with %q: %s", r.Subtype, inbox.ClipUTF8(msg, 500))
	}
	text := strings.TrimSpace(r.Result)
	if text == "" {
		return "", errors.New("claude returned no digest text")
	}
	return text, nil
}

// jobApproval denies a permission request of a digest job at once: a job has no
// built-in tools and propose_task is pre-allowed, so a request means something
// unexpected, and nobody watches a digest to answer it.
func (h *Hub) jobApproval(ctx context.Context, wc *workerConn, m *proto.ApprovalRequest) bool {
	s, err := h.store.Session(m.SessionID)
	if err != nil || s.Kind != KindDigest {
		return false
	}
	_ = wc.send(ctx, &proto.ApprovalResponse{SessionID: m.SessionID, RequestID: m.RequestID, Decision: proto.DecisionDeny,
		Message: "mado: a digest job runs without permission prompts."})
	h.log.Warn("denied a permission request of a digest job", "session", m.SessionID, "tool", m.ToolName)
	return true
}
