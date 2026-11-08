package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kinginu/mado/internal/hub"
	"github.com/kinginu/mado/internal/worker"
)

// Local mode (DESIGN §19.2): the hub and a worker in one process on one computer.

const (
	localPort       = 8740
	localConfigDir  = "~/.config/mado"
	localWorkerConf = localConfigDir + "/worker.toml"
	localTokenFile  = localConfigDir + "/machine.token"
)

// localHubTOML is the hub.toml written on the first run of `mado local`.
const localHubTOML = `# mado local mode (DESIGN §19). Written by the first run of ` + "`mado local`" + `.
# The hub listens on loopback only; the local login needs no identity provider, so
# no [oidc] section is needed.
listen = "%[1]s"
base_url = "http://%[1]s"
db_path = "~/.local/share/mado/hub.db"
secrets_file = "%[2]s"   # mode 0600: [keys] name = "value"

# Code sessions on the built-in "subscription" profile (plain claude, your normal
# Claude Code login) need nothing here. Chat models and router profiles need an API
# key: put it in the secrets file as
#
#   [keys]
#   aiand = "sk-..."
#
# and uncomment an example below, then restart ` + "`mado local`" + `.

# [chat]
# enabled = true
#
# [[chat.models]]
# name = "glm-5.3"
# label = "GLM 5.3"
# base_url = "https://api.aiand.com"
# model = "zai-org/glm-5.3"
# key_ref = "aiand"
# context_window = 1048576
# default = true

# [[profiles]]
# name = "glm"
# kind = "router"
# main = "strong"
# [profiles.aliases]
# fable = "strong"
# opus = "strong"
# sonnet = "strong"
# haiku = "strong"
# [profiles.upstreams.strong]
# base_url = "https://api.aiand.com"
# model = "zai-org/glm-5.3"
# key_ref = "aiand"
# context_window = 1048576
`

// localEnv is what runLocal needs from the outside world; tests replace it.
type localEnv struct {
	out         io.Writer
	log         *slog.Logger
	openBrowser func(url string) error // nil: never
	// ready is called once the hub accepts connections and the worker is starting.
	ready func(addr string, store *hub.Store)
}

func runLocal(ctx context.Context, args []string) error {
	env := localEnv{out: os.Stdout, log: logger()}
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		env.openBrowser = openBrowser
	}
	return localMain(ctx, args, env)
}

func localMain(ctx context.Context, args []string, env localEnv) error {
	fs := flag.NewFlagSet("local", flag.ContinueOnError)
	fs.SetOutput(env.out)
	cfgPath := fs.String("config", localConfigDir+"/hub.toml", "hub config (written on the first run)")
	port := fs.Int("port", localPort, "loopback port of the hub")
	noBrowser := fs.Bool("no-browser", false, "do not open the browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	portSet := false
	fs.Visit(func(f *flag.Flag) { portSet = portSet || f.Name == "port" })
	if *port < 0 || *port > 65535 {
		return errors.New("--port must be 0..65535")
	}
	path := worker.ExpandHome(*cfgPath)
	if err := writeLocalHubConfig(env.out, path, *port); err != nil {
		return err
	}
	override := -1
	if portSet {
		override = *port
	}
	cfg, err := hub.LoadLocalConfig(path, *port, override)
	if err != nil {
		return err
	}
	if err := hub.LocalLoginAllowed(cfg.Listen); err != nil {
		return fmt.Errorf("mado local serves loopback only: %w", err)
	}
	if err := writeLocalSecrets(env.out, cfg.SecretsFile); err != nil {
		return err
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
	tokPath := worker.ExpandHome(localTokenFile)
	machine, err := ensureLocalMachine(store, tokPath, localMachineName())
	if err != nil {
		return err
	}
	h, err := hub.New(cfg, store, secrets, hub.Options{LocalLogin: true, LocalMode: true, Log: env.log})
	if err != nil {
		return err
	}
	defer h.Close()

	// Listen before the worker dials, so its first connect succeeds.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	wcfg, err := worker.LocalConfig(worker.ExpandHome(localWorkerConf), "ws://"+addr+"/ws/worker", machine, tokPath)
	if err != nil {
		ln.Close()
		return err
	}
	d, err := worker.New(wcfg, env.log)
	if err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second}
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(ln) }()
	wctx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	workerErr := make(chan error, 1)
	go func() { workerErr <- d.Run(wctx) }()
	env.log.Info("mado local", "addr", addr, "base_url", cfg.BaseURL, "machine", machine)
	fmt.Fprintf(env.out, "mado is running at %s (Ctrl-C stops it)\n", cfg.BaseURL)
	if env.ready != nil {
		env.ready(addr, store)
	}
	if !*noBrowser && env.openBrowser != nil {
		if err := env.openBrowser(cfg.BaseURL); err != nil {
			env.log.Warn("could not open the browser", "err", err)
		}
	}

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-srvErr:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	case err := <-workerErr:
		runErr = fmt.Errorf("worker: %w", err)
		workerErr <- nil // already stopped
	}
	// The HTTP server first, then the worker; claude keeps running in tmux.
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	stopWorker()
	if err := <-workerErr; err != nil && runErr == nil {
		runErr = fmt.Errorf("worker: %w", err)
	}
	return runErr
}

// writeLocalHubConfig writes hub.toml when it is missing.
func writeLocalHubConfig(out io.Writer, path string, port int) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	listen := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	body := fmt.Sprintf(localHubTOML, listen, hub.DefaultLocalSecrets)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s (local defaults; chat models are commented out)\n", path)
	return nil
}

// writeLocalSecrets writes an empty secrets file (0600) when it is missing.
func writeLocalSecrets(out io.Writer, path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body := "# API keys for chat models and router profiles: name = \"value\", used as key_ref.\n[keys]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s (mode 0600): put API keys under [keys] for chat models and router profiles;\n"+
		"code sessions on the subscription profile need none\n", path)
	return nil
}

// ensureLocalMachine pairs the in-process worker with the hub: the token file is the
// worker's credential, the machine row holds its hash. A missing file means a new
// token for the row named name. It returns the row's name.
func ensureLocalMachine(store *hub.Store, tokPath, name string) (string, error) {
	b, err := os.ReadFile(tokPath)
	switch {
	case err == nil:
		if fi, err := os.Stat(tokPath); err != nil || fi.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("%s must be mode 0600", tokPath)
		}
		tok := strings.TrimSpace(string(b))
		if tok == "" {
			return "", fmt.Errorf("%s is empty; delete it and run mado local again", tokPath)
		}
		if m, err := store.MachineByToken(tok); err == nil {
			return m.Name, nil
		}
		// A token from another hub database: let this one know it.
		return name, store.SetMachineToken(name, tok)
	case os.IsNotExist(err):
		tok := hub.RandomToken(32)
		if err := store.SetMachineToken(name, tok); err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(tokPath), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(tokPath, []byte(tok+"\n"), 0o600); err != nil {
			return "", err
		}
		return name, os.Chmod(tokPath, 0o600)
	default:
		return "", err
	}
}

// localMachineName is the short hostname as a machine name ([A-Za-z0-9_-]), "local"
// when nothing usable is left.
func localMachineName() string {
	h, _ := os.Hostname()
	return machineNameFrom(h)
}

func machineNameFrom(host string) string {
	host, _, _ = strings.Cut(host, ".")
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 64 {
		name = name[:64]
	}
	if name == "" {
		return "local"
	}
	return name
}

func openBrowser(url string) error {
	bin := "xdg-open"
	if runtime.GOOS == "darwin" {
		bin = "open"
	}
	cmd := exec.Command(bin, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
