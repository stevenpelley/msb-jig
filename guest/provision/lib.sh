# lib.sh: provision.sh's settings and helpers, sourced by it.

SANDBOX_USER="${SANDBOX_USER:-agent}"
SANDBOX_UID="${SANDBOX_UID:-}"
PROVISION_COMPONENTS="${PROVISION_COMPONENTS:-base sshd git broker docker}"
GUEST_CONFIG="${GUEST_CONFIG:-/root/guest.json}"
GUEST_CA="${GUEST_CA:-/root/guest-ca.pem}"
BROKER_CA_FILE="${BROKER_CA_FILE:-}"
export DEBIAN_FRONTEND=noninteractive

want() { [[ " $PROVISION_COMPONENTS " == *" $1 "* ]]; }
log() { printf '[provision] %s\n' "$*"; }
# apt_lists: fetch the package lists unless they're here. Each layer of the
# image removes its own, and a first start on a plain image has none until
# base runs.
apt_lists() { compgen -G '/var/lib/apt/lists/*_Packages*' >/dev/null || apt-get update -qq; }

# client_env <name> [KEY=VALUE...]: the tools' settings, where every session
# reads them. /etc/profile.d/<name>.sh serves login shells; a block in
# /etc/environment serves sshd sessions without one (pam_env), such as
# `ssh host cmd` and VS Code's server, whose terminals and extensions inherit
# it. Values must not contain spaces or quotes. No pairs: remove both.
client_env() {
  local name=$1 kv; shift
  rm -f "/etc/profile.d/$name.sh"
  touch /etc/environment
  sed -i "/^# $name begin\$/,/^# $name end\$/d" /etc/environment
  (( $# )) || return 0
  {
    echo "# Written by mono-jig provision.sh."
    for kv in "$@"; do printf 'export %s="%s"\n' "${kv%%=*}" "${kv#*=}"; done
  } >"/etc/profile.d/$name.sh"
  chmod 0644 "/etc/profile.d/$name.sh"
  { echo "# $name begin"; printf '%s\n' "$@"; echo "# $name end"; } >>/etc/environment
}

if [[ $EUID -ne 0 ]]; then
  echo "provision.sh must run as root" >&2
  exit 1
fi
