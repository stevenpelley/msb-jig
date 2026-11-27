# AGENTS.md — mono-jig

**mono-jig: the jig for the monorepo.** A packaged sandbox for coding
agents, built for this monorepo's projects and their environment rather
than a sandbox product plus settings (what sets it apart from sbx and
OpenShell). It serves one-shot investigations and vulnerability
remediation, and everyday development. It limits what the agent can
reach, mainly over the network, with finer rules than the providers' own
roles and tokens allow. Design notes plus the code (Go module `github.com/stevenpelley/msb-jig`,
`docs/prototype-plan.md`).

**This repository is the library** (since 2026-11-24, split from the
user's personal monorepo with its history; `docs/library-split.md`). The
personal track's command, `mono-jig` (`jig.Main(jig.Personal())`), stays
in that monorepo and imports this module; the commands, state directory
and Keychain items keep the name `mono-jig`. A work track is its own
command in its own repository. **To write or change a track, start with
`docs/tracks.md`**: the `Track` fields, each seam, what's fixed, and a
checklist. Work names (hosts, services, accounts, tools) never go in this
repository; library changes a work track needs use placeholders
(`example.org`).

**Named mono-jig on 2026-11-23** (user): before that the project was
`mcp-restrict` (directory and branch) and had another CLI name, state
directory and Keychain prefix. Since 2026-11-27 (user) the names are
msb-jig (the library) and mono-jig (the personal track) everywhere,
history included: the docs call the components that are gone
`mono-jig-net` (the Lima-era network daemon), `mono-jig-msb` (the first
microsandbox CLI), `lima/mono-jig.yaml.in`, `JIG_*` variables and a
`LIMA_HOME` of `~/.jig`. Commit messages before 2026-11-27 still use
the old names.

## Purpose

1. **A packaged harness for coworkers.** One
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
   layers (`JIG_LAYERS`) and the user's `~/.mono-jig/allowlist.yaml`
   (2026-11-23; the `mono-jig` and `dev` profiles are gone). Domain
   filtering is a separate policy from the broker's: the runtime's own.
   **The broker's policy covers only the managed providers.**

   **One process per sandbox** (2026-11-22): everything that runs
   continuously on the Mac is packaged with the VM. The `mono-jig`
   supervisor owns the microsandbox VM and serves the broker; the VM's
   egress policy is microsandbox's own. They start and stop together. No
   `launchd` jobs, no containers on the Mac.

   **Not air-tight, on purpose** (2026-11-22): the harness makes the safe
   path the easy one; it doesn't stop deliberate acts. A user who pastes
   a token into the sandbox can use it, and we don't try to prevent that.
   Nor does the allowlist stop data leaving: some allowed hosts take
   uploads to storage anyone can own, given a signed URL
   (`*.blob.core.windows.net`, `*.googleapis.com`, and `*.amazonaws.com`
   when AWS isn't brokered). It limits where the agent reaches with
   credentials, not what it can send.

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
   PRs from those branches, never merge. Fine-grained tokens and
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
`docs/microsandbox.md`, `docs/prototype-plan.md` step 6b). `mono-jig up <name>`
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
  (`docs/prototype-plan.md` step 7). In proxy mode, since 2026-11-25, at
  the server's own URL (`aws-mcp.us-east-1.api.aws/mcp`), which the
  broker answers itself; proxy mode has no vsock route (checked on the
  Mac 2026-11-27 with every provider logged in:
  `docs/transparent-broker.md`, "End to end with every provider").

The Lima backend was removed the same day (in git at `695143a`), with its
netstack and gvisor.

Still open:
- VS Code: the bridges check runs unattended (`checks/vscode-e2e/`, step 2); the git extension's socket and the remote `code` CLI stay open; Claude Code's extension installs and works on the remote; a repo's `.vscode/settings.json` overrides our settings (no new bridge, but ports and askpass come back); `mono-jig code <name> [dir]` (now `mono-jig vscode`) checked by the user;
- allowing a host without `rm`: waiting for microsandbox#1538;
- sandbox creation speed. Done: per-component timings in `run.log` (`[provision] <name> took Ns`, `sandbox running`, `provisioned`), and every new sandbox starts from an image built on the Mac with Docker or Podman (cached when unchanged), so a first start runs only `ca broker apply`. Checked on the Mac 2026-11-27: the first creation, which builds the image, spent most of its time installing Claude Code; a second creation from the cached image was much faster. Still open, in order:
  1. one image layer per component, done 2026-11-27: `guest.ImageDockerfile` writes a layer per baked component, in provision.sh's order, each copying only its own file and removing its apt lists, with `PROVISION_HASH` in the last layer. Built in this sandbox's Docker: from scratch 454s (base 82s, git 31s, docker 163s, claude 163s); after a change to a component the image doesn't run (`apply`), 1s (it used to rebuild everything); after a change to `70-claude.sh`, 174s (only Claude's layer). Claude's install still takes minutes whenever its layer or one before it changes; an image built on another day still gets whatever version the installer serves then. *Unverified* with Podman and on the Mac;
  2. check spliced connections under msb 0.8.2: a guest `curl https://proxy.golang.org/` hung for minutes on 2026-11-25 (it answered at once in later runs);
  3. repeated creation from one image: a disk snapshot per image and provision hash, restored for each new sandbox (SDK 0.8.2 `CreateSnapshot`, `RestoreSandbox` with `WithRestoreNetworkPolicy`, `WithRestoreProxy`, `WithRestoreEnv`), then the sandbox's own CA, SSH keys and settings at start as now; whether a restore keeps mounts, the Docker disk and the vsock route is *unverified*;
  4. create the Docker disk only when Docker is first used; cache apt packages across sandboxes (now only for `--plain-image`);
- the work track: built in its own repository (2026-11-24) with GitHub limited to some owners (`github.Config.Owners`), npm (`providers.NPM`, `jig.NPM`), kiro-cli (`agent.KiroCLI`) and `Track.BrokerMode`; not yet run in a sandbox;
- the pitch (user, 2026-11-23): a full-featured sandbox (own image, host
  folders, brokered providers, an editor over SSH). Sell it first for
  one-shot investigations; with Kiro IDE's remote SSH, people may use it
  as their everyday dev sandbox.

The agent runs on the Mac itself and asks before running commands.

## Files

| File | What it is |
|---|---|
| `README.md`, `LICENSE`, `NOTICE` | The public overview; Apache 2.0 (2026-11-24), copyright Steven Pelley; NOTICE credits the host names taken from sbx's balanced policy |
| `docs/design.md` | The harness design (2026-11-21; editor over SSH added 2026-11-22): the two tracks, VS Code then Kiro over SSH into the sandbox, egress filtering, packaging as one process per sandbox, leaving sbx (for Lima, since removed, then libraries that boot VMs), dev containers, the broker, the sandbox backend interface, whether OpenShell is easier behind an IDE, packaging, open questions |
| `docs/prototype-plan.md` | The personal track, step by step, ending on microsandbox (6b): who tests what (this sandbox vs the user's Mac) |
| `docs/microsandbox.md` | microsandbox as the VM layer (2026-11-22, msb 0.7.16): checks on the Mac, the broker over vsock, the supervisor, Docker, the environment |
| `docs/transparent-broker.md` | A broker the guest doesn't name (2026-11-23, msb 0.7.16, sbx v0.54.0): the broker as microsandbox's outbound CONNECT proxy, terminating brokered SNI and splicing the rest; sbx's upstream proxy; declarative rules (Go decoders, lookups, CEL) and the broker as a library; the hook tested with a toy proxy and through the Go SDK (2026-11-24); proxy mode as built, `--broker proxy` at create, and its end-to-end checks; egress decided by the broker, with DNS through a resolver on the Mac (investigated and tested 2026-11-24, not built) |
| `docs/tracks.md` | Writing a track (2026-11-24): `jig.Track`'s fields; providers (broker and login halves), agent, editor, VM runtime, provisioning components, guest settings, allowlist layers, the CA; what a track can't change yet; a checklist for a new one |
| `docs/library-split.md` | Splitting the harness into a common library and per-track modules (reviewed 2026-11-24, not built): where providers, base image, corporate CA, agent, editor and VM runtime are hardwired; proposed `Track`, `Provider`, `Agent`, `Editor`, `Backend` seams and the order to build them; the PR marker (branch vs title tag vs label) |
| `docs/enforcement.md` | Making the harness required (2026-11-22): threats, model-gated chain of trust, network and provider-side options, small provider asks |
| `broker/providers/` | **Start reviewing here.** The provider registry: `Provider` (name, hosts per mode, handlers, guest settings), one file per provider (`gitlab.go`, `github.go`, `anthropic.go`, `aws.go`, `npm.go`, `mcpserver.go`, `jira.go`) naming the `Routes` function that holds its policy; `Config.Providers` holds those that are on. In proxy mode (`Config.Proxy`) they're served under their real hostnames (`Hosts`); GitHub builds only the active mode's tables (`github.ServerRoutes`, or `ProxyRoutes` for its web and API hosts, github.com and api.github.com by default) |
| `broker/<provider>/routes.go` | `Routes` (`gitlab`; `github` has `ServerRoutes` and `ProxyRoutes`, one set of tables per mode, from shared route lists; in `anthropic.go` and `aws.go` for the two without per-request rules): the provider's upstreams with their credentials, then its route table (every request it serves, the check it runs, where it goes); anything unmatched is denied, except in proxy mode, where GitHub and GitLab forward unmatched reads without the credential (their last route, `anonymous.read`). The checks are beside it (`checks.go`, `mcp.go`, `graphql.go`); tests run through `providers.Build` |
| `broker/` | The core: `broker.go` (routes, matching, decision log), `policy.go` (the shared rules: agent branches; no title tag since 2026-11-24), `upstream.go` (credential injection, header rewriting), `request.go` (reading requests as the provider does: no dot segments, no case-variant JSON keys, no URL parameters beside a checked body), `server.go` (routing by host, SNI check), `listen.go` (TLS, Unix socket, token commands), `connect.go` (proxy mode's front end: microsandbox's CONNECT proxy; terminates brokered SNI, splices the rest, refuses TLS without SNI), `ca.go` (the name-constrained CA). `gitproto/` parses pushes, `mcp/` JSON-RPC, `brokertest/` the shared test fixtures |
| `allowlist/` | The egress allowlist: groups (`lists/sbx-balanced.yaml`, copied from sbx's balanced policy, and `lists/groups.yaml`, ours and the providers'), the base layer, user and org layers; a provider's hosts drop out when the broker serves it |
| `jig/` | The CLI, as a library (`Track`: providers, agent, editor, backend, base image; `Main`; `Personal`). `setup`, `login`, `logout`, `open`, `stop`, `uninstall` for users (`login.go`; each provider's login, status, settings and broker provider in `login_<name>.go`, listed in `registry.go`; settings in `~/.mono-jig/settings.json`, by provider, tokens in the Keychain, extra hosts in `~/.mono-jig/allowlist.yaml`); beneath them `up\|run\|down\|shell\|code\|ssh-config\|rm <name>` (`lifecycle.go`, `supervisor.go`: broker plus VM (`brokerhost.go` the broker, `spec.go` the sandbox from the config, `provision.go` the guest, through `sandbox.Backend`); `guestconfig.go`: the guest's settings, from the providers and the agent; `track.go`: the agent and editor in use; `remote.go`: SSH and the editor; `sshd.go`: the guest's OpenSSH sshd, for an editor that needs it (Kiro)); `ca.go`: `ca`, extra CA certificates (a TLS-inspecting proxy's root) for the image build and every start; `image.go`: every new sandbox's image, built on the Mac first (Docker or Podman, from their cache when unchanged, then `msb load` unless cached; `--plain-image` skips it), `image build` ahead of time and your own Dockerfile on the base (recorded for later builds, tagged by image ID); `mounts.go`: the exchange folder, `--mount` at create (read-only unless `:rw`), `cp`; `brokermode.go`: `--broker server\|proxy` at create, the proxy's port and password; `allowlist` prints the compiled list; `ssh-setup` for sshd-based sandboxes |
| `cmd/mono-jig-sbx/` | The broker for an sbx sandbox: on the Mac's loopback, plain HTTP, behind a session token |
| `sandbox/` | The VM runtime (`Backend`: open from a `Spec`, exists, remove; `Guest`: files, run, jobs, stop) and `Microsandbox`, the only place the CLI's sandboxes touch the microsandbox SDK |
| `agent/` | The coding agent (`Agent`): its provision component, its guest settings and instructions file, its editor extensions. `ClaudeCode` (`70-claude.sh`); `KiroCLI` (`70-kiro.sh`, a pinned kiro-cli; steering, permissions and settings files; 2026-11-24) |
| `broker/npm/` | The npm provider (2026-11-24): read-only registry routes (no publish, dist-tags, access, login or tokens), tarball URLs rewritten to the broker in server mode |
| `broker/hostedmcp/` | A vendor's hosted MCP server (2026-11-24): its endpoint only, `tools/call` allowed for a list of tools, `tools/list` filtered to it; `providers.MCPServer` wraps it, one per server, with mcpcli in the guest |
| `broker/jira/` | Jira Cloud's REST API, read-only (2026-11-24): GETs and the POSTs that read, sent to Atlassian's gateway for the site's cloud ID with a scoped token; `providers.Jira`, `jig.Jira` (`login jira`, Keychain item `mono-jig-jira`) |
| `oauth/` | The harness as an OAuth client of a hosted MCP server (2026-11-24): discovery (RFC 9728, RFC 8414), registration of a public client (RFC 7591), sign-in with PKCE and a loopback redirect, and `Source`, the broker's refreshing access token; `jig.OAuthMCP` (`jig/login_oauth.go`) is its login, `jig/mcplist.go` lists a server's tools at login |
| `editor/` | The editor on the Mac (`Editor`: find, configure, install, open) and how it reaches a sandbox: per-sandbox SSH keys and config; `VSCode`, with the harness's VS Code settings; `Kiro` (Kiro IDE 1.5's Remote SSH, 2026-11-25) |
| `guest/` | What goes into the VM: `provision/` (`provision.sh`, one script for the image and the microsandbox guest, sources `lib.sh` and `components/NN-<name>.sh` in order; the `apply` component writes `guest.Config`, `ca` trusts the extra CAs), `config.go` (`guest.Config`: the environment, git settings, files and packages the supervisor builds per sandbox), `instructions/` (the agent's instructions, a section per provider), `install-keys.sh`, the image's `Dockerfile`, `mcpcli/` (the agent's MCP client, copied from the `sbx-mcp-client` skill) |
| `checks/` | `bridges.sh` (run in the editor's terminal), `vscode-e2e/` (VS Code over Remote-SSH, unattended, running `bridges.sh` through a probe extension), `step1-docker.sh` (step 1 against a container) |

## Working rules

- Verify: `test -z "$(gofmt -l .)" && go vet ./... && go test ./...`
  (`go vet` doesn't check formatting; `gofmt -l` lists unformatted
  files without failing, hence the `test -z`), and `checks/step1-docker.sh`
  (needs Docker; builds `mono-jig-sandbox:dev`).
- A check reads a request the way the provider will, or the request means
  one thing to the broker and another upstream. Read bodies with
  `ReadJSONBody` or `DecodeJSON` (`broker/request.go`), never plain
  `json.Unmarshal` into a struct (Go matches keys ignoring case); don't
  read a body while the URL's parameters could stand in for it; on
  GitLab, refuse text that holds quick actions (`/merge`, `/approve`).
- Research notes carry the date and tool versions they were checked
  against. Mark anything not tested or not confirmed in a source as
  *unverified*, and list sources at the end.
- Write plainly, in the style of `docs/design.md`: short
  sections, direct answers first, then detail.
- Never put real credentials, tokens or work-internal names, hosts or
  account IDs in this directory. Work details (Kiro as the work editor,
  Podman on work Macs, Okta for AWS) are fine at that level of
  generality.
- A provision component that downloads a tool pins its version and
  checks the download's SHA-256 (`agent/70-kiro.sh`); it doesn't pipe an
  installer into a shell.
- Experiments that need a sandbox tool or a provider account run on
  the user's machine or a throwaway account; ask before creating
  anything in a real provider.
- This is a project in the monorepo: work on branch `mono-jig` in
  `.sbx/mono-jig/`, per the top-level `AGENTS.md`.
