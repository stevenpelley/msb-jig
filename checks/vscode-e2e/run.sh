#!/usr/bin/env bash
# run.sh: VS Code over Remote-SSH into a sandbox, unattended. A probe
# extension, loaded for this run only, runs checks/bridges.sh in an
# integrated terminal in the sandbox, and VS Code quits. Opens a window.
#
#   [EXTRA=<script>] checks/vscode-e2e/run.sh <sandbox> ours|default [out-dir]
#
# More, by environment variable:
#   SETTINGS='<json>'  merged over the settings (e.g. remote.SSH.defaultExtensions)
#   SETUP=<script>     run in the guest over ssh before VS Code starts
#   FOLDER=<path>      the guest folder to open (default /home/agent)
#   INSTALL='<ids>'    extensions the probe tries to install (install-result.txt)
#   CLAUDE=1           check Claude Code's extension on the remote (claude-ext.txt)
#   PORT=<n>           after the checks, keep VS Code up and record whether the
#                      Mac listens on that port (ports.txt): forwarded or not
#
# "default" is VS Code's own settings (the baseline: bridges expected open);
# "ours" is the harness's (cli.VSCodeSettings). Each run gets a fresh
# user-data directory; extensions come from the harness's directory
# (Remote-SSH installed with `code --extensions-dir`).
set -euo pipefail
name=$1 mode=$2
here=$(cd "$(dirname "$0")" && pwd)
root="${JIG_STATE:-$HOME/.mono-jig}"
cfg="$root/ssh_config" host="mono-jig-$name"
out="${3:-$here/out}/$mode"
code="${CODE:-/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code}"
ud=$(mktemp -d "${TMPDIR:-/tmp}/jig-vscode-$mode.XXXXXX")
ssh=(ssh -F "$cfg" "$host")

mkdir -p "$out" "$ud/User"
(cd "$here/../.." && go run ./checks/vscode-e2e/settings "$mode" "$cfg" "$host" "${SETTINGS:-}") >"$ud/User/settings.json"
"${ssh[@]}" 'rm -rf /tmp/jig-e2e && mkdir -p /tmp/jig-e2e'
tar -C "$here" -cf - ext | "${ssh[@]}" 'tar -C /tmp/jig-e2e -xf -'
"${ssh[@]}" 'cat > /tmp/jig-e2e/bridges.sh' <"$here/../bridges.sh"
# EXTRA=<script>: run in the same terminal before bridges.sh (output: extra.txt).
if [[ -n "${EXTRA:-}" ]]; then "${ssh[@]}" 'cat > /tmp/jig-e2e/extra.sh' <"$EXTRA"; fi
if [[ -n "${SETUP:-}" ]]; then "${ssh[@]}" bash -s <"$SETUP"; fi
if [[ -n "${INSTALL:-}" ]]; then printf '%s\n' $INSTALL | "${ssh[@]}" 'cat > /tmp/jig-e2e/install.txt'; fi
if [[ -n "${CLAUDE:-}" ]]; then "${ssh[@]}" 'touch /tmp/jig-e2e/claude'; fi
if [[ -n "${PORT:-}" ]]; then "${ssh[@]}" 'touch /tmp/jig-e2e/hold'; fi

echo "launching VS Code ($mode; user data $ud)"
"$code" --user-data-dir "$ud" --extensions-dir "$root/_vscode-ext" --new-window \
  --remote "ssh-remote+$host" \
  --extensionDevelopmentPath="vscode-remote://ssh-remote+$host/tmp/jig-e2e/ext" "${FOLDER:-/home/agent}"

for _ in $(seq 600); do
  "${ssh[@]}" 'test -e /tmp/jig-e2e/done' 2>/dev/null && break
  sleep 1
done
if [[ -n "${PORT:-}" ]]; then
  for _ in $(seq 60); do "${ssh[@]}" 'test -e /tmp/jig-e2e/held' 2>/dev/null && break; sleep 1; done
  sleep 15 # VS Code looks for new listeners every few seconds
  { echo "Mac listeners on $PORT:"; lsof -nP -iTCP:"$PORT" -sTCP:LISTEN || echo "(none)"; } >"$out/ports.txt"
  "${ssh[@]}" 'touch /tmp/jig-e2e/release'
fi
for f in out.txt extra.txt terminal-env.txt exthost-env.txt probe-error.txt done \
  extensions.txt install-result.txt claude-ext.txt; do
  "${ssh[@]}" "cat /tmp/jig-e2e/$f" >"$out/$f" 2>/dev/null || rm -f "$out/$f"
done
sleep 10
pkill -f -- "--user-data-dir $ud" 2>/dev/null || true
pkill -f -- "--user-data-dir=$ud" 2>/dev/null || true
cp -R "$ud/logs" "$out/logs" 2>/dev/null || true
if [[ -e $out/done ]]; then
  echo "done: $out/out.txt"
  cat "$out/out.txt"
  for f in extensions.txt install-result.txt claude-ext.txt ports.txt; do
    if [[ -e $out/$f ]]; then echo "--- $f"; cat "$out/$f"; fi
  done
else
  echo "no result after 10 minutes; logs in $out/logs" >&2
  exit 1
fi
