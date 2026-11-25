# msb-jig

A sandbox for coding agents on a Mac, with a credential broker that keeps
your tokens out of the sandbox and enforces rules finer than GitHub's or
GitLab's own.

The agent (Claude Code today) runs without permission prompts inside a
[microsandbox](https://github.com/superradcompany/microsandbox) VM. Your
editor connects to it over SSH. The VM reaches the network only through an
allowlist, and reaches GitLab, GitHub, the Claude API and the AWS MCP
Server only through a broker on the Mac. The broker holds your credentials
and checks every request. For example, the agent may push only to
`agent/<user>/…` branches, edit only its own pull requests, and never merge
or approve.

**Status: a working prototype, used on one personal Mac.** It is not a
product: expect rough edges, and read `docs/` before relying on it. It
makes the safe path the easy one; it doesn't stop a user who deliberately
hands the agent a token, and the allowlist limits where the agent can reach
with credentials, not what it can send.

## How it works

```
 Mac                                         microsandbox VM
┌───────────────────────────────┐           ┌──────────────────────────┐
│ mono-jig supervisor (one per  │  vsock    │ agent (Claude Code)      │
│ sandbox)                      │◄──────────┤ git, gh, glab, mcpcli    │
│  • broker: your tokens, the   │           │   → *.broker.internal    │
│    providers' rules           │           │ Docker, your tools       │
│  • owns the VM: if it dies,   │  egress   │                          │
│    the VM stops               │  policy   │ everything else: only    │
│                               │◄──────────┤ allowlisted hosts        │
│ editor (VS Code) ── SSH ──────┼──────────►│ sshd                     │
└───────────────────────────────┘           └──────────────────────────┘
```

- **The broker** (`broker/`) serves each provider under a name the guest
  reaches over vsock (`gitlab.broker.internal`, …). In *proxy mode* it is
  instead the VM's outbound proxy, terminating the providers' real host
  names (the AWS MCP Server's included), with no vsock route, bridge or
  `/etc/hosts` names in the guest. Each provider's route table lists every request it allows and the
  check run on it; anything else is denied.
- **The allowlist** (`allowlist/`) comes in layers: a base, organisation
  layers, then your own. The hosts of brokered providers leave it.
- **Provisioning** (`guest/`) is a set of shell components, built into a
  base image once and applied at every start from settings the supervisor
  computes: environment, git rewrites, tool configs, the agent's
  instructions, extra CA certificates.

## A library with tracks

The code is a Go library, `github.com/stevenpelley/msb-jig`. A *track* is
one command that picks its providers, agent, editor, VM runtime and base
image (`jig.Track`) and runs `jig.Main`:

```go
package main

import "github.com/stevenpelley/msb-jig/jig"

func main() { jig.Main(jig.Personal()) }
```

`jig.Personal()` is GitLab, GitHub, Claude and AWS; Claude Code; VS Code;
microsandbox; Ubuntu. Another track replaces any of those with its own
implementations of `providers.Provider`, `agent.Agent`, `editor.Editor` and
`sandbox.Backend`, and adds its own allowlist layer, provisioning and guest
settings (`docs/tracks.md`).

## Using it

Requirements: macOS, Go 1.26, [microsandbox](https://github.com/superradcompany/microsandbox)
(`msb` 0.8.2, the Go SDK's version: SDK 0.7.16 refused runtime 0.8.2) and VS Code's `code` command. Optional: Docker
or Podman to build the base image; `glab`, `gh`, `claude`, the AWS CLI and
`uv` for the providers you log in to.

Build your track's command (above) as `mono-jig`, then:

```sh
mono-jig setup                 # check the Mac, the editor profile, the base image
mono-jig login all             # choose providers; tokens stay in your logins or the Keychain
mono-jig open <name> [dir]     # create or start a sandbox, open VS Code on it
mono-jig stop <name>
mono-jig                       # no arguments: every command (image build, ca, cp, allowlist, rm, …)
```

State lives in `~/.mono-jig`: settings, your allowlist layer
(`allowlist.yaml`) and one directory per sandbox.

## Documentation

| | |
|---|---|
| `AGENTS.md` | The project's purpose, trust model, current state and a map of the code |
| `docs/design.md` | The design |
| `docs/microsandbox.md` | microsandbox as the VM layer, and the checks behind it |
| `docs/transparent-broker.md` | Proxy mode: the broker as the VM's outbound proxy |
| `docs/tracks.md` | Writing a track: what you can customize, and how |
| `docs/library-split.md` | How the library and its seams were made |
| `docs/enforcement.md` | What could make a harness like this required |

## License

Apache License 2.0; see `LICENSE` and `NOTICE`.
