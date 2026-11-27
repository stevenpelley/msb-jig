# mono-jig: design notes

The goal is a packaged sandbox that coworkers can run with one command. It contains kiro-cli, every tool our projects need to run tests, and managed access to GitHub, AWS, Datadog and Wiz using each developer's own credentials. A developer hands it a Datadog alert link or a Wiz issue, and the agent investigates. The same setup should later serve as the foundation for headless, automated agents.

**Update 2026-11-22:** Kiro IDE 1.5 (released 2026-11-20) connects over SSH to a remote Linux host and runs its agent there. The target is now **an editor on the Mac, as the UI only, connected over SSH to the sandbox**, where files, terminals and the agent run. **Build it for VS Code first**, prototyped on a personal Mac with personal accounts, then move to Kiro. kiro-cli stays in the image for headless runs. See [The editor over SSH into the sandbox](#the-editor-over-ssh-into-the-sandbox).

**Update 2026-11-22, later:** everything that runs continuously on the Mac for a sandbox is **one process, `mono-jig-net`**: the VM's network stack, the domain filter and the broker, started and stopped with the VM. No `launchd` jobs and no container stack. See [Packaging: one process per sandbox](#packaging-one-process-per-sandbox).

**Update 2026-11-22, after a design review:** the broker denies every write it doesn't list ([Broker hardening](#broker-hardening-what-the-review-found)); `mono-jig-net` builds its netstack without gvisor-tap-vsock's forwarders ([Why the broker listens on the netstack](#why-the-broker-and-the-filter-listen-on-the-netstack)); the Lima lifecycle checks the VM's network after boot, not only the pidfile ([Lifecycle](#lifecycle)); and libkrun is assessed as a second VM runner and a path to the cloud ([libkrun](#libkrun-a-vm-inside-our-own-process-and-a-path-to-the-cloud)).

## Two tracks: personal first, then work

*Decided 2026-11-22.* Build the harness for personal use first, on a personal Mac, then adapt it for work. The personal version is useful on its own and proves the structure.

| | **Personal (build first)** | **Work (adapt)** | What carries over |
|---|---|---|---|
| Git provider | GitLab, with my own GitLab user's credentials through the broker (not a separate agent user), so it exercises the work model | GitHub, with each developer's own credentials | The broker core and the git push check. The provider adapter is new. |
| Hosted MCP servers | GitLab's | Datadog's, Wiz's | The MCP tool-call filter |
| AWS | AWS MCP proxy with personal roles | The same, with Okta roles | Nearly all of it |
| Agent | Claude Code | Kiro | Swapped: egress hosts, sign-in, settings, and the headless flags (`claude -p` vs kiro-cli) |
| Ways to run | Directly in the sandbox, or VS Code over SSH into it | Directly in the sandbox, or Kiro IDE over SSH into it | SSH, sshd and the CLI's configuration. The credential-bridge checks must be rerun for Kiro. |
| Sandbox | sbx for the prototype, then Lima | **Lima** (assume no Docker accounts); sbx only if work allows them | All of it, behind the [sandbox backend interface](#the-sandbox-backend-interface) |

The parts that don't carry over are small: one provider adapter, the agent profile and the editor settings.

Both tracks also serve as a **general dev sandbox**: the egress allowlist has an `mono-jig` profile and a broader `dev` profile with documentation and tool-download hosts (see [Egress filtering](#egress-filtering-a-separate-policy-from-the-brokers)).

These notes compare building it on **Docker Sandboxes (sbx)** and on **NVIDIA OpenShell**. The research was done on 2026-11-21 with OpenShell 0.1.9 and sbx v0.54.0 on macOS. *2026-11-27: OpenShell can't run Docker inside its sandboxes on any driver, so we stay on microsandbox; see [openshell.md](openshell.md).* Items marked *unverified* haven't been tested.

## Constraints

- **Our team mostly uses AI badly.** People ask questions about code they already have checked out and rarely let an agent make changes. When they do, every action needs manual approval. Nobody lets the agent handle git or PRs, or gives it read-only access to AWS, Datadog or Wiz. Kiro is the editor work provides, and nobody has worked out how to sandbox it.
- **The agent must not see the developer's home directory configuration or long-lived credentials.**
- **Tests use Docker,** so the sandbox needs a working Docker engine inside it. Running the agent in a plain container won't work.
- **Work Macs use Podman Desktop, not Docker.** CI still uses Docker, and we no longer have work Docker accounts.

## Direct answers

### Does Kiro work with sbx or VM sandboxes?

- **sbx: yes, it's built in.**
  - `sbx run kiro <workspace>` runs kiro-cli in a microVM, with every tool approved automatically.
  - You log in with a device code. The login is stored inside the sandbox (`~/.local/share/kiro-cli/data.sqlite3`); it survives a restart but not `sbx rm` followed by a new sandbox.
  - Docker publishes a Kiro kit and image (`docker.io/sbx/kiro-image`, which asks for Docker inside the sandbox) that we can build on.
- **OpenShell: no built-in support.**
  - It runs any image, so we'd install kiro-cli in our own image and write its network policy.
  - OpenShell turns every blocked request into a pending rule (`openshell rule get <sandbox>`), which is a quick way to discover the hosts Kiro needs.
  - NVIDIA's docs claim ready-made policy coverage only for Claude Code.
- **Kiro's own `/sandbox`: the doubts are justified.**
  - It wraps only the shell commands the agent runs, using bubblewrap on Linux (and presumably Seatbelt on macOS, *unverified*).
  - The Kiro process itself keeps full access to the home directory and credentials.
  - MCP servers aren't sandboxed by default (`mcpSandboxing: false`).
  - Docker won't run inside bubblewrap.
  - It limits what commands can touch, but it doesn't isolate the agent.
- **For teammates who want an IDE:** keep the agent in the sandbox and attach the editor over SSH. Kiro IDE supports this since 1.5 (2026-11-20), and VS Code has for years; see [The editor over SSH into the sandbox](#the-editor-over-ssh-into-the-sandbox).

### Can sbx run without a Docker login?

- **No.** Login is required even for local use, and a Docker maintainer said the reason is business, not technical (desktop-feedback #466).
- **Unattended runs** can sign in with a personal access token (`echo "$DOCKER_PAT" | sbx login --username <id> --password-stdin`), but that still needs a Docker account.
- **Podman doesn't matter here.** sbx installs on its own, without Docker Desktop or Docker Engine, and each sandbox gets its own Docker daemon inside the microVM.
- **The only blocker is the account.** The sbx CLI is free for commercial use once signed in, so the question is whether work allows free personal Docker accounts.

## The editor over SSH into the sandbox

*Added 2026-11-22. Not yet tested.*

**Build order: VS Code first, then Kiro.** VS Code's Remote - SSH has been mature for years, and a VS Code version is also useful for personal work, so the prototype runs on a personal Mac with personal accounts. Kiro IDE 1.5 (2026-11-20) works the same way, so most of this carries over; [Kiro IDE 1.5](#kiro-ide-15-after-vs-code-works) lists the differences.

```
 Mac                                          Sandbox (microVM)
┌─────────────────────────┐     ssh          ┌───────────────────────────────┐
│ VS Code / Kiro          │ ───────────────▶ │ sshd                          │
│   (editor only)         │                  │ editor server + agent         │
│ broker                  │ ◀─────────────── │ git / gh / docker / toolchains│
└─────────────────────────┘  git, API, MCP   └───────────────────────────────┘
```

In both editors, files, terminals and the agent run in the sandbox. Only the editor UI runs on the Mac.

### VS Code (build first)

- **Remote - SSH** (`ms-vscode-remote.remote-ssh`) installs the VS Code server on the remote and runs "workspace" extensions there, including the agent's extension.
  - The extension is Microsoft's and licensed only for Microsoft's VS Code, which is presumably why Kiro built its own.
- **The CLI can own the SSH configuration.** `remote.SSH.configFile` points VS Code at the CLI's own file, so `~/.ssh/config` stays untouched.
- **No Microsoft egress needed from the sandbox.** With `remote.SSH.localServerDownload: "always"`, the Mac downloads the VS Code server and copies it over SSH. The exact values of that setting are *unverified*.
- **Opening the window.** `code --remote ssh-remote+<host> /workspace` opens it directly, so `mono-jig open` can launch it.
- **A separate VS Code instance for the harness:**
  - Launch with `code --user-data-dir <state>/vscode --extensions-dir <state>/vscode-ext`.
  - That instance has its own settings, extensions and signed-in accounts, so the developer's everyday VS Code accounts (GitHub, Microsoft) and Settings Sync never reach the sandbox.
  - A VS Code *profile* is lighter, but whether profiles keep signed-in accounts separate is *unverified*, so start with `--user-data-dir`.
- **The agent** for personal use is Claude Code: the CLI in the remote terminal, and its VS Code extension running in the remote extension host (*unverified* that it runs remotely). It signs in inside the sandbox, so its model token lives there. sbx's Claude kit injects the credential through its proxy instead, but the harness doesn't use that (prototype-plan step 0); whether the broker can hold the model credential is an open question.
- **Installing the agent's extension on the remote** needs the Marketplace from the sandbox, or `remote.downloadExtensionsLocally: true`, so the Mac downloads it and copies it over (*unverified* setting name). `localServerDownload` covers only the VS Code server.
- **Extensions:** set `extensions.allowed` to our short list and ignore workspace extension recommendations, so a repo can't pull new extensions into the harness instance.
- **Dev Containers** (`ms-vscode-remote.remote-containers`) can build a container on the SSH host, using the host's Docker. That gives "Reopen in Container" in [Dev containers](#dev-containers-only-inside-the-vm).

### What the `mono-jig` CLI configures

- **sshd in the sandbox image:** key-only authentication, one user, no passwords. It also refuses forwarding on the server side: `AllowAgentForwarding no`, `X11Forwarding no`, `PermitTunnel no`, `AllowStreamLocalForwarding no`, and `AllowTcpForwarding local`, because the editors reach their server through local forwards but nothing needs remote ones. Then the SSH bridges stay closed even if the editor's settings are wrong. Whether VS Code and Kiro work with these settings is *unverified*. Lima's own user and key must keep working: Lima waits for SSH at boot and runs its checks over it.
  - **This is a guard against mistakes, not against the agent.** The agent may be root in the VM (and Docker access makes it root anyway), and it runs as the same user as the editor's server, so it can change `sshd_config` or `~/.vscode-server`. Only settings enforced on the Mac (the client SSH config, the harness instance's settings) count as controls.
- **A keypair per sandbox and a pinned host key.** The CLI generates both when it creates the sandbox and writes the host key to its own `known_hosts` file. The editor's fingerprint check then passes without a trust-on-first-use prompt.
- **Its own SSH config file,** such as `<state>/ssh/config`:
  - VS Code reads it through `remote.SSH.configFile`.
  - Kiro asks which config file to save a host to, so it may need one `Include` line in `~/.ssh/config`. Whether Kiro reads a separate file is *unverified*.
  - The host entry sets `IdentitiesOnly yes` with `IdentityFile` set to the per-sandbox key, `UserKnownHostsFile` set to the CLI's own file, `ForwardAgent no`, `ForwardX11 no`, and no `RemoteForward`.
- **How SSH reaches the sandbox:**
  - **sbx** has two options. Publish sshd on `127.0.0.1` (`sbx ports <name> --publish 127.0.0.1:2222:22`), or use a `ProxyCommand` through `sbx exec`. Whether `sbx exec` carries stdin and stdout cleanly enough is *unverified*.
  - The `ProxyCommand` route is better, because it leaves no listening port that any process on the Mac could reach. Both editors accept a `ProxyCommand` on the first hop.
  - **OpenShell** already sets up SSH (`sandbox connect --editor vscode`). Whether it writes a plain SSH config that Kiro accepts is *unverified*.
- **Editor settings**, written into the harness's own VS Code instance or Kiro's settings: the ones in the next section that close the credential bridges.
- **`mono-jig open`** starts `mono-jig-net` and the sandbox, then opens the editor on the remote folder.

### The new trust boundary: the IDE connection

SSH and the editor's remote channel form a second path between the sandbox and the Mac, beside the broker. Anything that path carries from the Mac into the sandbox breaks the rule that the sandbox never holds a real token. Setting names below are VS Code's. Kiro, as a fork, probably has the same ones (*unverified*).

| Bridge | Risk | Close it with |
|---|---|---|
| SSH agent forwarding | The agent pushes with the developer's own SSH key, skipping the broker's GitHub rules | `AllowAgentForwarding no` in the sandbox's sshd, which holds whatever the client asks for; also `ForwardAgent no` and `remote.SSH.enableAgentForwarding: false` (VS Code's default is on) |
| Git credential bridging | Git in a remote terminal, or the Git extension, asks the editor for credentials. The editor answers from the Mac's credential helper, or from its own GitHub sign-in. | `git.terminalAuthentication: false`, `github.gitAuthentication: false`, and git in the sandbox pointed at the broker so it never asks |
| The editor's authentication API | Any extension running in the sandbox can ask for the editor's GitHub or Microsoft session token, after a one-time consent prompt | The separate `--user-data-dir` instance, with no accounts signed in |
| Dev Containers forwarding | Dev Containers copies `.gitconfig` into containers and forwards git credentials and the SSH agent by default | Turn those off (`dev.containers.copyGitConfig` and related settings; exact names *unverified*) |
| Settings Sync | Brings the developer's everyday extensions and settings into the harness instance | Off in the harness instance |
| Port forwarding toward the Mac | Lima forwards every guest port bound on `127.0.0.1` or `0.0.0.0` to the Mac's `127.0.0.1` by default, and VS Code auto-forwards ports it sees. The agent could take the port of a login callback, or pose as one of the user's local dev servers. | A Lima `portForwards` rule with `ignore: true` for all ports and protocols; `remote.autoForwardPorts: false`; keep `remote.localPortHost` at `localhost`. Forward a port only when the user asks. |
| Opening URLs on the Mac | `$BROWSER` and `code --openExternal` in a remote terminal open a URL in the Mac's browser, with the user's cookies and outside the filter | Remove `$BROWSER` in the remote environment and test whether `openExternal` can be refused (*unverified*); `checks/bridges.sh` probes it |

**The agent's own sign-in token lives in the sandbox.** That's intended, since the agent needs model access there. It's one more reason to keep sandbox state in a volume that `mono-jig logout` wipes.

This adds a component to the trust table in `enforcement.md`: we rely on the editor not to pass credentials into the sandbox, and we control that only through the settings we ship.

### Dev containers: only inside the VM

Use dev containers only with the container running inside the sandbox VM, never on the Mac:
- **A dev container on the Mac** would run on Podman, outside any VM boundary of ours. Its `initializeCommand` would run on the Mac, and it couldn't run our Docker tests.
- **A dev container in the sandbox keeps the VM as the boundary.** The editor connects over SSH to the sandbox and runs "Reopen in Container" against the sandbox's own `dockerd`. Both VS Code and Kiro 1.5 support this. Docker tests inside the container use the VM's Docker, either through its mounted socket (the `docker-outside-of-docker` feature) or a privileged `docker-in-docker`. Either way the container gets root over the VM. That's acceptable because the VM is the confined, disposable unit. `initializeCommand` then runs on the VM, not the Mac.

| | **Toolchain in the image: SSH into the VM** | **Reopen in Container: SSH into the VM, then into a container** |
|---|---|---|
| Toolchain | Baked into the sandbox image | Each repo's `devcontainer.json` |
| Docker tests | The VM's `dockerd` directly | The VM's `dockerd` through a mounted socket. Bind-mount paths must match between container and VM. |
| Agent sign-in | Once per sandbox | Once per dev container in Kiro, and chat history is lost on rebuild |
| Broker and egress policy | The sandbox's network policy applies | Containers need `HTTPS_PROXY` and the broker's CA too. On Lima they share the VM's only NIC, so they can't get around the filter; on sbx, likely covered since its policy sits at the VM, but *unverified*. |
| Several repos | One image covers them all | Different toolchains per repo in one sandbox |

**Start with A, and use the dev-container spec rather than its runtime.** Where a repo has a `devcontainer.json`, `devcontainer build` turns it into an image that becomes the sandbox image, or a layer on top of ours. Move to "Reopen in Container" when one sandbox has to host repos with conflicting toolchains, or when teams already maintain dev containers and expect "Reopen in Container" to just work.

### Kiro IDE 1.5 (after VS Code works)

*From Kiro's 1.5.11 changelog and docs.* Kiro's remote support is its own, not Microsoft's extension, so the setting names and behaviour need checking again.

- **The agent runs on the remote.** In Kiro's words: "The editor stays on your computer, while files, terminals, and the agent run on the remote machine, so the agent can keep working after you disconnect."
- **Requirements for the remote:**
  - Linux on x64 or ARM64, glibc 2.28 or later.
  - At least 4 GiB free on the home filesystem, and a home directory that other users can't write to.
  - `/bin/sh` plus `stat`, `which`, `flock`, `tar`, `gzip`, `sha256sum`, `diff`, `find`, `id`, `sort` and `readlink`.
  - HTTPS access to Kiro's download, authentication and service endpoints. Unlike VS Code's `localServerDownload`, Kiro documents no way to have the Mac fetch the server. Kiro installs its remote components itself; no kiro-cli or Node.js is needed.
- **Proxies.** If a proxy blocks the install, set `kiroRemote.httpProxy` or `kiroRemote.httpsProxy`. Kiro also has a page on TLS inspection compatibility. That matters for sbx, whose proxy intercepts TLS to inject credentials.
- **SSH configuration:**
  - Hosts come from an SSH config file.
  - Supported: `ProxyJump` chains of up to eight hops, or a `ProxyCommand` on the first hop.
  - Not supported: a `ProxyCommand` after `ProxyJump`, `Match exec`, `ProxyUseFdpass`.
  - Host keys are verified.
- **A separate sign-in.** The remote window has its own Kiro sign-in and chat history, both kept on the host. Which machine opens the sign-in browser for a remote window is *unverified*.
- **Dev containers** work locally or on an SSH host. `initializeCommand` runs outside the container. Each dev-container window has its own sign-in and chat history, and a rebuild wipes session history stored in the container.
- **Kiro's endpoints for the egress allowlist**, from Kiro's page on firewalls, proxies and data perimeters:
  - `app.kiro.dev`, `assets.app.kiro.dev`
  - `prod.us-east-1.auth.desktop.kiro.dev`, `prod.download.desktop.kiro.dev`
  - `q.us-east-1.amazonaws.com` (legacy, still required)
  - `runtime.`, `management.` and `telemetry.us-east-1.kiro.dev`
  - the IAM Identity Center hosts, if we sign in through it
  - Europe variants of the regional hosts, if needed
- **Read from Kiro 1.5.11's app bundle (2026-11-25), now `editor.Kiro`:**
  - The built-in `kiro-remote` extension resolves the remote authority `kiro-ssh` (and `kiro-cloud`). Its settings are `kiroRemote.sshConfigFile` (empty means `~/.ssh/config`), `kiroRemote.httpProxy`, `kiroRemote.httpsProxy` and `kiroRemote.serverDownloadUrlTemplate`. It has no agent-forwarding or local-server-download setting.
  - `kiro --help` lists `--user-data-dir` and `--extensions-dir`, so the harness gets its own instance as with VS Code. `--remote` isn't listed (VS Code doesn't list it either); that `--remote kiro-ssh+<host>` works is *unverified*.
  - `git.terminalAuthentication`, `git.useIntegratedAskPass` and `github.gitAuthentication` exist, all on by default.
  - Dev Containers are `kiro-dev-containers` (`kiro-dev-container` authority); its only setting is `kiroDevContainers.dockerPath`, so the copy-gitconfig and credential-forwarding settings above have no Kiro equivalent found.
  - `--remote kiro-ssh+config-<alias in hex>` opens a configured host (`editor.KiroAuthority`); other spellings are refused.
  - **Kiro reaches its remote server only through a Unix socket** (`direct-streamlocal@openssh.com`): the server's readiness message must carry a `socketPath`. `msb ssh serve` (0.8.2) refuses that channel ("administratively prohibited") and carries TCP forwards (checked on the Mac, 2026-11-25). So Kiro connects to OpenSSH's sshd in the guest, reached by `ssh -W 127.0.0.1:22` through msb's SSH (built 2026-11-25, `jig/sshd.go`, for an editor that implements `editor.GuestSSHD`): the sshd component in the image without host keys; at each start the supervisor makes the sandbox's host key the first time, pins it on the Mac (`sshd_known_hosts`, `StrictHostKeyChecking yes`), authorizes the sandbox's key, and runs sshd as a job, on loopback only, with `AllowStreamLocalForwarding local`. Checked in an ubuntu:26.04 container: a local Unix-socket forward works; remote forwards and agent forwarding are refused. **Kiro 1.5.11 opens a microsandbox sandbox this way** (on the Mac, msb 0.8.2, 2026-11-25). Kiro connects with the ssh2 JavaScript library, running the Host block's ProxyCommand itself, and drops tunnel errors without logging them: the window shows only "WebSocket close with status code 1006". Its flattened SSH config skips itself (the root's Include glob matches `_kiro/ssh_config` too, and stale blocks there won). An older image gets sshd at its first start (the component runs `apt-get update` when it's missing).
  - Before connecting, Kiro asks whether to trust the remote host, warning that extensions on it run automatically and can run code on the Mac (opening a local terminal, for example). That is one more bridge from the sandbox to the Mac, still to test.
- **Cloud sessions.** 1.5 can also open Kiro's own cloud sessions in the IDE. They run in Kiro's cloud, where our broker isn't, so they're out of scope for now. They may matter for headless agents once the broker can run centrally.

## The piece to build first: the credential broker

Every requirement points to the same component: code on the Mac that holds each developer's credentials, enforces our rules, and serves the sandbox. The sandbox never holds a real token. This makes the sandbox tool interchangeable, and the broker is what we'd reuse for headless agents. It's a package inside `mono-jig-net` ([Packaging](#packaging-one-process-per-sandbox)), served over HTTPS on one hostname per provider. Logins are the CLI's job, not the broker's.

```
 Mac: mono-jig-net (developer's credentials)   Sandbox (VM, no real tokens)
┌──────────────────────────────────────────┐       ┌──────────────────────────┐
│ broker                                   │       │ the agent                │
│  ├─ github.broker.internal: git + API    │ ◀──── │ git / gh / stack tools   │
│  ├─ gitlab.broker.internal: git + API    │ ◀──── │ git / glab               │
│  └─ mcp.broker.internal/<server>         │ ◀──── │ MCP client, curl         │
│       ├─ AWS MCP (SigV4, read-only role) │       │ project toolchains       │
│       ├─ GitLab, Datadog, Wiz MCP        │       │ docker (for tests)       │
│       └─ tool-call filter                │       └──────────────────────────┘
│  policy (Go), decision log               │
└──────────────────────────────────────────┘
```

### Broker core and provider adapters

*Added 2026-11-22.* The broker splits into a core shared by every provider and a small adapter per provider.
- **Core:** serve the broker's own endpoints for each provider, inject the real credential, check git pushes, decide each request against the policy, log every decision. It doesn't filter general egress (see [Egress filtering](#egress-filtering-a-separate-policy-from-the-brokers)).
- **Adapter:** parse each request into a normalized decision request: the user, the operation, the repo or project, and the facts the rules need (ref, title, source branch, MCP tool name and arguments). It also names the upstream hosts and says how the provider expects its token.

GitLab is built first, and building it proves the split before GitHub.

### GitHub

GitHub is where the broker matters most at work.

**The requirements:**
- Allowed: clone, fetch and pull; search; push only to branches named `agent/<username>/...`.
- Allowed: create, update and delete PRs whose title contains `[agent/<username>]` (*2026-11-24: the title tag was dropped; the source branch decides, [library-split.md](library-split.md#the-pr-marker)*); manage stacks; change draft status; read CI status and logs; comment on and resolve review threads.
- Forbidden: merging.

**What a network proxy can enforce:**
- Both tools can swap in the real token on the way out, so the sandbox only ever sees a placeholder.
- OpenShell can also match REST method and path and GraphQL operation names, so it could block merging: `PUT /repos/*/*/pulls/*/merge`, the GraphQL `mergePullRequest` and `enablePullRequestAutoMerge` mutations, and merge-queue operations.
- sbx can only allow or deny whole hosts.

**What a network proxy can't enforce:**
- The branch-name rule lives in the `git-receive-pack` request body, and the PR-title rule in the JSON body.
- Neither tool's rules can inspect request bodies, and OpenShell's MCP inspection explicitly doesn't look at tool arguments.
- GitHub rulesets can't help either. The agent acts as the developer, and a ruleset can't say "this user, but only when it's the agent."

**What to build:** a small proxy that serves both Git's HTTP protocol and the GitHub API.
- **Pushes:** read the ref updates at the start of each `git-receive-pack` request (they're easy to parse) and reject any ref outside `refs/heads/agent/<user>/`.
- **API: deny every write that isn't listed.** Allow reads, and allow writes only from a short list, each with its checks. Several REST and GraphQL routes write refs without receive-pack (`PUT /repos/*/*/contents/*`, `PATCH /git/refs/*`, `POST /merges`, `createCommitOnBranch`, `mergeBranch`, releases and tags), so the ref rule applies to every one that's allowed, and the rest stay denied. Merging is never listed. See [Broker hardening](#broker-hardening-what-the-review-found).
- **PR updates:** fetch the existing PR first and require its head branch to be under `agent/<user>/`, so the agent can't re-tag, close or retarget a human's PR by sending a new title.
- **Policy:** plain Go, not Cedar, for now (decided 2026-11-22; see [Policy in code](#policy-in-code-not-cedar-for-now)).
- **Inside the sandbox:** `git`, `gh` and stack tooling keep working, pointed at the broker. That's a better experience than a custom MCP server that has to reimplement git. MCP tools can still go in front of the proxy later.

### GitLab (personal track)

*Added 2026-11-22.* The same rules as GitHub: push only to `agent/<user>/...`, create and edit only MRs whose title carries the tag, never merge. The broker holds my own GitLab user's token.

| | GitLab | Compared with GitHub |
|---|---|---|
| Push rule | The same git smart-HTTP protocol (`.../<project>.git/git-receive-pack`), with the same ref updates to parse | **Identical**, apart from path parsing: groups nest arbitrarily, so the repo path is `group/sub/.../project` |
| API | REST `/api/v4` plus GraphQL `/api/graphql`. A project is addressed by numeric ID *or* URL-encoded path, so the adapter normalizes both. | Same idea, different routes |
| Merging | Deny `PUT /projects/:id/merge_requests/:iid/merge` and the GraphQL merge mutation (probably `mergeRequestAccept`, *unverified*), and the auto-merge settings. Approving (`POST .../approve`) is a separate action, also denied by default. | The same deny-by-default |
| Title rule | Draft status is a `Draft:` title prefix, so the rule accepts `Draft: [agent/<user>] ...` (*the title rule was dropped 2026-11-24*) | A small difference |
| Auth | Git takes HTTP Basic with any username and the token as password. The API takes `PRIVATE-TOKEN` or `Authorization: Bearer`. | The broker picks the right form per path |
| Writes outside receive-pack | The commits API (`POST /projects/:id/repository/commits` takes a `branch`), files, branches, tags, `cherry_pick` and `revert` all write refs. Denied unless listed, and the ref rule applies to any that are. | GitHub has the same kind of routes |
| Token endpoints | `POST /personal_access_tokens/self/rotate` would hand the agent a new token and revoke the broker's. Every token, key and user-settings endpoint stays denied. | The same for GitHub's |
| Push options | `git push -o merge_request.create -o merge_request.auto_merge` creates an MR, untagged, set to merge itself. The broker strips push options (see [Broker hardening](#broker-hardening-what-the-review-found)). | GitHub ignores push options |

What this fixes in this repo today:
- **API calls from the sandbox.** sbx's proxy injects git-style Basic auth on *every* gitlab.com request, and the API rejects it. That's why the top-level `AGENTS.md` bans `curl` and `glab`. A broker that injects the right form per path makes both work.
- **"Never merge" is only an instruction today.** `main` is protected, but the `sbx/*` branches that stacked MRs target are not (since 2026-10-30). Only a line in `AGENTS.md` stops the agent from merging a stacked MR. The broker would enforce it, which makes this the first real rule to build.

### Policy in code, not Cedar (for now)

*Decided 2026-11-22.* Cedar was attractive while existing products might enforce it (OpenShell, provider-side hooks). Now the broker is the only enforcement point, and most of its work is code no policy language does: parsing receive-pack commands, GraphQL documents, REST routes and MCP arguments into facts. What's left after parsing is small. Per provider, it's an allowlist of operations (the reads, and a short list of writes; everything else is denied, including merge and approve) and three predicates: the ref is under `agent/<user>/`, the title carries `[agent/<user>]` (after an optional `Draft:`), and the source branch is in the namespace. *(2026-11-24: the title predicate was dropped: it authorized nothing beyond the source branch, and a squash merge put it in the target branch's history.)*

Cedar could express those predicates: its `like` operator handles `"refs/heads/agent/<user>/*"`. So it's not that Cedar can't, but that it would add a schema, an entity model and a second language for about a dozen rules. `cedar-go` also lacks the policy analysis tools of the Rust implementation, which were one of Cedar's main selling points.

**So the policy is Go:** a table of allowed operations per provider, plus typed predicate functions, all unit-tested. Keep the seam that would let a policy engine plug in later: each adapter produces a normalized decision request, and one `Decide(request)` function returns allow or deny with a reason, which is logged.

**Revisit Cedar if** the rules need to change without a release (an org layer edited by security, or different rules per team), or if the enumerated rules turn out to be mostly uniform and easy to express. Even then, a small YAML allowlist may be enough.

### How the agent uses git and gh

*Added 2026-11-22. Design, not built; the details marked unverified need testing in step 3.* The agent uses the ordinary command-line tools. It doesn't talk to the broker through a special CLI, `curl` or MCP for git work, and it doesn't hold any credentials.

| Agent's tool | Talks to | Protocol |
|---|---|---|
| `git` (clone, fetch, push) | `github.broker.internal`, `gitlab.broker.internal` | Git smart HTTP |
| `gh`, `glab` (PRs, MRs, CI) | The same hostnames, as a self-hosted GitHub or GitLab | REST and GraphQL |
| The agent's MCP client | `mcp.broker.internal/<server>` | MCP over HTTP, filtered per tool call ([below](#hosted-mcp-servers-filter-tool-calls)) |
| `curl` | `mcp.broker.internal` | For large MCP results saved to a file ([Curl-friendly MCP](#curl-friendly-mcp)) |

**Setup inside the VM** (written by `provision.sh` and the CLI):
- **One hostname per provider,** answered by `mono-jig-net`'s DNS. Then repo paths look normal (`owner/repo`, or `group/sub/project`), which `gh` and `glab` need to work out the repo from the remote.
- **The CLI clones with broker URLs,** so `origin` is `https://github.broker.internal/acme/api.git`. A system-wide `url.<broker>.insteadOf` also rewrites `https://github.com/`, `git@github.com:` and `ssh://git@github.com/` (and the same for GitLab), catching URLs the agent types itself. There are no SSH keys in the VM, and port 22 to the providers isn't reachable anyway.
- **`gh` and `glab` point at the broker as a self-hosted host:** `GH_HOST=github.broker.internal` with `GH_ENTERPRISE_TOKEN` set to a placeholder, and `GITLAB_HOST=gitlab.broker.internal` with a placeholder token. `gh` then calls `/api/v3/...` and `/api/graphql`, which the broker maps to `api.github.com`. Whether every `gh` command works this way is *unverified* (the open question about `gh` and `glab` against the broker).
- **No credential helper and no prompts:** `credential.helper` is empty and `GIT_TERMINAL_PROMPT=0`. The broker never asks for credentials.
- **TLS:** the broker serves HTTPS with a certificate from a small CA that the VM trusts. The CA carries an X.509 name constraint, so it can sign only `*.broker.internal`, and its key never leaves `mono-jig-net`. That's the broker's own server, not interception of anyone else's traffic.

**Who is calling?** On Lima, every connection on a sandbox's network comes from that sandbox, because the network belongs to it. So the broker needs no session token: the sandbox's identity is the network it arrived on. That holds only if nothing else can join the network: the daemon's socket directory is 0700 (Lima creates `_networks/<name>/` 0755), it serves no QEMU socket on `vz`, and it checks that the peer on the data socket is Lima's host agent for this sandbox (`LOCAL_PEERPID`). Otherwise any process of the user's on the Mac could connect and reach the broker. On sbx (prototype), where the broker listens on `127.0.0.1`, the placeholder tokens and a git `http.extraHeader` carry a random per-session token instead.

**A feature-branch push, step by step:**
1. The agent works on `agent/<user>/fix-timeout`, with the full name as the local branch. The rule is in the sandbox's `AGENTS.md`. **Don't set a push mapping** (`remote.origin.push = refs/heads/*:refs/heads/agent/<user>/*`): git applies it even to an explicit `git push origin <branch>` (checked in git's `push.c`, 2026-11-22), so a branch already named `agent/<user>/fix-timeout` would land on `agent/<user>/agent/<user>/fix-timeout`.
2. `git push -u origin agent/<user>/fix-timeout`. DNS for `github.broker.internal` resolves to the gateway, and the TCP connection lands in `mono-jig-net` without ever leaving the Mac process.
3. `GET /acme/api.git/info/refs?service=git-receive-pack`. The policy checks read access to `acme/api`, and the broker forwards the request to `https://github.com/acme/api.git/...` with the user's token as Basic auth. The ref advertisement comes back.
4. `POST /acme/api.git/git-receive-pack`. The body starts with one command line per ref (`<old> <new> refs/heads/agent/<user>/fix-timeout`) before the pack data. The broker reads those lines and checks each ref against the policy: the user, the push operation, the repo, the ref, and whether it's a create, update or delete. Ref names are checked with git's `check-ref-format` rules before the prefix match. Allowed: it streams the body upstream, streams the answer back, and logs it. Push options and request encodings are handled as in [Broker hardening](#broker-hardening-what-the-review-found).
   - **Force pushes and deletes inside the namespace are allowed,** because rebasing a stack needs them. Tags and every other branch are denied.
5. **A denied push** (`git push origin main`) gets a proper receive-pack status line from the broker, so git prints a clear reason: `! [remote rejected] main -> main (pushes are allowed only under agent/<user>/)`. That's better than a bare HTTP 403.
6. `gh pr create --head agent/<user>/fix-timeout --title "[agent/<user>] Fix timeout"`. This is a GraphQL `createPullRequest` mutation. The broker **parses the GraphQL document** rather than trusting operation names, since `gh api graphql` can send anything. It checks the head branch (and, until 2026-11-24, the title tag), then forwards to `api.github.com/graphql` with the user's token.
7. `gh pr merge` is a REST `PUT .../merge` or a GraphQL `mergePullRequest`, which the broker denies with a message. Fetching, `gh pr view`, `gh run view --log` and other reads pass through.

**GitLab is the same shape:** `glab mr create` is a REST `POST /projects/:id/merge_requests`, with the `Draft:` prefix allowed before the tag. Merging is `PUT .../merge`.

**Edges to handle:**
- **Git LFS** uses a batch API on the same host (`/info/lfs/objects/batch`). GitLab's batch response carries `Authorization: Basic <user:lfs_token>`, a two-hour token with read-write LFS access to the project, and the agent could use it directly against `gitlab.com`. So the broker rewrites batch responses: each `href` points back through the broker, and the token headers are removed. Allow the LFS locks API too.
- **Submodules** on other hosts are rewritten by the same `insteadOf` rules if they're on a managed provider, and fetched anonymously through the filter otherwise.

### Broker hardening: what the review found

*From the design review, 2026-11-22.* Rules every adapter follows. Each has a test against a fake upstream.

- **Deny writes by default.** Reads are allowed; writes are allowed only from a short list per provider, each with its predicates. Unknown routes, GraphQL fields and MCP tools are denied. The ref rule applies to every allowed endpoint that writes a ref (receive-pack, the commits and contents APIs, branch creation, MCP `add_commit` and `add_branch`).
- **Updates check what exists, not only what's sent.** Before an update to a PR or MR, fetch it and require its source branch to be under `agent/<user>/`.
- **Receive-pack:**
  - Remove the `push-options` capability from the ref advertisement, and reject a request that sends push options anyway.
  - Accept `Content-Encoding` identity or gzip only, decompress before parsing, and reject anything else.
  - Validate ref names with `check-ref-format` rules.
- **LFS:** rewrite batch responses so their links go through the broker and carry no token.
- **Token, key and account endpoints** (rotate a token, add an SSH or GPG key, deploy keys, webhooks, user settings) are never on the list.
- **One port, several hostnames:** choose the provider from TLS SNI, require `Host` to equal SNI, and reject absolute-form request URIs.
- **Upstream requests don't follow redirects** (`CheckRedirect` returns `http.ErrUseLastResponse`). Go's client would otherwise re-send bodies on 307 and 308 and forward `Authorization` to subdomains. Redirects go back to the client, which follows them through the broker or the filter.
- **MCP:** see [Hosted MCP servers](#hosted-mcp-servers-filter-tool-calls).

**What we accept:** CI on agent branches. A push to `agent/<user>/` that changes `.gitlab-ci.yml` or a GitHub workflow runs that code in CI with the project's CI variables and unfiltered network. That's the same as any developer's branch; protect sensitive variables to protected branches, which `agent/` branches never are.

### Hosted MCP servers: filter tool calls

A hosted MCP server calls its provider's API from the provider's side, where the broker can't see the requests. GitLab's MCP server is hosted (`gitlab.com/api/v4/mcp`, checked 2026-11-22), and so are Datadog's and Wiz's. So the broker also proxies MCP and applies policy to each tool call: its name and its arguments.
- **Deny unknown tools.** Allow read tools by name; allow writing tools only from a short list, with checks on their arguments. Filter `tools/list` to match, so the agent isn't offered tools it can't use, and reject JSON-RPC batches so a denied call can't hide in one.
- **GitLab:**
  - Deny `accept_merge_request`.
  - In `save_merge_request`, check `title` and `source_branch`; on an update, fetch the MR and check its source branch.
  - `add_commit` and `add_branch` write refs, so the ref rule applies to them.
  - Deny `apply_suggestion`, `save_project`, `fork_repository` and `save_pipeline` unless a use turns up.
- **Datadog and Wiz:** allow the read-only tools by name.

Built for GitLab in `broker/mcp.go` (2026-11-22): each tool call maps onto the HTTP policy's operations, so one `Decide` covers both. This is the second policy layer, beside the HTTP one. The tool name is the operation, and the policy checks its arguments. GitLab is the practice run for Datadog and Wiz.

### AWS

Keep the current pattern:
- `okta-aws-cli` (or AWS SSO) refreshes credentials on the Mac, in the CLI's own AWS files.
- Profiles assume MCP-specific, read-only roles.
- ~~The broker signs requests itself with the AWS SDK for Go's signer.~~ *Superseded 2026-11-22 (built, `prototype-plan.md` step 7):* the broker runs AWS's own `mcp-proxy-for-aws` on the Mac, one per sandbox, and serves it as MCP over plain HTTP at `aws.broker.internal/mcp` for `mcpcli` and `curl`. Keeping the proxy keeps its per-call profile switching (`aws_profile`), which work needs for its many accounts.

### Datadog

`mono-jig login` runs the OAuth login on the Mac, and the broker holds the token. Otherwise, the broker holds a read-only API key plus application key.

### Wiz

- The Wiz MCP server supports per-user OAuth with dynamic client registration. This keeps each person's identity and permissions, so prefer it.
- It also supports a shared service account (OAuth client credentials), but then every user acts as one identity.
- An admin must first enable the MCP server under **Settings > Tenant > AI Features**.

### Egress filtering: a separate policy from the broker's

*Added 2026-11-22; revised twice the same day.* The broker's policy covers **only the providers we manage**. Egress filtering by domain is a separate, simpler policy, with off-the-shelf code behind it:
- **the runtime's own policy where it has one** (sbx, OpenShell);
- on Lima, **an embedded explicit proxy** with a hostname ACL: our own, in `filter/` (about 400 lines). Stripe's smokescreen was the first choice, but it always resolves the destination itself and checks the address it gets, including when it dials an upstream proxy, so it can't chain to a proxy with a private address or run where external names don't resolve (both true in sbx and behind corporate proxies; decided 2026-11-22). It runs inside `mono-jig-net`, the same process as the network stack and the broker, but as its own component with its own ACL (see [Packaging: one process per sandbox](#packaging-one-process-per-sandbox)).

**The same harness serves as a general dev sandbox.** The mono-jig is one configuration of the allowlist. A broader one adds documentation sites for common tools and services, and the hosts that serve development and production tools (package registries, release downloads, container registries). It becomes the everyday sandbox for developers.

**The allowlist comes in layers:**
- **Base:** what every sandbox needs (the agent's own hosts, the OS package mirror, the broker).
- **Profile:** `mono-jig` (narrow: what tests need and a short documentation list) or `dev` (broad documentation and tool-download lists).
- **Org:** our additions, shipped with the CLI.
- **User:** personal additions.

Lists are grouped by tool or service, such as "Go toolchain", "Node", "Terraform", "Docker Hub" or "AWS docs", so a user turns on a group instead of hunting for hostnames. The CLI translates the layers into whichever filter the runtime uses: `sbx policy` rules, OpenShell policy, or the embedded proxy's ACL file.

**Provider hosts can be allowed directly.** The sandbox holds no credentials of its own, so direct access to `github.com`, `gitlab.com` or S3 is anonymous: public clones, Go modules, release downloads. Authenticated work goes to the broker's own endpoints, configured explicitly (git `insteadOf`, the `glab` or `gh` host, MCP URLs; see [How the agent uses git and gh](#how-the-agent-uses-git-and-gh)). The broker never intercepts other hosts' TLS. The sandbox trusts one extra CA, name-constrained to `*.broker.internal`, for the broker's own HTTPS. A user who pastes a token into the sandbox can use it directly. That's deliberate and out of scope (T3 in `enforcement.md`).

**What we accept, because the harness isn't meant to be air-tight:**
- **Exfiltration.** Every allowed host that accepts writes (an issue tracker, a paste site) is a channel for code or data, and a broad `dev` list can't close them all. A domain filter also has the usual leaks: shared CDN addresses, domain fronting, DNS queries. The filter's logs show what the agent reached.
- **Supply chain.** More download hosts mean more places untrusted code can come from.

The `mono-jig` profile stays narrow because it runs unattended on alerts. A user who picks `dev` trades some of this for convenience.

**What we do close, because it's cheap and accidents happen:**
- **The Mac's other local services.** Otherwise the agent reaches dev servers, MCP gateways, or a Docker or Podman API listening on TCP. On Lima, the netstack closes this by construction: the broker and the proxy listen on the VM's virtual network, not on the Mac, and the netstack forwards nothing ([why](#why-the-broker-and-the-filter-listen-on-the-netstack)). But **the filter dials from the Mac**, so it must refuse the Mac's own addresses itself:
  - take host names only, never IP literals, and match them against the ACL;
  - when dialing directly, resolve the name itself, drop loopback, private, link-local, CGNAT, ULA and other non-public addresses, and dial the address it checked (so DNS can't change the answer in between);
  - when chaining, send `CONNECT <name>` to the upstream proxy and let it resolve;
  - deny a request that names an upstream proxy (`X-Upstream-Https-Proxy`);
  - refuse to start without an ACL;
  - allow ports 80 and 443 only.

  On sbx, "allow the Mac" means the broker's port only.
- **The IDE as a back channel.** Extensions on the Mac side make network requests outside any filter, and a sandbox extension may be able to call them (*unverified* how freely). The separate `--user-data-dir` instance, with a minimal set of extensions, keeps this small.

### Curl-friendly MCP

MCP tool calls put the whole response into the agent's context. For large results such as CloudWatch logs, it's better to save the response to a file and process it repeatedly. If the broker's MCP gateway also has a plain HTTP endpoint, as the sbx MCP gateway does, the agent can `curl` it into a file. This works the same in both tools.

### Starting an investigation

The CLI's commands are in [Install and uninstall](#install-and-uninstall): `mono-jig login` runs the provider logins on the Mac, then the agent signs in inside the VM. An investigation is one more command on top, `mono-jig run <alert link or Wiz issue>`, which opens the sandbox and starts the agent with the link as its prompt: Claude Code with `claude -p`, or kiro-cli with its non-interactive flags (`--no-interactive`, `--trust-all-tools`, *unverified*). The same command covers headless runs.

## Packaging: one process per sandbox

*Since 2026-11-22 the one process is the `mono-jig` supervisor, which holds the microsandbox VM and serves the broker (`microsandbox.md`). The Lima form below is kept as the record.*

*Rewritten 2026-11-22. The earlier plan, a broker container stack under Podman with `docker/mcp-gateway`, is dropped: on a Mac without Docker Desktop a container means another VM (Podman machine), and whether a Lima VM could reach a port Podman forwards was unknown. Nothing runs under `launchd` either.*

**Everything that runs continuously for a sandbox is packaged with it.** On the Mac that's two processes per sandbox, both started and stopped with the VM: Lima's host agent, which runs the VM, and ours, `mono-jig-net`.

```
Mac
  mono-jig CLI ──starts, checks, stops──▶ the two processes below
  mono-jig-net <sandbox>      (ours; one per sandbox; runs only while the VM runs)
    gvisor-tap-vsock netstack ── the VM's only NIC (a Lima user-v2 network)
      ├─ gateway:53    DNS: answers only proxy.internal and *.broker.internal
      ├─ gateway:3128  domain filter: our explicit proxy, our generated ACL
      ├─ gateway:443   broker: git, REST and GraphQL, hosted-MCP proxy, AWS MCP; policy; logs
      └─ anything else: reset (no forwarders, no NAT to the Mac)
    reads the user's credentials (Keychain, AWS SSO cache); dials out only for the filter and the broker
  Lima host agent <sandbox>       (stock; vsock for SSH and the guest agent)
    └─ VM: Ubuntu cloud image + sandbox/provision.sh
         sshd (hardened), the agent, git, glab, toolchains
         dockerd ── test and dev containers (their traffic uses the same NIC)
  Editor: a separate VS Code instance with our settings, SSH over vsock
```

### The pieces

| Piece | Ours or stock | Role |
|---|---|---|
| `mono-jig` CLI | Ours | `setup`, `login`, `open`, `stop`, `allow`, `denied`, `logout`, `uninstall`. Generates the ACL, the policy settings (user name, allowed repos), the SSH config and the editor settings. Starts `mono-jig-net`, then the VM, and cleans up on delete. |
| `mono-jig-net` | Ours, built on gvisor-tap-vsock | The VM's network, the domain filter and the broker, in one Go binary with separate packages for each |
| VM image | Ours (`sandbox/provision.sh`) on a stock Ubuntu cloud image | Everything the agent and the tests need. `HTTPS_PROXY` points at `proxy.internal`; git `insteadOf`, `gh`, `glab` and the MCP client point at the `*.broker.internal` hosts. |
| Lima | Stock | Boots and runs the VM: disks, cloud-init, vsock SSH, the guest agent |
| Editor settings | Ours (configuration) | Close the credential bridges ([the IDE connection](#the-new-trust-boundary-the-ide-connection)) |
| Credentials | The user's own (Keychain, AWS SSO or Okta) | Read only by `mono-jig-net` |
| Allowlist layers | Ours (data) | Base, profile, org, user, grouped by tool |

So we write four things: the CLI, the provisioning script, the network daemon with the filter around it, and the broker. The broker is most of the work.

### Why the broker and the filter listen on the netstack

They're served on the VM network's gateway address, inside the process, the way gvisor-tap-vsock already serves DNS and DHCP. Nothing listens on the Mac for the VM.
- **No host ports,** so no port collisions, no session token needed to keep other Mac processes out of the broker (given the socket checks in [Who is calling?](#how-the-agent-uses-git-and-gh)), and the agent can't reach the Mac's other local services.
- **Build the stack ourselves, without forwarders.** gvisor-tap-vsock v0.9.0's `virtualnetwork.New` always installs TCP, UDP and ICMP forwarders that dial whatever the guest asks for (`services.go`), and doesn't expose the stack to remove them. Its DNS server passes every name outside its zones to the Mac's resolver. Lima's `gvproxy.go` also adds a NAT entry that maps the gateway to the Mac's `127.0.0.1`. So `mono-jig-net` uses gvisor-tap-vsock's tap switch and transport, but builds the gvisor stack itself with no forwarders and no NAT table, and runs its own DNS handler that returns NXDOMAIN for every other name. Only the filter and the broker open outbound connections, each by its own rules. If assembling it ourselves proves too awkward, vendor a patched copy, or use gvisor-tap-vsock#750's `DialFilter` once it merges.
- **Tested, not assumed:** a fake guest checks that a SYN to any other address or port gets a reset, UDP and ICMP go nowhere, and `example.com` gets NXDOMAIN.
- **It fails closed by construction.** If the process dies, the VM has no network at all.

### Lifecycle

- **Now:** `mono-jig open` starts `mono-jig-net`, writes its PID into Lima's `usernet_<name>.pid`, checks it, then runs `limactl start`. On `mono-jig stop`, Lima stops the VM and kills the pidfile's process, which is ours.
- **The pidfile check alone isn't race-free.** Lima only checks that the pidfile's process is alive, then starts its own unfiltered daemon if not. That leaves gaps: `mono-jig-net` dying between the check and the boot; a start that skips the CLI (`limactl start` typed by hand, a GUI); or a user's `_config/default.yaml` or `override.yaml` merging in `vzNAT` or another network, which Lima adds as an extra, unfiltered NIC. So the CLI also:
  - **uses its own `LIMA_HOME`,** so the user's Lima defaults and overrides never apply, and the harness's VMs don't show up in the user's `limactl list`;
  - **defines the network only while `mono-jig-net` runs:** the CLI writes the `networks.yaml` entry just before boot and removes it on stop, so a stray start fails with "network not found";
  - **checks after boot, from the Mac:** `limactl list --json` shows exactly one network for the VM, and the VM's MAC address got its DHCP lease from our daemon. If either check fails, it kills the VM.
- **VM settings the template fixes:**
  - `propagateProxyEnv: false`, or Lima copies the Mac's proxy variables into the guest;
  - a `portForwards` rule ignoring every port ([the IDE connection](#the-new-trust-boundary-the-ide-connection));
  - DNS pointed at the gateway: `hostResolver.enabled: false` and `dns: [<gateway>]` (*unverified* which of these apply with `user-v2`).
- **When lima#5813 (`daemon:`) merges:** Lima itself starts and stops our binary with the network, so the VM and its network are one Lima unit.
- **When lima#5818 (`external: true`) merges:** the CLI keeps ownership, and Lima refuses to boot if the daemon is missing. The pidfile workaround goes away either way.
- **State:** one directory per sandbox holds the ACL, the policy settings, the logs and the sockets. Deleting the sandbox deletes it, and the CLI also removes the network's entry in `~/.lima/_config/networks.yaml`.

### Credentials and logins

- `mono-jig-net` runs as the user on the Mac, so it can use **macOS Keychain** for GitLab, GitHub, Datadog and Wiz tokens. That's better than the old plan's unencrypted volume.
- **AWS:** `AWS_CONFIG_FILE` and `AWS_SHARED_CREDENTIALS_FILE` point into the CLI's own directory, so the real `~/.aws` is never read or written. `mono-jig login` runs the SSO or `okta-aws-cli` device login.
- **GitLab and GitHub:** a device-code login, or a token the user pastes into `mono-jig login`, stored in Keychain.
- **Datadog and Wiz:** browser OAuth with a redirect to `127.0.0.1`, handled by the CLI during `login`, not by the long-running process. Use a random port and PKCE, so a process that takes the port can't use the code. The VM can't take it anyway, since Lima forwards no guest ports.
- **Hosted MCP servers** (GitLab, Datadog, Wiz, AWS) are remote, so the broker proxies them itself and filters their tool calls ([Hosted MCP servers](#hosted-mcp-servers-filter-tool-calls)). No MCP gateway containers.

### On sbx (prototype only)

sbx owns the VM's network, so `mono-jig-net` runs without its netstack, and sbx's own policy does the domain filtering. The broker listens on `127.0.0.1` with a random per-session token. A script starts it with the sandbox and stops it after (the CLI isn't built until later). This mode exists only until the Lima backend works.

**Reaching the broker from sbx is the hard part, and none of it is tested:**
- sbx's VM resolves no external names, `host.docker.internal` isn't in `NO_PROXY` (only `gateway.docker.internal` is), and every other request goes through sbx's proxy, which intercepts TLS. So `*.broker.internal` on port 443 won't work as on Lima.
- **The plan:** one port per provider on the Mac, so each still has its own host for `gh` and `glab` (`host.docker.internal:<port>`), and sbx's policy allows `localhost:<port>` for each. The broker serves plain HTTP there, since sbx's proxy carries it and the hop stays on the Mac. git's `insteadOf`, `GITLAB_HOST` (with `api_protocol: http`) and the MCP URL take the ports.
- **To check first:** that requests to `host.docker.internal:<port>` arrive through sbx's proxy with their body and headers intact; whether `gh` accepts a host with a port over plain HTTP (probably not, so GitHub may wait for Lima); and whether git's `http.extraHeader` survives sbx's proxy.

### Security points

- **The credentials sit in the process that parses the guest's packets.** A bug in the netstack could expose the broker's tokens. gvisor is memory-safe Go, and sbx makes the same trade. If we want separation later, `mono-jig-net` can split into a netstack child and a broker child under one parent, still one unit.
- **Keep the packages apart inside the binary:** the netstack, the filter and the broker each test on their own, against a fake guest and fake upstreams. All of that runs in the agent's sandbox, without Lima.
- **Only `mono-jig-net`'s sockets reach its network:** the socket directory is 0700, and the data socket checks its peer ([Who is calling?](#how-the-agent-uses-git-and-gh)).

### Install and uninstall

Ship a small Homebrew tap that installs `mono-jig` and `mono-jig-net`, with Lima as a dependency.

```
mono-jig setup      # build or pull the VM image, write the Lima template and the allowlist layers
mono-jig login      # GitLab or GitHub, AWS, Datadog, Wiz; then the agent's own sign-in inside the VM
mono-jig open       # start mono-jig-net and the VM, then the editor over SSH
mono-jig stop       # stop the VM; mono-jig-net stops with it
mono-jig logout     # remove tokens from Keychain and revoke where the provider supports it
mono-jig uninstall  # delete the VMs, their networks and the CLI's directory, then: brew uninstall mono-jig
```

For headless agents in CI or Kubernetes, the broker package runs as a plain sidecar with workload credentials, without the netstack.

## Building it in each tool

| | **sbx** | **OpenShell** |
|---|---|---|
| Kiro | Built-in agent and kit | Custom image and policy we write |
| Packaging | `sbx template` / `kit`, declarative `sbx env` (experimental) | Image plus policy YAML; `sandbox template` |
| Docker for tests | **Yes**: a private daemon per sandbox | **Unknown and likely a problem**: we'd run as a non-root user with no sudo inside a locked-down VM. Must be tested. |
| Network control | Allow/deny by hostname, approval queue (`sbx policy approval`) | Rules per program and per host, REST path and GraphQL operation rules, a prover, an approval inbox (`openshell rule`) |
| Credentials | Proxy-injected `sbx secret`; OAuth MCP handled on the Mac | Proxy-injected provider credentials; no MCP gateway |
| MCP | Gateway with stdio and OAuth servers on the Mac | Broker on the Mac, reached via `host.openshell.internal`; can allow specific MCP tool names |
| Accounts | **Docker account for everyone** | None (Apache-2.0) |
| Setup on Macs | Smooth | Needs a setup script for the problems we hit in the 2026-11-21 trial (those notes weren't kept here) |
| Headless and fleets | PAT login; Docker's paid cloud sandboxes and schedules | Designed for it: Kubernetes driver, OIDC, central gateway, but alpha |

### sbx build outline

1. Build a template image from the Kiro kit, adding the project toolchains and sshd (or from a repo's `devcontainer.json` via `devcontainer build`).
2. Point the agent's MCP client at the broker's MCP endpoint. Don't use sbx's own MCP gateway or `sbx mcp auth` for the harness: Lima has neither, so the prototype must not depend on them.
3. Don't give sbx a GitHub secret. Route `git` and `gh` through the broker instead.
4. Add network policy with `sbx policy allow` for package registries and Kiro's endpoints.
5. Write the `mono-jig` wrapper around `sbx run`, including the SSH config, the pinned host key and the editor launch (VS Code first, then Kiro).

### OpenShell build outline

1. Build an image from `ghcr.io/nvidia/openshell-community/sandboxes/base`, adding kiro-cli and the toolchains. Fix or replace the policy bundled in that image: OpenShell 0.1.9 rejects its `tls: terminate` fields, and the sandbox then hangs (seen in the 2026-11-21 trial).
2. Write a policy scoped per program: kiro-cli to Kiro's endpoints, package managers to their registries, and everything to the broker on `host.openshell.internal`. Use `openshell rule get` to discover what's missing.
3. Optionally use providers for proxy-injected Datadog and Wiz keys, if we're not using OAuth through the broker.
4. Package a setup script for the Mac:
   - install the gateway and the VM driver's dependencies (`libkrun`, `e2fsprogs`) and select the `vm` driver,
   - or, for the Docker driver, use bridge networking with `host.docker.internal`.
5. **First, check whether `dockerd` can run inside an OpenShell VM sandbox.** Our tests need it. *(Answered 2026-11-27: it can't, on any driver; [openshell.md](openshell.md).)*

## The sandbox backend interface

*Not built as of 2026-11-24: the supervisor uses microsandbox directly. [library-split.md](library-split.md) proposes it as one of the seams between a common library and the personal and work tracks.*

*Added 2026-11-22.* The CLI treats the sandbox tool as a backend. sbx comes first, for the personal prototype; **Lima is the backend that doesn't need a Docker account** ([Leaving sbx](#leaving-sbx-lima)); OpenShell is an adapter we can try later. A backend must:
- create a sandbox from our image,
- give SSH access to it, through a `ProxyCommand` or a port on `127.0.0.1`,
- **filter egress by domain** from our layered allowlist: with its own policy (sbx, OpenShell), or by letting `mono-jig-net` be the VM's only network ([Enforcing the route](#enforcing-the-route)),
- give the sandbox a route to the broker: `mono-jig-net`'s gateway address on Lima, the Mac's address on sbx,
- run Docker inside the sandbox,
- stop it and remove it with all its state.

It should install from a script, need no vendor account, and allow commercial use. Booting directly from an OCI image is a bonus, because the output of `devcontainer build` then runs as is.

Unknown hosts: sbx has an approval queue, which the CLI shows the user. Where the runtime has none, unknown hosts are denied and logged, and the CLI lists them and adds hosts to the user's layer.

**Don't mount host files read-write.** The agent could write `.git/hooks` or `.vscode/settings.json` in a mounted repo, and the user's own tools on the Mac would later run them outside the sandbox. Clone inside the VM, as sbx's clone mode does, or mount read-only.

## Leaving sbx: Lima

*Superseded 2026-11-22: microsandbox replaced Lima as the VM layer (`microsandbox.md`), and the Lima code (`lima/`, `net/`, `mono-jig-net lima`, `checks/lima-net.sh`) was removed. It is in git at `695143a`. What follows is kept as the record.*

*Added 2026-11-22. The design must not depend on sbx, because we can't assume Docker accounts at work. This section is the path off it. Not yet tested. The networking details were checked against Lima v2.3.0's source the same day; the rest is from what I already knew and is unverified until the spike.*

**The runtime is Lima.** It's Apache-2.0 and a CNCF project, installs with `brew`, and Colima and Rancher Desktop are built on it, so it may already be acceptable at work. It's SSH-native, and Docker runs inside its VMs.

### What sbx does for us, and what replaces it

| sbx feature we use | Replacement on Lima |
|---|---|
| A microVM per sandbox | A Lima VM per sandbox, using Apple's virtualization framework (`vmType: vz`) |
| Our image as the sandbox | **One provisioning script, two packagings.** `sandbox/provision.sh` installs everything (sshd settings, git, Docker, the agent, toolchains). The sbx image's Dockerfile runs it, and so does the Lima template's `provision` step on a stock Ubuntu cloud image. Later, `devcontainer build` output can feed both. |
| Docker inside | `dockerd` installed by the provisioning script, as in Lima's Docker template |
| SSH for the editor | Built in. `limactl show-ssh` (or the VM's `ssh.config`) gives the host entry, which the CLI copies into its own SSH config with our settings. Our sshd settings keep Lima's own user and key working. |
| Domain allowlist and approvals | Our filter (`filter/`), in `mono-jig-net` and served on the VM's network, with a hostname ACL generated from our layers. Unknown hosts are denied and logged. `mono-jig denied` lists recent denials, and `mono-jig allow <host>` adds one to the user layer. That's simpler than sbx's queue and good enough. |
| Forcing traffic through the filter | sbx: its user-mode network stack does it ([How sbx enforces egress](#how-sbx-enforces-egress-observed-2026-11-22)). Lima: `mono-jig-net` serving a `user-v2` network, the VM's only NIC ([Option C on Lima](#option-c-on-lima-the-answers-researched-2026-11-22)). |
| Credential injection, MCP gateway | Not needed from the runtime: the broker does both |
| Clone mode (no host mounts) | The CLI clones repos inside the VM, through the broker. Lima's default host mounts are turned off (`mounts: []`). |
| Clean removal | `limactl delete <name>`, plus the CLI's per-sandbox directory and the network's `networks.yaml` entry |

### How sbx enforces egress (observed 2026-11-22)

Checked from inside this repo's own sbx sandbox (sbx version not recorded):

| Check | Result | What it means |
|---|---|---|
| Firewall rules inside the VM | Only Docker's own `iptables` rules. Nothing about egress. | The VM doesn't enforce anything itself. |
| Default route | `eth0` via `172.17.0.0`, which is also the only DNS server | One virtual NIC, and its far end is sbx |
| DNS for `example.com` | Doesn't resolve | External names aren't served at all, so clients must use the proxy. That also closes DNS exfiltration. |
| TCP to `192.0.2.1:9` (a reserved, unroutable test address) | **The handshake completes** | Nothing real answered. The far end is a **user-mode network stack** in sbx's host process: it accepts every connection itself, then decides. |
| TLS to `1.1.1.1` without the proxy | Accepted, then closed | Policy is applied at that stack. |
| Proxy settings and CA | `HTTPS_PROXY=http://gateway.docker.internal:3128`, plus `/usr/local/share/ca-certificates/proxy-ca.crt` | The explicit proxy and the TLS interception for injected credentials are in the same host process. |

**So sbx's filtering is attached to the VM, not to the Mac's network configuration.** The VM's NIC is backed by sbx's daemon. Every packet leaving the VM ends up in that process, which applies the allowlist, injects credentials and logs (`sbx policy log`). Nothing points to `pf` rules, bridge interfaces or routing changes on the Mac (*unverified* from the Mac side; checks below).

**What goes away with the VM:** the data path. Deleting the VM removes its NIC and its connection to the daemon, with no host network state to clean up. **What doesn't:** policy rules, which are daemon state. Global rules persist across sandboxes by design. Whether `sbx rm` removes per-sandbox (`--sandbox`) rules is *unverified*. The daemon itself is shared and keeps running.

To confirm on the Mac, before, during and after a sandbox's life:

```bash
ifconfig | grep -E '^(bridge|vmenet|feth|utun)'   # sbx shouldn't add bridge or vmnet interfaces
sudo pfctl -s rules; sudo pfctl -s Anchors         # sbx shouldn't add pf rules or anchors
sbx policy ls                                      # after `sbx rm`: are --sandbox rules gone?
```

### What else sbx provides (for perspective)

*Added 2026-11-22, from what sbx shows inside this repo's sandbox and its docs, not from its code.* Our design replaces sbx's VM, image and network and credential system. Mounts aren't a difference: sbx's workspace is a read-write virtiofs mount in direct mode, or a git clone in clone mode (kits use overlays to layer images), and Lima does virtiofs mounts too, though we mount nothing. What else sbx gives us:

| # | sbx provides | Our design | Gap? |
|---|---|---|---|
| 1 | **Agent kits:** the agent binary, session volumes and hooks, and **the agent's model credential injected by the proxy**, so not even the model token is in the VM | The agent signs in inside the VM, so its model token lives there. `enforcement.md` assumes the broker adds the model credential instead. | **Yes, the biggest.** The broker could proxy the model API too, but Kiro's sign-in may not allow it. |
| 2 | **TLS interception with its own CA:** credentials and rules on any host, with no client configuration | Explicit broker endpoints; the domain filter sees host names only | Deliberate. Every tool must be pointed at the broker. |
| 3 | **Approvals with options, and a policy log** | `mono-jig denied` and `allow`, no live prompt | Small |
| 4 | **Central org governance,** updated from Docker's side without reinstalling | An org layer shipped in the CLI, editable by the user | Deliberate ("not air-tight") |
| 5 | **Host-side MCP servers:** stdio servers run on the Mac (this repo's `simsrv`), OAuth for remote ones, one gateway | Only hosted MCP servers, through the broker | For a general dev sandbox, not the mono-jig |
| 6 | **Git plumbing:** host remotes copied in without credentials; in clone mode a git daemon for `git fetch sandbox-<name>`, and the host repo mounted read-only | Work leaves only by pushing through the broker to `agent/<user>/` branches | For everyday development |
| 7 | **A supervising daemon:** many sandboxes, agent sessions restarted after a crash, background processes, declarative `sbx env`, `sbx ports` | We turn Lima's automatic port forwarding off; nothing restarts the agent | For everyday development |
| 8 | **Fast start from a prebuilt OCI image** | `provision.sh` at first boot takes minutes | Yes, once it's used daily: build and ship a disk image ahead of time |
| 9 | **Networking robustness:** IPv4 and IPv6 detection with happy-eyeballs, DNS through the host, VPN and corporate proxy handling | Not designed yet | **Yes:** at work the Mac likely sits behind a corporate proxy or SASE client, so the filter and the broker must chain to it |
| 10 | **Maintenance:** signed releases, auto-updates, vendor support, Windows and Linux hosts | We maintain `mono-jig-net` and keep Lima pinned and current | The cost of owning it |

**In short, sbx is three products:** a VM runtime (Lima replaces it), a policy and credential plane (we rebuild a narrower, more transparent one), and an agent product (kits, the model credential, the MCP gateway, git sync, session supervision), where we have little. The gaps that matter now are 1 and 9; 6, 7 and 8 matter once the harness becomes the everyday dev sandbox.

### Enforcing the route

The VM must reach only the domain filter and the broker. **The goal is sbx's packaging:** enforcement attached to the VM and removed with it, with no host-wide network configuration left behind, and the agent allowed to be root inside the VM.

| | Where enforcement lives | Removed with the VM? | Root on the Mac | Strength |
|---|---|---|---|---|
| **sbx** (for comparison) | User-mode network stack in the host daemon, the VM's only NIC | Yes, the data path. Policy rules are daemon state. | No | The agent can be root in the VM |
| **C: our own user-mode network** (preferred) | A small per-VM daemon built on **gvisor-tap-vsock** (the user-mode stack behind Podman machine and Lima's own networks). It serves a Lima `user-v2` network, which on `vz` becomes the VM's **only** NIC. The domain filter and the broker are served inside it, and nothing else is reachable. This is `mono-jig-net`. | **Yes:** it starts and stops with the VM, as in sbx | **No** | Same shape as sbx |
| **Tart with Softnet** | A per-VM Softnet process, with rules passed as flags (`--net-softnet-allow`) | **Yes** | Yes, once (it's installed setuid) | Same packaging as sbx, IP-level only. The license still needs checking. |
| **B: `nftables` inside the VM** | The VM's own kernel; the agent is non-root and Docker is rootless | **Yes,** completely | No | Weaker: relies on Linux privilege separation, and rootless Docker breaks some tests (privileged containers, host networking) |
| **A: `pf` on the Mac** (last resort) | **The Mac's shared `pf` ruleset**: an anchor keyed on Lima's vmnet bridge and the VM's DHCP address | **No.** The CLI must remove it, and a crash leaves stale rules. It can also conflict with VPN clients or security agents that use `pf`. | Yes | Strong, but it's scattered host network configuration that outlives the VM |

**Prefer C.** The spike questions are answered below: it works on Lima today, without root, through a workaround that two upstream pull requests would replace. **Tart with Softnet** stays the off-the-shelf fallback, **B** the fallback that works on any runtime, and **A** is last.

### Option C on Lima: the answers (researched 2026-11-22)

*From Lima v2.3.0's source and docs, and open Lima and gvisor-tap-vsock issues and pull requests. Not yet run on a Mac.*

**1. Can Lima attach a VM to our own user-mode network with its default network off? Yes, if our network is a `user-v2` network, not a `socket` network.**
- **A `socket` network doesn't work for this.** On `vz`, Lima always gives the VM `eth0` on a user-mode network, and adds `socket:` networks (and `vzNAT`) as **extra** NICs. With no `user-v2` network configured, `eth0` is Lima's built-in gvisor-tap-vsock stack inside its host agent, which dials anything (`attachNetwork` in `pkg/driver/vz/vm_darwin.go`). There's no setting to turn it off. `networkOpts.hostLoopback: false` (new in v2.3) only blocks the Mac's loopback.
- **A `user-v2` network replaces `eth0`.** If the instance lists a `user-v2` network, `eth0` is attached to that network's daemon instead, and Lima starts no built-in stack (the "Enabling user-v2 network will disable the default user-mode network" note in Lima's docs). Lima talks to the daemon through three Unix sockets under `$LIMA_HOME/_networks/<name>/`: a data socket for `vz` (an fd-passing socket), one for QEMU, and an HTTP endpoint socket. Lima calls `/services/dhcp/leases` and the port-forwarding services on the endpoint socket.
- **The daemon can be ours.** Others already run their own gvisor-tap-vsock daemon on a `user-v2` network for the same purpose: a guest with one NIC, where every flow is decided on the host (Lima issues #5464, #5812 and #5820, opened by people sandboxing agents; #5820 says they chose this over Docker Sandboxes because sbx is closed source).
- **The catch today: it fails open.** If the daemon's pidfile (`usernet_<name>.pid`) has no live process when the VM starts, Lima silently starts its own unfiltered `limactl usernet` in its place, and the VM boots fine on an open network (#5464, reproduced on Lima 2.2.0). The workaround is to write our daemon's PID into that pidfile and have the CLI check it before every `open`. That check alone has gaps, so the CLI also uses its own `LIMA_HOME`, defines the network only while our daemon runs, and checks the VM's network after boot ([Lifecycle](#lifecycle)).
- **Two open pull requests fix it:**
  - **lima#5818** adds `external: true` for `user-v2` networks. Lima then never starts a daemon for the network, and `limactl start` **fails** unless the endpoint socket answers. That's the fail-closed behaviour we want, with the daemon run by our CLI.
  - **lima#5813** adds a `daemon:` path that Lima starts and stops for the network.
  - Both are on the agenda of Lima's monthly meeting (discussion #5819), with maintainer approval of the `external: true` name. Neither is merged yet.
- **SSH doesn't need the network.** `ssh.overVsock` defaults to true for `vz` with Linux guests: the host agent proxies SSH to the guest's vsock port 22, which needs systemd 256 or later in the guest (Ubuntu 24.04 ships 255, so use a newer image). Otherwise Lima falls back to a port forward through the network daemon's endpoint socket. `mono-jig-net` provides that one forward and refuses all others: from the Mac's loopback to a guest's port 22 only (built 2026-11-22). Port forwards from the guest agent also run over vsock.

**2. Does option C need root anywhere? No.**
- `vz` VMs, `user-v2` networks and the gvisor-tap-vsock daemon all run as the user. Only `socket_vmnet` (Lima's shared and bridged networks) needs root and a sudoers file, and we don't use it.

**What we build: `mono-jig-net`.** gvisor-tap-vsock v0.9.0 (the version Lima v2.3.0 uses) has **no outbound filter**: its TCP forwarder dials whatever the guest asks for. So the daemon is our own Go program. It uses Lima's `pkg/networks/usernet/gvproxy.go` (about 300 lines) as the reference for the sockets, **but not for its forwarders or its NAT table**, which open the network. It:
- serves the sockets Lima expects (on `vz`, the data socket and the endpoint socket; no QEMU socket);
- forwards nothing for the guest. gvisor-tap-vsock's `virtualnetwork.New` installs TCP, UDP and ICMP forwarders that can't be removed, so we build the gvisor stack ourselves on its tap switch ([why](#why-the-broker-and-the-filter-listen-on-the-netstack)). The domain filter and the broker are served on the gateway address inside the process;
- answers DNS for `proxy.internal` and `*.broker.internal` only, with its own handler. gvisor-tap-vsock's DNS server passes every other name to the Mac's resolver (`dns.go`, checked 2026-11-22), so it isn't used. Clients reach everything else through `HTTPS_PROXY`, which resolves names on the Mac. That also closes DNS exfiltration, as sbx does.

Upstream filtering is coming too, but not soon enough to wait for: gvisor-tap-vsock#687 (a `blockAllOutbound` switch and a domain allowlist with SNI checks, built on #750) and lima#4326 (egress policy for `user-v2`, open since 2025-11).

**What's left on the Mac after `limactl delete`:** no network devices or `pf` rules. Two things the CLI removes itself: the network's entry in `~/.lima/_config/networks.yaml` (one network per sandbox, so one daemon per sandbox), and its socket directory. Lima stops a `user-v2` daemon it believes it owns when the VM stops (it kills the PID in the pidfile), which with the workaround is our daemon. That fits the lifecycle.

**What the spike still has to check on a Mac:**
1. With our daemon (start with plain `limactl usernet` run by hand, to see the plumbing, then `mono-jig-net`), does the VM boot with exactly one NIC, reach the filter and the broker, and fail to reach anything else by IP or by name? A SYN to `1.1.1.1:443` and to the gateway's other ports must get a reset.
2. Does SSH work over vsock with a systemd 256+ guest, with no port forward on the daemon?
3. With the daemon stopped, does `mono-jig open` refuse? With the network entry removed, does a hand-typed `limactl start` fail? With lima#5818 built from its branch, does `limactl start` fail?
4. Does the post-boot check catch an extra NIC, added through an override file in our `LIMA_HOME`?
5. Do guest ports stay unforwarded with the ignore-all `portForwards` rule?
6. After delete: `ifconfig`, `pfctl`, `networks.yaml` and `$LIMA_HOME/_networks/` are back as they were.
7. The same checks with `vmType: krunkit` ([libkrun](#libkrun-a-vm-inside-our-own-process-and-a-path-to-the-cloud)).

### What changes elsewhere

- The sandbox gets `HTTPS_PROXY=http://proxy.internal:3128`. Tools that ignore it fail closed. That's safe, and the denial log shows them. Later, the filter could also catch port 443 transparently and check the TLS SNI, for such tools.
- **Provisioning goes through the filter too,** so `provision.sh` sets the proxy for everything that runs at boot or later:
  - apt (`Acquire::http::Proxy` and `Acquire::https::Proxy`);
  - `dockerd`, through a systemd drop-in, for image pulls;
  - containers, through Docker's client `proxies` setting, plus the broker's CA in images that need the broker;
  - the base allowlist layer: the Ubuntu mirrors, Docker's apt repository, and the hosts that serve `glab`, `gh` and Claude Code.
- **The filter needs an upstream of its own sometimes:** in this sandbox, which has no direct egress, it chains to sbx's proxy; at work, to the corporate proxy. Our filter takes an upstream proxy (`-upstream`), so it's one mechanism for both. Tested 2026-11-22 here, chained to sbx's proxy: curl, `go mod download`, `pip download`, `npm view`, `git ls-remote` and a Docker Hub pull done by hand with curl (token, manifest, blob) all work, and denials are logged.
- The broker's code doesn't change between runtimes. Only how it's served does: on the netstack on Lima, on `127.0.0.1` on sbx.

## VM runtimes beyond sbx and OpenShell

*Added 2026-11-22, from what I already knew (to mid-2026), not researched today, so everything in this section is unverified.* Once the broker holds the credentials, the runtime only needs the [backend interface](#the-sandbox-backend-interface). Lima is the chosen path ([Leaving sbx](#leaving-sbx-lima)); the rest are alternatives.

| Runtime | Strengths | Gaps |
|---|---|---|
| **Lima** (CNCF, Apache-2.0; Colima is built on it) | Mature, scriptable YAML templates, installs with `brew`. Uses Apple's virtualization framework with virtiofs. **SSH is how it does everything:** `limactl show-ssh` writes a config file, and VS Code Remote-SSH into Lima VMs is common. Its Docker template shows Docker runs inside. | No egress filtering of its own, but a `user-v2` network can be served by our own daemon, which becomes the VM's only NIC (option C, checked 2026-11-22). Boots its own images, not OCI images. |
| **Apple `container`** (open source, 2025) | **Each container is its own lightweight VM and boots OCI images,** so a `devcontainer build` image runs as a VM as is. The best fit for dev containers. | Young; needs macOS 26 and Apple silicon (work Macs run macOS 27, so that's met); no egress policy; Docker inside unknown; needs our own sshd. Whether we can attach our own netstack instead of vmnet decides it. |
| **Tart** (Cirrus Labs) with Softnet | **Host-side network isolation built in:** `--net-softnet-allow=<CIDR>` is exactly "only the broker". Scriptable, with OCI-distributed VM images. | Its VM images aren't container images. Softnet needs a one-time privileged install. **License:** Fair Source, free only up to a usage limit. Check it for work. |
| **Podman machine**, one dedicated machine per sandbox | Already on work Macs; SSH built in (`podman machine ssh`) | No egress filtering. Tests would use Podman's Docker-compatible API instead of `dockerd`, a compatibility risk. |
| **libkrun-based tools** (krunkit, smolvm, Microsandbox, OpenShell's libkrun driver) | A VM inside one process, on macOS and Linux; boot OCI images as microVMs | See [libkrun](#libkrun-a-vm-inside-our-own-process-and-a-path-to-the-cloud). Lima already drives krunkit. |
| OrbStack, UTM, Vagrant | — | OrbStack is paid for commercial use, UTM is GUI-first, Vagrant is heavy. Skip. |

### Libraries that boot VMs for us

*Checked 2026-11-22 against each project's README and docs; not tried.* The question: if we wanted one process to run the VM too (as sbx does), which libraries handle booting, disks, cloud-init and vsock SSH?

| Option | Level | Boot, disks, cloud-init, SSH | Network hook for us | Fit |
|---|---|---|---|---|
| **Lima** (CLI, YAML templates, Go module `github.com/lima-vm/lima/v2`) | High | All of it, including vsock SSH and a guest agent | `user-v2` network socket (what we use). Lima 2.0 also has **external VM driver plugins** over gRPC (experimental), so a driver can be replaced without forking. | **Use it through `limactl` and templates.** Its Go packages are importable but aren't a stable public API, so don't embed them. |
| **vfkit** (Red Hat; Apache-2.0; behind Podman machine and CRC) | Medium: a CLI plus a REST API on a Unix socket, and a Go config package | Boots on Apple's framework; disks; cloud-init via `--cloud-init`; virtio-vsock | `virtio-net` on a Unix socket, documented for use with gvisor-tap-vsock | The best "smaller Lima" if we ever drop Lima: we'd supply images and SSH setup ourselves. |
| **Apple Containerization** (Swift package; Apache-2.0; behind Apple `container`) | High, but for containers | One lightweight VM per container from an OCI image; ext4 builder; its own init (`vminitd`) with a gRPC API over vsock | Networking via vmnet; a custom netstack hook is *unverified* | Needs macOS 26 and Swift. A good fit for dev-container images later; no cloud-init or sshd by design. |
| **Code-Hex/vz** (Go bindings; MIT; what Lima's vz driver uses) | Low | None: we'd build boot, disks, cloud-init and vsock SSH ourselves | File-handle NICs, as Lima uses | Only if we write our own runtime. That's building sbx. |
| **Multipass** (Canonical; GPL-3.0) | High: a daemon with a gRPC API | All of it for Ubuntu, with cloud-init | None we found; its daemon runs as root on macOS (*unverified*) | No. |
| **libkrun / krunkit** (Red Hat; Apache-2.0; behind Podman machine's libkrun provider) | Low (the library) to medium (krunkit, a CLI with vfkit-style flags) | The library boots a VM inside the calling process; krunkit adds EFI disk images. No cloud-init or SSH setup of its own. | `virtio-net` on a Unix datagram socket, documented for gvisor-tap-vsock | **Lima drives krunkit already** (`vmType: krunkit`). Embedding the library is the path to one process and to Linux hosts. See the next section. |

**Conclusion: keep Lima as the thing that boots VMs, and tie our process to it through the `user-v2` network.** If that coupling proves painful, the next step is a Lima external driver plugin, or vfkit driven by our CLI. Writing our own runtime on `Code-Hex/vz` is last; embedding libkrun (next section) comes before it.

### libkrun: a VM inside our own process, and a path to the cloud

*Researched 2026-11-22 from Lima v2.3.0's template, the libkrun and krunkit READMEs and a web search; nothing tried. The tools' maturity is unverified.* Work Macs run macOS 27, so nothing here is held back by the OS.

**libkrun** is a C library (written in Rust) that runs a VM inside the process that calls it: KVM on Linux, Hypervisor.framework on macOS. There's no separate VMM process. It has two network modes, and only one can be used at a time:
- **TSI** turns the guest's socket calls into the host process's own sockets. That's elegant, but our filter would have to hook those calls, and we found no hook for it. Don't use it.
- **`virtio-net` on a Unix socket** (gvisor-tap-vsock or passt). Our netstack stays the VM's only NIC, exactly as on Lima. Use this one.

**What builds on it:**

| Tool | What it is | Fit for us |
|---|---|---|
| **Lima with `vmType: krunkit`** | Lima drives krunkit as a VM type; its template defaults `ssh.overVsock` to true for krunkit, as for `vz`, and supports nested virtualization on M3 or later | **The cheap experiment.** If our `user-v2` network works under krunkit (*unverified*), switching is one line of the template. |
| **krunkit** | Red Hat's macOS CLI on libkrun-efi; Podman machine uses it; macOS 14 or later | What Lima drives; we wouldn't use it directly |
| **libkrun, embedded in `mono-jig-net`** | cgo from Go | **The tightest packaging possible:** VM, netstack, filter and broker in one process. We'd then own boot, disks, cloud-init and SSH setup (what Lima does for us). libkrun's main branch is now 2.0, which breaks the 1.x API and isn't stable yet. On macOS the binary needs the `com.apple.security.hypervisor` entitlement and code signing. |
| **smolvm** | A single binary that runs libkrun in-process, with no daemon, on macOS, Linux and Windows | The closest existing "VM in a process". Check its maturity and whether we can attach our netstack. |
| **Microsandbox** | libkrun sandboxes for agents, with Rust, Python, TypeScript and Go SDKs | Agent-focused, but brings its own network policy, which overlaps ours. Check whether it takes our netstack. |
| **NVIDIA OpenShell** | Has an opt-in libkrun driver on macOS and Linux | Already an alternative runtime here; its fleet path uses the same library |

**The cloud path.** Most of the harness doesn't care where the VM runs: `provision.sh`, `mono-jig-net` (netstack, filter and broker) and the policy are the same on Linux. What changes:
- **The VM runner:** libkrun on Linux needs KVM, so bare metal or a cloud instance with nested virtualization. Firecracker is the usual cloud choice and takes the same kind of tap or socket NIC. libkrun's advantage is one library on both the Mac and Linux.
- **Credentials:** a cloud host has no Keychain and no developer at a browser. The broker gets workload identity or tokens handed in per run, and the per-user rules still need to know which user the agent acts for.
- **The editor:** SSH still works, through a bastion or a tunnel instead of vsock.

**Recommendation:** stay on Lima, and keep the VM runner behind the [sandbox backend interface](#the-sandbox-backend-interface). Run the krunkit spike check (option C check 7). Embed libkrun in `mono-jig-net` only if one-process packaging or Linux hosts become a priority; the netstack and the broker don't change when we do.

### Dev containers without a dev-container runtime

The spec doesn't need a runtime that understands it:
- **The reference `devcontainer` CLI.** `devcontainer build` makes an image for runtimes that boot OCI images (sbx, Apple `container`, OpenShell). `devcontainer up` runs against the VM's own Docker, with any runtime.
- **DevPod (Loft)** implements the whole spec with pluggable machine providers, and sets up VS Code over SSH automatically. That's close to our whole shape, for example DevPod with its SSH provider onto a Lima VM. But it **forwards git credentials and the SSH agent by default**, against our model, and its open-source activity slowed in 2025. Borrow its ideas rather than depend on it.
- **Coder's envbuilder** builds `devcontainer.json` inside the target itself, without a Docker daemon. Useful for building images inside a VM.

### Plan for runtimes

1. **Prototype on sbx,** using only what Lima can replace: the VM, Docker inside, and its domain policy. No sbx secrets, no sbx MCP gateway for the harness.
2. **Then bring up the Lima backend before starting the work track.** The design isn't done until it runs without sbx.
3. **Tart with Softnet** is the fallback if Lima's route can't be enforced on the Mac, provided its license suits us.
4. **Track Apple `container`** for its dev-container fit (work Macs run macOS 27), and **libkrun** for one-process packaging and the cloud.

## Would OpenShell be easier behind an IDE?

*Assessed 2026-11-22.* **It helps a little, and it's still rough.**
- **What the IDE hides:** day-to-day use of the `openshell` CLI. OpenShell also has SSH built in (`sandbox connect --editor vscode`), so it needs less plumbing than sbx, where we add our own sshd.
- **What it doesn't hide:**
  - **Docker inside the sandbox is unproven.** It runs non-root, with no sudo, in a locked-down VM, and our tests need Docker. That's a blocker with or without an editor.
  - **Installation and upgrades on Macs.** It needed a setup script for libkrun, e2fsprogs and the VM driver, and 0.1.9 rejected its own bundled policy, then hung. It's alpha. We'd own upgrading it on every Mac, and each release could break that again.
- **Approvals get harder to see.** A well-packaged policy makes approvals rare, not absent. An investigating agent will fetch docs from sites we didn't list, and tests pull images. Behind an IDE, a blocked request shows up as a puzzling network error in the agent's chat, and fixing it needs a terminal command the user never learned. sbx has the same problem with its approval queue. **In both, the CLI should surface pending approvals,** for example as a desktop notification with a one-line approve command.
- **OpenShell's main advantage shrinks.** Its method, path and GraphQL rules matter less once our broker enforces finer rules anyway. What's left is defence in depth, no vendor account, and its Kubernetes path for headless agents.

**OpenShell becomes a real option once** Docker runs inside its sandboxes, the setup script works repeatedly on clean Macs, and a release stays stable for a while. Until then, Lima is the path without a Docker account.

## Recommendation

*Revised 2026-11-22.*

1. **Build the broker first,** for the managed providers only, with an `mono-jig` CLI (see [Packaging: one process per sandbox](#packaging-one-process-per-sandbox)). It's the hard part, it doesn't depend on the sandbox tool, and it carries over to headless agents.
2. **Personal track first** (GitLab, AWS, Claude Code, VS Code; see [Two tracks](#two-tracks-personal-first-then-work)), then the work track. The steps are in `prototype-plan.md`.
3. **Prototype on sbx, then move to Lima before the work track.** Assume work won't allow Docker accounts. Use only the sbx features Lima can replace, and don't call the design done until it runs on Lima ([Leaving sbx](#leaving-sbx-lima)). If work does allow Docker accounts, sbx stays an option for those who want its polish.
4. **Interactive use is an editor over SSH into the sandbox, VS Code first, then Kiro.** Bake the toolchain into the sandbox image ("toolchain in the image" in [Dev containers](#dev-containers-only-inside-the-vm)). Close the editor's credential bridges before anyone uses it.
5. **Keep OpenShell for later,** for the automated-agent fleet, where its central gateway and Kubernetes driver pay off. Prove first that Docker works inside its sandboxes.

## Open questions

- Lima: do lima#5818 (`external: true`) or lima#5813 (`daemon:`) merge, and when? Until then we carry the pidfile workaround. Does `sbx rm` remove per-sandbox policy rules? Do work Macs give developers `sudo`? Tart: does its license allow our use at work? Apple `container`: does Docker run inside, and can we attach our own netstack? Does our `user-v2` network work with `vmType: krunkit`? (Answered 2026-11-22: work Macs run macOS 27; smokescreen can't chain to a private upstream, so the filter is our own.)
- **The model credential:** can the broker hold and inject the agent's model credential (Claude Code's API key or OAuth token; Kiro's sign-in), so no model token sits in the VM, as sbx's kits do?
- **Corporate networks:** how do `mono-jig-net`'s filter and broker chain to a corporate proxy or SASE client on work Macs, and handle VPN split DNS? The filter's upstream proxy covers it; the broker uses `HTTPS_PROXY` (`NewUpstreamClient`).
- **sbx mode:** do requests to `host.docker.internal:<port>` reach the broker intact through sbx's proxy, and does `gh` accept a host with a port over plain HTTP?
- **Building the netstack:** can gvisor-tap-vsock's tap switch be used with a stack we build ourselves, or do we vendor a patched copy?
- **Prebuilt images:** how do we build and ship a Lima disk image so a sandbox starts in seconds rather than provisioning at first boot?
- Does every `gh` command work against `github.broker.internal` as a GHES host? (Answered: git applies a `remote.origin.push` mapping even to an explicit branch, so we don't set one.)
- Which common tools ignore `HTTPS_PROXY`? Can `glab` and `gh` talk to the broker's endpoints as a self-hosted GitLab or GitHub host?
- Is the GitLab MCP server we use hosted by GitLab, so that the broker must filter its tool calls? Is GitLab's GraphQL merge mutation `mergeRequestAccept`?
- Can the remote's `code --openExternal` be refused, and what is the exact setting for installing remote extensions from the Mac?

- Can `dockerd` run inside an OpenShell VM sandbox, or is there another way to run our tests there? (Answered 2026-11-27: no; [openshell.md](openshell.md).)
- Does work allow free personal Docker accounts for sbx?
- Which credential bridges does VS Code actually open into a Remote - SSH window with default settings, and do our settings close all of them? Then the same for Kiro, whose remote support is its own.
- Does `remote.SSH.localServerDownload: "always"` let the sandbox run without any Microsoft egress?
- Does the Claude Code VS Code extension run in the remote extension host?
- Which machine does a remote Kiro window's sign-in run on, and where does it store the token?
- Can Kiro use an SSH config file outside `~/.ssh/config`? Does `sbx exec` work as a `ProxyCommand`? Does OpenShell's SSH config work with Kiro?
- Does Kiro's remote server install and run through sbx's TLS-intercepting proxy?
- Does sbx's network policy cover traffic from containers inside the sandbox VM (needed for "Reopen in Container")?
- Which hosts does kiro-cli need, and does it have reliable non-interactive flags?
- Will Wiz's per-user OAuth work for our tenant, or will we need a service account?

## Sources

- [Docker docs: Kiro in Docker Sandboxes](https://docs.docker.com/ai/sandboxes/agents/kiro/)
- [docker/sbx-kits-contrib: Kiro kit](https://github.com/docker/sbx-kits-contrib/blob/main/kiro/README.md)
- [Collabnix: Run Kiro in Docker Sandboxes](https://collabnix.com/run-kiro-docker-sandboxes-device-flow-login/)
- [Docker docs: Run sandboxes in CI](https://docs.docker.com/ai/sandboxes/workflows/automation/)
- [docker/desktop-feedback #466: Remove login requirement](https://github.com/docker/desktop-feedback/issues/466)
- [docker/sbx-releases #5: login breaks air-gapped usage](https://github.com/docker/sbx-releases/issues/5)
- [Docker Sandboxes vs. Containers](https://www.docker.com/products/docker-sandboxes/vs-containers/)
- [Arm install guide: sbx](https://learn.arm.com/install-guides/sbx/)
- [Kiro docs: slash commands](https://kiro.dev/docs/reference/slash-commands/)
- [VS Code: Remote development using SSH](https://code.visualstudio.com/docs/remote/ssh)
- [VS Code: Developing inside a container](https://code.visualstudio.com/docs/devcontainers/containers)
- [Kiro IDE changelog 1.5: Remote SSH, Dev Containers, and Cloud Sessions](https://kiro.dev/changelog/ide/1-5/)
- [Kiro docs: Connect with Remote SSH](https://kiro.dev/docs/ide/remote-development/ssh/)
- [Kiro docs: Develop in Dev Containers](https://kiro.dev/docs/ide/remote-development/dev-containers/)
- [Kiro docs: Firewalls, proxies, and data perimeters](https://kiro.dev/docs/privacy-and-security/firewalls/)
- [Kiro docs: VPC endpoints (AWS PrivateLink)](https://kiro.dev/docs/privacy-and-security/vpc-endpoints/)
- [Classmethod: Kiro CLI /sandbox feature](https://dev.classmethod.jp/en/articles/kiro-cli-local-command-sandbox/)
- [kirodotdev/Kiro #11941: bwrap fails in containers](https://github.com/kirodotdev/Kiro/issues/11941)
- [OpenShell policy schema reference](https://docs.nvidia.com/openshell/dev/manage/policies/schema)
- [OpenShell providers v2](https://docs.nvidia.com/openshell/sandboxes/providers-v2)
- [OpenShell provider profiles](https://docs.nvidia.com/openshell/latest/how-it-works/providers/profiles)
- [logeshwaran.org: OpenShell policy, prover, MCP](https://www.logeshwaran.org/2026/09/nvidia-openshell-policy-yaml-prover-mcp.html)
- [StackOne: OpenShell keeps API keys away from agents](https://www.stackone.com/blog/nvidia-openshell-agent-credentials/)
- [Aembit: Wiz MCP server](https://docs.aembit.io/ai-guide/mcp/identity-gateway/supported-servers/wiz/)
- [C1: Wiz MCP server](https://www.c1.ai/docs/product/admin/mcp-server/wiz)
- [roboticforce.io: OpenShell vs Docker Sandboxes](https://roboticforce.io/lab/openshell-vs-docker-sandboxes)
- [docker/mcp-gateway (GitHub)](https://github.com/docker/mcp-gateway)
- [Docker blog: MCP Gateway](https://www.docker.com/blog/docker-mcp-gateway-secure-infrastructure-for-agentic-ai/)
- [Running Docker MCP Gateway on Linux without Docker Desktop (DEV)](https://dev.to/udondan/running-docker-mcp-gateway-on-linux-without-docker-desktop-4da2)
- [mcp-gateway Go package docs](https://pkg.go.dev/github.com/docker/mcp-gateway)
- [Open-source MCP gateways compared (cortex-gateway.dev)](https://cortex-gateway.dev/alternatives/open-source-mcp-gateways/)
- [podman-desktop-extension-mcp (GitHub)](https://github.com/sparesparrow/podman-desktop-extension-mcp)
- [MCP Gateway & Registry macOS guide (GitHub)](https://github.com/agentic-community/mcp-gateway-registry/blob/main/docs/macos-setup-guide.md)
- [Lima v2.3.0: vz network attachment (`attachNetwork`)](https://github.com/lima-vm/lima/blob/v2.3.0/pkg/driver/vz/vm_darwin.go)
- [Lima v2.3.0: built-in user-mode network (`gvproxy.go`)](https://github.com/lima-vm/lima/blob/v2.3.0/pkg/networks/usernet/gvproxy.go)
- [Lima docs: user-v2 network](https://github.com/lima-vm/lima/blob/master/website/content/en/docs/config/network/user-v2.md) and [default user-mode network](https://github.com/lima-vm/lima/blob/master/website/content/en/docs/config/network/user.md)
- [lima#5464: externally managed user-v2 daemons](https://github.com/lima-vm/lima/issues/5464), [lima#5818: `external: true`](https://github.com/lima-vm/lima/pull/5818), [lima#5812](https://github.com/lima-vm/lima/issues/5812) and [lima#5813: custom daemon](https://github.com/lima-vm/lima/pull/5813), [lima#5820: egress rules](https://github.com/lima-vm/lima/issues/5820), [lima#4326: user-v2 network policy](https://github.com/lima-vm/lima/pull/4326)
- [gvisor-tap-vsock#750: dial filter and DNS observer](https://github.com/containers/gvisor-tap-vsock/pull/750), [#687: outbound filtering](https://github.com/containers/gvisor-tap-vsock/pull/687)
- [vfkit usage](https://github.com/crc-org/vfkit/blob/main/doc/usage.md)
- [Apple Containerization](https://github.com/apple/containerization)
- [Lima docs: VM driver plugins](https://github.com/lima-vm/lima/blob/master/website/content/en/docs/dev/drivers.md)
- [Code-Hex/vz](https://github.com/Code-Hex/vz)
- [gvisor-tap-vsock v0.9.0: `virtualnetwork` services](https://github.com/containers/gvisor-tap-vsock/blob/v0.9.0/pkg/virtualnetwork/services.go) and [DNS](https://github.com/containers/gvisor-tap-vsock/blob/v0.9.0/pkg/services/dns/dns.go)
- [Lima v2.3.0: default template](https://github.com/lima-vm/lima/blob/v2.3.0/templates/default.yaml) (`vmType: krunkit`, `propagateProxyEnv`, `portForwards`)
- [smokescreen](https://github.com/stripe/smokescreen) (considered, not used)
- [git: `push.c`](https://github.com/git/git/blob/master/builtin/push.c) (push mappings applied to explicit refspecs)
- [GitLab docs: push options](https://docs.gitlab.com/topics/git/commit/#push-options)
- [GitLab: LFS API controller](https://gitlab.com/gitlab-org/gitlab/-/blob/master/app/controllers/repositories/lfs_api_controller.rb)
- [libkrun](https://github.com/containers/libkrun), [krunkit](https://github.com/containers/krunkit) and its [usage docs](https://docs.rs/crate/krunkit/1.2.0/source/docs/usage.md), [Lima PR #4137](https://github.com/lima-vm/lima/pull/4137)
- [smolvm (AgentPatterns)](https://agentpatterns.ai/security/smolvm-library-microvm-cross-os-sandbox/), [Microsandbox](https://ai.miraheze.org/wiki/Microsandbox), [awesome-agent-sandbox](https://github.com/fishman/awesome-agent-sandbox), [Agent sandboxes compared](https://engine.build/lab/agent-sandboxes), [Why agent sandboxes are converging on libkrun](https://iii.dev/blog/why-agent-sandboxes-are-converging-on-libkrun-not-firecracker/)
