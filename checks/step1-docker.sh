#!/usr/bin/env bash
# step1-docker.sh: docs/prototype-plan.md step 1, against a Docker container
# standing in for the sandbox, with `docker exec -i ... nc localhost 22` in
# place of `sbx exec`.
#
# Checks:
#   1. ssh -F <config> connects with no host-key prompt (StrictHostKeyChecking yes);
#   2. the server refuses agent forwarding even when the client asks (-A);
#   3. the server refuses remote forwards and allows local ones;
#   4. password and other-user logins fail;
#   5. bridges.sh in the session reports no OPEN bridge.
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
name="${NAME:-step1-$$}"
image="${IMAGE:-mono-jig-sandbox:dev}"
state="$(mktemp -d)"
fail=0
pass() { echo "PASS  $*"; }
bad() { echo "FAIL  $*"; fail=1; }
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  [[ -n "${agent_pid:-}" ]] && kill "$agent_pid" 2>/dev/null || true
  rm -rf "$state"
}
trap cleanup EXIT

if ! docker image inspect "$image" >/dev/null 2>&1; then
  docker build -q -t "$image" "$here/guest" >/dev/null
fi
docker run -d --name "$name" "$image" >/dev/null

(cd "$here" && go run ./checks/ssh-setup -name "$name" -state "$state" \
  -proxy-command "docker exec -i $name nc localhost 22") >/dev/null
sb="$state/sandboxes/$name/ssh"
tar -C "$sb/sandbox" -cf - . | docker exec -i "$name" /opt/mono-jig/install-keys.sh agent >/dev/null
docker cp "$here/checks/bridges.sh" "$name:/usr/local/bin/bridges.sh" >/dev/null

cfg="$sb/config"
host="mono-jig-$name"
for _ in $(seq 50); do
  ssh -F "$cfg" -o BatchMode=yes -o ConnectTimeout=5 "$host" true 2>/dev/null && break
  sleep 0.2
done

# 1. connects, host key pinned, no prompt
if out=$(ssh -F "$cfg" -o BatchMode=yes "$host" 'id -un' 2>&1) && [[ $out == agent ]]; then
  pass "ssh connects as agent with the pinned host key, no prompt"
else
  bad "ssh connect: $out"
fi
# A different host key must be refused, not prompted.
echo "$host ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKpZ2b7pZ2Y6s3n6hW5a0xz7m1VQ3wC8G5vKZ4n1o6eT" >"$state/wrong_kh"
if ssh -F "$cfg" -o BatchMode=yes -o UserKnownHostsFile="$state/wrong_kh" "$host" true 2>/dev/null; then
  bad "connected with a wrong pinned host key"
else
  pass "a wrong pinned host key is refused"
fi

# 2. agent forwarding refused by the server even with -A and ForwardAgent yes
eval "$(ssh-agent -s)" >/dev/null
agent_pid=$SSH_AGENT_PID
ssh-add -q "$sb/id_ed25519" 2>/dev/null
remote_sock=$(ssh -F "$cfg" -A -o ForwardAgent=yes -o IdentityAgent="$SSH_AUTH_SOCK" -o BatchMode=yes "$host" 'echo "${SSH_AUTH_SOCK:-unset}"' 2>/dev/null)
if [[ $remote_sock == unset ]]; then
  pass "agent forwarding refused by sshd (SSH_AUTH_SOCK unset with -A)"
else
  bad "agent forwarding reached the sandbox: $remote_sock"
fi

# 3. remote forwards refused, local forwards allowed
rf=$(ssh -F "$cfg" -o BatchMode=yes -o ExitOnForwardFailure=yes -R 127.0.0.1:2999:127.0.0.1:9 "$host" true 2>&1 || true)
if [[ $rf == *"forwarding failed"* || $rf == *"administratively prohibited"* || $rf == *"remote port forwarding failed"* ]]; then
  pass "remote forward refused"
else
  bad "remote forward not refused: $rf"
fi
docker exec -d "$name" bash -c 'while true; do echo hi | nc -l -s 127.0.0.1 -p 8123 -q 1; done'
docker exec -d "$name" bash -c 'while true; do rm -f /tmp/u.sock; echo hi | nc -lU /tmp/u.sock -q 1; done'
sleep 0.5
fwd() { # -L spec; prints what a client reads through it
  local spec=$1 target=$2
  ssh -F "$cfg" -o BatchMode=yes -N -L "$spec" "$host" 2>/dev/null & local pid=$!
  sleep 1
  if [[ $target == /* ]]; then
    timeout 3 nc -U "$target" </dev/null 2>/dev/null || true
  else
    timeout 3 bash -c "exec 3<>/dev/tcp/127.0.0.1/$target; head -c 2 <&3" 2>/dev/null || true
  fi
  kill $pid 2>/dev/null; wait $pid 2>/dev/null || true
}
if [[ "$(fwd 127.0.0.1:18123:127.0.0.1:8123 18123)" == hi* ]]; then
  pass "local TCP forward works (what the editors use)"
else
  bad "local TCP forward failed"
fi
st=$(ssh -F "$cfg" -o BatchMode=yes -W 127.0.0.1:8123 "$host" </dev/null 2>&1 | head -c 2 || true)
[[ $st == hi ]] && pass "direct-tcpip (-W) works" || echo "INFO  -W returned: $st"
if [[ "$(fwd "$state/sock:/tmp/u.sock" "$state/sock")" == hi* ]]; then
  bad "streamlocal forward to a Unix socket in the sandbox allowed"
else
  pass "streamlocal forward refused"
fi

# 4. no password logins, no other users
if ssh -F "$cfg" -o BatchMode=yes -o PreferredAuthentications=password "$host" true 2>/dev/null; then
  bad "password authentication accepted"
else
  pass "password authentication refused"
fi
if ssh -F "$cfg" -o BatchMode=yes -l root "$host" true 2>/dev/null; then
  bad "root login accepted"
else
  pass "root login refused"
fi
eff=$(docker exec "$name" sshd -T 2>/dev/null | grep -Ei '^(allowagentforwarding|allowtcpforwarding|allowstreamlocalforwarding|permittunnel|x11forwarding|passwordauthentication) ' | tr '\n' ' ')
echo "INFO  effective sshd: $eff"

# 5. bridges.sh in the session
br=$(ssh -F "$cfg" -o BatchMode=yes "$host" 'bash -l /usr/local/bin/bridges.sh' 2>&1)
echo "$br" | sed 's/^/      /'
if grep -q '^# 0 bridge(s) OPEN' <<<"$br"; then
  pass "bridges.sh: no OPEN bridges"
else
  bad "bridges.sh reported OPEN bridges"
fi

[[ $fail == 0 ]] && echo "ALL PASS" || { echo "SOME FAILED"; exit 1; }
