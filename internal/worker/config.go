package worker

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultRouterListen is the router's fixed loopback address (DESIGN §8).
const DefaultRouterListen = "127.0.0.1:8742"

// Config is ~/.config/mado/worker.toml (DESIGN §8).
type Config struct {
	HubURL       string   `toml:"hub_url"`
	MachineName  string   `toml:"machine_name"`
	TokenFile    string   `toml:"token_file"`
	ProjectRoots []string `toml:"project_roots"`
	ClaudeBin    string   `toml:"claude_bin"`
	TmuxSocket   string   `toml:"tmux_socket"`
	RouterListen string   `toml:"router_listen"`
	// Optional.
	TmuxBin          string `toml:"tmux_bin"`           // default /usr/bin/tmux or PATH lookup
	RuntimeDir       string `toml:"runtime_dir"`        // default $XDG_RUNTIME_DIR/mado (sockets)
	SpoolDir         string `toml:"spool_dir"`          // default $XDG_STATE_HOME/mado/spool (survives reboot)
	TmuxScope        string `toml:"tmux_scope"`         // auto (default), always, never: start tmux via systemd-run --user --scope
	ClaudeConfigBase string `toml:"claude_config_base"` // default ~/.mado
	ShimBin          string `toml:"shim_bin"`           // default: this executable
	// SpoolRetention: how long the spool and token files of a session the hub reports
	// as dead are kept (Go duration, default 168h).
	SpoolRetention string `toml:"spool_retention"`
	// ClaudeHistory: ship ~/.claude/projects/*/*.jsonl (and the per-profile config dirs
	// under claude_config_base) to the hub, read-only (DESIGN §14). Default false.
	ClaudeHistory bool `toml:"claude_history"`
	// ClaudeHome: the default Claude Code config dir scanned for history (default ~/.claude).
	ClaudeHome string `toml:"claude_home"`
	// Jobs: the operator asserts that this machine's claude is logged in to the
	// subscription; the worker then runs job sessions (inbox digests, DESIGN §23.6).
	// Default false.
	Jobs bool `toml:"jobs"`
	// JobDir is the cwd of job sessions (default $XDG_STATE_HOME/mado/jobs, created at
	// start when jobs = true). It need not be inside project_roots.
	JobDir string `toml:"job_dir"`

	spoolRetention time.Duration
}

// defaultTmuxBin: /usr/bin/tmux, else tmux on PATH, else the Homebrew locations (a
// process started from launchd or Finder has no Homebrew in PATH); "" when none exists.
func defaultTmuxBin(lookPath func(string) (string, error)) string {
	if _, err := os.Stat("/usr/bin/tmux"); err == nil {
		return "/usr/bin/tmux"
	}
	if p, err := lookPath("tmux"); err == nil {
		return p
	}
	for _, p := range []string{"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// defaultClaudeBin: the native installer's ~/.local/bin/claude, else claude on PATH;
// the installer path when neither exists, so New's error names it.
func defaultClaudeBin(lookPath func(string) (string, error)) string {
	native := ExpandHome("~/.local/bin/claude")
	if _, err := os.Stat(native); err == nil {
		return native
	}
	if p, err := lookPath("claude"); err == nil {
		return p
	}
	return native
}

// Retention is the parsed spool_retention.
func (c *Config) Retention() time.Duration { return c.spoolRetention }

func LoadConfig(path string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, err
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("%s: unknown keys %v", path, und)
	}
	if err := c.normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// LocalConfig is the worker config of `mado local` (DESIGN §19.2): path is read when it
// exists (a missing file is an empty config), then the hub URL, machine name and token
// file of the in-process hub replace whatever the file says, and project_roots
// defaults to the home directory.
func LocalConfig(path, hubURL, machineName, tokenFile string) (*Config, error) {
	var c Config
	if _, err := os.Stat(path); err == nil {
		md, err := toml.DecodeFile(path, &c)
		if err != nil {
			return nil, err
		}
		if und := md.Undecoded(); len(und) > 0 {
			return nil, fmt.Errorf("%s: unknown keys %v", path, und)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	c.HubURL, c.MachineName, c.TokenFile = hubURL, machineName, tokenFile
	if len(c.ProjectRoots) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		c.ProjectRoots = []string{home}
	}
	if err := c.normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// MaxSocketPath is the longest unix socket path every supported OS accepts: sun_path
// is 104 bytes on macOS (108 on Linux), including the terminating NUL.
const MaxSocketPath = 103

// socketNameLen is the length of "/<session uuid>.sock" under runtime_dir.
const socketNameLen = 1 + 36 + len(".sock")

// ExpandHome expands a leading ~/.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func (c *Config) normalize() error {
	u, err := url.Parse(c.HubURL)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return fmt.Errorf("hub_url must be ws:// or wss://")
	}
	if c.MachineName == "" {
		return fmt.Errorf("machine_name is required")
	}
	if c.TokenFile == "" {
		c.TokenFile = "~/.config/mado/machine.token"
	}
	c.TokenFile = ExpandHome(c.TokenFile)
	if len(c.ProjectRoots) == 0 {
		return fmt.Errorf("project_roots is empty")
	}
	for i, r := range c.ProjectRoots {
		r = ExpandHome(r)
		if !filepath.IsAbs(r) {
			return fmt.Errorf("project root %q is not absolute", r)
		}
		if real, err := filepath.EvalSymlinks(r); err == nil {
			r = real
		}
		c.ProjectRoots[i] = filepath.Clean(r)
	}
	if c.ClaudeBin == "" {
		c.ClaudeBin = defaultClaudeBin(exec.LookPath)
	}
	c.ClaudeBin = ExpandHome(c.ClaudeBin)
	if c.TmuxSocket == "" {
		c.TmuxSocket = "mado"
	}
	if c.RouterListen == "" {
		// Fixed so a claude child keeps reaching the router across a daemon restart.
		c.RouterListen = DefaultRouterListen
	}
	if c.TmuxBin == "" {
		c.TmuxBin = defaultTmuxBin(exec.LookPath) // "" when not found: New reports it
	}
	c.TmuxBin = ExpandHome(c.TmuxBin)
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		state = "~/.local/state"
	}
	if c.RuntimeDir == "" {
		// macOS has no XDG_RUNTIME_DIR; the state dir is private (0700) and short
		// enough for socket paths (DESIGN §19.4).
		if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
			c.RuntimeDir = filepath.Join(xdg, "mado")
		} else {
			c.RuntimeDir = filepath.Join(ExpandHome(state), "mado", "run")
		}
	}
	c.RuntimeDir = ExpandHome(c.RuntimeDir)
	if n := len(c.RuntimeDir) + socketNameLen; n > MaxSocketPath {
		return fmt.Errorf("runtime_dir %q is too long: session socket paths would be %d bytes, the limit is %d (macOS); choose a shorter runtime_dir",
			c.RuntimeDir, n, MaxSocketPath)
	}
	if c.SpoolDir == "" {
		c.SpoolDir = filepath.Join(ExpandHome(state), "mado", "spool")
	}
	c.SpoolDir = ExpandHome(c.SpoolDir)
	switch c.TmuxScope {
	case "":
		c.TmuxScope = "auto"
	case "auto", "always", "never":
	default:
		return fmt.Errorf("tmux_scope must be auto, always or never")
	}
	if c.ClaudeConfigBase == "" {
		c.ClaudeConfigBase = "~/.mado"
	}
	c.ClaudeConfigBase = ExpandHome(c.ClaudeConfigBase)
	if c.ShimBin == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		c.ShimBin = exe
	}
	c.ShimBin = ExpandHome(c.ShimBin)
	c.spoolRetention = DefaultSpoolRetention
	if c.SpoolRetention != "" {
		d, err := time.ParseDuration(c.SpoolRetention)
		if err != nil || d < time.Hour {
			return fmt.Errorf("spool_retention must be a Go duration of at least 1h (e.g. \"168h\")")
		}
		c.spoolRetention = d
	}
	if c.ClaudeHome == "" {
		c.ClaudeHome = "~/.claude"
	}
	c.ClaudeHome = ExpandHome(c.ClaudeHome)
	if c.JobDir == "" {
		c.JobDir = filepath.Join(ExpandHome(state), "mado", "jobs")
	}
	c.JobDir = filepath.Clean(ExpandHome(c.JobDir))
	if !filepath.IsAbs(c.JobDir) {
		return fmt.Errorf("job_dir %q is not absolute", c.JobDir)
	}
	return nil
}

// JobsReady reports whether the worker advertises the jobs capability: jobs = true
// and a subscription login, i.e. <claude_home>/.credentials.json on Linux. On macOS
// Claude Code keeps the login in the keychain, which a daemon cannot cheaply check, so
// jobs = true is taken at its word there.
func (c *Config) JobsReady(goos string) bool {
	if !c.Jobs {
		return false
	}
	if goos == "darwin" {
		return true
	}
	fi, err := os.Stat(filepath.Join(c.ClaudeHome, ".credentials.json"))
	return err == nil && fi.Mode().IsRegular()
}
