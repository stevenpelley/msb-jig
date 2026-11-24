#!/usr/bin/env bash
# provision.sh: install everything the mono-jig sandbox needs.
#
# One script for every packaging: the sandbox image's Dockerfile runs it at
# build time, and the supervisor runs it (or some components) at every
# start on microsandbox. Run as root. Idempotent.
#
# Environment:
#   SANDBOX_USER         the one user that may log in over SSH (default: agent).
#   SANDBOX_UID          its UID, when it's created (default: useradd's choice);
#                        the supervisor fixes it so host folders mount as its
#   PROVISION_COMPONENTS space-separated subset of: base sshd git broker docker
#                        claude apply (default: all but apply). apply writes the
#                        settings the supervisor built (guest.Config, from the
#                        providers and the agent): the environment, git's
#                        settings, files such as glab's config, mcpcli and the
#                        agent's instructions. The supervisor runs it on every
#                        start, so the guest follows the broker's providers.
#   GUEST_CONFIG         apply's settings, JSON (default /root/guest.json)
#   BROKER_CA_FILE       the broker's CA certificate, installed as trusted if given
set -euo pipefail

SANDBOX_USER="${SANDBOX_USER:-agent}"
SANDBOX_UID="${SANDBOX_UID:-}"
PROVISION_COMPONENTS="${PROVISION_COMPONENTS:-base sshd git broker docker claude}"
GUEST_CONFIG="${GUEST_CONFIG:-/root/guest.json}"
BROKER_CA_FILE="${BROKER_CA_FILE:-}"
export DEBIAN_FRONTEND=noninteractive

want() { [[ " $PROVISION_COMPONENTS " == *" $1 "* ]]; }
log() { printf '[provision] %s\n' "$*"; }

# client_env <name> [KEY=VALUE...]: the tools' settings, where every session
# reads them. /etc/profile.d/<name>.sh serves login shells; a block in
# /etc/environment serves sshd sessions without one (pam_env), such as
# `ssh host cmd` and VS Code's server, whose terminals and extensions inherit
# it. Values must not contain spaces or quotes. No pairs: remove both.
client_env() {
  local name=$1 kv; shift
  rm -f "/etc/profile.d/$name.sh"
  touch /etc/environment
  sed -i "/^# $name begin\$/,/^# $name end\$/d" /etc/environment
  (( $# )) || return 0
  {
    echo "# Written by mono-jig provision.sh."
    for kv in "$@"; do printf 'export %s="%s"\n' "${kv%%=*}" "${kv#*=}"; done
  } >"/etc/profile.d/$name.sh"
  chmod 0644 "/etc/profile.d/$name.sh"
  { echo "# $name begin"; printf '%s\n' "$@"; echo "# $name end"; } >>/etc/environment
}

if [[ $EUID -ne 0 ]]; then
  echo "provision.sh must run as root" >&2
  exit 1
fi

# --- base: the user and the tools Kiro's and VS Code's remote servers need ---
if want base; then
  log "base packages"
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends \
    ca-certificates curl tar gzip coreutils diffutils findutils util-linux \
    procps iproute2 netcat-openbsd jq less sudo locales \
    socat python3 python3-venv >/dev/null
  # socat: the supervisor's bridge to the broker. python3-venv: mcpcli.
  if ! id "$SANDBOX_USER" >/dev/null 2>&1; then
    log "user $SANDBOX_USER"
    useradd --create-home --shell /bin/bash ${SANDBOX_UID:+--uid "$SANDBOX_UID"} "$SANDBOX_USER"
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
  cat >/etc/ssh/sshd_config.d/10-mono-jig.conf <<EOF
# Written by mono-jig provision.sh. See docs/design.md,
# "What the mono-jig CLI configures".
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
  # GIT_TERMINAL_PROMPT=0 is in the settings apply writes.
fi

# --- broker: the broker's CA, trusted ---
# The providers' settings (URL rewrites, the tools' hosts and placeholder
# tokens) are in apply's settings, so they follow the broker's providers.
if want broker && [[ -n "$BROKER_CA_FILE" ]]; then
  install -m 0644 "$BROKER_CA_FILE" /usr/local/share/ca-certificates/mono-jig-broker.crt
  update-ca-certificates >/dev/null
fi

# --- docker: the VM's own daemon, for tests ---
if want docker; then
  log "docker"
  # buildx: BuildKit for docker build (the legacy builder is deprecated).
  apt-get install -y -qq --no-install-recommends docker.io docker-buildx docker-compose-v2 >/dev/null
  usermod -aG docker "$SANDBOX_USER" 2>/dev/null || true
fi

# --- Claude Code: the personal track's agent, installed for the user ---
# Its settings and instructions are in apply's settings.
if want claude; then
  log "claude code"
  su - "$SANDBOX_USER" -c 'curl -fsSL https://claude.ai/install.sh | bash' >/dev/null
  su - "$SANDBOX_USER" -c '~/.local/bin/claude --version'
  # On PATH without a login shell too (ssh host cmd, VS Code's server). The
  # installer's own link in ~/.local/bin follows its updates.
  ln -sfn "$(getent passwd "$SANDBOX_USER" | cut -d: -f6)/.local/bin/claude" /usr/local/bin/claude
fi

# --- apply: the settings the supervisor built (guest.Config), every start ---
if want apply; then
  log "settings ($GUEST_CONFIG)"
  cfg=$GUEST_CONFIG
  home="$(getent passwd "$SANDBOX_USER" | cut -d: -f6)"
  state=/var/lib/mono-jig
  install -d -m 0755 "$state"

  # Packages missing from an older image.
  missing=()
  while read -r p; do
    [[ -n $p ]] && ! dpkg -s "$p" >/dev/null 2>&1 && missing+=("$p")
  done < <(jq -r '.packages // [] | .[]' "$cfg")
  if (( ${#missing[@]} )); then
    log "packages: ${missing[*]}"
    apt-get update -qq
    apt-get install -y -qq --no-install-recommends "${missing[@]}" >/dev/null
  fi

  # The environment: one block, replacing the per-tool ones older
  # provisioning wrote (80-84).
  for f in /etc/profile.d/8[0-4]-mono-jig-*.sh; do
    [[ -e $f ]] && client_env "$(basename "$f" .sh)"
  done
  env=()
  while read -r kv; do [[ -n $kv ]] && env+=("$kv"); done \
    < <(jq -r '.env // {} | to_entries[] | "\(.key)=\(.value)"' "$cfg")
  client_env 80-mono-jig "${env[@]}"
  echo 'unset GIT_ASKPASS SSH_ASKPASS' >>/etc/profile.d/80-mono-jig.sh

  # git: each key's values replace the guest's. The keys the last apply
  # set (recorded) and gitClear's go first, so a provider no longer served
  # leaves nothing.
  gitkeys=$state/guest-git
  while read -r k; do
    [[ -n $k ]] && { git config --system --unset-all "$k" 2>/dev/null || true; }
  done < <(jq -r '(.gitClear // []) + (.git // {} | keys) | .[]' "$cfg"; cat "$gitkeys" 2>/dev/null || true)
  while IFS=$'\t' read -r k v; do
    [[ -n $k ]] && git config --system --add "$k" "$v"
  done < <(jq -r '.git // {} | to_entries[] | .key as $k | .value[] | [$k, .] | @tsv' "$cfg")
  jq -r '.git // {} | keys | .[]' "$cfg" >"$gitkeys"

  # Files: written when absent or ours (a marker, if set, is the first
  # line). Those the last apply wrote and this one doesn't are removed, if
  # still ours. Merged JSON is never removed.
  ours() { [[ ! -e $1 || -z $2 ]] || [[ "$(head -n1 "$1")" == "$2" ]]; }
  mkdirs() { # <dir> <owner>: the missing directories, owned by owner
    [[ -d $1 ]] && return 0
    mkdirs "$(dirname "$1")" "$2"
    if [[ $2 == root ]]; then install -d -m 0755 "$1"; else install -d -m 0700 -o "$2" -g "$2" "$1"; fi
  }
  manifest=$state/guest-files
  written=$(mktemp)
  n=$(jq '.files // [] | length' "$cfg")
  for ((i = 0; i < n; i++)); do
    f=$(jq -c ".files[$i]" "$cfg")
    p=$(jq -r .path <<<"$f"); [[ $p == "~/"* ]] && p="$home/${p#\~/}"
    marker=$(jq -r '.marker // ""' <<<"$f")
    owner=$(jq -r '.owner // "root"' <<<"$f"); [[ $owner == user ]] && owner=$SANDBOX_USER
    mode=$(jq -r '.mode // "0644"' <<<"$f")
    link=$(jq -r '.link // ""' <<<"$f")
    if ! ours "$p" "$marker"; then
      log "$p exists and isn't ours: left alone"
      continue
    fi
    mkdirs "$(dirname "$p")" "$owner"
    if [[ -n $link ]]; then
      ln -sfn "$link" "$p"
    elif jq -e 'has("merge")' <<<"$f" >/dev/null; then
      [[ -s $p ]] || echo '{}' >"$p"
      jq --argjson m "$(jq -c .merge <<<"$f")" '. * $m' "$p" >"$p.tmp"
      mv "$p.tmp" "$p"
    else
      jq -j '.content // ""' <<<"$f" >"$p.tmp"
      mv "$p.tmp" "$p"
    fi
    if [[ ! -L $p ]]; then
      chown "$owner:$owner" "$p"
      chmod "$mode" "$p"
    fi
    jq -e 'has("merge")' <<<"$f" >/dev/null || printf '%s\t%s\n' "$p" "$marker" >>"$written"
  done
  if [[ -e $manifest ]]; then
    while IFS=$'\t' read -r p marker; do
      [[ -n $p ]] && [[ -e $p || -L $p ]] || continue
      cut -f1 "$written" | grep -qxF -- "$p" && continue
      if [[ -L $p ]] || ours "$p" "$marker"; then
        log "removing $p"
        rm -f "$p"
      fi
    done <"$manifest"
  fi
  mv "$written" "$manifest"
  chmod 0644 "$manifest"
fi

log "done"
