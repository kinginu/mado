package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/roles"
)

func TestResolveCWD(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	os.Mkdir(filepath.Join(root, "proj"), 0o755)
	os.Symlink(other, filepath.Join(root, "escape"))
	os.WriteFile(filepath.Join(root, "file"), nil, 0o644)
	roots := []string{root}
	if _, err := ResolveCWD(filepath.Join(root, "proj"), roots); err != nil {
		t.Errorf("proj: %v", err)
	}
	if _, err := ResolveCWD(root, roots); err != nil {
		t.Errorf("root itself: %v", err)
	}
	for _, bad := range []string{
		filepath.Join(root, "escape"),           // symlink out of the root
		filepath.Join(root, "proj", "..", ".."), // traversal
		filepath.Join(root, "file"),
		filepath.Join(root, "missing"),
		"proj",
		other,
		root + "x",
	} {
		if _, err := ResolveCWD(bad, roots); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestValidateExtraArgs(t *testing.T) {
	ok := [][]string{nil, {"--effort", "high"}, {"--max-turns", "5", "--effort", "low"}}
	for _, a := range ok {
		if err := ValidateExtraArgs(a); err != nil {
			t.Errorf("%v: %v", a, err)
		}
	}
	bad := [][]string{{"--dangerously-skip-permissions"}, {"--effort"}, {"--effort", "; rm -rf /"},
		{"--max-turns", "0"}, {"--mcp-config", "x"}, {"--settings", "{}"}, {"--max-turns", "1e9"}}
	for _, a := range bad {
		if err := ValidateExtraArgs(a); err == nil {
			t.Errorf("%v accepted", a)
		}
	}
}

func startFrame(root string, prof proto.Profile) *proto.StartSession {
	return &proto.StartSession{SessionID: proto.NewID(), CWD: root, Profile: prof}
}

func TestBuildSpawnSubscription(t *testing.T) {
	root := t.TempDir()
	p := SpawnParams{ClaudeBin: "/c", ShimBin: "/oc", SocketPath: "/run/x.sock", ClaudeConfigBase: t.TempDir()}
	f := startFrame(root, proto.Profile{Name: "subscription", Kind: proto.KindSubscription})
	f.MainModel = "opus"
	sp, err := BuildSpawn(f, []string{root}, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.Env) != 0 {
		t.Errorf("subscription must not set env: %v", sp.Env)
	}
	a := strings.Join(sp.Args, " ")
	for _, want := range []string{"-p --input-format stream-json --output-format stream-json --verbose",
		"--permission-mode default", "--permission-prompt-tool mcp__mado__approve", "--model opus", "--strict-mcp-config"} {
		if !strings.Contains(a, want) {
			t.Errorf("args lack %q: %s", want, a)
		}
	}
	for _, bad := range []string{"oc-strong", "x; rm", "bypass"} {
		f.MainModel = bad
		if _, err := BuildSpawn(f, []string{root}, p); err == nil {
			t.Errorf("model %q accepted", bad)
		}
	}
	f.MainModel = ""
	f.PermissionMode = "bypassPermissions"
	if _, err := BuildSpawn(f, []string{root}, p); err == nil {
		t.Error("bypassPermissions accepted")
	}
}

func TestBuildSpawnRouter(t *testing.T) {
	root := t.TempDir()
	base := t.TempDir()
	p := SpawnParams{ClaudeBin: "/c", ShimBin: "/oc", SocketPath: "/run/x.sock", ClaudeConfigBase: base,
		RouterURL: "http://127.0.0.1:4000", RouterToken: "tok"}
	prof := proto.Profile{Name: "glm-pair", Kind: proto.KindRouter, Main: "oc-strong",
		Aliases: map[string]string{"opus": "oc-worker", "sonnet": "oc-worker", "haiku": "oc-small"},
		Upstreams: map[string]proto.Upstream{
			"oc-strong": {BaseURL: "http://gpu1:8000", Model: "zai-org/GLM-5.1", Key: "secret-1"},
			"oc-worker": {BaseURL: "http://gpu2:8000", Model: "Qwen/Qwen3-Coder-480B"},
			"oc-small":  {BaseURL: "http://gpu2:8001", Model: "Qwen/Qwen3-8B"},
		}}
	f := startFrame(root, prof)
	f.ResumeClaudeSessionID = "ef4d08a3-0fb3-45bf-927f-28034415014c"
	sp, err := BuildSpawn(f, []string{root}, p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"ANTHROPIC_BASE_URL": "http://127.0.0.1:4000", "ANTHROPIC_AUTH_TOKEN": "tok",
		"ANTHROPIC_DEFAULT_OPUS_MODEL": "oc-worker", "ANTHROPIC_DEFAULT_SONNET_MODEL": "oc-worker",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL": "oc-small", "ANTHROPIC_DEFAULT_FABLE_MODEL": "oc-strong",
		"CLAUDE_CODE_ATTRIBUTION_HEADER": "0", "API_TIMEOUT_MS": "3000000",
		"CLAUDE_CONFIG_DIR": filepath.Join(base, "claude-glm-pair"),
	}
	for k, v := range want {
		if sp.Env[k] != v {
			t.Errorf("%s = %q, want %q", k, sp.Env[k], v)
		}
	}
	b, _ := json.Marshal(sp)
	if strings.Contains(string(b), "secret-1") {
		t.Fatal("upstream key leaked into the spawn spec")
	}
	a := strings.Join(sp.Args, " ")
	if !strings.Contains(a, "--model oc-strong") || !strings.Contains(a, "--resume ef4d08a3-") {
		t.Errorf("args: %s", a)
	}
	if fi, err := os.Stat(want["CLAUDE_CONFIG_DIR"]); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("config dir: %v %v", fi, err)
	}
	f.ResumeClaudeSessionID = "--dangerously-skip-permissions"
	if _, err := BuildSpawn(f, []string{root}, p); err == nil {
		t.Error("bad resume id accepted")
	}
	f.ResumeClaudeSessionID = ""
	f.Profile.Upstreams["evil"] = proto.Upstream{BaseURL: "file:///etc/passwd", Model: "x"}
	if _, err := BuildSpawn(f, []string{root}, p); err == nil {
		t.Error("file:// upstream accepted")
	}
}

// The dedicated server must never read the user's tmux.conf (continuum restore).
func TestTmuxIgnoresUserConfig(t *testing.T) {
	bin := "/usr/bin/tmux"
	if _, err := os.Stat(bin); err != nil {
		t.Skip("no tmux")
	}
	if os.Getenv("MADO_NO_TMUX_TESTS") != "" {
		t.Skip("MADO_NO_TMUX_TESTS set")
	}
	home := t.TempDir()
	marker := filepath.Join(home, "conf-was-read")
	os.WriteFile(filepath.Join(home, ".tmux.conf"), []byte("run-shell 'touch "+marker+"'\n"), 0o644)
	t.Setenv("HOME", home)
	t.Setenv("TMUX_TMPDIR", home)
	tm := Tmux{Bin: bin, Socket: "mado-test"}
	defer exec.Command(bin, "-L", "mado-test", "-f", "/dev/null", "kill-server").Run()
	if err := tm.StartWindow(context.Background(), "s-12345678", []string{"/bin/sleep", "30"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("user tmux.conf was loaded")
	}
	if ws := tm.Windows(context.Background()); len(ws) != 1 || ws[0] != "s-12345678" {
		t.Fatalf("windows %v", ws)
	}
}

func TestBuildSpawnModelMetadata(t *testing.T) {
	root := t.TempDir()
	p := SpawnParams{ClaudeBin: "/c", ShimBin: "/oc", SocketPath: "/run/x.sock", ClaudeConfigBase: t.TempDir(),
		RouterURL: "http://127.0.0.1:8742", RouterToken: "tok"}
	prof := proto.Profile{Name: "p", Kind: proto.KindRouter, Main: "oc-a", Upstreams: map[string]proto.Upstream{
		"oc-a": {BaseURL: "http://a:1", Model: "A", ContextWindow: 131072, MaxOutput: 32000},
		"oc-b": {BaseURL: "http://b:1", Model: "B", ContextWindow: 8192},
	}}
	sp, err := BuildSpawn(startFrame(root, prof), []string{root}, p)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] != "131072" || sp.Env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"] != "32000" {
		t.Fatalf("env %v", sp.Env)
	}
	f := startFrame(root, prof)
	f.MainModel = "oc-b"
	sp, _ = BuildSpawn(f, []string{root}, p)
	if sp.Env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] != "8192" {
		t.Fatalf("main oc-b: %v", sp.Env)
	}
	if _, ok := sp.Env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"]; ok {
		t.Fatal("max output set without max_output")
	}
}

func TestUseScope(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/systemd-run", nil }
	missing := func(string) (string, error) { return "", os.ErrNotExist }
	underSystemd := func(k string) string {
		if k == "INVOCATION_ID" {
			return "abc"
		}
		return ""
	}
	none := func(string) string { return "" }
	cases := []struct {
		mode string
		env  func(string) string
		look func(string) (string, error)
		want string
	}{
		{"auto", underSystemd, found, "/usr/bin/systemd-run"},
		{"auto", none, found, ""},
		{"auto", underSystemd, missing, ""},
		{"always", none, found, "/usr/bin/systemd-run"},
		{"never", underSystemd, found, ""},
	}
	for _, c := range cases {
		if got := UseScope(c.mode, c.env, c.look); got != c.want {
			t.Errorf("%s: %q", c.mode, got)
		}
	}
}

func TestTmuxServerArgv(t *testing.T) {
	plain := Tmux{Bin: "/usr/bin/tmux", Socket: "mado"}
	a := strings.Join(plain.serverArgv([]string{"new-session", "-d"}), " ")
	if a != "/usr/bin/tmux -L mado -f /dev/null new-session -d" {
		t.Fatal(a)
	}
	scoped := Tmux{Bin: "/usr/bin/tmux", Socket: "mado", SystemdRun: "/usr/bin/systemd-run"}
	a = strings.Join(scoped.serverArgv([]string{"new-session", "-d"}), " ")
	if a != "/usr/bin/systemd-run --user --scope --quiet --collect --unit mado-tmux -- /usr/bin/tmux -L mado -f /dev/null new-session -d" {
		t.Fatal(a)
	}
}

func TestShimArgvAndDefaults(t *testing.T) {
	a := strings.Join(ShimArgv("/bin/mado", "abcdef12-x", "/run/m", "/st/m", "", 0), " ")
	if a != "/bin/mado shim --session abcdef12-x --runtime-dir /run/m --spool-dir /st/m" {
		t.Fatal(a)
	}
	a = strings.Join(ShimArgv("/bin/mado", "abcdef12-x", "/run/m", "/st/m", "ef4d08a3-0fb3-45bf-927f-28034415014c", 17), " ")
	if !strings.HasSuffix(a, "--resume ef4d08a3-0fb3-45bf-927f-28034415014c --seq-base 17") {
		t.Fatal(a)
	}
	dir := shortHome(t) // runtime_dir holds sockets: keep it short (macOS TMPDIR is long)
	cfgPath := filepath.Join(dir, "w.toml")
	os.WriteFile(cfgPath, []byte(`hub_url = "ws://h/ws/worker"
machine_name = "m"
project_roots = ["`+dir+`"]
runtime_dir = "`+dir+`/run"
`), 0o600)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	c, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.RouterListen != "127.0.0.1:8742" || c.SpoolDir != filepath.Join(dir, "state", "mado", "spool") || c.TmuxScope != "auto" {
		t.Fatalf("%+v", c)
	}
}

// argAfter returns the value after flag in args, or "" (and false) when absent.
func argAfter(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func aiandPair() proto.Profile {
	return proto.Profile{Name: "aiand-pair", Kind: proto.KindRouter, Main: "aiand-glm53",
		Aliases: map[string]string{"fable": "aiand-glm53", "opus": "aiand-dsv4pro", "sonnet": "aiand-dsv4flash", "haiku": "aiand-dsv4flash"},
		Upstreams: map[string]proto.Upstream{
			"aiand-glm53":     {BaseURL: "https://api.example.org", Model: "zai-org/GLM-5.3"},
			"aiand-dsv4pro":   {BaseURL: "https://api.example.org", Model: "deepseek-ai/deepseek-v4-pro"},
			"aiand-dsv4flash": {BaseURL: "https://api.example.org", Model: "deepseek-ai/deepseek-v4-flash"},
		}}
}

func TestBuildSpawnRoles(t *testing.T) {
	root := t.TempDir()
	p := SpawnParams{ClaudeBin: "/c", ShimBin: "/oc", SocketPath: "/run/x.sock", ClaudeConfigBase: t.TempDir(),
		RouterURL: "http://127.0.0.1:8742", RouterToken: "tok"}
	f := startFrame(root, aiandPair())
	sp, err := BuildSpawn(f, []string{root}, p)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := argAfter(sp.Args, "--model"); v != "aiand-glm53" {
		t.Errorf("--model %q, want the profile main", v)
	}
	if v, _ := argAfter(sp.Args, "--permission-mode"); v != "default" {
		t.Errorf("--permission-mode %q", v)
	}
	if v, _ := argAfter(sp.Args, "--agents"); v != roles.AgentsJSON(nil) {
		t.Errorf("--agents %s", v)
	}
	var agents map[string]roles.Agent
	v, _ := argAfter(sp.Args, "--agents")
	if err := json.Unmarshal([]byte(v), &agents); err != nil || agents["implementer"].Model != "opus" ||
		agents["researcher"].Model != "opus" || agents["simple"].Model != "sonnet" {
		t.Errorf("agents %v %v", agents, err)
	}
	if v, _ := argAfter(sp.Args, "--append-system-prompt"); v != roles.DefaultPrompt {
		t.Errorf("--append-system-prompt %q", v)
	}
	if !slices.Contains(sp.Args, "--forward-subagent-text") {
		t.Error("--forward-subagent-text missing")
	}
	// The guard hook runs this mado binary (ShimBin) in exec form, edits by default.
	if v, _ := argAfter(sp.Args, "--settings"); v != roles.GuardSettings("edits", "/oc") ||
		!strings.Contains(v, `"command":"/oc","args":["guard-tool","--enforce","edits"]`) {
		t.Errorf("--settings %s", v)
	}
	// The aliases the agents use resolve to the profile's upstreams.
	if sp.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != "aiand-dsv4pro" || sp.Env["ANTHROPIC_DEFAULT_SONNET_MODEL"] != "aiand-dsv4flash" {
		t.Errorf("env %v", sp.Env)
	}

	f.PermissionMode = "acceptEdits"
	sp, err = BuildSpawn(f, []string{root}, p)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := argAfter(sp.Args, "--permission-mode"); v != "acceptEdits" {
		t.Errorf("--permission-mode %q", v)
	}
	f.PermissionMode = "auto"
	if sp, err := BuildSpawn(f, []string{root}, p); err != nil {
		t.Fatal(err)
	} else if v, _ := argAfter(sp.Args, "--permission-mode"); v != "auto" {
		t.Errorf("--permission-mode %q", v)
	}
	for _, m := range []string{"plan", "dontAsk", "bypassPermissions", "manual", "Auto"} {
		f.PermissionMode = m
		if _, err := BuildSpawn(f, []string{root}, p); err == nil {
			t.Errorf("permission mode %q accepted", m)
		}
	}
	f.PermissionMode = ""

	off := false
	f.Profile.Roles = &proto.Roles{Enabled: &off}
	sp, _ = BuildSpawn(f, []string{root}, p)
	for _, fl := range []string{"--agents", "--append-system-prompt", "--forward-subagent-text", "--settings"} {
		if slices.Contains(sp.Args, fl) {
			t.Errorf("roles disabled but %s present", fl)
		}
	}
	f.Profile.Roles = &proto.Roles{Implementer: "aiand-glm53"}
	if _, err := BuildSpawn(f, []string{root}, p); err == nil {
		t.Error("router name accepted as a role alias")
	}
	// enforce: off drops the hook, all adds Bash, anything else is refused.
	f.Profile.Roles = &proto.Roles{Enforce: "off"}
	sp, _ = BuildSpawn(f, []string{root}, p)
	if slices.Contains(sp.Args, "--settings") || !slices.Contains(sp.Args, "--agents") {
		t.Errorf("enforce off: %q", sp.Args)
	}
	f.Profile.Roles = &proto.Roles{Enforce: "all"}
	sp, _ = BuildSpawn(f, []string{root}, p)
	if v, _ := argAfter(sp.Args, "--settings"); !strings.Contains(v, `"matcher":"Edit|Write|NotebookEdit|Bash|PowerShell"`) {
		t.Errorf("enforce all: %s", v)
	}
	f.Profile.Roles = &proto.Roles{Enforce: "bash"}
	if _, err := BuildSpawn(f, []string{root}, p); err == nil {
		t.Error("enforce bash accepted")
	}

	// Subscription: the same block with the real aliases.
	sub := startFrame(root, proto.Profile{Name: "subscription", Kind: proto.KindSubscription,
		Roles: &proto.Roles{Implementer: "opus", Simple: "haiku", Prompt: "Delegate."}})
	sub.MainModel = "fable"
	sp, err = BuildSpawn(sub, []string{root}, p)
	if err != nil {
		t.Fatal(err)
	}
	v, _ = argAfter(sp.Args, "--agents")
	if err := json.Unmarshal([]byte(v), &agents); err != nil || agents["simple"].Model != "haiku" {
		t.Errorf("subscription agents %s", v)
	}
	if v, _ := argAfter(sp.Args, "--append-system-prompt"); v != "Delegate." {
		t.Errorf("custom prompt %q", v)
	}
}

// M8: hub tools reach the MCP server argv, web_search is pre-allowed, and the
// researcher gets both; unknown names from a newer hub are ignored.
func TestBuildSpawnHubTools(t *testing.T) {
	root := t.TempDir()
	p := SpawnParams{ClaudeBin: "/c", ShimBin: "/oc", SocketPath: "/run/x.sock", ClaudeConfigBase: t.TempDir()}
	f := startFrame(root, proto.Profile{Name: "subscription", Kind: proto.KindSubscription})
	sp, err := BuildSpawn(f, []string{root}, p)
	if err != nil {
		t.Fatal(err)
	}
	a := strings.Join(sp.Args, " ")
	if strings.Contains(a, "--tools") || strings.Contains(a, "--allowedTools") || strings.Contains(a, "mcp__mado__web_search") {
		t.Fatalf("tools offered without the hub asking: %s", a)
	}
	f.Tools = []string{"web_search", "fetch_url", "teleport", "web_search"}
	sp, err = BuildSpawn(f, []string{root}, p)
	if err != nil {
		t.Fatal(err)
	}
	a = strings.Join(sp.Args, " ")
	for _, want := range []string{`"args":["approve-tool","--socket","/run/x.sock","--tools","web_search,fetch_url"]`,
		"--allowedTools mcp__mado__web_search ", `"Read","Grep","Glob","Bash","WebFetch","mcp__mado__web_search","mcp__mado__fetch_url"`} {
		if !strings.Contains(a, want) {
			t.Errorf("args lack %s:\n%s", want, a)
		}
	}
	if strings.Contains(a, "teleport") || strings.Contains(a, "allowedTools mcp__mado__fetch_url") {
		t.Errorf("args %s", a)
	}
}
