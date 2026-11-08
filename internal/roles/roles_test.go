package roles

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/kinginu/mado/internal/proto"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := "testdata/" + name
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs from golden:\n got: %s\nwant: %s", name, got, want)
	}
}

func TestDefaultsGolden(t *testing.T) {
	golden(t, "agents_default.json", AgentsJSON(nil)+"\n")
	golden(t, "prompt_default.txt", SystemPrompt(nil)+"\n")
	golden(t, "agents_custom.json", AgentsJSON(&proto.Roles{Implementer: "sonnet", Researcher: "haiku", Simple: "inherit"})+"\n")
}

func TestPromptRules(t *testing.T) {
	if n := len(strings.Fields(DefaultPrompt)); n >= 150 {
		t.Fatalf("default prompt has %d words, want < 150", n)
	}
	for _, role := range []string{"implementer", "researcher", "simple"} {
		if !strings.Contains(DefaultPrompt, "- "+role+":") {
			t.Errorf("prompt does not name %s", role)
		}
	}
	// The testable rules (M5): >2 file reads or a real command -> researcher, every
	// file change -> implementer, answer only after reviewing the report.
	for _, rule := range []string{"more than two file reads", "summaries and explanations included, even of one file", "any command beyond a quick check",
		"implementer: every file change", "Never edit a file yourself", "only after you have reviewed the subagent's report"} {
		if !strings.Contains(DefaultPrompt, rule) {
			t.Errorf("prompt lacks %q", rule)
		}
	}
}

func TestAgentTools(t *testing.T) {
	ag := Agents(nil)
	if ag["implementer"].Tools != nil {
		t.Errorf("implementer tools %v, want all (omitted)", ag["implementer"].Tools)
	}
	if got := strings.Join(ag["researcher"].Tools, ","); got != "Read,Grep,Glob,Bash,WebFetch" {
		t.Errorf("researcher tools %s", got)
	}
	if got := strings.Join(ag["simple"].Tools, ","); got != "Read,Edit,Write,Bash" {
		t.Errorf("simple tools %s", got)
	}
	for n, a := range ag {
		if !strings.Contains(a.Description, "Use proactively") {
			t.Errorf("%s description lacks \"Use proactively\": %s", n, a.Description)
		}
	}
}

func TestArgs(t *testing.T) {
	a := Args(nil, "/bin/mado")
	if len(a) != 7 || a[0] != "--agents" || a[2] != "--append-system-prompt" || a[3] != DefaultPrompt || a[4] != "--forward-subagent-text" ||
		a[5] != "--settings" || a[6] != GuardSettings(EnforceEdits, "/bin/mado") {
		t.Fatalf("args %q", a)
	}
	f := false
	if Args(&proto.Roles{Enabled: &f}, "/bin/mado") != nil {
		t.Fatal("disabled roles produce args")
	}
	if got := Args(&proto.Roles{Prompt: "custom"}, "/bin/mado"); got[3] != "custom" {
		t.Fatalf("custom prompt not used: %q", got[3])
	}
	if got := Args(&proto.Roles{Enforce: EnforceOff}, "/bin/mado"); len(got) != 5 {
		t.Fatalf("enforce off still has a hook: %q", got)
	}
	if got := Args(nil, ""); len(got) != 5 {
		t.Fatalf("no binary but a hook: %q", got)
	}
	ag := Agents(&proto.Roles{Implementer: "fable"})
	if ag["implementer"].Model != "fable" || ag["researcher"].Model != "opus" || ag["simple"].Model != "sonnet" {
		t.Fatalf("models %+v", ag)
	}
}

func TestGuardSettingsGolden(t *testing.T) {
	golden(t, "settings_edits.json", GuardSettings(EnforceEdits, "/usr/local/bin/mado")+"\n")
	golden(t, "settings_all.json", GuardSettings(EnforceAll, "/usr/local/bin/mado")+"\n")
	if GuardSettings(EnforceOff, "/x") != "" {
		t.Fatal("off has settings")
	}
}

func TestGuard(t *testing.T) {
	main := func(tool string) string {
		return `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"` + tool + `","tool_input":{},"tool_use_id":"toolu_1"}`
	}
	// Recorded shape of a subagent call (Claude Code 2.1.302, docs/claude-code-facts.md).
	sub := func(tool string) string {
		return `{"session_id":"s","agent_id":"a2dac047ebae07259","agent_type":"implementer","hook_event_name":"PreToolUse","tool_name":"` + tool + `","tool_input":{}}`
	}
	cases := []struct {
		enforce, in string
		deny        bool
	}{
		{EnforceEdits, main("Edit"), true},
		{EnforceEdits, main("Write"), true},
		{EnforceEdits, main("NotebookEdit"), true},
		{EnforceEdits, main("Bash"), false},
		{EnforceEdits, main("Read"), false},
		{EnforceEdits, main("Agent"), false},
		{EnforceEdits, main("mcp__x__Edit"), false},
		{EnforceEdits, sub("Edit"), false},
		{EnforceEdits, sub("Write"), false},
		{EnforceAll, main("Bash"), true},
		{EnforceAll, main("PowerShell"), true},
		{EnforceAll, main("Write"), true},
		{EnforceAll, main("Read"), false},
		{EnforceAll, sub("Bash"), false},
		{EnforceOff, main("Write"), false},
	}
	for _, c := range cases {
		msg, err := Guard(strings.NewReader(c.in), c.enforce)
		if err != nil || (msg != "") != c.deny {
			t.Errorf("%s %s: msg %q err %v", c.enforce, c.in, msg, err)
		}
		if c.deny && (!strings.Contains(msg, "Agent tool") || !strings.Contains(msg, "implementer") || !strings.Contains(msg, "researcher")) {
			t.Errorf("denial does not say how to delegate: %s", msg)
		}
	}
	if _, err := Guard(strings.NewReader("not json"), EnforceEdits); err == nil {
		t.Error("bad input accepted")
	}
	if _, err := Guard(strings.NewReader(main("Edit")), "bash"); err == nil {
		t.Error("unknown mode accepted")
	}
	var out struct {
		H struct {
			Event, Decision, Reason string
		} `json:"hookSpecificOutput"`
	}
	b := DenyOutput("go delegate")
	if err := json.Unmarshal(b, &out); err != nil || string(b) !=
		`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"go delegate"}}` {
		t.Fatalf("deny output %s", b)
	}
}

func TestValidate(t *testing.T) {
	for _, bad := range []proto.Roles{{Implementer: "gpt"}, {Simple: "claude-opus-5-5"}, {Prompt: "a\x00b"},
		{Prompt: strings.Repeat("x", MaxPrompt+1)}, {Researcher: "aiand-glm53"}, {Enforce: "bash"}, {Enforce: "Edits"}} {
		if Validate(&bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := Validate(&proto.Roles{Implementer: "opus", Researcher: "inherit", Simple: "haiku", Prompt: "ok"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range EnforceModes {
		if err := Validate(&proto.Roles{Enforce: m}); err != nil {
			t.Errorf("enforce %s: %v", m, err)
		}
	}
	if e, _ := Effective(nil); e.Enforce != EnforceEdits {
		t.Errorf("default enforce %q", e.Enforce)
	}
	if Validate(nil) != nil {
		t.Fatal("nil roles rejected")
	}
}
