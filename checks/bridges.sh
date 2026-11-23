#!/usr/bin/env bash
# bridges.sh: report which credential bridges from the Mac reach this sandbox.
#
# Run it in the editor's terminal inside the sandbox (VS Code or Kiro over
# SSH), once with default editor settings and once with ours. Each line is
# OPEN (a bridge exists), closed, or info. Paste the whole output back.
#
#   checks/bridges.sh [private-repo-url]
#
# The optional URL is a private repo on a provider (e.g. a throwaway GitLab
# project). The script asks git for credentials for it, with prompts off, and
# reports whether anything answered; it never prints a secret.
set -u

repo="${1:-}"
open=0
report() { # status, name, detail
  printf '%-7s %-28s %s\n' "$1" "$2" "$3"
  [[ $1 == OPEN ]] && open=$((open + 1))
}

echo "# bridges.sh on $(hostname) at $(date -u +%FT%TZ)"
echo "# TERM_PROGRAM=${TERM_PROGRAM:-} VSCODE_IPC_HOOK_CLI=${VSCODE_IPC_HOOK_CLI:+set}"

# --- SSH agent forwarding ---
if [[ -n "${SSH_AUTH_SOCK:-}" ]]; then
  if [[ -S "$SSH_AUTH_SOCK" ]] && keys=$(ssh-add -l 2>/dev/null); then
    report OPEN ssh-agent "SSH_AUTH_SOCK=$SSH_AUTH_SOCK, $(printf '%s\n' "$keys" | wc -l) key(s)"
  else
    report closed ssh-agent "SSH_AUTH_SOCK set ($SSH_AUTH_SOCK) but no agent answers"
  fi
else
  report closed ssh-agent "SSH_AUTH_SOCK unset"
fi
# Agent sockets left by other sessions (VS Code puts its own under /tmp).
socks=$(find /tmp /run/user -maxdepth 3 -type s \( -name 'agent.*' -o -name 'vscode-ssh-auth-*' -o -name 'ssh-*' \) 2>/dev/null | head -5)
[[ -n $socks ]] && report info agent-sockets "$(echo $socks)"

# --- askpass helpers (the editor answering git or ssh prompts) ---
for v in GIT_ASKPASS SSH_ASKPASS VSCODE_GIT_ASKPASS_MAIN VSCODE_GIT_ASKPASS_NODE VSCODE_GIT_IPC_HANDLE; do
  if [[ -n "${!v:-}" ]]; then report OPEN "$v" "${!v}"; else report closed "$v" unset; fi
done

# --- token environment variables (names only) ---
tokens=$(env | cut -d= -f1 | grep -Ei '(token|secret|password|api_key|apikey|_pat$|credential)' | grep -Ev '^(GIT_TERMINAL_PROMPT)$' | sort | tr '\n' ' ')
if [[ -n $tokens ]]; then report OPEN token-env "$tokens"; else report closed token-env "none"; fi

# --- git's credential configuration ---
helpers=$(git config --show-origin --get-all credential.helper 2>/dev/null | awk '$2 != ""' | tr '\t' ' ' | tr '\n' ';')
if [[ -n $helpers ]]; then report OPEN credential.helper "$helpers"; else report closed credential.helper "none set"; fi
for f in ~/.git-credentials ~/.config/git/credentials; do
  [[ -s $f ]] && report OPEN git-credentials-file "$f"
done
[[ -s ~/.config/gh/hosts.yml ]] && report OPEN gh-hosts "~/.config/gh/hosts.yml exists"
[[ -s ~/.config/glab-cli/config.yml ]] && grep -q 'token:' ~/.config/glab-cli/config.yml 2>/dev/null && report OPEN glab-config "token in ~/.config/glab-cli/config.yml"

# --- does anything answer a credential request? ---
ask() { # url
  local proto host
  proto=${1%%://*}; host=${1#*://}; host=${host%%/*}
  printf 'protocol=%s\nhost=%s\n\n' "$proto" "$host" |
    GIT_TERMINAL_PROMPT=0 timeout 10 git credential fill 2>/dev/null | grep -q '^password='
}
for u in https://github.com https://gitlab.com ${repo:+"$repo"}; do
  if ask "$u"; then report OPEN "credential-fill" "$u answered with a password"; else report closed credential-fill "$u: nothing answered"; fi
done
if [[ -n $repo ]]; then
  if GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=/bin/false timeout 20 git ls-remote "$repo" >/dev/null 2>&1; then
    report info ls-remote "$repo readable (through the broker, or a bridge: check the lines above)"
  else
    report closed ls-remote "$repo not readable without credentials"
  fi
fi

# --- opening URLs on the Mac ---
[[ -n "${BROWSER:-}" ]] && report OPEN BROWSER "$BROWSER" || report closed BROWSER unset
if command -v code >/dev/null 2>&1 && [[ -n "${VSCODE_IPC_HOOK_CLI:-}" ]]; then
  report OPEN code-openExternal "the remote 'code' CLI can reach the editor (try: code --openExternal https://example.com)"
else
  report closed code-openExternal "no remote 'code' CLI with an IPC hook"
fi

# --- ports listening in the sandbox (candidates for forwarding to the Mac) ---
if command -v ss >/dev/null; then
  ports=$(ss -Hltn 2>/dev/null | awk '{print $4}' | grep -Ev '(^|:)22$' | sort -u | tr '\n' ' ')
  report info listening "${ports:-none besides sshd} (check the editor's Ports view)"
fi

# --- the editor's own sign-ins, as visible from the remote ---
for d in ~/.vscode-server/data/User/globalStorage ~/.kiro-server; do
  [[ -d $d ]] && report info editor-state "$d exists"
done

echo "# $open bridge(s) OPEN"
exit 0
