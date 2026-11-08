// Command mado is the single binary for the hub, the worker daemon, the per-session
// shim, the approval MCP server and machine enrollment.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kinginu/mado/internal/approve"
	"github.com/kinginu/mado/internal/hub"
	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/roles"
	"github.com/kinginu/mado/internal/shim"
	modelusage "github.com/kinginu/mado/internal/usage"
	"github.com/kinginu/mado/internal/worker"
)

var version = "dev"

const usage = `usage:
  mado local [--config ~/.config/mado/hub.toml] [--port 8740] [--no-browser]
                                                 run hub and worker on this computer (no server, no IdP)
  mado hub [--config hub.toml] [--local-login]   run the hub
  mado hub enroll <name> [--config hub.toml]     print a one-time enrollment code
  mado hub remove-machine <name> [--config hub.toml]
                                                 remove an offline machine and revoke its token
  mado enroll --hub <url> --name <name> --code <code> [--token-file path]
  mado worker [--config ~/.config/mado/worker.toml]
  mado shim --session <id> [--runtime-dir d] [--spool-dir d] [--resume <claude-id>] [--seq-base n]
                                                 (started by the worker inside tmux)
  mado approve-tool --socket <path> [--tools web_search,fetch_url|propose_task]
                                                 (started by claude as an MCP server)
  mado session-models <spool file>               print the models that did the work in a session
  mado guard-tool --enforce edits|all            (PreToolUse hook: keeps the main model from editing)
  mado print-argv --profile p.json --cwd d [--mode m] [--main-model n] [--config-base d] [--socket s] [--tools t]
  mado print-argv --job digest --cwd <job_dir> [--main-model n] [--job-system prompt.txt]
                                                 print the claude argv/env the worker would spawn
  mado version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	worker.Version = version
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "hub":
		err = runHub(ctx, os.Args[2:])
	case "local":
		err = runLocal(ctx, os.Args[2:])
	case "enroll":
		err = runEnroll(ctx, os.Args[2:])
	case "worker":
		err = runWorker(ctx, os.Args[2:])
	case "shim":
		err = runShim(ctx, os.Args[2:])
	case "approve-tool":
		err = runApprove(ctx, os.Args[2:])
	case "session-models":
		err = runSessionModels(os.Args[2:])
	case roles.GuardTool:
		os.Exit(runGuard(os.Stdin, os.Stdout, os.Stderr, os.Args[2:]))
	case "print-argv":
		err = runPrintArgv(os.Args[2:])
	case "version", "--version":
		fmt.Println(version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mado:", err)
		os.Exit(1)
	}
}

func logger() *slog.Logger { return slog.New(slog.NewTextHandler(os.Stderr, nil)) }

func runHub(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "enroll" {
		return runHubEnroll(args[1:])
	}
	if len(args) > 0 && args[0] == "remove-machine" {
		return runHubRemoveMachine(args[1:], os.Stdout)
	}
	fs := flag.NewFlagSet("hub", flag.ExitOnError)
	cfgPath := fs.String("config", "hub.toml", "hub config")
	local := fs.Bool("local-login", false, "offer the local login (needs a loopback listen address)")
	fs.BoolVar(local, "dev-login", false, "") // old name, kept as an alias
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: mado hub [--config hub.toml] [--local-login]")
		fs.VisitAll(func(f *flag.Flag) {
			if f.Usage != "" {
				fmt.Fprintf(fs.Output(), "  -%s\n    \t%s\n", f.Name, f.Usage)
			}
		})
	}
	fs.Parse(args)
	cfg, err := hub.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *local {
		if err := hub.LocalLoginAllowed(cfg.Listen); err != nil {
			return err
		}
	}
	secrets, err := hub.LoadSecrets(cfg.SecretsFile)
	if err != nil {
		return err
	}
	store, err := hub.OpenStore(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	log := logger()
	h, err := hub.New(cfg, store, secrets, hub.Options{LocalLogin: *local, Log: log})
	if err != nil {
		return err
	}
	defer h.Close() // running chat turns store their partial answers before the store closes
	srv := &http.Server{Addr: cfg.Listen, Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Info("hub listening", "addr", cfg.Listen, "base_url", cfg.BaseURL, "local_login", *local)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runHubEnroll(args []string) error {
	fs := flag.NewFlagSet("hub enroll", flag.ExitOnError)
	cfgPath := fs.String("config", "hub.toml", "hub config")
	// Allow `hub enroll <name> --config x` as well as flags first.
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fs.Parse(args)
	if name == "" {
		name = fs.Arg(0)
	}
	if name == "" {
		return errors.New("usage: mado hub enroll <name>")
	}
	cfg, err := hub.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	store, err := hub.OpenStore(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	code, err := store.CreateEnrollCode(name, 15*time.Minute)
	if err != nil {
		return err
	}
	fmt.Printf("One-time code for machine %q (valid 15 minutes):\n\n  %s\n\nOn the machine run:\n\n  mado enroll --hub %s --name %s --code %s\n",
		name, code, cfg.BaseURL, name, code)
	return nil
}

// runHubRemoveMachine removes a machine through the database (DESIGN §24). The hub
// keeps a connected flag per machine there, so the CLI refuses a connected worker
// without having to reach the running hub. Sessions there that had not ended are
// marked exited; a running hub shows the new state on the next page load.
func runHubRemoveMachine(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("hub remove-machine", flag.ExitOnError)
	cfgPath := fs.String("config", "hub.toml", "hub config")
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fs.Parse(args)
	if name == "" {
		name = fs.Arg(0)
	}
	if name == "" {
		return errors.New("usage: mado hub remove-machine <name>")
	}
	cfg, err := hub.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	store, err := hub.OpenStore(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	ended, err := store.RemoveMachine(name)
	if err != nil {
		return fmt.Errorf("cannot remove machine %q: %w", name, err)
	}
	store.Audit("cli", "machine.remove", fmt.Sprintf("%s ended_sessions=%d", name, len(ended)))
	fmt.Fprintf(out, "removed machine %q; its token no longer works (enroll the name again to reuse it)\n", name)
	fmt.Fprintf(out, "ended %d session(s) that were still live on it\n", len(ended))
	for _, id := range ended {
		fmt.Fprintf(out, "  %s\n", id)
	}
	return nil
}

func runEnroll(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	hubURL := fs.String("hub", "", "hub base URL (https://...)")
	name := fs.String("name", "", "machine name")
	code := fs.String("code", "", "one-time code from `mado hub enroll`")
	tokenFile := fs.String("token-file", "~/.config/mado/machine.token", "where to store the machine token")
	fs.Parse(args)
	if *hubURL == "" || *name == "" || *code == "" {
		return errors.New("--hub, --name and --code are required")
	}
	u, err := url.Parse(strings.TrimRight(*hubURL, "/"))
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	}
	u.Path = strings.TrimSuffix(u.Path, "/ws/worker")
	body, _ := json.Marshal(map[string]string{"name": *name, "code": *code})
	req, _ := http.NewRequestWithContext(ctx, "POST", u.String()+"/enroll", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("enroll refused: %s", resp.Status)
	}
	var out struct{ Token string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		return errors.New("bad enroll response")
	}
	path := worker.ExpandHome(*tokenFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(out.Token+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	fmt.Printf("enrolled %q; token written to %s\n", *name, path)
	return nil
}

func runWorker(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	cfgPath := fs.String("config", "~/.config/mado/worker.toml", "worker config")
	fs.Parse(args)
	cfg, err := worker.LoadConfig(worker.ExpandHome(*cfgPath))
	if err != nil {
		return err
	}
	d, err := worker.New(cfg, logger())
	if err != nil {
		return err
	}
	return d.Run(ctx)
}

func runShim(_ context.Context, args []string) error {
	err := shim.Main(args, logger())
	if err != nil {
		fmt.Fprintln(os.Stderr, "shim:", err)
		time.Sleep(30 * time.Second) // keep the tmux window readable for a moment
	}
	return err
}

func runApprove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("approve-tool", flag.ExitOnError)
	sock := fs.String("socket", "", "shim socket")
	toolList := fs.String("tools", "", "hub tools to offer, comma separated (web_search,fetch_url)")
	fs.Parse(args)
	if *sock == "" {
		return errors.New("--socket is required")
	}
	o := approve.Options{Decide: approve.SocketDecider(*sock)}
	if *toolList != "" {
		o.Tools = strings.Split(*toolList, ",")
		o.Call = approve.SocketToolCaller(*sock, approve.ToolTimeout)
	}
	return approve.ServeWith(ctx, os.Stdin, os.Stdout, o)
}

// runSessionModels prints the model usage table of a spool (or a stream-json file).
func runSessionModels(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: mado session-models <spool file>")
	}
	f, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer f.Close()
	a, err := modelusage.ReadJSONL(f)
	if err != nil {
		return err
	}
	s := a.Stats()
	fmt.Print(s.Table())
	if short := s.Short(); short != "" {
		fmt.Println("Summary:", short)
	}
	return nil
}

// runGuard is the PreToolUse hook (DESIGN §16). It denies a blocked tool when the main
// model calls it and stays silent for subagents and other tools. Unreadable input
// exits 2, which Claude Code treats as a block with stderr as the reason.
func runGuard(in io.Reader, out, errOut io.Writer, args []string) int {
	fs := flag.NewFlagSet(roles.GuardTool, flag.ContinueOnError)
	fs.SetOutput(errOut)
	enforce := fs.String("enforce", roles.DefaultEnforce, "edits or all")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	msg, err := roles.Guard(in, *enforce)
	if err != nil {
		fmt.Fprintln(errOut, "mado guard-tool:", err)
		return 2
	}
	if msg != "" {
		out.Write(roles.DenyOutput(msg))
		fmt.Fprintln(out)
	}
	return 0
}

// runPrintArgv prints the spawn spec BuildSpawn makes for a profile, so the exact
// argv can be run by hand. The router token is a placeholder.
func runPrintArgv(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return printArgv(args, exe, os.Stdout)
}

// printArgv is runPrintArgv with the mado binary path and the output given. With
// --job digest it prints a job session's spawn (DESIGN §23.6): --cwd is then the
// job_dir and --job-system names a file with the system prompt.
func printArgv(args []string, exe string, out io.Writer) error {
	fs := flag.NewFlagSet("print-argv", flag.ContinueOnError)
	profPath := fs.String("profile", "", "profile JSON (proto.Profile, as the hub sends it); default for a job: subscription")
	cwd := fs.String("cwd", "", "session cwd (a job: the worker's job_dir)")
	mode := fs.String("mode", "default", "permission mode")
	main := fs.String("main-model", "", "main model (default: the profile's; a job: sonnet)")
	base := fs.String("config-base", "", "CLAUDE_CONFIG_DIR base for router profiles (created)")
	routerURL := fs.String("router-url", "http://127.0.0.1:7655", "router URL to put in the env")
	claudeBin := fs.String("claude", "claude", "claude binary")
	sock := fs.String("socket", "/nonexistent/mado.sock", "shim socket for the approval MCP tool")
	job := fs.String("job", "", "job kind (digest): print a job session's argv")
	jobSystem := fs.String("job-system", "", "file with the job's system prompt (default: a placeholder)")
	toolList := fs.String("tools", "", "hub tools the session is offered, comma separated")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cwd == "" || (*profPath == "" && *job == "") {
		return errors.New("--cwd and --profile (or --job) are required")
	}
	prof := proto.Profile{Name: "subscription", Kind: proto.KindSubscription}
	if *profPath != "" {
		b, err := os.ReadFile(*profPath)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &prof); err != nil {
			return fmt.Errorf("profile: %w", err)
		}
	}
	if prof.Kind == proto.KindRouter && *base == "" {
		return errors.New("--config-base is required for a router profile")
	}
	abs, err := filepath.Abs(*cwd)
	if err != nil {
		return err
	}
	f := &proto.StartSession{SessionID: proto.NewID(), CWD: abs, Profile: prof, MainModel: *main, PermissionMode: *mode}
	if *toolList != "" {
		f.Tools = strings.Split(*toolList, ",")
	}
	params := worker.SpawnParams{ClaudeBin: *claudeBin, ShimBin: exe, SocketPath: *sock,
		RouterURL: *routerURL, RouterToken: "<router-token>", ClaudeConfigBase: *base}
	if *job != "" {
		sys := "<the digest prompt>"
		if *jobSystem != "" {
			b, err := os.ReadFile(*jobSystem)
			if err != nil {
				return err
			}
			sys = string(b)
		}
		f.Job = &proto.Job{Kind: *job, MainModel: *main, System: sys}
		if *toolList == "" {
			f.Tools = []string{"propose_task"}
		}
		params.JobDir = abs
	}
	sp, err := worker.BuildSpawn(f, []string{abs}, params)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(sp)
}
