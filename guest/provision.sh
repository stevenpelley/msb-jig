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
#   PROVISION_COMPONENTS space-separated subset of: base sshd git broker docker claude
#                        (default: all of them). broker-env alone rewrites only
#                        git's, glab's and gh's settings for the broker (broker
#                        includes it). claude-env alone rewrites only
#                        Claude Code's settings and the agent's instructions
#                        (claude includes it), and aws-env sets up mcpcli for
#                        the AWS MCP Server: the supervisor runs both on every
#                        start (with broker-env), so they follow which providers the broker serves.
#   BROKER_GITLAB_URL    the broker's GitLab endpoint (default, when unset,
#                        https://gitlab.broker.internal; sbx mode:
#                        http://host.docker.internal:<port>). Empty: the broker
#                        serves no GitLab, so gitlab.com stays gitlab.com
#                        (anonymous, through the domain filter)
#   BROKER_GITHUB_URL    the same for GitHub (https://github.broker.internal).
#                        Default empty: github.com stays github.com
#   BROKER_CA_FILE       the broker's CA certificate, installed as trusted if given
#   BROKER_SESSION_TOKEN sbx mode only: the token mono-jig-sbx requires,
#                        sent by git (extraHeader), glab (its token) and the MCP client
#   BROKER_GITLAB_USER   the GitLab user the agent acts for: writes the broker's
#                        rules into the agent's instructions (~/.claude/CLAUDE.md)
#   BROKER_GITHUB_USER   the same for GitHub
#   BROKER_GITLAB_MCP    1 to register GitLab's MCP server (through the broker's
#                        MCP filter) with Claude Code; it needs a token with the
#                        mcp scope. Default: glab only.
#   BROKER_ANTHROPIC_URL the broker's Anthropic endpoint (https://anthropic.broker.internal):
#                        Claude Code calls it with a placeholder token, and the
#                        broker injects the user's. Empty: sign in inside the VM.
#   BROKER_AWS_URL       the broker's AWS MCP endpoint (https://aws.broker.internal/mcp),
#                        for mcpcli and curl. Empty: no AWS.
#   BROKER_AWS_PROFILES  the profiles the broker's AWS MCP proxy signs with, the
#                        first the default (for the agent's instructions)
#   MCPCLI_DIR           where the supervisor wrote mcpcli (aws-env)
set -euo pipefail

SANDBOX_USER="${SANDBOX_USER:-agent}"
SANDBOX_UID="${SANDBOX_UID:-}"
PROVISION_COMPONENTS="${PROVISION_COMPONENTS:-base sshd git broker docker claude}"
BROKER_GITLAB_URL="${BROKER_GITLAB_URL-https://gitlab.broker.internal}"
BROKER_GITHUB_URL="${BROKER_GITHUB_URL:-}"
BROKER_CA_FILE="${BROKER_CA_FILE:-}"
BROKER_SESSION_TOKEN="${BROKER_SESSION_TOKEN:-}"
BROKER_GITLAB_USER="${BROKER_GITLAB_USER:-}"
BROKER_GITHUB_USER="${BROKER_GITHUB_USER:-}"
BROKER_GITLAB_MCP="${BROKER_GITLAB_MCP:-}"
BROKER_ANTHROPIC_URL="${BROKER_ANTHROPIC_URL:-}"
BROKER_AWS_URL="${BROKER_AWS_URL:-}"
BROKER_AWS_PROFILES="${BROKER_AWS_PROFILES:-}"
MCPCLI_DIR="${MCPCLI_DIR:-/usr/local/lib/mono-jig/mcpcli}"
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
  client_env 80-mono-jig-git GIT_TERMINAL_PROMPT=0
  echo 'unset GIT_ASKPASS SSH_ASKPASS' >>/etc/profile.d/80-mono-jig-git.sh
fi

# --- broker: every git, gh and glab call to a managed provider goes to it ---
# See docs/design.md, "How the agent uses git and gh". Only for the
# providers the broker serves: a URL rewritten to a name the broker has no
# certificate for fails its TLS handshake, public clones included.
if want broker || want broker-env; then
  log "broker client settings (${BROKER_GITLAB_URL:-no GitLab}, ${BROKER_GITHUB_URL:-no GitHub})"
  # Older provisioning mapped GitHub unconditionally: clear both first.
  for b in "$BROKER_GITLAB_URL" "$BROKER_GITHUB_URL" https://gitlab.broker.internal https://github.broker.internal; do
    [[ -n $b ]] && { git config --system --unset-all "url.$b/.insteadOf" 2>/dev/null || true; }
  done
  for spec in "gitlab.com=$BROKER_GITLAB_URL" "github.com=$BROKER_GITHUB_URL"; do
    host=${spec%%=*}; b=${spec#*=}
    [[ -n $b ]] || continue
    for from in "https://$host/" "git@$host:" "ssh://git@$host/" "http://$host/"; do
      git config --system --add "url.$b/.insteadOf" "$from"
    done
  done
  # No push mapping: git applies remote.<name>.push even to explicit branches.
  gl_host=${BROKER_GITLAB_URL#*://}; gl_proto=${BROKER_GITLAB_URL%%://*}
  gh_host=${BROKER_GITHUB_URL#*://}
  # On microsandbox the network is the sandbox's identity and the tokens
  # are placeholders. In sbx mode the broker listens on a Mac port, so
  # every tool sends the session token instead.
  tok="${BROKER_SESSION_TOKEN:-placeholder}"
  if [[ -n "$BROKER_SESSION_TOKEN" && -n "$BROKER_GITLAB_URL" ]]; then
    git config --system "http.$BROKER_GITLAB_URL/.extraHeader" "X-Jig-Session: $BROKER_SESSION_TOKEN"
  fi
  # The tools treat the broker as a self-hosted GitLab and GitHub Enterprise
  # Server. The broker injects the real tokens.
  gl=()
  gh=()
  [[ -n $BROKER_GITLAB_URL ]] && gl=("GITLAB_HOST=$gl_host" "GITLAB_TOKEN=$tok" "GITLAB_API_PROTOCOL=$gl_proto")
  [[ -n $BROKER_GITHUB_URL ]] && gh=("GH_HOST=$gh_host" "GH_ENTERPRISE_TOKEN=$tok")
  client_env 81-mono-jig-broker "${gl[@]}" "${gh[@]}"
  # glab's own config too: auth status and clone read it, not GITLAB_HOST.
  # Only the broker's host: glab checks every host listed (its default
  # gitlab.com fails with the placeholder), and https, since glab repo clone
  # would otherwise use SSH, which the sandbox doesn't have. Rewritten on
  # every start, like the rest; removed (if ours) when there's no GitLab.
  home="$(getent passwd "$SANDBOX_USER" | cut -d: -f6)"
  glab_cfg="$home/.config/glab-cli/config.yml"
  glab_mark="# Written by mono-jig provision.sh; rewritten on every start."
  if [[ -n $BROKER_GITLAB_URL ]]; then
    install -d -m 0700 -o "$SANDBOX_USER" -g "$SANDBOX_USER" "$home/.config" "$home/.config/glab-cli"
    cat >"$glab_cfg" <<EOF
$glab_mark
host: $gl_host
git_protocol: https
check_update: false
hosts:
    $gl_host:
        api_protocol: $gl_proto
        git_protocol: https
EOF
    chown "$SANDBOX_USER:$SANDBOX_USER" "$glab_cfg"
    chmod 0600 "$glab_cfg"
  elif [[ -e $glab_cfg && "$(head -n1 "$glab_cfg")" == "$glab_mark" ]]; then
    rm -f "$glab_cfg"
  fi
fi
if want broker; then
  if [[ -n "$BROKER_CA_FILE" ]]; then
    install -m 0644 "$BROKER_CA_FILE" /usr/local/share/ca-certificates/mono-jig-broker.crt
    update-ca-certificates >/dev/null
    # Node-based tools (Claude Code's MCP client) don't read the system store.
    client_env 82-mono-jig-node-ca NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt
  fi
fi

# --- docker: the VM's own daemon, for tests ---
if want docker; then
  log "docker"
  # buildx: BuildKit for docker build (the legacy builder is deprecated).
  apt-get install -y -qq --no-install-recommends docker.io docker-buildx docker-compose-v2 >/dev/null
  usermod -aG docker "$SANDBOX_USER" 2>/dev/null || true
fi

# --- Claude Code: the personal track's agent, installed for the user ---
# --- Claude Code's settings: the broker's endpoint, or none ---
# The credential stays on the Mac: Claude Code sends a placeholder to the
# broker, which swaps in the user's token.
if want claude || want claude-env; then
  if [[ -n "$BROKER_ANTHROPIC_URL" ]]; then
    client_env 83-mono-jig-claude \
      "ANTHROPIC_BASE_URL=$BROKER_ANTHROPIC_URL" CLAUDE_CODE_OAUTH_TOKEN=placeholder
    # Until onboarding is done, interactive claude checks api.anthropic.com
    # directly, ignoring ANTHROPIC_BASE_URL, and the sandbox can't resolve
    # it ("Unable to connect to Anthropic services"; -p skips the check).
    cj="$(getent passwd "$SANDBOX_USER" | cut -d: -f6)/.claude.json"
    if [[ -s $cj ]]; then
      jq '.hasCompletedOnboarding = true' "$cj" >"$cj.tmp" && mv "$cj.tmp" "$cj"
    else
      echo '{"hasCompletedOnboarding": true}' >"$cj"
    fi
    chown "$SANDBOX_USER:$SANDBOX_USER" "$cj"
    chmod 0600 "$cj"
  else
    client_env 83-mono-jig-claude
  fi
fi

if want claude; then
  log "claude code"
  su - "$SANDBOX_USER" -c 'curl -fsSL https://claude.ai/install.sh | bash' >/dev/null
  su - "$SANDBOX_USER" -c '~/.local/bin/claude --version'
  # On PATH without a login shell too (ssh host cmd, VS Code's server). The
  # installer's own link in ~/.local/bin follows its updates.
  ln -sfn "$(getent passwd "$SANDBOX_USER" | cut -d: -f6)/.local/bin/claude" /usr/local/bin/claude
  # GitLab's MCP server, through the broker's MCP filter (which injects the
  # token), only when asked: glab through the broker does everything it does,
  # with one credential fewer (docs/prototype-plan.md, step 6).
  su - "$SANDBOX_USER" -c "~/.local/bin/claude mcp remove --scope user gitlab >/dev/null 2>&1" || true
  if [[ "$BROKER_GITLAB_MCP" == 1 && -n "$BROKER_GITLAB_URL" ]]; then
    su - "$SANDBOX_USER" -c "~/.local/bin/claude mcp add --scope user --transport http gitlab '$BROKER_GITLAB_URL/api/v4/mcp' ${BROKER_SESSION_TOKEN:+--header 'X-Jig-Session: $BROKER_SESSION_TOKEN'}" >/dev/null
  fi
fi

# --- AWS: mcpcli, the agent's client for the broker's AWS MCP endpoint ---
# The agent calls the AWS MCP Server from the shell (mcpcli or curl), never
# as native MCP tools, so it chooses what enters its context. The supervisor
# writes mcpcli to $MCPCLI_DIR on every start.
if want aws-env; then
  if [[ -n "$BROKER_AWS_URL" ]]; then
    if ! python3 -c 'import ensurepip, venv' 2>/dev/null; then
      log "python3-venv (mcpcli)"
      apt-get update -qq
      apt-get install -y -qq --no-install-recommends python3 python3-venv >/dev/null
    fi
    chmod 0755 "$MCPCLI_DIR" "$MCPCLI_DIR/mcpcli" "$MCPCLI_DIR/mcpcli.py"
    ln -sfn "$MCPCLI_DIR/mcpcli" /usr/local/bin/mcpcli
    client_env 84-mono-jig-aws "MCP_GATEWAY_URL=$BROKER_AWS_URL"
  else
    rm -f /usr/local/bin/mcpcli
    client_env 84-mono-jig-aws
  fi
fi

# --- the agent's instructions: the broker's rules, per provider ---
# So the agent follows them instead of learning them from refusals. Ours to
# rewrite only while it carries our first line.
if want claude || want claude-env; then
  home="$(getent passwd "$SANDBOX_USER" | cut -d: -f6)"
  md="$home/.claude/CLAUDE.md"
  marker="<!-- Written by mono-jig provision.sh; edits are lost on reprovisioning. -->"
  if [[ -e $md ]] && [[ "$(head -n1 "$md")" != "$marker" ]]; then
    log "$md exists and isn't ours: left alone"
  elif [[ -z "$BROKER_GITLAB_USER" && -z "$BROKER_GITHUB_USER" && -z "$BROKER_AWS_URL" ]]; then
    rm -f "$md"
  else
    install -d -o "$SANDBOX_USER" -g "$SANDBOX_USER" "$home/.claude"
    {
      cat <<EOF
$marker
# Working in the mono-jig sandbox

The mono-jig broker, on the user's Mac, holds the credentials for the
services below and enforces their rules. Its refusals come from
"mono-jig broker" and say why. Don't log in, and don't ask for tokens.
Other hosts go through a domain filter; if one is blocked, ask the user to
allow it rather than working around it.
EOF
      if [[ -n "$BROKER_GITLAB_USER" ]]; then
        u="$BROKER_GITLAB_USER"
        cat <<EOF

## GitLab

- Use \`git\` and \`glab\` as usual: gitlab.com URLs are routed to the broker,
  and \`glab\` is already configured (\`GITLAB_HOST\`).
- Push only to branches under \`agent/$u/\`, e.g. \`agent/$u/fix-timeout\`.
  Pushes to any other branch, and push options (\`git push -o\`), are refused.
- Merge request titles must start with \`[agent/$u]\`, after an optional
  \`Draft:\`. Only merge requests from \`agent/$u/\` branches can be edited
  or commented on.
- Never merge, approve or set auto-merge: a human does that.
- Run \`glab\` non-interactively: pass \`--yes\`, titles and descriptions as
  flags, and \`-R <group>/<project>\` when outside a clone.
- For anything \`glab\` has no command for, use \`glab api\` (REST, or
  \`glab api graphql\` for queries). Save large responses to a file and read
  what you need with \`jq\`, \`head\` or \`grep\`.
EOF
      fi
      if [[ -n "$BROKER_GITHUB_USER" ]]; then
        u="$BROKER_GITHUB_USER"
        cat <<EOF

## GitHub

- Use \`git\` and \`gh\` as usual: github.com URLs are routed to the broker,
  and \`gh\` is already configured (\`GH_HOST\`).
- Push only to branches under \`agent/$u/\`, e.g. \`agent/$u/fix-timeout\`.
  Pushes to any other branch, and push options, are refused. Then open a
  pull request: \`gh pr create --head agent/$u/<branch> --base <target>\`.
- Pull request titles must start with \`[agent/$u]\`. Only pull requests
  from \`agent/$u/\` branches can be edited, closed or commented on; for a
  stack, target the branch below.
- Never merge, approve or set auto-merge: a human does that.
- Run \`gh\` non-interactively: pass \`--title\` and \`--body\` (or
  \`--fill\`), and \`-R <owner>/<repo>\` when outside a clone.
- CI: \`gh pr checks\`, \`gh run view --log-failed\`. For anything else,
  \`gh api\` (REST) or \`gh api graphql\`. Writing files, refs or releases
  through the API is refused: push instead.
EOF
      fi
      if [[ -n "$BROKER_AWS_URL" ]]; then
        read -r -a profiles <<<"$BROKER_AWS_PROFILES"
        cat <<EOF

## AWS

The AWS MCP Server is at \`\$MCP_GATEWAY_URL\` ($BROKER_AWS_URL). The
broker signs each call on the Mac with the user's AWS profile
\`${profiles[0]:-?}\`. There are no AWS credentials or CLI logins in the
sandbox, and none are needed.

- Call it from the shell with \`mcpcli\`, never as native MCP tools. Send
  results to a file and read what you need with \`jq\`, \`head\` or \`grep\`:

      mcpcli tools [substring]              # tool names
      mcpcli describe <tool>                # description and input schema
      mcpcli call <tool> '<json-args>' >out.json
      mcpcli py '<python>' | @file.py       # aws___run_script, in AWS's sandbox
      mcpcli logs <minutes> '<query>' --session <id> --group <log-group>

- \`aws___run_script\` is the tool that reaches AWS. Its Python can't import
  boto3: it calls \`await call_boto3(service_name=..., operation_name=...,
  params={...})\` and ends with \`result = ...\` then \`result\`. Read
  \`mcpcli describe aws___run_script\` before the first script.
- CloudWatch Logs Insights queries cost money per GB scanned. Run them only
  with \`mcpcli logs\`, which estimates first and enforces spend budgets
  (\`mcpcli spend --session <id>\`), with your session's id; raise a budget
  only when the user says so.
- \`curl\` works too, without a session, for one-off calls:
  \`curl -s \$MCP_GATEWAY_URL -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'\`
- An access denied from AWS is the role's: report it, don't look for
  another way in.
EOF
        if (( ${#profiles[@]} > 1 )); then
          cat <<EOF
- Other profiles: ${profiles[*]:1}. Pick one per call with the tools'
  \`aws_profile\` argument (\`mcpcli py --profile <p>\`).
EOF
        fi
      fi
    } >"$md"
    chown "$SANDBOX_USER:$SANDBOX_USER" "$md"
    chmod 0644 "$md"
  fi
fi

log "done"
