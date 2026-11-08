package render

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) [][]byte {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	return lines
}

// The fixture is a real `claude -p --input-format stream-json --output-format stream-json
// --verbose` run (Claude Code 2.1.302) that used the Write tool after an approval.
func TestRecordedFixture(t *testing.T) {
	lines := loadFixture(t, "write_tool.jsonl")
	if len(lines) != 6 {
		t.Fatalf("fixture has %d lines", len(lines))
	}
	var all []Block
	for _, l := range lines {
		all = append(all, Blocks(l)...)
	}
	kinds := []string{}
	for _, b := range all {
		kinds = append(kinds, b.Kind)
	}
	want := []string{KindSystem, KindToolUse, KindToolResult, KindAssistant, KindResult}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if !strings.Contains(all[0].Text, "model claude-haiku-5-5") || !strings.Contains(all[0].Text, "mode default") {
		t.Errorf("init block: %q", all[0].Text)
	}
	if all[1].Label != "Write" || !strings.HasSuffix(all[1].Text, "probe-cwd/probe.txt") {
		t.Errorf("tool_use block: %+v", all[1])
	}
	if !strings.HasPrefix(all[2].Text, "File created successfully") || all[2].IsError {
		t.Errorf("tool_result block: %+v", all[2])
	}
	if all[3].Text != "done" {
		t.Errorf("assistant text: %q", all[3].Text)
	}
	if !strings.HasPrefix(all[4].Text, "turn done") || !strings.Contains(all[4].Text, "$0.0024") {
		t.Errorf("result: %q", all[4].Text)
	}
	tty := TTY(all)
	for _, s := range []string{"⚙ Write", "↳ File created", "● \x1b[0mdone", "✓ turn done"} {
		if !strings.Contains(tty, s) {
			t.Errorf("tty output lacks %q:\n%s", s, tty)
		}
	}
}

func TestTolerantParsing(t *testing.T) {
	cases := map[string]string{
		`not json`:                                                            KindRaw,
		`{"type":"brand_new_thing","x":1}`:                                    KindRaw,
		`{"type":"assistant"}`:                                                KindRaw,
		`{"type":"user","message":{"content":"hello"}}`:                       KindUser,
		`{"type":"system","subtype":"compact_boundary"}`:                      KindSystem,
		`{"type":"assistant","message":{"content":[{"type":"weird_block"}]}}`: KindRaw,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"is_error":true}]}}`: KindToolResult,
		`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"error_status":529,"error":"overloaded"}`:                                     KindSystem,
	}
	for in, want := range cases {
		bs := Blocks([]byte(in))
		if len(bs) != 1 || bs[0].Kind != want {
			t.Errorf("%s -> %+v, want kind %s", in, bs, want)
		}
	}
	if bs := Blocks([]byte(`{"type":"rate_limit_event"}`)); len(bs) != 0 {
		t.Errorf("rate limit should be hidden: %+v", bs)
	}
	bs := Blocks([]byte(`{"type":"user","parent_tool_use_id":"toolu_1","message":{"content":[{"type":"tool_result","content":"x","is_error":true}]}}`))
	if !bs[0].Subagent || !bs[0].IsError {
		t.Errorf("flags: %+v", bs[0])
	}
}

func TestAbbrevAndSummary(t *testing.T) {
	if got := Abbrev("héllo wörld", 5); got != "héllo…" {
		t.Errorf("abbrev = %q", got)
	}
	if got := ToolInputSummary("Bash", []byte(`{"command":"ls -la","description":"list"}`)); got != "ls -la" {
		t.Errorf("summary = %q", got)
	}
	if got := ToolInputSummary("AskUserQuestion", []byte(`{"questions":[{"question":"Which?","options":[]},{"question":"And?"}]}`)); got != "Which? (+1 more)" {
		t.Errorf("AskUserQuestion summary %q", got)
	}
	if got := ToolInputSummary("Grep", []byte(`{"pattern":"foo","path":"/src"}`)); got != "foo in /src" {
		t.Errorf("summary = %q", got)
	}
	long := strings.Repeat("x", 1000)
	if got := ToolInputSummary("X", []byte(`{"command":"`+long+`"}`)); len([]rune(got)) != maxInput+1 {
		t.Errorf("not abbreviated: %d", len(got))
	}
}

func TestNoiseAndTaskEvents(t *testing.T) {
	bs := Blocks([]byte(`{"type":"system","subtype":"thinking_tokens","estimated_tokens":12,"estimated_tokens_delta":5}`))
	if len(bs) != 1 || bs[0].Kind != KindThinkingTokens || bs[0].Delta != 5 {
		t.Fatalf("thinking_tokens: %+v", bs)
	}
	if TTY(bs) != "" {
		t.Errorf("thinking_tokens printed on the tty: %q", TTY(bs))
	}
	bs = Blocks([]byte(`{"type":"system","subtype":"task_progress","tool_use_id":"toolu_9","subagent_type":"Explore","description":"Running ls","usage":{"total_tokens":8701,"tool_uses":2}}`))
	if len(bs) != 1 || bs[0].Kind != KindStatus || bs[0].Parent != "toolu_9" || bs[0].Text != "Explore: Running ls (2 tools, 8.7k tokens)" {
		t.Fatalf("task_progress: %+v", bs)
	}
	for _, sub := range []string{"task_started", "task_updated", "task_notification", "background_tasks_changed"} {
		if bs := Blocks([]byte(`{"type":"system","subtype":"` + sub + `"}`)); len(bs) != 1 || bs[0].Kind != KindStatus {
			t.Errorf("%s: %+v", sub, bs)
		}
	}
	bs = Blocks([]byte(`{"type":"mado_stderr","text":"[claude-code:unrecognized_model] {\"model\":\"oc-a\"}"}`))
	if !bs[0].Noise {
		t.Error("unrecognized_model not marked as noise")
	}
	if Blocks([]byte(`{"type":"mado_stderr","text":"real error"}`))[0].Noise {
		t.Error("other stderr marked as noise")
	}
}

func TestGroupingHints(t *testing.T) {
	bs := Blocks([]byte(`{"type":"assistant","parent_tool_use_id":null,"message":{"model":"zai-org/glm-5.3","content":[{"type":"tool_use","id":"t1","name":"Agent","input":{"subagent_type":"implementer","model":"opus"}},{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"ls"}}]}}`))
	if bs[0].ToolID != "t1" || bs[0].Agent != "implementer" || bs[0].Model != "opus" || bs[0].Parent != "" {
		t.Errorf("agent call: %+v", bs[0])
	}
	if bs[1].ToolID != "t2" || bs[1].Agent != "" || bs[1].Model != "zai-org/glm-5.3" {
		t.Errorf("bash call: %+v", bs[1])
	}
	bs = Blocks([]byte(`{"type":"user","parent_tool_use_id":"t1","message":{"content":[{"type":"tool_result","tool_use_id":"t3","content":"x"}]}}`))
	if bs[0].ToolID != "t3" || bs[0].Parent != "t1" || !bs[0].Subagent {
		t.Errorf("subagent result: %+v", bs[0])
	}
}

func TestMadoFileAndTrimmed(t *testing.T) {
	bs := Blocks([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"mado_file","file_id":"f1","name":"cat.png","mime":"image/png"},{"type":"text","text":"what is this?"}]}}`))
	if len(bs) != 2 || bs[0].Kind != KindFile || bs[0].FileID != "f1" || bs[0].Label != "cat.png" || !bs[0].IsImage() || bs[1].Kind != KindUser {
		t.Fatalf("%+v", bs)
	}
	bs = Blocks([]byte(`{"type":"system","subtype":"mado_trimmed","dropped":3,"before_seq":9}`))
	if len(bs) != 1 || bs[0].Kind != KindSystem || !strings.Contains(bs[0].Text, "3 earlier") {
		t.Fatalf("%+v", bs)
	}
}

func TestMadoErrorWithStringMessage(t *testing.T) {
	bs := Blocks([]byte(`{"type":"mado_error","message":"claude is not running; message dropped"}`))
	if len(bs) != 1 || !bs[0].IsError || bs[0].Text != "claude is not running; message dropped" {
		t.Fatalf("%+v", bs)
	}
}

// M12 (DESIGN §20): the concierge's plan card and mado's notes.
func TestPlanAndNoteBlocks(t *testing.T) {
	bs := Blocks([]byte(`{"type":"system","subtype":"mado_plan","plan_id":"p1","plan":{"summary":"a worktree"},"state":"proposed"}`))
	if len(bs) != 1 || bs[0].Kind != KindPlan || bs[0].PlanID != "p1" || bs[0].Text != "a worktree" {
		t.Fatalf("plan %+v", bs)
	}
	bs = Blocks([]byte(`{"type":"user","mado_note":"plan","message":{"role":"user","content":[{"type":"text","text":"Plan p1 was approved and ran."}]}}`))
	if len(bs) != 1 || bs[0].Kind != KindNote || bs[0].Label != "plan" {
		t.Fatalf("note %+v", bs)
	}
}
