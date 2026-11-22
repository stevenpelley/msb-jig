#!/usr/bin/env bash
# install-keys.sh: install the per-sandbox SSH host key and the user's
# authorized key, generated on the Mac by `investigator`.
#
# Reads a tar stream on stdin with two files:
#   ssh_host_ed25519_key   the host's private key (its public half is pinned
#                          in the CLI's known_hosts)
#   authorized_keys        the per-sandbox client key
# Usage (run as root in the sandbox):
#   tar -C <state>/ssh/sandbox -cf - . | <exec into sandbox> install-keys.sh <user>
set -euo pipefail

user="${1:?usage: install-keys.sh <user>}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
tar -C "$tmp" -xf -

install -m 0600 -o root -g root "$tmp/ssh_host_ed25519_key" /etc/ssh/ssh_host_ed25519_key
ssh-keygen -y -f /etc/ssh/ssh_host_ed25519_key >/etc/ssh/ssh_host_ed25519_key.pub
# Only our host key: remove the image's generated ones so no other key is offered.
rm -f /etc/ssh/ssh_host_{rsa,ecdsa}_key{,.pub}

home="$(getent passwd "$user" | cut -d: -f6)"
install -d -m 0700 -o "$user" -g "$user" "$home/.ssh"
install -m 0600 -o "$user" -g "$user" "$tmp/authorized_keys" "$home/.ssh/authorized_keys"
# sshd loads host keys at startup, so restart it where it already runs.
if command -v systemctl >/dev/null && systemctl is-active --quiet ssh 2>/dev/null; then
  systemctl restart ssh
fi
echo "[install-keys] host key and authorized key installed for $user"
