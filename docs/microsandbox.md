# microsandbox as the VM layer

Status (2026-11-22): tested on the user's Mac (arm64, macOS 27.2.1) with
microsandbox 0.7.16, released 2026-11-18 and installed through Homebrew
(`superradcompany/tap`). The results are under "What we checked". "Verify"
marks what's still open.

Update (2026-11-25): the Go SDK is at v0.8.2, for Homebrew's msb 0.8.2.
SDK v0.7.16 refused that runtime at start (`no tested sandbox launch
contract for runtime 0.8.2`); the SDK's and msb's versions have to match.
The only API change we use: `Sandbox.Status` takes a context and returns
an error. The checks below are from 0.7.16.

**Verdict:** it works as the VM layer, and replaced Lima plus
`mono-jig-net`'s netstack and filter on 2026-11-22. `mono-jig up`,
below, runs it end to end, and the broker fits behind it unchanged. The
broker lives in the same process that owns the VM: the coupling we wanted,
without Lima's fail-open problems.

## Why look at it

The broker is ours and stays ours, with sbx and with anything else. What has
grown complex and hard to trust is the Lima lifecycle around it:

- hijacking the pidfile so Lima uses `mono-jig-net` as its network;
- keeping `networks.yaml` in step, because if our daemon is dead Lima starts
  its own unfiltered network (fail-open, still open after a SIGKILL);
- the boot checks for a second NIC and for port forwards;
- a cloud-init template and `provision.sh`.

## What it is

- **One libkrun microVM per sandbox**, on Hypervisor.framework. That's the
  same VMM as our krunkit option. It needs Apple Silicon.
- **No daemon.** Each sandbox is one host process, `msb machine`, which
  holds the VMM, the guest's network stack, the egress policy, DNS and
  secret injection. The guest's only network is that userspace stack: "no
  host kernel routing or NAT in the path" (`security/network`).
- **CLI and SDKs:** `msb create/run/exec/stop/rm/ssh`, and SDKs for Rust,
  Python, TypeScript, Go and Ruby. The Go SDK
  (`github.com/superradcompany/microsandbox/sdk/go`) embeds a native
  library. Images are OCI. Named sandboxes keep their disk across a stop.
- **Beta**, Apache-2.0, releasing about weekly. Open issues that matter to us:
  - #1998: DNS stops working after the Mac changes networks, until the
    sandbox restarts;
  - #2013: TLS interception ignores intermediate certificates the Mac has;
  - #1717: a published port outlived its sandbox.

## How it maps onto our design

| Our piece | With microsandbox |
|---|---|
| `mono-jig-net` netstack, DHCP, DNS | Its in-process stack and DNS forwarder |
| The domain filter (CONNECT proxy, groups, layers) | Its policy: `--net-default deny` plus `--net-rule allow@<host>[:tcp:<port>]`. We'd compile `filter/lists` into rules. |
| Fail-closed lifecycle (pidfile, `networks.yaml`, NIC checks) | Nothing to do: the policy lives in the VM's own process (checked below) |
| SSH over vsock, port-forward checks | `msb ssh serve <name> --stdio` as a ProxyCommand (VS Code). There's no sshd in the guest. |
| `lima/mono-jig.yaml.in`, cloud-init | An OCI image: `provision.sh` as a Dockerfile, or run once with `msb exec` |
| The broker (GitLab rules, the MCP filter, Anthropic) | **Unchanged.** `mono-jig-net broker` serves it on a Unix socket, and the guest reaches it over vsock. |

Two of its features don't fit, and we wouldn't use them:

- **Its outbound proxy** sends a CONNECT for the destination *IP*, after its
  own policy has already decided (`networking/outbound-proxy`). That's for
  corporate egress. Its own policy replaces our filter.
- **Its secret placeholders.** They have no per-operation rules, and their
  values come from environment variables read at start. They would be
  enough for the Anthropic token alone, but the broker already does that
  job, and one mechanism is simpler to trust.

## The broker as the sandbox's proxy

```
Mac                                          guest
~/.mono-jig/<name>/broker.sock  <-vsock-  127.0.0.1:443  <- git, glab, claude
   ^ broker (TLS, *.broker.internal)            ^ socat (root)
                                                /etc/hosts: gitlab|github|anthropic.broker.internal -> 127.0.0.1
```

- **The route.** `--vsock <sock>:443` gives the guest vsock port 443 on
  host CID 2. In the guest, `socat TCP-LISTEN:443,bind=127.0.0.1,fork
  VSOCK-CONNECT:2:443` makes it a TCP port. `/etc/hosts` points the
  broker's names at 127.0.0.1.
- **Identity.** The socket's path identifies the sandbox. It lives in a 0700
  directory and is mode 0600, so only the user's own processes can reach it.
  No session token goes in the guest, no TCP port opens on the Mac, and
  `host` stays denied in the policy.
- **TLS stays ours.** The client talks TLS to `gitlab.broker.internal`
  under our CA, which is mounted read-only into the guest's trust store.
  microsandbox never sees this traffic, because it isn't network traffic.
- **The bridge isn't a security boundary.** If the agent got root in the
  guest, it could only aim names elsewhere: egress still meets the host's
  policy, and the vsock route leads only to the broker. The agent runs as a
  non-root user anyway.
- **The egress allowlist** holds package registries and the layers' hosts.
  `gitlab.com`, `github.com` and `api.anthropic.com` aren't in it. In the
  test they didn't even resolve in the guest.

## Tying the broker's lifecycle to the sandbox's

On Lima, the broker and the filter are the network, so a dead
`mono-jig-net` can leave the VM with an unfiltered one. Here the
restriction lives in the VM's own process. A dead broker only takes away
capabilities: the guest gets connection refused at
`gitlab.broker.internal`, and the rest of its egress is unchanged. Every
failure is fail-closed (checked).

That still leaves the coupling you wanted. With the Go SDK, the process that
creates the sandbox *owns* its VM:

- the VM process starts as its child, with `--parent-watch-fd`;
- a SIGKILL of the owner stops the VM (checked).

So the broker and the VM owner can be one process: a Go program that serves
the broker on the socket and holds the sandbox through the SDK.

**Shape:** `mono-jig run <name>` is one process per sandbox, started in
the background by `mono-jig up <name>` with its log in the state
directory. It:

1. loads the CA and serves the broker on `~/.mono-jig/<name>/broker.sock`;
2. creates or starts the named sandbox through the SDK, attached (not
   `StartSandboxDetached`), with the vsock route, the egress rules, the user
   and the image;
3. waits:
   - the sandbox stops: close the broker, remove the socket, exit;
   - SIGTERM (`mono-jig down`): `Stop` the sandbox, then the broker, exit;
   - the process dies: the VM goes with it (parent watch), and the socket
     is cleaned up on the next `up`.

A broker that breaks while the process lives can be restarted in place: the
guest's route reconnects to a new listener at the same path (checked).

**What users would run:**
- `mono-jig up <name>`, `mono-jig down <name>`;
- `mono-jig shell <name>`, i.e. `msb exec -t -u agent <name> -- bash -l`;
- VS Code through the SSH config we already generate, with
  `ProxyCommand msb ssh serve <name> --stdio`.

There's no Lima, no `networks.yaml` and no pidfile. The custom pieces left
are the broker, the supervisor of about a hundred lines, the image, and the
compiled allowlist.

## What we checked

Run by the agent on 2026-11-22, with the user's go-ahead. Sandboxes
`alpine/curl`, `debian:bookworm` and `alpine`. The broker was
`mono-jig-net broker` with the user's glab token and `mono-jig-claude`
Keychain item.

| Check | Result |
|---|---|
| `msb doctor` | Host setup is ready (arm64, `libkrunfw.5.dylib`) |
| Default deny, one allowed host | **With strict mode on (the default) and no TLS interception, the allowed host fails too** (TLS reset). With `--net-strict=false` it works; with `--tls-intercept` it works. |
| Strict off: denied names | `pastebin.com` doesn't resolve (NXDOMAIN); 8.8.8.8:53 is unreachable |
| Strict off: IP literals and SNI tricks | `1.1.1.1`, an allowed SNI sent to 1.1.1.1, and a denied name sent to the allowed host's IP all fail (TLS reset) |
| TLS interception on | The same three come back as a 403 from the policy instead of a reset |
| Metadata, host | `169.254.169.254` and `host.microsandbox.internal` get 403 |
| SIGKILL of `msb machine` | The VM is gone and the sandbox is `crashed`. `msb start` brings it back; writes not yet flushed were lost (an empty file). |
| Clean stop and start | Files kept |
| SIGKILL of an attached `msb run` | The VM stops (its child has `--parent-watch-fd`) |
| SIGKILL of a Go SDK owner | The VM stops |
| Broker over vsock | `/api/v4/version` 200 under our CA; `/api/v4/user` shows `<user>`; `git ls-remote` of `<user>/test.git` lists `main` and `agent/<user>/x` |
| SIGKILL of the broker | The guest gets connection refused; `deb.debian.org` still works and `pastebin.com` is still denied |
| Broker restarted at the same path | The guest reconnects with no sandbox restart |
| Claude Code as user `agent` | `claude -p "say hi"` answers. Through the broker: `HEAD /api/hello`, `POST /v1/messages`. Installer hosts: `claude.ai`, `downloads.claude.ai`, `storage.googleapis.com` (the last unverified as needed). |
| The agent against the bridge | It can't write `/etc/hosts` or kill socat: "Operation not permitted" |
| SSH (`msb ssh serve --stdio` as ProxyCommand) | Logs in as `agent` with the login profile; `ssh -W` to a guest port works; agent forwarding isn't offered (`SSH_AUTH_SOCK` empty) |
| Host-side denial log | **Only at debug level.** At the default level `runtime.log` and `msb logs --source system` don't record denied hosts. With `--debug`, `~/.microsandbox/sandboxes/<name>/logs/runtime.log` has `DNS query denied by network policy domain=example.org` and `TCP egress denied by strict hostname policy sni=… dst=…`; a connect to a bare IP left no line. Debug is global (`msb machine --log-level`; `RUST_LOG` is ignored) and writes about 16 KB/s while idle, mostly virtio, roughly 1.4 GB a day. |

Found on the way, not microsandbox's:

- **The broker refuses git URLs without `.git`** (`/<user>/test/info/refs`):
  "request not recognized". GitLab accepts both, and this happens on Lima
  too. Worth fixing in `broker/`.

## The supervisor, built: `mono-jig up`

Built 2026-11-22 as `cmd/mono-jig-msb`, about 450 lines, and folded into
the `mono-jig` CLI the same day (`cmd/mono-jig/sandbox.go`), once
microsandbox replaced Lima. The commands below are `mono-jig <cmd>`;
`code <name>` (since 2026-11-27 `vscode <name>`, the editor's name)
opens VS Code on the sandbox with the harness's settings.
The broker setup it shares with `mono-jig-net` lives in `brokerd/`.

- `up <name>` starts `run <name>` in the background (setsid, log in
  `~/.mono-jig/<name>/run.log`) and waits for it to be ready.
- `run` takes a lock, then does the following in order:
  1. serves the broker on `broker.sock`;
  2. creates the sandbox the first time (`ubuntu:26.04`, 4 GiB, 4 vCPUs);
  3. provisions it the first time;
  4. starts the bridge;
  5. waits.

  It creates the sandbox through the SDK, attached, with:
  - the allowlist compiled from `filter/lists` (`JIG_PROFILE`, `JIG_LAYERS`);
  - strict mode off;
  - the vsock route;
  - the CA written in as a patch.

  To provision, it runs `provision.sh` (embedded, as `sandbox.ProvisionScript`)
  as root, with `base git broker claude` plus socat. A marker in the guest
  means this happens once.

  The bridge (socat, plus the `/etc/hosts` line) runs as a root exec job,
  restarted if it ends.
- `down` sends the supervisor SIGTERM, which stops the sandbox, then
  closes the broker and removes the socket.
- `shell` is `msb exec -t -u agent`.
- `ssh-config` prints a Host block with `ProxyCommand msb ssh serve`.
- `rm` is `down` plus removing the sandbox.
- Configuration comes from `mono-jig login` (since 2026-11-23, `login.go`), under the `JIG_*` variables (the header of `sandbox.go`); before that, only the variables:
  - `JIG_GITLAB_USER`;
  - glab's login as the GitLab token by default;
  - the `mono-jig-claude` Keychain item for Claude.

Checked on the Mac with `JIG_GITLAB_USER=<user> mono-jig-msb up jig2` (now `mono-jig up jig2`):

| Check | Result |
|---|---|
| First `up` | Created and provisioned in about a minute; the VM process is the supervisor's child (`--parent-watch-fd`) |
| Claude Code as `agent` | `claude -p` answers through the broker |
| glab | `glab api user` returns `<user>` |
| git | A clone of `https://gitlab.com/<user>/test` (no `.git`) works. A push to `agent/<user>/msb` is accepted, a push to `main` is rejected with the broker's reason, and the branch is deleted again. |
| Egress | `proxy.golang.org` and `ports.ubuntu.com:80` work. `pastebin.com` and `api.anthropic.com` fail. |
| **DNS** | **An explicit `AllowDNS` rule made every name resolve**, so the denial came only at connect, and DNS stayed open as a channel out. Without it, a denied name gets no answer (curl exit 6). The rule is gone. |
| The agent | It can't write `/etc/hosts`, and `sudo` asks for a password |
| SIGKILL of the supervisor | The VM is gone (`stopped`). `up` starts it again over the stale pid and ready files. |
| `down` and `up` | The home directory is kept, there's no reprovisioning, and the socket, pid and ready files are removed |
| `ssh-config` | Logs in as `agent` with no agent socket |

Still open:

- **One authorization for every sandbox.** `msb ssh authorize` adds the key
  to `~/.microsandbox/ssh/authorized_keys`, which covers all sandboxes, not
  one. Fine for one user's Mac.
- **Rules are fixed at create time.** `msb modify` changes CPUs, memory,
  disk, env, secrets and ports, but not the network policy, so changing
  the layers needs `rm` and `up`, which loses the guest's files.

### Docker in the guest (done)

The guest's root is overlayfs (`/.msb/rootfs/lower` plus an upper layer), so
Docker's overlay snapshots fail on it: "failed to mount … fstype: overlay
… invalid argument". dockerd itself starts fine (6.12 kernel, cgroup v2,
iptables). The fix is an ext4 disk of the sandbox's own at
`/var/lib/docker` (`Mount.Owned`, kind disk; removed with the sandbox).

- The supervisor creates the sandbox with a 32 GiB root disk
  (`JIG_DISK`; the default was 4 GiB) and a 64 GiB Docker disk
  (`JIG_DOCKER_DISK`, 0 for none). Both are sparse: 121 MB on the Mac after
  pulling a few images.
- `provision.sh`'s `docker` component installs `docker.io`, `docker-buildx`
  and `docker-compose-v2`, and adds the agent to the `docker` group.
- dockerd runs as a root exec job, restarted if it ends, like the bridge;
  there's no systemd.

Checked on `jig2` as `agent`:
- `hello-world`;
- a BuildKit build;
- `nginx` on a published port, reached with curl from the guest;
- `alpine` reaching `proxy.golang.org`, while `pastebin.com` doesn't
  resolve. Containers go out through the guest, so the sandbox's egress
  policy covers them.
- After `down` and `up`, the images are still there.

Not covered: containers can't reach the broker, since the bridge listens on
the guest's loopback. The `docker` group makes the agent root in the guest
in effect, so it can edit `/etc/hosts` or stop the bridge. That only breaks
its own access: the policy and the credentials stay on the Mac, and the
VM remains the boundary.

`claude` is linked into `/usr/local/bin`, so a session without a login
shell finds it too.

### The environment outside login shells (fixed)

`msb ssh` and `msb exec` run `/bin/sh -c` straight from the guest's init:
no PAM, no login shell, and the env is only `HOME`, `PATH`, `TERM` plus the
sandbox's own. So `provision.sh`'s profile.d files reached login shells
only, and a plain `ssh host cmd`, as VS Code's server is started, had no
`ANTHROPIC_BASE_URL` or `GITLAB_HOST`. Checked with a throwaway sandbox: a
variable set at create (`-e`) reaches both SSH and exec sessions.

- The supervisor sets the client variables (`clientEnv`) on the
  sandbox at create. On every start it re-applies them to the stopped
  sandbox (`Modify` with `next_start`; a running sandbox refuses: "env
  requires restart"), so a sandbox from an older supervisor, or one whose
  Claude setting changed, follows.
- `provision.sh` writes the same variables to
  `/etc/profile.d/80-mono-jig.sh` and to a marked block in
  `/etc/environment`, which pam_env reads for every sshd session, login
  or not (Lima). Unverified on Lima itself.
- *Since 2026-11-24 both come from one Go value, `guestConfig`
  (`cmd/mono-jig/guestconfig.go`, since moved to `jig/`), and `provision.sh`'s `apply` component
  writes it; before, Go's `clientEnv` and bash's `client_env` calls were
  kept in step by a test, one profile.d file per component (80–84).*

Checked on `jig2` (recreated): a non-login SSH command gets all of them,
and `glab api user` and `claude -p` work in it. Restarted with
`JIG_CLAUDE_TOKEN_COMMAND=` the Claude variables are gone, and back on the
next normal `up`. Login shells follow too: on every start after the
first, the supervisor writes the current `provision.sh` into the guest and
runs only its `claude-env` component (since 2026-11-24, `apply`), which
rewrites (or removes) the Claude profile.d file and `/etc/environment`
block. Checked both ways on
`jig2`: login shell, plain command and `/etc/environment` all agree.

## What we'd lose or have to settle

- **The denial log: built.** In the guest, a denied name just doesn't
  resolve. The runtime records denied names only at debug level, which is
  too noisy to keep as is (above).
  - The supervisor creates sandboxes at debug level and follows
    `runtime.log` (`jig/denials.go`).
  - It appends each denial to `<state>/denials.jsonl`, in the filter's
    `Denial` format (method `dns` or `connect`), and logs it in `run.log`.
  - Once `runtime.log` passes 16 MB, the supervisor truncates it. The runtime
    appends, so writing restarts at zero (checked); a line written between
    the last read and the truncate is lost.
  - Checked on `jig2`: `pastebin.com` and `api.anthropic.com` from the
    agent, and `evil.example.net` from a Docker container, were all
    recorded. So were Claude Code's direct calls to `api.anthropic.com`
    during provisioning.
  - Not recorded: a connect to a bare IP. `mono-jig denied <name>`
    lists the hosts, most recent first.
  - **Upstream (checked 2026-11-22):** #691, "Capture & expose network
    policy deny events", is open and asks for exactly this: a public API
    for deny events with kind, host, port and time. The maintainers say it
    will come with the plugin system, under an RFC, with no date yet. A
    comment there (v0.7.11) lists what debug level misses: denials of an
    allowed host on a port it isn't allowed, plain TCP to an IP, and UDP.
    #1854, "Log blocked domains" (count and last time per domain), was
    closed in favour of #691, with the debug log plus `jq` as the answer
    for now. #2064 (open) asks for a WARN-level line with the SNI and
    reason when strict mode denies TLS. Our follower stays a stopgap
    until #691 lands; we won't build more.
- **Strict mode or TLS interception.** Strict mode with no interception
  breaks allowed hosts, so one of them has to give:
  - **Strict off:** hostname rules match on SNI plus DNS. The tests show it
    still refuses IP literals and mismatched SNI, which is what our CONNECT
    filter does today. **Recommended.**
  - **Interception on:** clearer 403s, but its CA has to go into every
    trust store (Node, Python, Java), pinned clients must be bypassed,
    there's no QUIC, and #2013 is open.
- **Allowing a host while running.** Rules are set at create time and
  `msb modify` can't change them (0.7.16), so a new host means `rm` and
  `up`, which loses the guest's files. #1538 ("Allow modifying network
  rules of a running sandbox") is open; we wait for it (user, 2026-11-22).
- **SDK runtime discovery.** The Go SDK looks for `~/.microsandbox/bin/msb`.
  With Homebrew's install it needs `MSB_PATH` and `MSB_LIBKRUNFW_PATH`, or
  `msb`'s own installer. It adds a 33 MB binary, through cgo (verify).
- **Beta churn.** Pin a version and re-run these checks on upgrade.
- **Corporate track.** The work Mac may not allow a hypervisor-entitled
  binary from a third-party tap. Ask before betting the corporate track on it.
- **Not yet checked:**
  - #1998 (a Wi-Fi change with a sandbox running): not tested on purpose
    (user, 2026-11-22);
  - VS Code Remote-SSH itself (the SSH pieces it needs work);
  - an image built from `provision.sh`.

## Sources

- [#691: Capture & expose network policy deny events](https://github.com/superradcompany/microsandbox/issues/691) (open)
- [#1854: Log blocked domains](https://github.com/superradcompany/microsandbox/issues/1854) (closed for #691)
- [#2064: Strict-hostname denials show up in the guest as bare TLS EOF](https://github.com/superradcompany/microsandbox/issues/2064) (open)
- [#1538: Allow modifying network rules of a running sandbox](https://github.com/superradcompany/microsandbox/issues/1538) (open)
- [#1998: DNS after the host changes networks](https://github.com/superradcompany/microsandbox/issues/1998)
