#!/usr/bin/env bash
# Install the mado hub as a systemd user service on the hub host.
#
#   deploy/install-hub.sh --base-url https://hub.tailnet.ts.net --issuer https://id.tailnet.ts.net \
#       [--client-id mado] [--client-secret-file PATH | --public-client] [--binary bin/mado]
#
# Writes ~/.config/mado/hub.toml and secrets.toml (only if missing), creates
# ~/.local/share/mado for the database, installs mado-hub.service and enables linger.
set -euo pipefail

base_url="" issuer="" client_id="mado" binary="bin/mado"
secret_file="$HOME/.config/mado/oidc-client-secret" public_client=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --base-url) base_url="$2"; shift 2 ;;
    --issuer) issuer="$2"; shift 2 ;;
    --client-id) client_id="$2"; shift 2 ;;
    --client-secret-file) secret_file="$2"; shift 2 ;;
    --public-client) public_client=1; shift ;;
    --binary) binary="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[[ -n "$base_url" && -n "$issuer" ]] || { echo "need --base-url and --issuer" >&2; exit 2; }
[[ "$base_url" == https://* ]] || echo "warning: --base-url is not https; cookies will not be Secure" >&2
[[ "$client_id" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "bad --client-id" >&2; exit 2; }

here="$(cd "$(dirname "$0")" && pwd)"
dest="$HOME/.local/bin/mado"
if [[ -e "$dest" && "$binary" -ef "$dest" ]] || cmp -s "$binary" "$dest" 2>/dev/null; then
  echo "binary already installed at $dest"
else
  install -D -m 0755 "$binary" "$dest"
fi
install -d -m 0700 "$HOME/.config/mado" "$HOME/.local/share/mado"

toml_str() { local s="${1//\\/\\\\}"; s="${s//\"/\\\"}"; printf '"%s"' "$s"; }
cfg="$HOME/.config/mado/hub.toml"
if [[ ! -e "$cfg" ]]; then
  {
    echo 'listen = "127.0.0.1:8740"'
    echo "base_url = $(toml_str "${base_url%/}")"
    echo 'db_path = "~/.local/share/mado/hub.db"'
    echo 'secrets_file = "~/.config/mado/secrets.toml"'
    echo
    echo '[oidc]'
    echo "issuer = $(toml_str "${issuer%/}")"
    echo "client_id = $(toml_str "$client_id")"
    echo '# admin_group = "mado_admins"'
    if [[ "$public_client" == 1 ]]; then
      echo '# client_secret_file = "~/.config/mado/oidc-client-secret"   # omit for a public PKCE client'
    else
      echo "client_secret_file = $(toml_str "$secret_file")"
    fi
    echo
    echo '# Router profiles: see deploy/hub.toml.example.'
  } > "$cfg"
  chmod 0600 "$cfg"
  echo "wrote $cfg"
else
  echo "kept existing $cfg"
fi
sec="$HOME/.config/mado/secrets.toml"
if [[ ! -e "$sec" ]]; then
  printf '# Upstream API keys, referenced by key_ref in profiles. Read once at hub start.\n[keys]\n' > "$sec"
  chmod 0600 "$sec"
  echo "wrote $sec"
fi

unit_src="$here/mado-hub.service"
if [[ ! -f "$unit_src" ]]; then
  # Script copied without its deploy/ directory: use the embedded unit
  # (kept identical to deploy/mado-hub.service by `make check-deploy`).
  unit_src="$(mktemp)"
  trap 'rm -f "$unit_src"' EXIT
  cat > "$unit_src" <<'UNIT'
# systemd *user* unit. Installed by deploy/install-hub.sh into ~/.config/systemd/user/.
[Unit]
Description=mado hub (web UI and worker endpoint)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%h/.local/bin/mado hub --config %h/.config/mado/hub.toml
Restart=on-failure
RestartSec=5
NoNewPrivileges=yes
UMask=0077

[Install]
WantedBy=default.target
UNIT
fi
install -D -m 0644 "$unit_src" "$HOME/.config/systemd/user/mado-hub.service"
if ! loginctl enable-linger "$USER"; then
  echo "WARN: could not enable linger (needs root here); the service will stop at logout. Run as root: sudo loginctl enable-linger $USER" >&2
fi
systemctl --user daemon-reload
systemctl --user enable --now mado-hub.service
echo "mado-hub.service enabled and started (listening on 127.0.0.1:8740)"

cat <<MSG

Next steps
----------
1. Publish the hub on the tailnet over HTTPS:

     tailscale serve --bg --https=443 http://127.0.0.1:8740

2. Pocket ID: create an OIDC client "$client_id"
   - callback URL:        ${base_url%/}/callback
   - public client (PKCE) or put the secret in ~/.config/mado/oidc-client-secret
   - scopes: openid profile email groups  (the "groups" scope must be allowed)
   - allowed user groups: mado_users (everyone who may use mado) and mado_admins
   - put yourself in mado_admins (admins enroll machines, edit profiles, see /history and /audit)

3. Enroll each machine (on the hub):  mado hub enroll <name> --config $cfg
   or use "Enroll machine" in the web UI after the first login.
MSG
