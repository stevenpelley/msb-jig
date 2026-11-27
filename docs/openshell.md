# OpenShell, revisited: Docker inside the sandbox

*Researched 2026-11-27 against OpenShell v0.1.10 (2026-11-24) and `main`
at `659b45f`, from its docs, RFCs and source (`ai-openshell/openshell`,
formerly `NVIDIA/OpenShell`). Nothing was run: this sandbox has no KVM,
and nothing was tried on the Mac. Sysbox facts are from its docs at
v0.7.2 (2026-11-23).*

**Decision (user, 2026-11-27): stay on microsandbox.** OpenShell can't run
Docker inside its sandboxes, on any of its drivers, and that follows from
how it builds a sandbox, not from a missing setting. Docker in the sandbox
is the reason the harness uses a VM at all, so this rules it out for now.

## Why we looked again

- The Mac install is now one script (Homebrew; CLI, gateway, VM driver,
  prover; the gateway runs as a brew service). Automating it is fine.
- The project moved to the `ai-openshell` organization, with a governance
  document that commits to vendor neutrality, and stable releases every
  Tuesday (RFC 0014). An easier sell to management than microsandbox.
- It's built for automated agents: a central gateway, a Kubernetes
  driver, SPIFFE identities, audit records, OCSF logs.
- Supervisor middleware looked like microsandbox's outbound proxy, the
  hook our broker uses.

## Our broker on OpenShell (it would fit)

Two OpenShell features together cover what microsandbox's outbound proxy
gives the broker:

| The broker does | microsandbox | OpenShell |
|---|---|---|
| Sees all of the sandbox's egress | CONNECT or SOCKS outbound proxy | `https_proxy` with `proxy_ca_bundle`: the supervisor chains policy-approved connections through an upstream proxy and trusts one that re-signs certificates. Our existing CONNECT broker could be that proxy with little change. It's set per gateway, not per sandbox. |
| Route rules, receive-pack ref checks, fetching a PR before an update | Our code, after it terminates TLS | **Supervisor middleware**: a gRPC service called for each HTTP request after OpenShell terminates TLS and policy allows it. It gets method, path, headers and a streamed body, plus `sandbox_id` and the calling binary, and can allow, deny or change the request. It fails closed. |
| Injects the credential | Our code | **Providers** only. Middleware runs before injection and can't see or set credential headers. There are profiles for GitHub (including GitHub App tokens), AWS SigV4 with STS, and Anthropic and Claude Code; GitLab would need a custom profile. |

Caveats: middleware gets no body for a compressed request (whether git
compresses push bodies is *unverified*); its API is "still evolving" (the
older hooks are removed in 0.2.0); the VM driver is labelled Experimental.

## Docker inside an OpenShell sandbox: why it can't run

OpenShell's sandbox is the `openshell-sandbox` runtime, and it's the same
on every driver (Docker, Podman, Kubernetes, the libkrun VM). It:

- runs the workload as non-root, with no capabilities and `no_new_privs`
  ("Root and the invalid identity sentinel are always rejected");
- installs seccomp filters that block `mount` and the new mount API,
  `setns`, `pivot_root`, `umount2`, `bpf`, `io_uring_setup`, `ptrace`, and
  `unshare`/`clone` with `CLONE_NEWUSER`; the docs call this "not a
  user-facing knob";
- applies Landlock;
- gives the workload no network of its own: each `connect()` is caught
  and carried to the supervisor, which applies policy.

dockerd needs root, `mount`, new namespaces and cgroup writes. Rootless
Docker and Podman need user namespaces. All of that is blocked, so the
agent can't run an engine, and nothing it starts can either.

### The layers, with the Docker driver on a Mac

```
1  macOS
2  Linux VM (Docker Desktop / Podman machine)
3  engine (dockerd / podman), using runc to create level 4
4  OpenShell sandbox container   cap_drop ALL, no-new-privileges
5  openshell-sandbox             non-root, seccomp, Landlock
6  agent (bash, claude…)
7  dockerd                       ← what we want
8  test containers
```

For dockerd at level 7 to start, every level above it must allow root,
`mount`, namespaces and cgroup writes. Two refuse:

- **Level 4**, because of how the Docker driver asks level 3 to create it:
  `cap_drop: ["ALL"]`, `no-new-privileges:true`, OpenShell's own
  entrypoint, plain runc. The driver has no option to choose a runtime.
- **Level 5**, whatever level 4 is: `openshell-sandbox` drops levels 6–8 to
  non-root under seccomp.

### Sysbox doesn't change that

Sysbox (`sysbox-runc`, Apache-2.0, from Nestybox, which Docker bought in
2022) is the runtime an engine uses to create a container that can safely
host dockerd: root inside maps to an unprivileged host range, and parts of
`/proc` and `/sys` are virtualized. With a Docker front end it works out
of the box on Ubuntu and Debian (`docker run --runtime=sysbox-runc …`);
Podman isn't supported. It works at one level only:

- **At level 3**, creating level 4 as a system container. That would fix
  level 4, but level 5 still blocks dockerd. (On a Mac it's also hard to
  install there: Docker Desktop's VM takes Sysbox only through Enhanced
  Container Isolation, a Business-subscription feature, and Podman machine
  is Fedora CoreOS with a Podman front end.)
- **At level 7**, as the runtime of the agent's own dockerd: no help. That
  dockerd can't start, and Sysbox's own daemons (`sysbox-mgr`,
  `sysbox-fs`) need root, FUSE, `mount` and user namespaces.

OpenShell issue #4970 (2026-10-27) proposes Sysbox for the container
drivers; it's open, marked stale, with no reply from a maintainer.

### The libkrun VM driver has the same restrictions

A VM doesn't block syscalls; the guest kernel enforces seccomp, Landlock
and capabilities as any Linux kernel does. OpenShell decides what it
enforces:

- **Its own kernel.** The kernel is compiled into libkrunfw, which the VM
  driver embeds, built with OpenShell's config fragment
  (`runtime/kernel/openshell.kconfig`: cgroups, seccomp, Landlock). You
  can't bring your own, which is true of every libkrun tool, microsandbox
  included.
- **Your image, with its PID 1.** You bring an OCI image as the root
  filesystem, but the driver boots from its own bootstrap disk, mounts
  your image read-only underneath a writable overlay, copies its init
  script and binaries in (`/srv`, `/opt/openshell/bin`), and adds or
  rewrites a `sandbox` user with home `/sandbox`. Its init runs root
  setup, then starts `openshell-sandbox` as PID 1, which drops privileges
  as above. Your image's `ENTRYPOINT` and `USER` don't control PID 1.
- **No network card.** The driver attaches no virtio-net device. Only
  processes under the runtime's seccomp filter get network, over vsock to
  the supervisor on the host. A root dockerd started some other way would
  have no network, and neither would its containers.

The one opening: guest init runs root "drop-ins", but only those that a
lifecycle extension compiled into the driver injects
(`crates/openshell-driver-vm/src/extensions`). Docker would mean building
our own `openshell-driver-vm` with an extension that starts dockerd as root
on its own disk, plus a CONNECT proxy inside the workload for dockerd's
and the containers' egress. That's a driver we'd maintain against weekly
releases. All container traffic would look like it came from that proxy,
and the Docker socket makes the agent root in the guest, which disables
the guest-side checks (Landlock, seccomp, binary identity). The rules on
the Mac (policy, middleware, credentials) would still hold.

**microsandbox does the opposite:** the VM is the boundary and the guest is
ours. Its agent runs our commands as root with no seccomp policy, and the
guest has a network path through microsandbox's own policy. That's why
dockerd runs there, once it has its own ext4 disk at `/var/lib/docker`
(`microsandbox.md`, "Docker in the guest").

### What OpenShell says about Docker workflows

Nothing in the docs, tutorials or examples: every Docker mention is
Docker or Podman as the driver that runs sandboxes. Only two issues:

- **#113, "Secure Docker daemon inside sandboxes"** (March 2026, closed as
  a duplicate). It names seccomp, Landlock, capabilities and the network
  namespace as the blockers and lists four approaches as "needs spike":
  rootless Docker or Podman; Sysbox; a scoped containerd or BuildKit
  socket; or sending Docker API calls to a build service outside the
  sandbox, with policy enforced at the proxy. It requires that container
  traffic stay under network policy and that the sandbox not become
  privileged. Only the last approach fits the current design.
- **#4970, "Add support for Docker-in-Docker"**, as above.

## Container sandboxes vs a VM per sandbox, on a Mac

OpenShell's container sandbox is heavily hardened (levels 4 and 5 above),
which removes much of the kernel's attack surface. But all sandboxes share
level 2's kernel, and an exploit that escapes to root in that VM reaches:
the supervisor containers, which hold the real provider credentials;
every other sandbox; the engine; and the Mac directories the VM shares by
default (`/Users` on Docker Desktop, `$HOME` on Podman machine). With a VM
per sandbox, an escape has to break the hypervisor, and it lands in a VM
holding only that sandbox, while the broker and credentials stay in a Mac
process. "Comparable" holds only if kernel exploits are ruled out.

## Process sandboxes (bubblewrap, Seatbelt)

They wrap the agent's processes on the host, like level 5. Docker can't
run inside either: Seatbelt has no Linux kernel, and bubblewrap forbids
what dockerd needs (`design.md`, on Kiro's `/sandbox`). Docker would run
outside the sandbox, and the agent would reach it through a socket. Then
the containers aren't inside the boundary: the Docker API is root over
wherever the engine runs (on a Mac, the VM with the home directory shared
in), and container egress escapes the sandbox's policy. That would need a
proxy filtering Docker API calls (no bind mounts outside the workspace, no
`--privileged`, no host networking, container egress through our filter):
#113's fourth approach, which nobody has built.

## When to look again

- We stop needing Docker inside the agent's sandbox
  ([no-docker-in-sandbox.md](no-docker-in-sandbox.md)).
- OpenShell adds a mode in which something in the sandbox may be root and
  create namespaces (watch #4970), or a filtered remote Docker.
- For unattended agents on Linux or Kubernetes hosts, where a shared,
  hardened kernel is acceptable and OpenShell's gateway, identities and
  audit records pay off. Docker there is still blocked by level 5.

## Sources

- OpenShell docs in the repository: `docs/extensibility/supervisor-middleware/`,
  `docs/how-it-works/sandboxes/runtimes.mdx`, `docs/security/best-practices.mdx`
  ("Process Controls"), `docs/how-it-works/gateways/configuration.mdx`
  (`https_proxy`, `proxy_ca_bundle`), `docs/how-it-works/providers/`
- `crates/openshell-driver-vm/README.md`, `runtime/README.md`,
  `runtime/kernel/openshell.kconfig`, `scripts/openshell-vm-sandbox-init.sh`
  (init drop-ins); `crates/openshell-driver-docker/src/lib.rs` (container
  settings); RFC 0012 (isolation backend, network mediation)
- v0.1.10 release notes; issues #113 and #4970
- Sysbox: `docs/user-guide/dind.md`, `security.md`, `limitations.md`,
  `docs/distro-compat.md`
