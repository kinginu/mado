package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunGuard(t *testing.T) {
	run := func(in string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runGuard(strings.NewReader(in), &out, &errb, args)
		return code, out.String(), errb.String()
	}
	if code, out, _ := run(`{"tool_name":"Write"}`, "--enforce", "edits"); code != 0 || !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("main Write: %d %s", code, out)
	}
	if code, out, _ := run(`{"tool_name":"Write","agent_id":"a1","agent_type":"implementer"}`, "--enforce", "edits"); code != 0 || out != "" {
		t.Errorf("subagent Write: %d %q", code, out)
	}
	if code, out, _ := run(`{"tool_name":"Bash"}`, "--enforce", "edits"); code != 0 || out != "" {
		t.Errorf("main Bash under edits: %d %q", code, out)
	}
	if code, out, _ := run(`{"tool_name":"Bash"}`, "--enforce", "all"); code != 0 || !strings.Contains(out, "researcher") {
		t.Errorf("main Bash under all: %d %q", code, out)
	}
	// Fail closed: Claude Code treats exit 2 as a block with stderr as the reason.
	if code, _, e := run(`garbage`, "--enforce", "edits"); code != 2 || e == "" {
		t.Errorf("bad input: %d %q", code, e)
	}
	if code, _, _ := run(`{"tool_name":"Write"}`, "--enforce", "bash"); code != 2 {
		t.Errorf("bad mode: %d", code)
	}
}
