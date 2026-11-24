# claude: a provision.sh component, sourced by it (lib.sh's helpers).

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
