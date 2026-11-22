#!/usr/bin/env bash
# lima-net.sh: check, from inside a Lima investigator sandbox, that the
# filter and the broker are the only ways out. lima/sandbox.sh check copies
# it in and runs it; by hand:
#
#   checks/lima-net.sh <gateway> [broker]
#
# <gateway> is investigator-net's address (the subnet's .2). Pass "broker"
# when investigator-net runs the GitLab broker. Each line is PASS or FAIL.
set -u

gw="${1:?usage: lima-net.sh <gateway> [broker]}"
broker="${2:-}"
proxy=http://proxy.internal:3128
fails=0
pass() { printf 'PASS %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*"; fails=$((fails + 1)); }

echo "# lima-net.sh on $(hostname) at $(date -u +%FT%TZ)"

# --- names: ours resolve to the gateway, nothing else resolves ---
got=$(getent ahostsv4 proxy.internal | awk 'NR==1{print $1}')
[[ $got == "$gw" ]] && pass "proxy.internal is $gw" || fail "proxy.internal resolved to '${got}', want $gw"
got=$(getent ahostsv4 gitlab.broker.internal | awk 'NR==1{print $1}')
[[ $got == "$gw" ]] && pass "gitlab.broker.internal is $gw" || fail "gitlab.broker.internal resolved to '${got}', want $gw"
if out=$(getent hosts example.com); then fail "example.com resolved: $out"; else pass "example.com doesn't resolve"; fi
if timeout 3 bash -c 'exec 3<>/dev/tcp/8.8.8.8/53' 2>/dev/null; then
  fail "TCP to 8.8.8.8:53 connected"
else
  pass "no outside DNS server (8.8.8.8:53) is reachable"
fi

# --- the filter: an allowed host works, a denied one gets 403 ---
code=$(curl -s -o /dev/null -w '%{http_connect} %{http_code}' --max-time 20 -x "$proxy" https://claude.ai/install.sh)
[[ $code == "200 "[1-5]?? ]] && pass "claude.ai through the filter (CONNECT, then HTTP: $code)" ||
  fail "claude.ai through the filter: CONNECT, then HTTP: $code"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 -x "$proxy" http://archive.ubuntu.com/ubuntu/)
[[ $code == 200 || $code == 30? ]] && pass "archive.ubuntu.com:80 through the filter ($code)" ||
  fail "archive.ubuntu.com:80 through the filter: $code"
# curl reports a refused CONNECT as 000 with the proxy's code in %{http_connect}.
code=$(curl -s -o /dev/null -w '%{http_connect}' --max-time 20 -x "$proxy" https://example.com/)
[[ $code == 403 ]] && pass "example.com is denied by the filter (403)" || fail "example.com through the filter: CONNECT $code, want 403"
code=$(curl -s -o /dev/null -w '%{http_connect}' --max-time 20 -x "$proxy" https://1.1.1.1/)
[[ $code == 403 ]] && pass "an IP literal is denied by the filter (403)" || fail "1.1.1.1 through the filter: CONNECT $code, want 403"

# --- around the filter: nothing, and it fails at once ---
start=$(date +%s)
if curl -s -o /dev/null --noproxy '*' --max-time 10 https://1.1.1.1/; then
  fail "a direct connection to 1.1.1.1 worked"
else
  took=$(($(date +%s) - start))
  ((took <= 3)) && pass "a direct connection to 1.1.1.1 fails (${took}s)" ||
    fail "a direct connection to 1.1.1.1 failed only after ${took}s (want a reset)"
fi
if ping -c 1 -W 2 1.1.1.1 >/dev/null 2>&1; then fail "ping 1.1.1.1 answered"; else pass "ping 1.1.1.1 gets no answer"; fi
for p in 22 80 8080; do  # 53 is the DNS server's, 3128 the filter's, 443 the broker's
  if timeout 3 bash -c "exec 3<>/dev/tcp/$gw/$p" 2>/dev/null; then
    fail "the gateway's TCP port $p answers"
  else
    pass "the gateway's TCP port $p is closed"
  fi
done

# --- the broker: its TLS, under the CA provisioning installed ---
if [[ $broker == broker ]]; then
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 --noproxy '*' https://gitlab.broker.internal/api/v4/version)
  [[ $code =~ ^[1-5][0-9][0-9]$ ]] && pass "the broker answers over trusted TLS ($code)" ||
    fail "the broker over TLS: $code (curl exit; 000 means the connection or the certificate failed)"
else
  echo "skip the broker (investigator-net runs without one)"
fi

# --- one NIC, no default route out but the gateway ---
nics=$(ip -o link show | awk -F': ' '{print $2}' | sed 's/@.*//' | grep -Ev '^(lo|docker[0-9]*|br-.*|veth.*)$')
[[ $(grep -c . <<<"$nics") -eq 1 ]] && pass "one NIC ($nics)" || fail "NICs: $(echo $nics)"
route=$(ip -4 route show default)
[[ -z $route || $route == *"via $gw "* ]] && pass "default route: ${route:-none}" || fail "default route not via $gw: $route"

if ((fails == 0)); then echo "ALL PASS"; else echo "$fails FAILED"; exit 1; fi
