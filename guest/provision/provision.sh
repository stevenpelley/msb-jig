#!/usr/bin/env bash
# provision.sh: install everything the mono-jig sandbox needs.
#
# It sources lib.sh (the settings below, and helpers), then each file in
# components/, in name order; each runs only if PROVISION_COMPONENTS names
# it. A track replaces a component (base, for an image that isn't Debian
# or Ubuntu) or adds one by changing the files (guest.Provision).
#
# One script for every packaging: the sandbox image's Dockerfile runs it at
# build time, and the supervisor runs it (or some components) at every
# start on microsandbox. Run as root. Idempotent.
#
# Environment:
#   SANDBOX_USER         the one user that may log in over SSH (default: agent).
#   SANDBOX_UID          its UID, when it's created (default: useradd's choice);
#                        the supervisor fixes it so host folders mount as its
#   PROVISION_COMPONENTS space-separated subset of: ca base sshd git broker
#                        docker apply, and the agent's (claude, from the agent
#                        package) (default: base sshd git broker docker).
#                        apply writes the
#                        settings the supervisor built (guest.Config, from the
#                        providers and the agent): the environment, git's
#                        settings, files such as glab's config, mcpcli and the
#                        agent's instructions. The supervisor runs it on every
#                        start, so the guest follows the broker's providers.
#                        ca, first on every start, trusts the extra CAs in
#                        GUEST_CA (a TLS-inspecting proxy's root), before
#                        anything downloads.
#   GUEST_CONFIG         apply's settings, JSON (default /root/guest.json)
#   GUEST_CA             ca's certificates, PEM (default /root/guest-ca.pem)
#   BROKER_CA_FILE       the broker's CA certificate, installed as trusted if given
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$here/lib.sh"
for component in "$here"/components/*.sh; do
  # shellcheck disable=SC1090
  source "$component"
done

log "done"
