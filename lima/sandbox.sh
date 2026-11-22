#!/usr/bin/env bash
# sandbox.sh: create, run and remove one investigator sandbox on Lima, with
# investigator-net as the VM's only network. Runs on the Mac, as the user.
# See prototype-plan.md step 6 and investigator-design.md, "Leaving sbx: Lima".
#
#   lima/sandbox.sh up <name>       start investigator-net, then create (once) and start the VM
#   lima/sandbox.sh check <name>    run checks/lima-net.sh in the guest, and the Mac-side checks
#   lima/sandbox.sh shell <name>    a shell in the guest
#   lima/sandbox.sh down <name>     stop the VM and investigator-net
#   lima/sandbox.sh delete <name>   down, then delete the VM and its network
#
# Environment:
#   INV_LIMA_HOME   our own LIMA_HOME (default ~/.inv). Short, because Lima's
#                   socket paths must stay under 104 bytes, and our own, so a
#                   hand-typed limactl never sees these instances or networks.
#   INV_STATE       state directories, one per sandbox (default ~/.investigator)
#   INV_NET         the investigator-net binary (default: built from this repo)
#   INV_SUBNET      the network's /24 (default: the first free 192.168.1xx.0/24
#                   from 192.168.104.0/24, kept in the state directory)
#   INV_VMTYPE      vz (default) or krunkit
#   INV_USER        the guest user (default: your Mac user name, as Lima does)
#   INV_GITLAB_USER the GitLab user the agent acts for; empty runs no broker
#   INV_GITLAB_URL, INV_GITLAB_TOKEN_COMMAND, INV_GITLAB_MCP_TOKEN_COMMAND
#                   passed to investigator-net (see its -help)
#   INV_PROFILE     the filter's profile layer (default investigator)
#   INV_LAYERS      space-separated org or user layer files
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(dirname "$here")

export LIMA_HOME="${INV_LIMA_HOME:-$HOME/.inv}"
state_root="${INV_STATE:-$HOME/.investigator}"
vmtype="${INV_VMTYPE:-vz}"
guest_user="${INV_USER:-$(id -un)}"

die() { printf 'sandbox.sh: %s\n' "$*" >&2; exit 1; }
log() { printf '[sandbox] %s\n' "$*"; }

usage() { sed -n '2,25p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 2; }

[[ $# -eq 2 ]] || usage
cmd=$1 name=$2
[[ $name =~ ^[a-z][a-z0-9-]{0,14}$ ]] || die "name must be lowercase letters, digits and dashes, at most 15 characters"

state="$state_root/$name"
netdir="$LIMA_HOME/_networks/$name"
pidfile="$netdir/usernet_$name.pid"
ep="$netdir/${name}_ep.sock"
netpid="$state/net.pid"
netlog="$state/net.log"
networks_yaml="$LIMA_HOME/_config/networks.yaml"

need() { command -v "$1" >/dev/null || die "$1 not found"; }

# --- investigator-net ---

net_bin() {
  if [[ -n "${INV_NET:-}" ]]; then
    echo "$INV_NET"
    return
  fi
  local bin="$LIMA_HOME/_bin/investigator-net"
  need go
  mkdir -p "$LIMA_HOME/_bin"
  (cd "$repo" && go build -o "$bin" ./cmd/investigator-net) || die "building investigator-net failed"
  echo "$bin"
}

alive() { [[ -s $1 ]] && kill -0 "$(cat "$1")" 2>/dev/null; }

net_running() { alive "$netpid"; }

ep_get() { curl -sf --max-time 5 --unix-socket "$ep" "http://ep$1"; }

# The subnet: the one this sandbox had, else INV_SUBNET, else the first /24
# from 192.168.104.0 that no other sandbox holds.
pick_subnet() {
  if [[ -s $state/subnet ]]; then
    cat "$state/subnet"
    return
  fi
  if [[ -n "${INV_SUBNET:-}" ]]; then
    [[ $INV_SUBNET =~ ^([0-9]+\.){3}0/24$ ]] || die "INV_SUBNET must be a /24, like 192.168.104.0/24"
    echo "$INV_SUBNET"
    return
  fi
  local i s
  for ((i = 104; i < 200; i++)); do
    s="192.168.$i.0/24"
    if ! grep -qsx "$s" "$state_root"/*/subnet; then
      echo "$s"
      return
    fi
  done
  die "no free subnet"
}

prefix_of() { local s=${1%/24}; echo "${s%.0}"; }

start_net() {
  if net_running; then
    log "investigator-net already running (pid $(cat "$netpid"))"
    return
  fi
  local bin subnet
  bin=$(net_bin)
  subnet=$(pick_subnet)
  mkdir -p "$state"
  chmod 700 "$state"
  echo "$subnet" >"$state/subnet"
  local args=(lima -name "$name" -lima-dir "$netdir" -subnet "$subnet" -state "$state" -pidfile "$pidfile"
    -profile "${INV_PROFILE:-investigator}" -denials "$state/denials.jsonl")
  local l
  for l in ${INV_LAYERS:-}; do args+=(-layer "$l"); done
  if [[ -n "${INV_GITLAB_USER:-}" ]]; then
    args+=(-gitlab-user "$INV_GITLAB_USER" -gitlab-url "${INV_GITLAB_URL:-https://gitlab.com}")
    [[ -n "${INV_GITLAB_TOKEN_COMMAND:-}" ]] && args+=(-gitlab-token-command "$INV_GITLAB_TOKEN_COMMAND")
    [[ -n "${INV_GITLAB_MCP_TOKEN_COMMAND:-}" ]] && args+=(-gitlab-mcp-token-command "$INV_GITLAB_MCP_TOKEN_COMMAND")
  else
    log "no INV_GITLAB_USER: running without the broker"
  fi
  log "starting investigator-net on $subnet (log: $netlog)"
  nohup "$bin" "${args[@]}" >>"$netlog" 2>&1 </dev/null &
  echo $! >"$netpid"
  local i
  for ((i = 0; i < 50; i++)); do
    if ep_get /services/dhcp/leases >/dev/null 2>&1 && [[ -s $pidfile ]]; then
      break
    fi
    net_running || { tail -n 20 "$netlog" >&2; die "investigator-net exited"; }
    sleep 0.2
  done
  ep_get /services/dhcp/leases >/dev/null || die "investigator-net's endpoint doesn't answer"
  check_pidfile
}

# Lima starts its own (unfiltered) usernet daemon unless the pidfile names a
# live process; make sure that process is ours.
check_pidfile() {
  [[ -s $pidfile ]] || die "$pidfile is missing"
  [[ "$(cat "$pidfile")" == "$(cat "$netpid")" ]] ||
    die "$pidfile holds $(cat "$pidfile"), not investigator-net's $(cat "$netpid")"
}

stop_net() {
  if net_running; then
    local pid
    pid=$(cat "$netpid")
    log "stopping investigator-net (pid $pid)"
    kill "$pid"
    local i=0
    while kill -0 "$pid" 2>/dev/null && ((i++ < 50)); do sleep 0.1; done
    if kill -0 "$pid" 2>/dev/null; then kill -9 "$pid"; fi
  fi
  rm -f "$netpid"
}

# networks.yaml lists exactly the networks whose investigator-net is running,
# so Lima can't start an instance on a network we aren't serving.
write_networks() {
  mkdir -p "$(dirname "$networks_yaml")"
  local tmp="$networks_yaml.tmp" d n p entries=""
  for d in "$state_root"/*/; do
    n=$(basename "$d")
    alive "$d/net.pid" && [[ -s $d/subnet ]] || continue
    p=$(prefix_of "$(cat "$d/subnet")")
    entries+=$(printf '\n  %s:\n    mode: user-v2\n    gateway: %s.1\n    netmask: 255.255.255.0' "$n" "$p")
  done
  {
    echo "# Written by mcp-restrict's lima/sandbox.sh: only running investigator-net networks."
    if [[ -n $entries ]]; then echo "networks:$entries"; else echo "networks: {}"; fi
  } >"$tmp"
  mv "$tmp" "$networks_yaml"
}

# --- the instance ---

instance_exists() { limactl list -q 2>/dev/null | grep -qx "$name"; }

instance_status() { limactl list --format '{{.Status}}' "$name" 2>/dev/null || true; }

# Fill the template. A placeholder alone on its line is replaced by a file,
# indented four spaces; the others are replaced in place.
render() {
  local prefix ca="$state/broker-ca/ca.pem" line
  prefix=$(prefix_of "$(cat "$state/subnet")")
  if [[ -n "${INV_GITLAB_USER:-}" ]]; then
    [[ -s $ca ]] || die "the broker's CA ($ca) is missing"
  else
    ca=/dev/null
  fi
  while IFS= read -r line || [[ -n $line ]]; do
    case $line in
    @PROVISION_SH@) sed 's/^/    /' "$repo/sandbox/provision.sh" ;;
    @BROKER_CA_PEM@) sed 's/^/    /' "$ca" ;;
    *)
      line=${line//@VMTYPE@/$vmtype}
      line=${line//@NETWORK@/$name}
      line=${line//@USER@/$guest_user}
      line=${line//@GATEWAY@/$prefix.2}
      printf '%s\n' "$line"
      ;;
    esac
  done <"$here/investigator.yaml.in"
}

guest() { limactl shell --workdir / "$name" "$@"; }

# After boot: the VM has one NIC besides lo and Docker's, and its MAC holds
# one of our leases (so its network is ours, not Lima's).
check_boot() {
  local links macs leases bad=0
  links=$(guest ip -o link show | awk -F': ' '{print $2}' | sed 's/@.*//' |
    grep -Ev '^(lo|docker[0-9]*|br-.*|veth.*)$' || true)
  if [[ $(printf '%s\n' "$links" | grep -c .) -ne 1 ]]; then
    log "FAIL the VM has these NICs: $(echo $links)"
    bad=1
  fi
  macs=$(guest cat "/sys/class/net/$(printf '%s\n' "$links" | head -n1)/address" 2>/dev/null || true)
  leases=$(ep_get /services/dhcp/leases || true)
  if [[ -z $macs ]] || ! grep -qi "\"$macs\"" <<<"$leases"; then
    log "FAIL the VM's MAC ($macs) has no lease from investigator-net: $leases"
    bad=1
  fi
  [[ $bad -eq 0 ]] || { do_down; die "the VM isn't on investigator-net only; stopped it"; }
  log "PASS one NIC ($links, $macs), leased by investigator-net"
}

do_up() {
  need limactl
  need curl
  start_net
  write_networks
  if ! instance_exists; then
    local yaml="$state/instance.yaml"
    render >"$yaml"
    log "creating instance $name ($vmtype)"
    limactl create --tty=false --name "$name" "$yaml"
  fi
  check_pidfile
  if [[ $(instance_status) != Running ]]; then
    log "starting instance $name"
    limactl start --tty=false "$name"
  fi
  check_pidfile
  check_boot
  log "up: limactl shell $name (with LIMA_HOME=$LIMA_HOME), or lima/sandbox.sh shell $name"
}

do_down() {
  if instance_exists && [[ $(instance_status) == Running ]]; then
    log "stopping instance $name"
    limactl stop "$name" || limactl stop --force "$name"
  fi
  stop_net
  write_networks
}

do_delete() {
  do_down
  if instance_exists; then
    log "deleting instance $name"
    limactl delete --force "$name"
  fi
  rm -rf "$netdir"
  log "deleted. The state directory ($state: the broker's CA, logs, denials) is kept; remove it by hand."
}

# The checks from outside: no guest port reaches the Mac, and investigator-net
# forwards nothing but SSH.
do_check() {
  instance_exists && [[ $(instance_status) == Running ]] || die "$name isn't running"
  net_running || die "investigator-net isn't running"
  check_pidfile
  check_boot
  local fails=0 prefix
  prefix=$(prefix_of "$(cat "$state/subnet")")
  limactl copy "$repo/checks/lima-net.sh" "$name:/tmp/lima-net.sh"
  guest bash /tmp/lima-net.sh "$prefix.2" "${INV_GITLAB_USER:+broker}" || fails=1

  # A guest listener on all addresses must not appear on the Mac.
  local port=18999
  guest sh -c "nohup python3 -m http.server $port >/dev/null 2>&1 & echo \$! >/tmp/lima-net-listen.pid"
  sleep 3
  if curl -s --max-time 2 -o /dev/null "http://127.0.0.1:$port/"; then
    log "FAIL a guest port ($port) is reachable on the Mac's loopback"
    fails=1
  else
    log "PASS a guest listener on :$port isn't forwarded to the Mac"
  fi
  guest sh -c 'kill "$(cat /tmp/lima-net-listen.pid)"' || true

  local fw ports
  fw=$(ep_get /services/forwarder/all || echo '?')
  # Over vsock, Lima asks for no forwards at all: an empty list passes.
  ports=$(grep -o '"remote":"[^"]*"' <<<"$fw" | sed 's/.*:\([0-9]*\)"$/\1/' | sort -u || true)
  if [[ $fw == '?' ]] || { [[ -n $ports ]] && grep -qvx 22 <<<"$ports"; }; then
    log "FAIL investigator-net forwards more than SSH: $fw"
    fails=1
  else
    log "PASS investigator-net forwards only SSH: $fw"
  fi

  if [[ $fails -eq 0 ]]; then log "ALL PASS"; else log "SOME CHECKS FAILED"; exit 1; fi
}

case $cmd in
up) do_up ;;
down) do_down ;;
delete) do_delete ;;
check) do_check ;;
shell) guest bash -l ;;
*) usage ;;
esac
