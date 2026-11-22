#!/usr/bin/env bash
# provision.sh: install everything the investigator sandbox needs.
#
# One script for every packaging: the sandbox image's Dockerfile runs it at
# build time, and the Lima template runs it as its provisioning step on a
# stock Ubuntu cloud image. Run as root. Idempotent.
#
# Environment:
#   SANDBOX_USER         the one user that may log in over SSH (default: agent).
#                        On Lima this is Lima's own user, which must keep working.
#   PROVISION_COMPONENTS space-separated subset of: base sshd git broker docker claude
#                        (default: all of them)
#   BROKER_GITLAB_URL    the broker's GitLab endpoint (default https://gitlab.broker.internal;
#                        sbx mode: http://host.docker.internal:<port>)
#   BROKER_GITHUB_URL    the same for GitHub (default https://github.broker.internal)
#   BROKER_CA_FILE       the broker's CA certificate, installed as trusted if given
#   PROVISION_PROXY      optional explicit proxy URL (e.g. http://proxy.internal:3128)
#                        written for apt, dockerd and login shells. Leave unset
#                        where the runtime sets proxies itself (sbx).
set -euo pipefail

SANDBOX_USER="${SANDBOX_USER:-agent}"
PROVISION_COMPONENTS="${PROVISION_COMPONENTS:-base sshd git broker docker claude}"
BROKER_GITLAB_URL="${BROKER_GITLAB_URL:-https://gitlab.broker.internal}"
BROKER_GITHUB_URL="${BROKER_GITHUB_URL:-https://github.broker.internal}"
BROKER_CA_FILE="${BROKER_CA_FILE:-}"
PROVISION_PROXY="${PROVISION_PROXY:-}"
export DEBIAN_FRONTEND=noninteractive

want() { [[ " $PROVISION_COMPONENTS " == *" $1 "* ]]; }
log() { printf '[provision] %s\n' "$*"; }

if [[ $EUID -ne 0 ]]; then
  echo "provision.sh must run as root" >&2
  exit 1
fi

# --- proxy settings (Lima: everything goes through investigator-net's filter) ---
if [[ -n "$PROVISION_PROXY" ]]; then
  log "proxy: $PROVISION_PROXY"
  cat >/etc/apt/apt.conf.d/90investigator-proxy <<EOF
Acquire::http::Proxy "$PROVISION_PROXY";
Acquire::https::Proxy "$PROVISION_PROXY";
EOF
  cat >/etc/profile.d/90-investigator-proxy.sh <<EOF
export HTTPS_PROXY="$PROVISION_PROXY" HTTP_PROXY="$PROVISION_PROXY"
export https_proxy="$PROVISION_PROXY" http_proxy="$PROVISION_PROXY"
export NO_PROXY="localhost,127.0.0.1,::1,.broker.internal" no_proxy="localhost,127.0.0.1,::1,.broker.internal"
EOF
  export HTTPS_PROXY="$PROVISION_PROXY" HTTP_PROXY="$PROVISION_PROXY"
fi

# --- base: the user and the tools Kiro's and VS Code's remote servers need ---
if want base; then
  log "base packages"
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends \
    ca-certificates curl tar gzip coreutils diffutils findutils util-linux \
    procps iproute2 netcat-openbsd jq less sudo locales >/dev/null
  if ! id "$SANDBOX_USER" >/dev/null 2>&1; then
    log "user $SANDBOX_USER"
    useradd --create-home --shell /bin/bash "$SANDBOX_USER"
  fi
  # Kiro requires a home directory other users can't write to.
  chmod 0750 "$(getent passwd "$SANDBOX_USER" | cut -d: -f6)"
fi

# --- sshd: key-only, one user, no forwarding toward the sandbox ---
# These settings guard against mistakes in the editor's settings. They don't
# bind the agent, which can be root in the VM; the Mac-side settings do.
if want sshd; then
  log "sshd"
  apt-get install -y -qq --no-install-recommends openssh-server >/dev/null
  install -d -m 0755 /etc/ssh/sshd_config.d /run/sshd
  # Ubuntu's sshd_config includes sshd_config.d/*.conf first, and the first
  # value sshd reads wins, so this file overrides the defaults below it.
  cat >/etc/ssh/sshd_config.d/10-investigator.conf <<EOF
# Written by investigator provision.sh. See investigator-design.md,
# "What the investigator CLI configures".
AllowUsers $SANDBOX_USER
PermitRootLogin no
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitEmptyPasswords no
AuthenticationMethods publickey
AllowAgentForwarding no
X11Forwarding no
PermitTunnel no
AllowStreamLocalForwarding no
# The editors reach their server through local forwards; nothing needs remote ones.
AllowTcpForwarding local
GatewayPorts no
PermitUserEnvironment no
PermitUserRC no
HostKey /etc/ssh/ssh_host_ed25519_key
EOF
  sshd -t
fi

# --- git, gh, glab: pointed at the broker, never asking for credentials ---
if want git; then
  log "git, gh, glab"
  apt-get install -y -qq --no-install-recommends git gh glab >/dev/null
  # No credential helper and no prompts: the broker injects credentials.
  git config --system credential.helper ""
  git config --system core.askPass ""
  cat >/etc/profile.d/80-investigator-git.sh <<'EOF'
export GIT_TERMINAL_PROMPT=0
unset GIT_ASKPASS SSH_ASKPASS
EOF
fi

# --- broker: every git, gh and glab call to a managed provider goes to it ---
# See investigator-design.md, "How the agent uses git and gh".
if want broker; then
  log "broker client settings ($BROKER_GITLAB_URL, $BROKER_GITHUB_URL)"
  for spec in "gitlab.com=$BROKER_GITLAB_URL" "github.com=$BROKER_GITHUB_URL"; do
    host=${spec%%=*}; b=${spec#*=}
    git config --system --unset-all "url.$b/.insteadOf" 2>/dev/null || true
    for from in "https://$host/" "git@$host:" "ssh://git@$host/" "http://$host/"; do
      git config --system --add "url.$b/.insteadOf" "$from"
    done
  done
  # No push mapping: git applies remote.<name>.push even to explicit branches.
  gl_host=${BROKER_GITLAB_URL#*://}; gl_proto=${BROKER_GITLAB_URL%%://*}
  gh_host=${BROKER_GITHUB_URL#*://}
  cat >/etc/profile.d/81-investigator-broker.sh <<EOF
# The tools treat the broker as a self-hosted GitLab and GitHub Enterprise
# Server. The tokens are placeholders; the broker injects the real ones.
export GITLAB_HOST="$gl_host" GITLAB_TOKEN="placeholder" GITLAB_API_PROTOCOL="$gl_proto"
export GH_HOST="$gh_host" GH_ENTERPRISE_TOKEN="placeholder"
EOF
  if [[ -n "$BROKER_CA_FILE" ]]; then
    install -m 0644 "$BROKER_CA_FILE" /usr/local/share/ca-certificates/investigator-broker.crt
    update-ca-certificates >/dev/null
    # Node-based tools (Claude Code's MCP client) don't read the system store.
    echo 'export NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt' >/etc/profile.d/82-investigator-node-ca.sh
  fi
fi

# --- docker: the VM's own daemon, for tests ---
if want docker; then
  log "docker"
  apt-get install -y -qq --no-install-recommends docker.io >/dev/null
  usermod -aG docker "$SANDBOX_USER" 2>/dev/null || true
  if [[ -n "$PROVISION_PROXY" ]]; then
    install -d /etc/systemd/system/docker.service.d
    cat >/etc/systemd/system/docker.service.d/90-investigator-proxy.conf <<EOF
[Service]
Environment="HTTPS_PROXY=$PROVISION_PROXY" "HTTP_PROXY=$PROVISION_PROXY" "NO_PROXY=localhost,127.0.0.1"
EOF
    # Containers get the proxy too, through the client's config.
    home="$(getent passwd "$SANDBOX_USER" | cut -d: -f6)"
    install -d -o "$SANDBOX_USER" -g "$SANDBOX_USER" "$home/.docker"
    cat >"$home/.docker/config.json" <<EOF
{"proxies": {"default": {"httpProxy": "$PROVISION_PROXY", "httpsProxy": "$PROVISION_PROXY", "noProxy": "localhost,127.0.0.1,.broker.internal"}}}
EOF
    chown "$SANDBOX_USER:$SANDBOX_USER" "$home/.docker/config.json"
  fi
fi

# --- Claude Code: the personal track's agent, installed for the user ---
if want claude; then
  log "claude code"
  su - "$SANDBOX_USER" -c 'curl -fsSL https://claude.ai/install.sh | bash' >/dev/null
  su - "$SANDBOX_USER" -c '~/.local/bin/claude --version'
  # GitLab's MCP server, through the broker's MCP filter (which injects the token).
  su - "$SANDBOX_USER" -c "~/.local/bin/claude mcp remove --scope user gitlab >/dev/null 2>&1; ~/.local/bin/claude mcp add --scope user --transport http gitlab '$BROKER_GITLAB_URL/api/v4/mcp'" >/dev/null
fi

log "done"
