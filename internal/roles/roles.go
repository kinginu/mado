// Package roles turns a profile's role split (DESIGN §5, §15) into the claude
// arguments that make the two-model split happen: a --agents JSON with the
// implementer, researcher and simple subagents, and a delegation prompt for
// --append-system-prompt. Claude Code's main model only uses a second model when it
// calls the Agent tool, and the subagent's model comes from the agent definition,
// so mado sets both up itself.
package roles

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/kinginu/mado/internal/proto"
)

// DefaultPrompt is appended to the main model's system prompt when the profile's
// roles.prompt is empty. Keep it under 150 words; TestPromptRules checks its rules.
const DefaultPrompt = `You direct this session and review the work; subagents do it. Delegate with the Agent tool:
- researcher: investigation. Any question about what code, docs or logs say or do (summaries and explanations included, even of one file), any task that needs more than two file reads, and any command beyond a quick check.
- implementer: every file change, however small, with the build and tests that go with it.
- simple: mechanical edits such as renames, boilerplate and formatting.
Yourself, you may read at most two files and run quick checks, only to decide what to delegate or to verify a report. Never edit a file yourself.
Give each subagent a self-contained brief: goal, files, constraints and how to check the result. Answer the user only after you have reviewed the subagent's report against the brief; if it falls short, send it back with corrections.`

// Defaults for the model aliases.
const (
	DefaultImplementer = "opus"
	DefaultResearcher  = "opus"
	DefaultSimple      = "sonnet"
)

// Enforcement of the split on the main model (DESIGN §16).
const (
	EnforceOff   = "off"
	EnforceEdits = "edits" // main may not use Edit, Write, NotebookEdit
	EnforceAll   = "all"   // nor Bash (or PowerShell)

	DefaultEnforce = EnforceEdits
)

// EnforceModes are the allowed roles.enforce values.
var EnforceModes = []string{EnforceOff, EnforceEdits, EnforceAll}

// MaxPrompt bounds a custom prompt (it travels in argv).
const MaxPrompt = 4000

// Aliases a role may use. For a router profile they resolve through the
// ANTHROPIC_DEFAULT_*_MODEL variables to the profile's aliases.
var Aliases = []string{"fable", "opus", "sonnet", "haiku", "inherit"}

// Effective fills the defaults in. enabled is false only for an explicit
// enabled = false.
func Effective(r *proto.Roles) (out proto.Roles, enabled bool) {
	if r != nil {
		out = *r
	}
	enabled = out.Enabled == nil || *out.Enabled
	if out.Implementer == "" {
		out.Implementer = DefaultImplementer
	}
	if out.Researcher == "" {
		out.Researcher = DefaultResearcher
	}
	if out.Simple == "" {
		out.Simple = DefaultSimple
	}
	if out.Enforce == "" {
		out.Enforce = DefaultEnforce
	}
	t := true
	out.Enabled = &t
	if !enabled {
		f := false
		out.Enabled = &f
	}
	return out, enabled
}

// Validate checks a roles block from config, the editor or a hub frame.
func Validate(r *proto.Roles) error {
	if r == nil {
		return nil
	}
	for name, v := range map[string]string{"implementer": r.Implementer, "researcher": r.Researcher, "simple": r.Simple} {
		if v != "" && !validAlias(v) {
			return fmt.Errorf("roles.%s: %q is not one of %s", name, v, strings.Join(Aliases, ", "))
		}
	}
	if r.Enforce != "" && !slices.Contains(EnforceModes, r.Enforce) {
		return fmt.Errorf("roles.enforce: %q is not one of %s", r.Enforce, strings.Join(EnforceModes, ", "))
	}
	if len(r.Prompt) > MaxPrompt {
		return fmt.Errorf("roles.prompt: longer than %d bytes", MaxPrompt)
	}
	if !utf8.ValidString(r.Prompt) || strings.ContainsRune(r.Prompt, 0) {
		return errors.New("roles.prompt: invalid text")
	}
	return nil
}

func validAlias(s string) bool {
	for _, a := range Aliases {
		if s == a {
			return true
		}
	}
	return false
}

// Agent is one CLI-defined subagent (https://code.claude.com/docs/en/sub-agents,
// "Choose the subagent scope").
type Agent struct {
	Description     string   `json:"description"`
	Prompt          string   `json:"prompt"`
	Model           string   `json:"model"`
	Tools           []string `json:"tools,omitempty"` // omitted = every tool (implementer)
	DisallowedTools []string `json:"disallowedTools,omitempty"`
}

// Agents returns the three role agents for r (defaults applied).
func Agents(r *proto.Roles) map[string]Agent { return AgentsWith(r, nil) }

// AgentsWith also gives the researcher the extra tools (the hub's web tools as
// mcp__mado__… names, DESIGN §17.12); the implementer has every tool anyway.
func AgentsWith(r *proto.Roles, researcherExtra []string) map[string]Agent {
	e, _ := Effective(r)
	return map[string]Agent{
		"implementer": {
			Description: "Implementation specialist: writes and edits code, then builds and runs the tests. Use proactively for every file change, however small.",
			Prompt: "You are the implementer. Carry out the brief you are given: make the code changes, build, run the relevant tests, " +
				"and report what you changed (files, key decisions) and the test results. Stay inside the brief; if it is unclear or wrong, say so instead of guessing.",
			Model: e.Implementer,
		},
		"researcher": {
			Description: "Investigator: reads and searches code, docs and logs and runs read-only commands, then reports findings. Use proactively for any question that needs more than two file reads or any command beyond a quick check.",
			Prompt: "You are the researcher. Answer the question in the brief by reading and searching the code, docs and logs. Do not edit files. " +
				"Report concise findings with file paths and line numbers, and say what you could not confirm.",
			Model:           e.Researcher,
			Tools:           append([]string{"Read", "Grep", "Glob", "Bash", "WebFetch"}, researcherExtra...),
			DisallowedTools: []string{"Edit", "Write", "NotebookEdit"},
		},
		"simple": {
			Description: "Mechanical edits: scaffolding, boilerplate, renames and formatting that need no design decisions. Use proactively for such edits.",
			Prompt:      "You do mechanical edits. Do exactly what the brief says, without redesigning anything, and report the files you changed.",
			Model:       e.Simple,
			Tools:       []string{"Read", "Edit", "Write", "Bash"},
		},
	}
}

// AgentsJSON is the value of --agents (keys sorted, so the output is stable).
func AgentsJSON(r *proto.Roles) string { return agentsJSON(r, nil) }

func agentsJSON(r *proto.Roles, extra []string) string {
	b, _ := json.Marshal(AgentsWith(r, extra))
	return string(b)
}

// SystemPrompt is the value of --append-system-prompt.
func SystemPrompt(r *proto.Roles) string {
	if r != nil && strings.TrimSpace(r.Prompt) != "" {
		return r.Prompt
	}
	return DefaultPrompt
}

// Args returns the claude arguments for the role split, or nil when it is off.
// --forward-subagent-text makes the stream carry the subagents' messages (with
// parent_tool_use_id), so mado can show which model did what. guardBin is the mado
// executable; unless enforce is off, a --settings PreToolUse hook runs
// `guardBin guard-tool --enforce <mode>` on the main model's blocked tools.
func Args(r *proto.Roles, guardBin string) []string { return ArgsWith(r, guardBin, nil) }

// ArgsWith is Args with extra researcher tools (see AgentsWith).
func ArgsWith(r *proto.Roles, guardBin string, researcherExtra []string) []string {
	e, on := Effective(r)
	if !on {
		return nil
	}
	a := []string{"--agents", agentsJSON(r, researcherExtra), "--append-system-prompt", SystemPrompt(r), "--forward-subagent-text"}
	if s := GuardSettings(e.Enforce, guardBin); s != "" {
		a = append(a, "--settings", s)
	}
	return a
}

// GuardTool is the subcommand the hook runs.
const GuardTool = "guard-tool"

// BlockedTools are the tools the main model may not use under an enforce mode.
func BlockedTools(enforce string) []string {
	switch enforce {
	case EnforceEdits:
		return []string{"Edit", "Write", "NotebookEdit"}
	case EnforceAll:
		return []string{"Edit", "Write", "NotebookEdit", "Bash", "PowerShell"}
	}
	return nil
}

type hookCmd struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
}

type hookMatcher struct {
	Matcher string    `json:"matcher"`
	Hooks   []hookCmd `json:"hooks"`
}

// GuardSettings is the --settings JSON with the PreToolUse guard hook, or "" when
// enforce is off or there is no binary. The hook uses the exec form (command +
// args, no shell). Hooks merge across settings sources, so a user's own hooks keep
// running (https://code.claude.com/docs/en/settings, "Lists merge instead of
// overriding").
func GuardSettings(enforce, guardBin string) string {
	tools := BlockedTools(enforce)
	if len(tools) == 0 || guardBin == "" {
		return ""
	}
	v := map[string]any{"hooks": map[string]any{"PreToolUse": []hookMatcher{{
		Matcher: strings.Join(tools, "|"),
		Hooks:   []hookCmd{{Type: "command", Command: guardBin, Args: []string{GuardTool, "--enforce", enforce}, Timeout: 10}},
	}}}}
	b, _ := json.Marshal(v)
	return string(b)
}

// hookInput is the part of the PreToolUse hook input the guard reads. agent_id is
// set only when the call comes from a subagent (https://code.claude.com/docs/en/hooks,
// "Common input fields"; confirmed by experiment, docs/claude-code-facts.md).
type hookInput struct {
	HookEvent string `json:"hook_event_name"`
	ToolName  string `json:"tool_name"`
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
}

// DenyMessage tells the main model to delegate instead.
func DenyMessage(tool, enforce string) string {
	what := "File changes go to the implementer subagent (mechanical edits to simple)"
	if tool == "Bash" || tool == "PowerShell" {
		what = "Commands and investigation go to the researcher subagent; builds, tests and file changes to implementer"
	}
	return fmt.Sprintf("Blocked by mado (roles.enforce = %s): the main model may not use %s. %s. "+
		"Delegate now with the Agent tool (subagent_type implementer, researcher or simple) and a self-contained brief, "+
		"then review its report before you answer.", enforce, tool, what)
}

// Guard decides one PreToolUse call. It returns a non-empty denial message when the
// main model calls a blocked tool. Unreadable input is an error (the caller denies).
func Guard(in io.Reader, enforce string) (string, error) {
	blocked := BlockedTools(enforce)
	if enforce != EnforceOff && blocked == nil {
		return "", fmt.Errorf("unknown enforce mode %q", enforce)
	}
	var h hookInput
	if err := json.NewDecoder(io.LimitReader(in, 4<<20)).Decode(&h); err != nil {
		return "", fmt.Errorf("hook input: %w", err)
	}
	if h.AgentID != "" || !slices.Contains(blocked, h.ToolName) {
		return "", nil
	}
	return DenyMessage(h.ToolName, enforce), nil
}

// DenyOutput is the hook's stdout for a denial.
func DenyOutput(msg string) []byte {
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": msg}})
	return b
}
