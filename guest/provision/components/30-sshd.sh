# sshd: a provision.sh component, sourced by it (lib.sh's helpers).

# --- sshd: key-only, one user, no forwarding toward the sandbox ---
# These settings guard against mistakes in the editor's settings. They don't
# bind the agent, which can be root in the VM; the Mac-side settings do.
if want sshd; then
  log "sshd"
  # At a first start on an image built without sshd, base hasn't run and
  # the image keeps no apt lists (apt-cache's stale cache can still name
  # the package, so update whenever it's missing).
  if ! dpkg -s openssh-server >/dev/null 2>&1; then
    apt-get update -qq
    apt-get install -y -qq --no-install-recommends openssh-server >/dev/null
  fi
  install -d -m 0755 /etc/ssh/sshd_config.d /run/sshd
  # Ubuntu's sshd_config includes sshd_config.d/*.conf first, and the first
  # value sshd reads wins, so this file overrides the defaults below it.
  cat >/etc/ssh/sshd_config.d/10-mono-jig.conf <<EOF
# Written by mono-jig provision.sh. See docs/design.md,
# "What the mono-jig CLI configures".
AllowUsers $SANDBOX_USER
PermitRootLogin no
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitEmptyPasswords no
AuthenticationMethods publickey
AllowAgentForwarding no
X11Forwarding no
PermitTunnel no
AllowStreamLocalForwarding no
# The editors reach their server through local forwards; nothing needs remote ones.
AllowTcpForwarding local
GatewayPorts no
PermitUserEnvironment no
PermitUserRC no
HostKey /etc/ssh/ssh_host_ed25519_key
EOF
  # No host keys in the image: openssh-server's install makes some, and
  # every sandbox from the image would share them. Each sandbox gets its
  # own (install-keys.sh on sbx; the supervisor on microsandbox), and
  # sshd -t runs then.
  rm -f /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub
fi
