# The base image for microsandbox sandboxes (on BASE_IMAGE, default
# Ubuntu): provision.sh's parts that
# depend on nothing but the image (packages, the agent user, git, Docker,
# Claude Code), so a sandbox's first start doesn't install them.
# mono-jig image build builds it from the copies embedded in the
# binary and passes the arguments; /etc/mono-jig/image tells the
# supervisor which parts are here and from which provision.sh.
# The parts that depend on your logins and on the sandbox (the broker's
# CA and settings, Claude's and AWS's settings) run at every start.
ARG BASE_IMAGE=ubuntu:26.04
FROM ${BASE_IMAGE}
ARG SANDBOX_USER
ARG SANDBOX_UID
COPY provision/provision.sh provision/lib.sh /opt/mono-jig/provision/
# Extra CAs (mono-jig ca), before anything downloads: ca-certificates,
# installed by provision.sh's base, trusts them.
COPY ca/ /usr/local/share/ca-certificates/mono-jig-build/
# One layer per component, in provision.sh's order, each with only its own
# file (ImageDockerfile writes them here): a changed component rebuilds
# from its layer on, and the layers before it come from the build cache.
# Each removes the apt lists it fetched, so none stay in the image.
# COMPONENT LAYERS
# Last, so a change to provision files the image doesn't run (the
# settings every start writes) rebuilds only this layer.
ARG PROVISION_COMPONENTS
ARG PROVISION_HASH
RUN mkdir -p /etc/mono-jig \
 && printf 'components=%s\nprovision=%s\n' "$PROVISION_COMPONENTS" "$PROVISION_HASH" >/etc/mono-jig/image
