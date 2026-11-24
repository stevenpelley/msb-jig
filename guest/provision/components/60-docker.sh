# docker: a provision.sh component, sourced by it (lib.sh's helpers).

# --- docker: the VM's own daemon, for tests ---
if want docker; then
  log "docker"
  # buildx: BuildKit for docker build (the legacy builder is deprecated).
  apt-get install -y -qq --no-install-recommends docker.io docker-buildx docker-compose-v2 >/dev/null
  usermod -aG docker "$SANDBOX_USER" 2>/dev/null || true
fi
