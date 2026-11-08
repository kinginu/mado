package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kinginu/mado/internal/approve"
	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/roles"
	"github.com/kinginu/mado/internal/router"
	"github.com/kinginu/mado/internal/tools"
)

// Hub frames are untrusted input (DESIGN §4). Everything that reaches argv, env or the
// file system goes through here.

var (
	reProfileName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	reClaudeModel = regexp.MustCompile(`^claude-[a-z0-9.-]{1,64}(\[1m\])?$`)
	reUUID        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

var subscriptionAliases = map[string]bool{
	"default": true, "best": true, "fable": true, "opus": true, "sonnet": true, "haiku": true,
	"opusplan": true, "opus[1m]": true, "sonnet[1m]": true,
}

// permissionModes a hub may ask for. auto came back in M5 at the user's request (its
// classifier still denies risky calls, and a session that cannot use it starts in
// default). dontAsk and bypassPermissions skip the approval prompts mado exists to
// show; plan is not offered either (DESIGN §15, §16).
var permissionModes = map[string]bool{"default": true, "acceptEdits": true, "auto": true}

var effortLevels = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// ResolveCWD checks that cwd is an existing directory inside one of the roots,
// after resolving symlinks. It returns the resolved path.
func ResolveCWD(cwd string, roots []string) (string, error) {
	if !filepath.IsAbs(cwd) {
		return "", errors.New("cwd must be absolute")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(cwd))
	if err != nil {
		return "", fmt.Errorf("cwd: %w", err)
	}
	fi, err := os.Stat(real)
	if err != nil || !fi.IsDir() {
		return "", errors.New("cwd is not a directory")
	}
	for _, r := range roots {
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rr, real)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return real, nil
		}
	}
	return "", errors.New("cwd is outside the allowlisted project roots")
}

// ValidateExtraArgs allows a small set of flags with checked values.
func ValidateExtraArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--effort":
			if i+1 >= len(args) || !effortLevels[args[i+1]] {
				return errors.New("--effort needs low|medium|high|xhigh|max")
			}
			i++
		case "--max-turns":
			if i+1 >= len(args) {
				return errors.New("--max-turns needs a value")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 || n > 10000 {
				return errors.New("--max-turns must be 1..10000")
			}
			i++
		default:
			return fmt.Errorf("extra arg %q is not allowed", args[i])
		}
	}
	return nil
}

// SpawnParams are the daemon-side values that are not in the hub frame.
type SpawnParams struct {
	ClaudeBin        string
	ShimBin          string
	SocketPath       string
	RouterURL        string
	RouterToken      string
	ClaudeConfigBase string
	// JobDir is the cwd of job sessions; "" when jobs are off on this worker.
	JobDir string
}

// BuildSpawn validates a start_session frame and turns it into a spawn spec.
// roots are the worker's project roots.
func BuildSpawn(f *proto.StartSession, roots []string, p SpawnParams) (*proto.Spawn, error) {
	if !proto.ValidSessionID(f.SessionID) {
		return nil, errors.New("bad session id")
	}
	if f.Job != nil {
		return buildJobSpawn(f, p)
	}
	cwd, err := ResolveCWD(f.CWD, roots)
	if err != nil {
		return nil, err
	}
	mode := f.PermissionMode
	if mode == "" {
		mode = "default"
	}
	if !permissionModes[mode] {
		return nil, fmt.Errorf("permission mode %q is not allowed", mode)
	}
	if err := ValidateExtraArgs(f.ExtraArgs); err != nil {
		return nil, err
	}
	if f.ResumeClaudeSessionID != "" && !reUUID.MatchString(f.ResumeClaudeSessionID) {
		return nil, errors.New("bad resume session id")
	}
	prof := f.Profile
	if !reProfileName.MatchString(prof.Name) {
		return nil, errors.New("bad profile name")
	}
	if err := roles.Validate(prof.Roles); err != nil {
		return nil, err
	}
	// Hub tools (M8): only names this worker knows; a newer hub's extras are ignored.
	var hubTools, mcpNames []string
	for _, n := range f.Tools {
		if slices.Contains(tools.CodeTools, n) && !slices.Contains(hubTools, n) {
			hubTools = append(hubTools, n)
			mcpNames = append(mcpNames, approve.MCPToolName(n))
		}
	}
	mcpArgv := []string{p.ShimBin, "approve-tool", "--socket", p.SocketPath}
	if len(hubTools) > 0 {
		mcpArgv = append(mcpArgv, "--tools", strings.Join(hubTools, ","))
	}
	args := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-mode", mode,
		"--strict-mcp-config",
		"--mcp-config", approve.MCPConfig(mcpArgv),
		"--permission-prompt-tool", approve.PromptTool,
	}
	// web_search only sends a query to the hub's search backend; it needs no
	// prompt, like Claude Code's own WebSearch. fetch_url asks, like WebFetch.
	if slices.Contains(hubTools, tools.WebSearch) {
		args = append(args, "--allowedTools", approve.MCPToolName(tools.WebSearch))
	}
	env := map[string]string{}
	model := f.MainModel
	switch prof.Kind {
	case proto.KindSubscription:
		if len(prof.Upstreams) > 0 {
			return nil, errors.New("subscription profile must not have upstreams")
		}
		if model != "" && !subscriptionAliases[model] && !reClaudeModel.MatchString(model) {
			return nil, fmt.Errorf("model %q is not allowed for a subscription profile", model)
		}
	case proto.KindRouter:
		if err := router.Validate(prof); err != nil {
			return nil, err
		}
		if model == "" {
			model = prof.Main
		}
		if _, ok := prof.Upstreams[model]; !ok {
			return nil, fmt.Errorf("main model %q is not an upstream of the profile", model)
		}
		if p.RouterURL == "" || p.RouterToken == "" {
			return nil, errors.New("router not available")
		}
		env["ANTHROPIC_BASE_URL"] = p.RouterURL
		env["ANTHROPIC_AUTH_TOKEN"] = p.RouterToken
		for alias, name := range map[string]string{"OPUS": "opus", "SONNET": "sonnet", "HAIKU": "haiku", "FABLE": "fable"} {
			target := prof.Aliases[name]
			if target == "" {
				target = model
			}
			env["ANTHROPIC_DEFAULT_"+alias+"_MODEL"] = target
		}
		// One claude process has one value for each; they describe the session's main
		// model (DESIGN §5). Router names never start with claude-, so Claude Code
		// applies CLAUDE_CODE_MAX_CONTEXT_TOKENS directly to them.
		if main := prof.Upstreams[model]; main.ContextWindow > 0 {
			env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] = strconv.Itoa(main.ContextWindow)
		}
		if main := prof.Upstreams[model]; main.MaxOutput > 0 {
			env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"] = strconv.Itoa(main.MaxOutput)
		}
		env["CLAUDE_CODE_ATTRIBUTION_HEADER"] = "0"
		env["API_TIMEOUT_MS"] = "3000000"
		dir := filepath.Join(p.ClaudeConfigBase, "claude-"+prof.Name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		env["CLAUDE_CONFIG_DIR"] = dir
	default:
		return nil, fmt.Errorf("unknown profile kind %q", prof.Kind)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, roles.ArgsWith(prof.Roles, p.ShimBin, mcpNames)...)
	if f.ResumeClaudeSessionID != "" {
		args = append(args, "--resume", f.ResumeClaudeSessionID)
	}
	args = append(args, f.ExtraArgs...)
	return &proto.Spawn{ClaudeBin: p.ClaudeBin, CWD: cwd, Args: args, Env: env}, nil
}

// JobMaxTurns caps the agentic turns of a job (tool calls consume turns).
const JobMaxTurns = 12

// maxJobSystem bounds the job's system prompt (it goes into argv).
const maxJobSystem = 64 << 10

// buildJobSpawn validates a job start_session (DESIGN §23.6) and builds its spawn:
// the subscription login (no router env, the default config dir), no built-in tools,
// propose_task through the mado MCP server, no roles, no guard hook, one turn.
func buildJobSpawn(f *proto.StartSession, p SpawnParams) (*proto.Spawn, error) {
	j := f.Job
	if j.Kind != proto.JobDigest {
		return nil, fmt.Errorf("unknown job kind %q", j.Kind)
	}
	if p.JobDir == "" {
		return nil, errors.New("jobs are not enabled on this worker (jobs = true in worker.toml)")
	}
	cwd, err := ResolveCWD(f.CWD, []string{p.JobDir})
	if err != nil {
		return nil, fmt.Errorf("job: %w", err)
	}
	if want, err := filepath.EvalSymlinks(p.JobDir); err != nil || cwd != want {
		return nil, errors.New("job: cwd must be the worker's job_dir")
	}
	if f.Profile.Kind != proto.KindSubscription || len(f.Profile.Upstreams) > 0 {
		return nil, errors.New("job: only the subscription profile runs jobs")
	}
	if f.PermissionMode != "" && f.PermissionMode != "default" {
		return nil, errors.New("job: permission mode must be default")
	}
	if len(f.ExtraArgs) > 0 || f.ResumeClaudeSessionID != "" {
		return nil, errors.New("job: no extra args and no resume")
	}
	model := j.MainModel
	if model == "" {
		model = f.MainModel
	}
	if model == "" {
		model = "sonnet"
	}
	if !subscriptionAliases[model] && !reClaudeModel.MatchString(model) {
		return nil, fmt.Errorf("job: model %q is not allowed", model)
	}
	sys := strings.TrimSpace(j.System)
	if sys == "" || len(sys) > maxJobSystem || !utf8.ValidString(sys) || strings.ContainsRune(sys, 0) {
		return nil, errors.New("job: the system prompt is empty, too long or not text")
	}
	var jobTools []string
	for _, n := range f.Tools {
		if slices.Contains(approve.JobTools, n) && !slices.Contains(jobTools, n) {
			jobTools = append(jobTools, n)
		}
	}
	mcpArgv := []string{p.ShimBin, "approve-tool", "--socket", p.SocketPath}
	if len(jobTools) > 0 {
		mcpArgv = append(mcpArgv, "--tools", strings.Join(jobTools, ","))
	}
	args := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "default",
		"--strict-mcp-config",
		"--mcp-config", approve.MCPConfig(mcpArgv),
		"--permission-prompt-tool", approve.PromptTool,
		"--tools", "", // no built-in tools at all (CLI reference: "" disables all)
	}
	for _, n := range jobTools {
		args = append(args, "--allowedTools", approve.MCPToolName(n))
	}
	args = append(args, "--max-turns", strconv.Itoa(JobMaxTurns), "--model", model, "--append-system-prompt", sys)
	return &proto.Spawn{ClaudeBin: p.ClaudeBin, CWD: cwd, Args: args, Env: map[string]string{}}, nil
}

// ListProjects returns the roots and their non-hidden child directories.
func ListProjects(roots []string) []string {
	var out []string
	for _, r := range roots {
		out = append(out, r)
		ents, err := os.ReadDir(r)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if len(out) >= 500 {
				return out
			}
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			full := filepath.Join(r, e.Name())
			if fi, err := os.Stat(full); err == nil && fi.IsDir() {
				out = append(out, full)
			}
		}
	}
	return out
}
