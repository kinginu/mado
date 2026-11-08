package shim

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/proto"
)

// fakeControl answers set_permission_mode control requests the way Claude Code 2.1.314
// does (DESIGN §25.3): a control_response, then a system/status event with the new
// mode. "default" is refused, to exercise claude's own error. Any other line is
// ignored.
func fakeControl() int {
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var req struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
				Mode    string `json:"mode"`
			} `json:"request"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || req.Type != "control_request" || req.Request.Subtype != "set_permission_mode" {
			continue
		}
		if req.Request.Mode == "default" {
			fmt.Printf(`{"type":"control_response","response":{"subtype":"error","request_id":%q,"error":"refused by the fake"}}`+"\n", req.RequestID)
			continue
		}
		fmt.Printf(`{"type":"control_response","response":{"subtype":"success","request_id":%q,"response":{"mode":%q}}}`+"\n", req.RequestID, req.Request.Mode)
		fmt.Printf(`{"type":"system","subtype":"status","status":null,"permissionMode":%q}`+"\n", req.Request.Mode)
	}
	return 0
}

func modeResult(d *daemon, reqID string) proto.Local {
	d.t.Helper()
	got := d.until("mode_result "+reqID, func(m proto.Local) bool { return m.T == proto.LModeResult && m.RequestID == reqID })
	return got[len(got)-1]
}

func TestSetModeControlRequest(t *testing.T) {
	dir := sockDir(t)
	sid := proto.NewID()
	self, _ := os.Executable()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Config{SessionID: sid, RuntimeDir: dir, ControlTimeout: 2 * time.Second})
	d := dialDaemon(t, SockPath(dir, sid), 0)
	// Before claude runs: an error at once.
	d.lc.Write(proto.Local{T: proto.LSetMode, RequestID: "r0", Mode: "auto"})
	if r := modeResult(d, "r0"); r.Error != "claude is not running" {
		t.Fatalf("before start %+v", r)
	}
	d.lc.Write(proto.Local{T: proto.LStart, Spawn: &proto.Spawn{ClaudeBin: self, CWD: dir, Env: map[string]string{"MADO_FAKE": "control"}}})
	d.until("idle", func(m proto.Local) bool { return m.T == proto.LState && m.State == proto.StateIdle })

	d.lc.Write(proto.Local{T: proto.LSetMode, RequestID: "r1", Mode: "auto"})
	if r := modeResult(d, "r1"); r.Error != "" || r.Mode != "auto" {
		t.Fatalf("auto %+v", r)
	}
	d.lc.Write(proto.Local{T: proto.LSetMode, RequestID: "r2", Mode: "default"})
	if r := modeResult(d, "r2"); r.Error != "refused by the fake" {
		t.Fatalf("claude's error %+v", r)
	}
	for _, bad := range []string{"bypassPermissions", "dontAsk", "plan", ""} {
		d.lc.Write(proto.Local{T: proto.LSetMode, RequestID: "bad", Mode: bad})
		if r := modeResult(d, "bad"); !strings.Contains(r.Error, "not allowed") {
			t.Fatalf("%q: %+v", bad, r)
		}
	}
	// Notes, alone and before a message.
	d.lc.Write(proto.Local{T: proto.LNote, Note: "rate_limit_cap", Text: "stopped"})
	d.lc.Write(proto.Local{T: proto.LUserMessage, Text: "go on", Note: "rate_limit_resume", Message: "Resumed automatically after the limit reset."})
	got := d.until("echo", func(m proto.Local) bool {
		return m.T == proto.LEvent && strings.Contains(string(m.Raw), `"mado":"echo"`)
	})
	var notes []string
	for _, m := range got {
		if m.T == proto.LEvent && strings.Contains(string(m.Raw), `"mado":"note"`) {
			var e struct {
				Note string `json:"mado_note"`
			}
			json.Unmarshal(m.Raw, &e)
			notes = append(notes, e.Note)
		}
		if m.T == proto.LEvent && strings.Contains(string(m.Raw), "control_response") {
			t.Fatalf("a control_response became an event: %s", m.Raw)
		}
	}
	if strings.Join(notes, ",") != "rate_limit_cap,rate_limit_resume" {
		t.Fatalf("notes %v in %+v", notes, got)
	}
	// claude's status event after a switch is an ordinary event.
	sawStatus := false
	for _, e := range mustSpool(t, SpoolPath(dir, sid)) {
		if strings.Contains(string(e.Raw), "control_response") {
			t.Fatalf("spooled control_response %s", e.Raw)
		}
		if strings.Contains(string(e.Raw), `"permissionMode":"auto"`) {
			sawStatus = true
		}
	}
	if !sawStatus {
		t.Error("status event missing from the spool")
	}
}

// A claude that never answers (cat echoes the request back): the shim gives up after
// ControlTimeout.
func TestSetModeTimeout(t *testing.T) {
	dir := sockDir(t)
	d, cancel, done := startCat(t, Config{SessionID: proto.NewID(), RuntimeDir: dir, ControlTimeout: 300 * time.Millisecond})
	defer func() { cancel(nil); <-done }()
	start := time.Now()
	d.lc.Write(proto.Local{T: proto.LSetMode, RequestID: "t1", Mode: "acceptEdits"})
	if r := modeResult(d, "t1"); r.Error != "claude did not answer the control request" || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("timeout %+v after %v", r, time.Since(start))
	}
}

func mustSpool(t *testing.T, path string) []SpoolEntry {
	t.Helper()
	es, err := ReadSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	return es
}
