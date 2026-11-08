package worker

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Tmux drives the dedicated server `tmux -L <socket>`. argv slices only.
type Tmux struct {
	Bin    string
	Socket string
	// SystemdRun, when set, is the systemd-run binary used to start the server in its
	// own transient user scope (unit ScopeUnit), outside the worker service's cgroup,
	// so that stopping or restarting the service never kills a session.
	SystemdRun string
	ScopeUnit  string
	Log        func(msg string, args ...any)
}

// ScopeUnitName is the transient scope the tmux server runs in.
const ScopeUnitName = "mado-tmux"

// UseScope decides whether the tmux server goes under systemd-run: mode "always",
// or "auto" when the daemon itself runs under systemd (INVOCATION_ID is set by
// systemd for every unit it starts) and systemd-run exists.
func UseScope(mode string, getenv func(string) string, lookPath func(string) (string, error)) string {
	if mode == "never" {
		return ""
	}
	p, err := lookPath("systemd-run")
	if err != nil {
		return ""
	}
	if mode == "always" || getenv("INVOCATION_ID") != "" {
		return p
	}
	return ""
}

func (t Tmux) base() []string { return []string{t.Bin, "-L", t.Socket, "-f", "/dev/null"} }

// serverArgv is the argv that starts the server (the first new-session).
func (t Tmux) serverArgv(args []string) []string {
	argv := append(t.base(), args...)
	if t.SystemdRun == "" {
		return argv
	}
	unit := t.ScopeUnit
	if unit == "" {
		unit = ScopeUnitName
	}
	return append([]string{t.SystemdRun, "--user", "--scope", "--quiet", "--collect", "--unit", unit, "--"}, argv...)
}

const tmuxSession = "mado"

func (t Tmux) run(ctx context.Context, args ...string) (string, error) {
	// -f /dev/null: never load the user's tmux.conf in our server. Plugins such as
	// tmux-continuum would otherwise restore the user's saved sessions into it (seen in
	// the M1 smoke test) and autosave our layout over theirs.
	return t.exec(ctx, append(t.base(), args...))
}

func (t Tmux) exec(ctx context.Context, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s: %v: %s", strings.Join(argv[:min(len(argv), 6)], " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// WindowName for a session id.
func WindowName(sessionID string) string { return "s-" + sessionID[:8] }

// StartWindow runs argv in a new detached window, creating the tmux session if needed.
// With more than one argument tmux execs argv directly, without a shell.
func (t Tmux) StartWindow(ctx context.Context, name string, argv []string) error {
	if _, err := t.run(ctx, "has-session", "-t", "="+tmuxSession); err != nil {
		args := append([]string{"new-session", "-d", "-s", tmuxSession, "-n", name, "-x", "200", "-y", "50", "--"}, argv...)
		if _, err := t.exec(ctx, t.serverArgv(args)); err != nil {
			if t.SystemdRun == "" {
				return err
			}
			// e.g. no user bus: fall back to a plain fork (dies with the service cgroup).
			if t.Log != nil {
				t.Log("systemd-run failed; starting tmux directly", "err", err)
			}
			if _, err := t.run(ctx, args...); err != nil {
				return err
			}
		}
		// Keep window names stable (they carry the session id).
		_, _ = t.run(ctx, "set-option", "-g", "-w", "automatic-rename", "off")
		_, _ = t.run(ctx, "set-option", "-g", "allow-rename", "off")
		_, _ = t.run(ctx, "set-option", "-w", "-t", "="+tmuxSession+":"+name, "automatic-rename", "off")
		return nil
	}
	args := append([]string{"new-window", "-d", "-t", "=" + tmuxSession + ":", "-n", name, "--"}, argv...)
	_, err := t.run(ctx, args...)
	return err
}

// Windows lists window names; an absent server yields an empty list.
func (t Tmux) Windows(ctx context.Context) []string {
	out, err := t.run(ctx, "list-windows", "-a", "-F", "#{window_name}")
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

// StartCommands maps window names to the command their pane was started with.
func (t Tmux) StartCommands(ctx context.Context) map[string]string {
	out, err := t.run(ctx, "list-panes", "-a", "-F", "#{window_name}\t#{pane_start_command}")
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if name, cmd, ok := strings.Cut(l, "\t"); ok {
			m[name] = cmd
		}
	}
	return m
}
