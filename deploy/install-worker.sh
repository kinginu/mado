#!/usr/bin/env bash
# Install the mado worker as a systemd user service on this machine.
#
#   deploy/install-worker.sh --hub wss://hub.tailnet.ts.net/ws/worker --name gpu1 \
#       --root ~/work --root ~/src [--binary bin/mado]
#
# Then enroll once:  ~/.local/bin/mado enroll --hub https://hub.tailnet.ts.net --name gpu1 --code <code>
set -euo pipefail

hub="" name="" binary="bin/mado" roots=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --hub) hub="$2"; shift 2 ;;
    --name) name="$2"; shift 2 ;;
    --root) roots+=("$2"); shift 2 ;;
    --binary) binary="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[[ -n "$hub" && -n "$name" && ${#roots[@]} -gt 0 ]] || { echo "need --hub, --name and at least one --root" >&2; exit 2; }
[[ "$name" =~ ^[A-Za-z0-9_-]+$ ]] || { echo "bad --name" >&2; exit 2; }

here="$(cd "$(dirname "$0")" && pwd)"
dest="$HOME/.local/bin/mado"
if [[ -e "$dest" && "$binary" -ef "$dest" ]] || cmp -s "$binary" "$dest" 2>/dev/null; then
  echo "binary already installed at $dest"
else
  install -D -m 0755 "$binary" "$dest"
fi
install -d -m 0700 "$HOME/.config/mado"

cfg="$HOME/.config/mado/worker.toml"
if [[ ! -e "$cfg" ]]; then
  toml_str() { local s="${1//\\/\\\\}"; s="${s//\"/\\\"}"; printf '"%s"' "$s"; }
  {
    echo "hub_url = $(toml_str "$hub")"
    echo "machine_name = $(toml_str "$name")"
    echo 'token_file = "~/.config/mado/machine.token"'
    printf 'project_roots = ['
    sep=""
    for r in "${roots[@]}"; do printf '%s%s' "$sep" "$(toml_str "$r")"; sep=", "; done
    echo ']'
    echo 'claude_bin = "~/.local/bin/claude"'
    echo 'tmux_socket = "mado"'
    echo '# A fixed port keeps running claude children reachable across a daemon restart.'
    echo 'router_listen = "127.0.0.1:8742"'
  } > "$cfg"
  chmod 0600 "$cfg"
  echo "wrote $cfg"
else
  echo "kept existing $cfg"
fi

unit_src="$here/mado-worker.service"
if [[ ! -f "$unit_src" ]]; then
  # Script copied without its deploy/ directory: use the embedded unit
  # (kept identical to deploy/mado-worker.service by `make check-deploy`).
  unit_src="$(mktemp)"
  trap 'rm -f "$unit_src"' EXIT
  cat > "$unit_src" <<'UNIT'
# systemd *user* unit. Installed by deploy/install-worker.sh into ~/.config/systemd/user/.
[Unit]
Description=mado worker (relays Claude Code sessions to the mado hub)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%h/.local/bin/mado worker --config %h/.config/mado/worker.toml
Restart=on-failure
RestartSec=5
# Sessions live in `tmux -L mado`, which the daemon starts through
# `systemd-run --user --scope --unit mado-tmux` (DESIGN §8), i.e. outside this unit's cgroup.
# So the default KillMode=control-group is safe: stopping or restarting the service never
# kills a session. Stop the sessions themselves with `tmux -f /dev/null -L mado kill-server`.
NoNewPrivileges=yes
PrivateTmp=no

[Install]
WantedBy=default.target
UNIT
fi
install -D -m 0644 "$unit_src" "$HOME/.config/systemd/user/mado-worker.service"
if ! loginctl enable-linger "$USER"; then
  echo "WARN: could not enable linger (needs root here); the service will stop at logout. Run as root: sudo loginctl enable-linger $USER" >&2
fi
systemctl --user daemon-reload
if [[ -e "$HOME/.config/mado/machine.token" ]]; then
  systemctl --user enable --now mado-worker.service
  echo "mado-worker.service enabled and started"
else
  systemctl --user enable mado-worker.service
  echo "enabled; enroll first, then: systemctl --user start mado-worker.service"
fi
