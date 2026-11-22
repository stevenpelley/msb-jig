# AGENTS.md — mcp-restrict

Exploration project: tools that limit what a coding agent in a sandbox can
reach, mainly over the network, with finer rules than the providers' own
roles and tokens allow. Design notes plus the prototype's code (Go module
`mcp-restrict`, `prototype-plan.md`).

## Purpose

1. **A packaged harness for coworkers** ("investigator in a box"). One
   command starts a sandboxed agent that runs without permission prompts
   and reaches GitHub, AWS, Datadog and Wiz in restricted ways, using the
   developer's own credentials. A developer hands it a Datadog alert or a
   Wiz issue and it investigates. Interactive use is an editor on the Mac
   connected over SSH into the VM sandbox (Lima; sbx for the
   prototype), where the agent, terminals and Docker run. **VS Code first**, prototyped on a
   personal Mac with personal accounts (also for personal use); then Kiro
   IDE, which gained remote SSH in 1.5 (2026-11-20). Dev containers only
   ever run inside that VM, never on the Mac: our tests need Docker, and
   the VM is the isolation boundary. Everything (sandbox, broker, SSH and
   editor configuration) is packaged so the user configures nothing.
   Later the same pieces should serve headless, automated agents.

   **Configurable into a general dev sandbox** (2026-11-22): the egress
   allowlist comes in layers (base, profile, org, user). The
   `investigator` profile is narrow; a `dev` profile adds documentation
   sites and hosts that serve dev and production tools. Domain filtering
   is a separate policy from the broker's: the runtime's own, or an
   embedded off-the-shelf proxy. **The broker's policy covers only the
   managed providers.**

   **One process per sandbox** (2026-11-22): everything that runs
   continuously on the Mac is packaged with the VM. `investigator-net`
   is the VM's only network (a Lima `user-v2` daemon) and serves the
   domain filter and the broker inside it. It starts and stops with the
   VM. No `launchd` jobs, no containers on the Mac.

   **Not air-tight, on purpose** (2026-11-22): the harness makes the safe
   path the easy one; it doesn't stop deliberate acts. A user who pastes
   a token into the sandbox can use it, and we don't try to prevent that.

   **No dependency on sbx** (2026-11-22): assume work won't allow Docker
   accounts. Prototype on sbx, but the design isn't done until it runs on
   Lima ("Leaving sbx: Lima" in `investigator-design.md`).

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
| Sandbox runtime (Lima with `investigator-net`; sbx for the prototype) | Trusted to confine the agent: no host credentials, egress only through the domain filter and the broker. |
| Broker / MCP proxy (ours) | Trusted: holds credentials, enforces policy. Its integrity on a Mac rests on the OS (code signing, MDM). |
| Model provider / model gateway | The org controls access to it; the strongest candidate chokepoint, since every agent needs a model and users don't bring their own corporate one. |
| Provider (GitHub, AWS, Datadog, Wiz) | Goal: rely on nothing it doesn't already ship. Secondary goal: find small, standard features it would happily add (e.g. per-credential network binding, delegation markers in audit logs). |

## Current state

Step 1 of `prototype-plan.md` is built and passes here (2026-11-22): the
sandbox image (`sandbox/`), the CLI's SSH setup and VS Code settings
(`cli/`, `cmd/investigator`), `checks/bridges.sh`, and
`checks/step1-docker.sh`. Step 3 too: the broker's GitLab adapter
(`broker/`), tested with real git against `git-http-backend` and with a
fake GitLab API; `glab` against the broker is still unchecked. Step 3b too:
the filter (`filter/`, `investigator allowlist`; our own proxy, since
smokescreen can't chain), tested with real tools, and the netstack
(`net/`), tested with a fake guest; `investigator-net lima` serves both and
the broker on Lima's user-v2 sockets, not yet against a real VM. Step 4
(the MCP filter) is next. Then step 4. The Lima spike questions are answered on paper
(2026-11-22, "Option C on Lima" in `investigator-design.md`), and the
Mac side is packaged as one process per sandbox, `investigator-net`
("Packaging: one process per sandbox"). Lima fails open until lima#5818
or #5813 merges, so the CLI must check the daemon and the VM's network
after boot. A design review (2026-11-22) tightened the broker (deny
unlisted writes, push options, LFS tokens, MCP tools), the netstack (no
gvisor-tap-vsock forwarders) and the Lima lifecycle; libkrun is assessed
as a second VM runner and a path to the cloud. Next:
`prototype-plan.md`, step 1: the sandbox image and SSH, tested here
against Docker containers standing in for sbx. Steps marked for the
user run on the user's Mac: this sandbox can't run sbx, Lima or VS Code.

## Files

| File | What it is |
|---|---|
| `investigator-design.md` | The harness design (2026-11-21; editor over SSH added 2026-11-22): the two tracks, VS Code then Kiro over SSH into the sandbox, egress filtering, packaging as one process per sandbox (`investigator-net`), leaving sbx for Lima and libraries that boot VMs (libkrun and the cloud path), dev containers, the broker (core plus per-provider adapters, GitLab and GitHub, hosted-MCP tool filtering), the sandbox backend interface, whether OpenShell is easier behind an IDE, packaging, open questions |
| `prototype-plan.md` | The personal track, step by step, ending on Lima: who tests what (this sandbox vs the user's Mac) |
| `sandbox/` | `provision.sh` (one script for the sbx image and the Lima VM), `install-keys.sh`, the image's `Dockerfile` |
| `cli/`, `cmd/investigator/` | The CLI: per-sandbox SSH keys, pinned host key, SSH config, VS Code settings; `allowlist` compiles the layers |
| `filter/` | The domain filter: allowlist groups and layers (`lists/*.yaml`, embedded), the ACL, our explicit proxy (direct or chained to an upstream) |
| `net/` | Package `netstack`: the VM's only network (gvisor stack on gvisor-tap-vsock's switch, no forwarders), DHCP, our DNS, Lima's user-v2 sockets and endpoint API |
| `cmd/investigator-net/` | The per-sandbox network process: `lima` (netstack, filter, broker) or `filter` (the filter on TCP) |
| `broker/` | The broker: policy (`Decide`), receive-pack parsing, the GitLab adapter (git, LFS, REST, GraphQL), the name-constrained CA, the shared server checks |
| `checks/` | `bridges.sh` (run in the editor's terminal), `step1-docker.sh` (step 1 against a container) |
| `enforcement.md` | Making the harness required (2026-11-22): threats, model-gated chain of trust, network and provider-side options, small provider asks |

## Working rules

- Verify: `go vet ./... && go test ./...`, and `checks/step1-docker.sh`
  (needs Docker; builds `investigator-sandbox:dev`).
- Research notes carry the date and tool versions they were checked
  against. Mark anything not tested or not confirmed in a source as
  *unverified*, and list sources at the end.
- Write plainly, in the style of `investigator-design.md`: short
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
