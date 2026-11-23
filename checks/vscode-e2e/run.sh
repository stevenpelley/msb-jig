#!/usr/bin/env bash
# run.sh: VS Code over Remote-SSH into a sandbox, unattended. A probe
# extension, loaded for this run only, runs checks/bridges.sh in an
# integrated terminal in the sandbox, and VS Code quits. Opens a window.
#
#   [EXTRA=<script>] checks/vscode-e2e/run.sh <sandbox> ours|default [out-dir]
#
# "default" is VS Code's own settings (the baseline: bridges expected open);
# "ours" is the harness's (cli.VSCodeSettings). Each run gets a fresh
# user-data directory; extensions come from the harness's directory
# (Remote-SSH installed with `code --extensions-dir`).
set -euo pipefail
name=$1 mode=$2
here=$(cd "$(dirname "$0")" && pwd)
root="${INV_STATE:-$HOME/.investigator}"
cfg="$root/ssh_config" host="investigator-$name"
out="${3:-$here/out}/$mode"
code="${CODE:-/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code}"
ud=$(mktemp -d "${TMPDIR:-/tmp}/inv-vscode-$mode.XXXXXX")
ssh=(ssh -F "$cfg" "$host")

mkdir -p "$out" "$ud/User"
(cd "$here/../.." && go run ./checks/vscode-e2e/settings "$mode" "$cfg" "$host") >"$ud/User/settings.json"
"${ssh[@]}" 'rm -rf /tmp/inv-e2e && mkdir -p /tmp/inv-e2e'
tar -C "$here" -cf - ext | "${ssh[@]}" 'tar -C /tmp/inv-e2e -xf -'
"${ssh[@]}" 'cat > /tmp/inv-e2e/bridges.sh' <"$here/../bridges.sh"
# EXTRA=<script>: run in the same terminal before bridges.sh (output: extra.txt).
if [[ -n "${EXTRA:-}" ]]; then "${ssh[@]}" 'cat > /tmp/inv-e2e/extra.sh' <"$EXTRA"; fi

echo "launching VS Code ($mode; user data $ud)"
"$code" --user-data-dir "$ud" --extensions-dir "$root/_vscode-ext" --new-window \
  --remote "ssh-remote+$host" \
  --extensionDevelopmentPath="vscode-remote://ssh-remote+$host/tmp/inv-e2e/ext" /home/agent

for _ in $(seq 300); do
  "${ssh[@]}" 'test -e /tmp/inv-e2e/done' 2>/dev/null && break
  sleep 1
done
for f in out.txt extra.txt terminal-env.txt exthost-env.txt probe-error.txt done; do
  "${ssh[@]}" "cat /tmp/inv-e2e/$f" >"$out/$f" 2>/dev/null || rm -f "$out/$f"
done
sleep 10
pkill -f -- "--user-data-dir $ud" 2>/dev/null || true
pkill -f -- "--user-data-dir=$ud" 2>/dev/null || true
cp -R "$ud/logs" "$out/logs" 2>/dev/null || true
if [[ -e $out/done ]]; then
  echo "done: $out/out.txt"
  cat "$out/out.txt"
else
  echo "no result after 5 minutes; logs in $out/logs" >&2
  exit 1
fi
