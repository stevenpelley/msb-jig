# AGENTS.md — mcp-restrict

Exploration project: tools that limit what a coding agent in a sandbox can
reach, mainly over the network, with finer rules than the providers' own
roles and tokens allow. Design notes plus the prototype's code (Go module
`mcp-restrict`, `docs/prototype-plan.md`).

## Purpose

1. **A packaged harness for coworkers** ("investigator in a box"). One
   command starts a sandboxed agent that runs without permission prompts
   and reaches GitHub, AWS, Datadog and Wiz in restricted ways, using the
   developer's own credentials. A developer hands it a Datadog alert or a
   Wiz issue and it investigates. Interactive use is an editor on the Mac
   connected over SSH into the VM sandbox (microsandbox), where the agent, terminals and Docker run. **VS Code first**, prototyped on a
   personal Mac with personal accounts (also for personal use); then Kiro
   IDE, which gained remote SSH in 1.5 (2026-11-20). Dev containers only
   ever run inside that VM, never on the Mac: our tests need Docker, and
   the VM is the isolation boundary. Everything (sandbox, broker, SSH and
   editor configuration) is packaged so the user configures nothing.
   Later the same pieces should serve headless, automated agents.

   **Configurable into a general dev sandbox** (2026-11-22): the egress
   allowlist comes in layers: the base (sbx's balanced list plus our
   own groups, without the hosts of providers the broker serves), org
   layers (`INV_LAYERS`) and the user's `~/.investigator/allowlist.yaml`
   (2026-11-23; the `investigator` and `dev` profiles are gone). Domain
   filtering is a separate policy from the broker's: the runtime's own.
   **The broker's policy covers only the managed providers.**

   **One process per sandbox** (2026-11-22): everything that runs
   continuously on the Mac is packaged with the VM. The `investigator`
   supervisor owns the microsandbox VM and serves the broker; the VM's
   egress policy is microsandbox's own. They start and stop together. No
   `launchd` jobs, no containers on the Mac.

   **Not air-tight, on purpose** (2026-11-22): the harness makes the safe
   path the easy one; it doesn't stop deliberate acts. A user who pastes
   a token into the sandbox can use it, and we don't try to prevent that.

   **No dependency on sbx** (2026-11-22): assume work won't allow Docker
   accounts. Prototyped on sbx, then Lima; it now runs on microsandbox
   (`docs/microsandbox.md`).

   **Two tracks (2026-11-22): personal first, then work.**
   - *Personal* (build first): GitLab, with my own GitLab user's
     credentials through the broker (not the `<user>-agent` user), and
     AWS; the agent is Claude Code; it runs directly in the sandbox or
     from VS Code over SSH into it.
   - *Work* (adapt it): GitHub, AWS, Datadog and Wiz; the agent is Kiro;
     it runs directly in the sandbox or from Kiro IDE over SSH into it.
2. **Restrictions finer than the providers offer.** The main case is
   GitHub: push only to `agent/<user>/...` branches, open and edit only
   PRs tagged `[agent/<user>]`, never merge. Fine-grained tokens and
   rulesets can't say "this user, but only when it is the agent", so a
   proxy we own enforces it (policy written in Go over git, API and MCP
   requests; Cedar dropped for now, 2026-11-22).
3. **Elective use, and what enforcement is possible.** The harness uses
   the developer's normal credentials, and the developer keeps using
   them elsewhere. Nothing today forces an agent through the harness.
   The second track asks what *could* force it, and what each enforcement
   point requires us to trust.

## The trust question

Keep these parties separate when reasoning about enforcement:

| Party | Stance |
|---|---|
| The user | Not malicious, but not relied on to configure anything correctly or to resist convenience. A deliberate insider (mints a PAT, writes a script) is out of scope; existing controls cover that. |
| Agent software (Kiro, Claude Code, …) | Vendor code and replaceable by any other client. Useful for UX, not an enforcement point on its own. |
| The IDE connection (VS Code, then Kiro, over SSH) | A second path between sandbox and Mac. Relied on, through settings we ship, not to pass the Mac's credentials (SSH agent, git credential helper) into the sandbox. |
| Sandbox runtime (microsandbox, run by our supervisor) | Trusted to confine the agent: no host credentials, egress only through the domain filter and the broker. |
| Broker / MCP proxy (ours) | Trusted: holds credentials, enforces policy. Its integrity on a Mac rests on the OS (code signing, MDM). |
| Model provider / model gateway | The org controls access to it; the strongest candidate chokepoint, since every agent needs a model and users don't bring their own corporate one. |
| Provider (GitHub, AWS, Datadog, Wiz) | Goal: rely on nothing it doesn't already ship. Secondary goal: find small, standard features it would happily add (e.g. per-credential network binding, delegation markers in audit logs). |

## Current state

As of 2026-11-22. **microsandbox is the VM layer** (user's decision;
`docs/microsandbox.md`, `docs/prototype-plan.md` step 6b). `investigator up <name>`
starts one supervisor process per sandbox. It owns the VM through
microsandbox's Go SDK and serves the broker on a Unix socket, which the
guest reaches over vsock. If the process dies, so does the VM. The egress
policy is microsandbox's, compiled from the allowlist's layers.

On the user's Mac, against `<user>/test`, these pass:
- git push rules and glab, through the broker, with the user's own
  credentials;
- Claude Code, with its credential in the broker;
- egress and DNS limited to the allowlist;
- the lifecycle: down and up, SIGKILL fails closed;
- SSH;
- Docker in the guest, on its own ext4 disk;
- AWS: the agent calls the AWS MCP Server with `mcpcli` or `curl` at
  `aws.broker.internal/mcp`; the broker runs AWS's stdio proxy on the Mac
  (`docs/prototype-plan.md` step 7).

The Lima backend was removed the same day (in git at `695143a`), with its
netstack and gvisor.

Still open:
- VS Code: the bridges check runs unattended (`checks/vscode-e2e/`, step 2); the git extension's socket and the remote `code` CLI stay open; Claude Code's extension installs and works on the remote; a repo's `.vscode/settings.json` overrides our settings (no new bridge, but ports and askpass come back); `investigator code <name> [dir]` checked by the user;
- allowing a host without `rm`: waiting for microsandbox#1538;
- the work track;
- the name and pitch (user, 2026-11-23): it's no longer just an
  "investigator" but a full-featured sandbox (own image, host folders,
  brokered providers, an editor over SSH). Sell it first for one-shot
  investigations; with Kiro IDE's remote SSH, people may use it as their
  everyday dev sandbox. Rename later.

The agent runs on the Mac itself and asks before running commands.

## Files

| File | What it is |
|---|---|
| `docs/design.md` | The harness design (2026-11-21; editor over SSH added 2026-11-22): the two tracks, VS Code then Kiro over SSH into the sandbox, egress filtering, packaging as one process per sandbox, leaving sbx (for Lima, since removed, then libraries that boot VMs), dev containers, the broker, the sandbox backend interface, whether OpenShell is easier behind an IDE, packaging, open questions |
| `docs/prototype-plan.md` | The personal track, step by step, ending on microsandbox (6b): who tests what (this sandbox vs the user's Mac) |
| `docs/microsandbox.md` | microsandbox as the VM layer (2026-11-22, msb 0.7.16): checks on the Mac, the broker over vsock, the supervisor, Docker, the environment |
| `docs/enforcement.md` | Making the harness required (2026-11-22): threats, model-gated chain of trust, network and provider-side options, small provider asks |
| `broker/providers/` | **Start reviewing here.** `Build` lists the providers: what turns each on, and the `Routes` function that holds its policy |
| `broker/<provider>/routes.go` | `Routes` (`gitlab`, `github`; in `anthropic.go` and `aws.go` for the two without per-request rules): the provider's upstreams with their credentials, then its route table (every request it serves, the check it runs, where it goes); anything unmatched is denied. The checks are beside it (`checks.go`, `mcp.go`, `graphql.go`); tests run through `providers.Build` |
| `broker/` | The core: `broker.go` (routes, matching, decision log), `policy.go` (the shared rules: agent branches, title tags), `upstream.go` (credential injection, header rewriting), `server.go` (routing by host, SNI check), `listen.go` (TLS, Unix socket, token commands), `ca.go` (the name-constrained CA). `gitproto/` parses pushes, `mcp/` JSON-RPC, `brokertest/` the shared test fixtures |
| `allowlist/` | The egress allowlist: groups (`lists/sbx-balanced.yaml`, copied from sbx's balanced policy, and `lists/groups.yaml`, ours and the providers'), the base layer, user and org layers; a provider's hosts drop out when the broker serves it |
| `cmd/investigator/` | The CLI. `setup`, `login`, `logout`, `open`, `stop`, `uninstall` for users (`login.go`; settings in `~/.investigator/settings.json`, tokens in the Keychain, extra hosts in `~/.investigator/allowlist.yaml`); beneath them `up\|run\|down\|shell\|code\|ssh-config\|rm <name>` (`lifecycle.go`, `supervisor.go`: broker plus VM through the microsandbox Go SDK; `remote.go`: SSH and VS Code); `image.go`: `image build`, your own image from a Dockerfile (Docker or Podman, then `msb load`); `mounts.go`: the exchange folder, `--mount` at create, `cp`; `allowlist` prints the compiled list; `ssh-setup` for sshd-based sandboxes |
| `cmd/investigator-sbx/` | The broker for an sbx sandbox: on the Mac's loopback, plain HTTP, behind a session token |
| `editor/` | How the editor reaches a sandbox: per-sandbox SSH keys and config, the harness's VS Code settings |
| `guest/` | What goes into the VM: `provision.sh` (one script for the image and the microsandbox guest), `install-keys.sh`, the image's `Dockerfile`, `mcpcli/` (the agent's MCP client, copied from the `sbx-mcp-client` skill) |
| `checks/` | `bridges.sh` (run in the editor's terminal), `vscode-e2e/` (VS Code over Remote-SSH, unattended, running `bridges.sh` through a probe extension), `step1-docker.sh` (step 1 against a container) |

## Working rules

- Verify: `test -z "$(gofmt -l .)" && go vet ./... && go test ./...`
  (`go vet` doesn't check formatting; `gofmt -l` lists unformatted
  files without failing, hence the `test -z`), and `checks/step1-docker.sh`
  (needs Docker; builds `investigator-sandbox:dev`).
- Research notes carry the date and tool versions they were checked
  against. Mark anything not tested or not confirmed in a source as
  *unverified*, and list sources at the end.
- Write plainly, in the style of `docs/design.md`: short
  sections, direct answers first, then detail.
- Never put real credentials, tokens or work-internal names, hosts or
  account IDs in this directory. Work details (Kiro as the work editor,
  Podman on work Macs, Okta for AWS) are fine at that level of
  generality.
- Experiments that need a sandbox tool or a provider account run on
  the user's machine or a throwaway account; ask before creating
  anything in a real provider.
- This is a project in the monorepo: work on branch `mcp-restrict` in
  `.sbx/mcp-restrict/`, per the top-level `AGENTS.md`.
