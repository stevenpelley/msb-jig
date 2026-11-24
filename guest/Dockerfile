# The sandbox image: the same provision.sh the supervisor runs at start.
# Used as the sbx template image, and here as a stand-in sandbox for tests.
FROM ubuntu:26.04
ARG SANDBOX_USER=agent
ARG PROVISION_COMPONENTS="base sshd git docker claude"
COPY provision.sh install-keys.sh /opt/mono-jig/
RUN SANDBOX_USER=$SANDBOX_USER PROVISION_COMPONENTS="$PROVISION_COMPONENTS" \
    bash /opt/mono-jig/provision.sh \
 && rm -rf /var/lib/apt/lists/* \
 # No baked-in host keys: each sandbox gets its own from install-keys.sh.
 && rm -f /etc/ssh/ssh_host_*
# sshd in the foreground; the host key arrives later through install-keys.sh.
CMD ["bash", "-c", "until [ -f /etc/ssh/ssh_host_ed25519_key ]; do sleep 0.2; done; exec /usr/sbin/sshd -D -e"]
