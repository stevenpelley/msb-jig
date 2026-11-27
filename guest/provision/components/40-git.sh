# git: a provision.sh component, sourced by it (lib.sh's helpers).

# --- git, gh, glab: pointed at the broker, never asking for credentials ---
if want git; then
  log "git, gh, glab"
  apt_lists
  apt-get install -y -qq --no-install-recommends git gh glab >/dev/null
  # No credential helper and no prompts: the broker injects credentials.
  git config --system credential.helper ""
  git config --system core.askPass ""
  # GIT_TERMINAL_PROMPT=0 is in the settings apply writes.
fi
