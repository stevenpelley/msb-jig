# microsandbox as the VM layer

Status (2026-11-22): tested on the user's Mac (arm64, macOS 27.2.1) with
microsandbox 0.7.16, released 2026-11-18 and installed through Homebrew
(`superradcompany/tap`). The results are under "What we checked". "Verify"
marks what's still open.

**Verdict:** it works as the VM layer, and the broker fits behind it
unchanged. The broker can live in the same process that owns the VM: the
coupling we wanted, without Lima's fail-open problems. Recommended as the
replacement for Lima plus `investigator-net`'s netstack and filter.

## Why look at it

The broker is ours and stays ours, with sbx and with anything else. What has
grown complex and hard to trust is the Lima lifecycle around it:

- hijacking the pidfile so Lima uses `investigator-net` as its network;
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
| `investigator-net` netstack, DHCP, DNS | Its in-process stack and DNS forwarder |
| The domain filter (CONNECT proxy, groups, layers) | Its policy: `--net-default deny` plus `--net-rule allow@<host>[:tcp:<port>]`. We'd compile `filter/lists` into rules. |
| Fail-closed lifecycle (pidfile, `networks.yaml`, NIC checks) | Nothing to do: the policy lives in the VM's own process (checked below) |
| SSH over vsock, port-forward checks | `msb ssh serve <name> --stdio` as a ProxyCommand (VS Code). There's no sshd in the guest. |
| `lima/investigator.yaml.in`, cloud-init | An OCI image: `provision.sh` as a Dockerfile, or run once with `msb exec` |
| The broker (GitLab rules, the MCP filter, Anthropic) | **Unchanged.** `investigator-net broker` serves it on a Unix socket, and the guest reaches it over vsock. |

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
~/.investigator/<name>/broker.sock  <-vsock-  127.0.0.1:443  <- git, glab, claude
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
`investigator-net` can leave the VM with an unfiltered one. Here the
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

**Shape:** `investigator run <name>` is one process per sandbox, started in
the background by `investigator up <name>` with its log in the state
directory. It:

1. loads the CA and serves the broker on `~/.investigator/<name>/broker.sock`;
2. creates or starts the named sandbox through the SDK, attached (not
   `StartSandboxDetached`), with the vsock route, the egress rules, the user
   and the image;
3. waits:
   - the sandbox stops: close the broker, remove the socket, exit;
   - SIGTERM (`investigator down`): `Stop` the sandbox, then the broker, exit;
   - the process dies: the VM goes with it (parent watch), and the socket
     is cleaned up on the next `up`.

A broker that breaks while the process lives can be restarted in place: the
guest's route reconnects to a new listener at the same path (checked).

**What users would run:**
- `investigator up <name>`, `investigator down <name>`;
- `investigator shell <name>`, i.e. `msb exec -t -u agent <name> -- bash -l`;
- VS Code through the SSH config we already generate, with
  `ProxyCommand msb ssh serve <name> --stdio`.

There's no Lima, no `networks.yaml` and no pidfile. The custom pieces left
are the broker, the supervisor of about a hundred lines, the image, and the
compiled allowlist.

## What we checked

Run by the agent on 2026-11-22, with the user's go-ahead. Sandboxes
`alpine/curl`, `debian:bookworm` and `alpine`. The broker was
`investigator-net broker` with the user's glab token and `inv-claude`
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
| Host-side denial log | **None found.** `runtime.log` and `msb logs --source system` don't record denied hosts at the default level; only the guest's own output shows them. |

Found on the way, not microsandbox's:

- **The broker refuses git URLs without `.git`** (`/<user>/test/info/refs`):
  "request not recognized". GitLab accepts both, and this happens on Lima
  too. Worth fixing in `broker/`.

## What we'd lose or have to settle

- **The denial log.** Our filter writes `denials.jsonl`, which drives "ask the
  user to allow it". Here a denied name just doesn't resolve in the guest,
  and the Mac keeps no record. Try `--debug` or `--trace` runtime logs. If
  they don't record it, the guest's error message is all the agent sees.
- **Strict mode or TLS interception.** Strict mode with no interception
  breaks allowed hosts, so one of them has to give:
  - **Strict off:** hostname rules match on SNI plus DNS. The tests show it
    still refuses IP literals and mismatched SNI, which is what our CONNECT
    filter does today. **Recommended.**
  - **Interception on:** clearer 403s, but its CA has to go into every
    trust store (Node, Python, Java), pinned clients must be bypassed,
    there's no QUIC, and #2013 is open.
- **Allowing a host while running.** Rules are set at create time. A new
  host means `down` and `up`; verify whether `msb modify` can change them.
- **SDK runtime discovery.** The Go SDK looks for `~/.microsandbox/bin/msb`.
  With Homebrew's install it needs `MSB_PATH` and `MSB_LIBKRUNFW_PATH`, or
  `msb`'s own installer. It adds a 33 MB binary, through cgo (verify).
- **Beta churn.** Pin a version and re-run these checks on upgrade.
- **Corporate track.** The work Mac may not allow a hypervisor-entitled
  binary from a third-party tap. Ask before betting the corporate track on it.
- **Not yet checked:**
  - #1998 (a Wi-Fi change with a sandbox running);
  - VS Code Remote-SSH itself (the SSH pieces it needs work);
  - Docker inside the guest (our tests need it);
  - an image built from `provision.sh`.
