# The base image for microsandbox sandboxes: provision.sh's parts that
# depend on nothing but the image (packages, the agent user, git, Docker,
# Claude Code), so a sandbox's first start doesn't install them.
# mono-jig image build builds it from the copies embedded in the
# binary and passes the arguments; /etc/mono-jig/image tells the
# supervisor which parts are here and from which provision.sh.
# The parts that depend on your logins and on the sandbox (the broker's
# CA and settings, Claude's and AWS's settings) run at every start.
FROM ubuntu:26.04
ARG SANDBOX_USER
ARG SANDBOX_UID
ARG PROVISION_COMPONENTS
ARG PROVISION_HASH
COPY provision.sh /opt/mono-jig/provision.sh
RUN SANDBOX_USER="$SANDBOX_USER" SANDBOX_UID="$SANDBOX_UID" \
    PROVISION_COMPONENTS="$PROVISION_COMPONENTS" BROKER_GITLAB_URL= \
    bash /opt/mono-jig/provision.sh \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /etc/mono-jig \
 && printf 'components=%s\nprovision=%s\n' "$PROVISION_COMPONENTS" "$PROVISION_HASH" >/etc/mono-jig/image
