# base: a provision.sh component, sourced by it (lib.sh's helpers).

# --- base: the user and the tools Kiro's and VS Code's remote servers need ---
if want base; then
  log "base packages"
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends \
    ca-certificates curl tar gzip coreutils diffutils findutils util-linux \
    procps iproute2 netcat-openbsd jq less sudo locales \
    socat python3 python3-venv >/dev/null
  # Any extra CAs copied in before ca-certificates was (the image build).
  update-ca-certificates >/dev/null
  # socat: the supervisor's bridge to the broker. python3-venv: mcpcli.
  if ! id "$SANDBOX_USER" >/dev/null 2>&1; then
    log "user $SANDBOX_USER"
    useradd --create-home --shell /bin/bash ${SANDBOX_UID:+--uid "$SANDBOX_UID"} "$SANDBOX_USER"
  fi
  # Kiro requires a home directory other users can't write to.
  chmod 0750 "$(getent passwd "$SANDBOX_USER" | cut -d: -f6)"
fi
