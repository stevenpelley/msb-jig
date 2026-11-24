# Splitting mono-jig into a library and tracks

*Reviewed 2026-11-24, at commit 0491dc0 (msb 0.7.16, Go 1.26.0). Nothing
here is built yet. Items marked* unverified *haven't been checked.*

The work track differs from the personal one in its providers, base
image, corporate CA (a TLS-inspecting proxy such as Zscaler), agent
(kiro-cli) and editor (Kiro). The goal: a common library where the code
is shared, and modules a track replaces wholesale where it isn't.

## Direct answer

- **The broker core is already a library.** The rest isn't: each
  provider, the agent and the editor is named in five to eight places,
  mostly in `cmd/mono-jig`, `guest/provision.sh` and
  `broker/providers/providers.go`.
- **The one change that collapses most of it:** Go decides what the
  guest needs (environment, files, git settings, the agent's
  instructions); `provision.sh` only applies it. Then each axis has one
  plug-in point.
- **The library needs its own repo.** The module path is `mono-jig`,
  inside the personal monorepo, so a work repo can't import it, and work
  names must stay out of this repo. The work track imports the library;
  it doesn't live beside it.

## Already reusable

- `broker`: `Server`, `Provider` and `Route`, `Upstream`, `policy.go`,
  `connect.go`, `ca.go`, `listen.go`; and `gitproto` and the `mcp`
  filter. None of them know which providers exist.
- The provider packages (`github`, `gitlab`, `anthropic`, `awsmcp`):
  each is closed, with its own `Routes` and `Config`. Hosted MCP servers
  (Datadog's, Wiz's) are new packages on `broker/mcp`, like
  `gitlab/mcp.go`.
- `allowlist`: layered. A work org layer goes in `JIG_LAYERS`.
- `mono-jig image build` already takes `-engine podman`.
- Credentials come from token commands, so the Keychain, a password
  manager's CLI or an Okta helper all fit.

## Where each axis is hardwired

### Providers

A provider is named in each of these:

| Place | What's per provider |
|---|---|
*Since step 2 (done 2026-11-24, below) a provider is a file in
`broker/providers` (`Provider`: hosts per mode, handlers, guest
settings), a file in `cmd/mono-jig` (`login_<name>.go`: login, status,
settings, the JIG_* overrides) listed in `registry.go`, an instructions
section (`guest/instructions/<name>.md`) and an allowlist group. Before,
it was named in each of these:*

| Place | What was per provider |
|---|---|
| `providers.Config` | its fields |
| `Hosts`, `Brokered`, `Build` | an `if` block each |
| `cmd/mono-jig/settings.go` | `gitlabSettings`, `githubSettings`, the Keychain names |
| `login.go` | `loginGitLab`, `loginGitHub`, … and the `switch` |
| `supervisor.go` | `brokerConfig`, `brokerNames` and `hostsLine` |
| `guestconfig.go` | a function each (`gitlabGuest`, …) |
| `allowlist/lists/base.yaml` | the provider groups (still) |
| `cmd/mono-jig-sbx` | a second wiring of the same providers (now builds `providers.GitLab` and `GitHub` values) |

*Until step 1 (below, done 2026-11-24) the guest's environment was
written twice, in Go (`clientEnv`) and in bash (`client_env`), and
`provision.sh` had a block per provider.*

GitHub was fixed to github.com. *Since step 2 its host is configurable
(`login github --host`, `JIG_GITHUB_HOST`), with the API at
`api.<host>`: GitHub Enterprise Cloud with data residency (`*.ghe.com`)
has that shape (*unverified*). GitHub Enterprise Server, with the API on
the web host under `/api/v3`, doesn't fit the route tables yet.*

### Base image

*Since step 4 (done 2026-11-24, below) the image is `JIG_BASE_IMAGE` and
`provision.sh` is component files; a non-Debian base still needs its own
`base` (and `ca`) component.*

- `image.Dockerfile` and `defaultImage` say `ubuntu:26.04`.
- `provision.sh` assumes Debian: `apt-get`, `update-ca-certificates`,
  `useradd`, glab from apt.
- A corporate base image, registry mirror or apt mirror means editing
  `provision.sh`.

### A corporate CA (TLS inspection)

*Since step 3 (done 2026-11-24, below) the first two are handled; the
Mac's two are still the user's to set up.* It matters in four places:

- **The image build on the Mac.** `apt-get` and Claude Code's installer
  run inside `docker build` or `podman build` and fail verification.
  `image.Dockerfile` copies only `provision.sh`: it must copy the extra
  CAs and trust them before provisioning.
- **The guest.** It trusts one extra CA, the broker's: one `WithPatches`
  file, installed by the `broker` component, so only at the first start.
  It needs a list, installed at every start, and beyond
  `NODE_EXTRA_CA_CERTS` also `SSL_CERT_FILE` and `REQUESTS_CA_BUNDLE`
  (Python ignores the system store), and `AWS_CA_BUNDLE` if the AWS CLI
  is in the guest.
- **The broker on the Mac.** Go on macOS verifies through the system's
  trust settings, so a root in the System keychain should work
  (*unverified*).
- **The AWS MCP proxy under uv.** Python with certifi doesn't read the
  keychain; it likely needs `UV_NATIVE_TLS=1` or `SSL_CERT_FILE`
  (*unverified*).

### Editor

*Since step 5 (done 2026-11-24, below) `editor.Editor` is the seam and
`editor.VSCode` the one behind it; the list below is where VS Code was
named before.*

- `editor/vscode.go`: Microsoft Remote-SSH's settings and extension
  IDs. Kiro's remote support is its own; its setting names are
  *unverified*.
- `remote.go` `code`: the `code` binary, `--user-data-dir`,
  `ssh-remote+`, the `_vscode` directories.
- `setup`: installs `ms-vscode-remote.remote-ssh`, checks for `code`.
- `checks/bridges.sh` and `checks/vscode-e2e`.
- Two SSH config writers: `editor/ssh.go` (sbx) and `remote.go`
  `sshConfig` (microsandbox). The editor should take a host alias and a
  config path from the backend and own nothing else.

### Agent

The agent is its own axis, separate from the editor:

- `provision.sh`: the `claude` component (the installer).
- `guestconfig.go` `claudeCode`: `ANTHROPIC_BASE_URL`,
  `CLAUDE_CODE_OAUTH_TOKEN`, `hasCompletedOnboarding`, and the
  instructions in `~/.claude/CLAUDE.md`; kiro-cli reads steering files
  from elsewhere (*unverified*).
- The editor settings name `anthropic.claude-code`.
- The allowlist groups `claude-code` and `anthropic`.

The rules in the instructions (push only under `agent/<user>/`) belong
to the providers; the agent says only where the file goes.

### Other things likely to change

- **The VM runtime.** Work may not allow microsandbox
  (`microsandbox.md`). `supervisor.go` mixed the SDK, the broker's start
  and provisioning; the [backend interface](design.md#the-sandbox-backend-interface)
  was never built. *Since step 6, `sandbox.Backend` (below).*
- **The agent's namespace.** `broker/policy.go` fixes `agent/<user>/`.
  (The title tag is gone; see [the PR marker](#the-pr-marker).)
- **The name.** `mono-jig` is in `~/.mono-jig`, the Keychain items, the
  SSH host prefix, the CA's subject, image tags, `/etc/mono-jig` and the
  refusal text. One constant lets a work build have its own and coexist.
- **AWS sign-in.** `login aws` assumes `aws sso login`; Okta
  (`credential_process` or a SAML helper) signs in differently. The
  broker side doesn't change.
- **An explicit corporate proxy**, if the work network uses one rather
  than a tunnel: the broker's upstream client honours `HTTPS_PROXY`,
  proxy mode's pass-through dials directly, and microsandbox's own
  egress is *unverified*.
- **The base allowlist.** `base.yaml` turns on sbx's balanced groups;
  work may want a stricter base, replaced rather than extended.
- **`mono-jig-sbx`**: a second entry point for a backend we left. Drop
  it, or build it on the same registry.

## Proposed structure

The library holds `broker/…`, the provider packages, `allowlist`, and
these seams:

```go
type Provider interface {
    Name() string                    // login name, allowlist group
    Enabled(s Settings) bool
    Handlers(b *Builder, mode Mode) (map[string]http.Handler, error) // by host
    Guest(mode Mode) GuestConfig
    Login(args []string) error       // optional
}

type GuestConfig struct {
    Env          map[string]string
    Files        map[string]File     // glab's config, CA certificates, …
    GitConfig    []string            // insteadOf
    Instructions string              // the provider's section for the agent
}

type Agent interface {               // Claude Code, kiro-cli
    Install() Component              // image-time shell
    Guest(Mode, []Provider) GuestConfig // its env, where the instructions go, MCP registration
    AllowlistGroups() []string
}

type Editor interface {              // VS Code, Kiro
    Setup(dataDir string) error
    Open(sshConfig, host, dir string) error
}

type Backend interface {             // microsandbox
    // create, start, stop, remove, exec, write a file, SSH host
}

type Track struct {
    Name, BaseImage string
    ExtraCAs        []string
    Providers       []Provider
    Agent           Agent
    Editor          Editor
    Backend         Backend
    Policy          broker.PolicyConfig // the agent's namespace
    AllowlistBase   string
}
```

Each track is a thin `cmd/<name>/main.go` that builds a `Track` and
calls `jig.Main(track)`. The work repo has its own, with its providers,
kiro-cli, Kiro, its base image and CA.

`provision.sh` becomes component files (`base`, `sshd`, `git`, `docker`,
`ca`, one per agent) and one generic step that applies the Go-built
`GuestConfig`: the environment into `/etc/environment` and `profile.d`,
the files, git's settings, the instructions file, then
`update-ca-certificates`. A track replaces a component (`base` for a
non-Debian image) or adds one; the image's hash covers the set.

## Order

Each step stands alone and is checked by the existing end-to-end runs.

1. **Guest settings in Go.** `GuestConfig`; `clientEnv` and the
   provisioned files come from it; `broker-env` and `claude-env` shrink
   to the generic step. Removes the Go and bash duplication.
   *Done 2026-11-24, see [Step 1 as built](#step-1-as-built).*
2. **The provider registry.** GitLab, GitHub, Anthropic and AWS on it,
   replacing the wiring in `providers.go`, `settings.go`, `login.go` and
   `supervisor.go`. GitHub's host configurable.
   *Done 2026-11-24, see [Step 2 as built](#step-2-as-built).*
3. **Extra CAs**, at image build and every start, with `SSL_CERT_FILE`
   and `REQUESTS_CA_BUNDLE`. Small, and needed on the first day at work.
   *Done 2026-11-24, see [Step 3 as built](#step-3-as-built).*
4. **The base image configurable** (`BASE_IMAGE`, `Track.BaseImage`);
   `provision.sh` split into components.
   *Done 2026-11-24, see [Step 4 as built](#step-4-as-built).*
5. **Agent and Editor**, with Claude Code and VS Code as the first.
   *Done 2026-11-24, see [Step 5 as built](#step-5-as-built).*
6. **The supervisor split**: broker host, `Backend`, provisioning.
   *Done 2026-11-24, see [Step 6 as built](#step-6-as-built).*
7. **The module renamed and moved** to its own repo; the personal track
   is one `Track`. *The `Track` half done 2026-11-24, see [Step 7 so
   far](#step-7-so-far); the move needs decisions.*

Steps 1 to 3 carry most of the value; 5 and 6 make Kiro, kiro-cli and
another VM runtime replacements rather than forks.

## Step 1 as built

*2026-11-24. Unit tests only; the end-to-end runs (a sandbox created,
restarted, and restarted with a provider removed) are* unverified.

- `guest.Config` (`guest/config.go`): `Env`, `Git` (each key's values
  replace the guest's), `GitClear`, `Files` (text, a symbolic link, or
  JSON merged into what's there; a marker first line marks a file as
  ours), `Packages`, and `Instructions` (Markdown sections, which the
  agent puts in its own file).
- `cmd/mono-jig/guestconfig.go` builds it: one function per provider
  (`gitlabGuest`, `githubGuest`, `awsGuest`) and one for the agent
  (`claudeCode`). The sandbox's own environment is its `Env`;
  `optionalEnv` and `GitClear` come from the same functions with every
  provider on, so nothing lists names by hand.
- The supervisor writes it as `/root/guest.json`; `provision.sh`'s
  `apply` component (every start) applies it with `jq`. It replaces
  `broker-env`, `claude-env` and `aws-env`; `broker` now only trusts the
  broker's CA. One environment file, `/etc/profile.d/80-mono-jig.sh` and
  one `/etc/environment` block, replace the five older ones, which
  `apply` removes. Files it wrote and no longer wants are removed, if
  still ours (`/var/lib/mono-jig/guest-files` lists them).
- The agent's instructions are templates, `guest/instructions/*.md`,
  rendered in Go. The output is unchanged but for one fix: GitLab's
  host is the configured one, not always gitlab.com (and git's
  `insteadOf` for GitLab follows it too).
- Changing a provider's guest settings no longer changes `provision.sh`,
  so no image rebuild.
- Dropped: `BROKER_GITLAB_MCP` (registering GitLab's MCP server with
  Claude Code), which nothing set since the 2026-11-22 decision for glab;
  and `BROKER_SESSION_TOKEN`, the sbx broker's session header, which
  nothing set either. A guest for `mono-jig-sbx` would need its own
  `guest.Config`.
- Edge left: a sandbox provisioned before this whose provider is removed
  at the same restart as the upgrade keeps that provider's glab config,
  instructions or mcpcli link, since there's no list of files yet.

## Step 2 as built

*2026-11-24. Unit tests only; logging in with the new code, and a
sandbox started from it, are* unverified.

- `providers.Provider` (`broker/providers/providers.go`): `Name` (the
  allowlist group and login name), `Hosts(proxy)`, `Handlers` and
  `Guest(proxy)`. `GitLab`, `GitHub`, `Anthropic` and `AWS` implement it,
  one file each, holding their own settings. `Config.Providers` is those
  that are on; `Hosts`, `Internal`, `Brokered`, `InternalOnly` and
  `Guest` are computed from them. A work provider is a new type, in the
  work repo once the library has moved (step 7).
- The guest settings moved from `cmd/mono-jig/guestconfig.go` into each
  provider's `Guest`; `guestconfig.go` keeps the base environment and the
  agent (`claudeCode`, which looks for the `anthropic` provider).
- Cleanup no longer enumerates every provider: the supervisor records the
  sandbox environment's names (`<state>/guest-env`) and removes those no
  longer set; `apply` records git's keys (`/var/lib/mono-jig/guest-git`).
  The names older sandboxes may have are listed once (`legacyEnv`,
  `legacyGitKeys`).
- `/etc/hosts` maps only the internal names of the providers that are on
  (before: all four in server mode, whether on or not).
- The CLI side is a registry too (`cliProvider` in `registry.go`):
  `login`, `login all`, the status, `logout`, `uninstall`'s Keychain
  items and `brokerConfig` iterate it. `settings.json` keeps its shape
  (each provider under its name, and `image`), but is read generically.
- GitHub's host: see above.

## Step 3 as built

*2026-11-24. Unit tests, and the certificate split checked with awk on the
Mac; an image build and a sandbox behind a TLS-inspecting proxy are*
unverified.

- `mono-jig ca <pem-file>...` records the files (absolute paths, so a
  rotated file is read again; `--clear` forgets them, no arguments shows
  them). `JIG_EXTRA_CAS` overrides. Each certificate must parse and be a
  CA. On a Mac whose proxy root is in the System keychain, one way to get
  the PEM is `security find-certificate -a -p -c <name>
  /Library/Keychains/System.keychain` (*unverified*).
- **The image build** copies them into the context (`ca/`), and the
  Dockerfile puts them in `/usr/local/share/ca-certificates/mono-jig-build/`
  before `provision.sh` runs, so its downloads verify; installing
  `ca-certificates` trusts them, and `base` runs `update-ca-certificates`
  again. They're part of the base image's tag, so changing them rebuilds
  it. They stay in that image until the next build.
- **Every start**: the supervisor writes them to `/root/guest-ca.pem`, and
  `provision.sh`'s new `ca` component, first, splits them into
  `/usr/local/share/ca-certificates/mono-jig-extra/` (replacing the last
  start's) and runs `update-ca-certificates --fresh`. It uses awk, not jq:
  on a first start from a plain image nothing is installed yet. That
  `update-ca-certificates` reads subdirectories is *unverified*.
- The guest's environment always has `SSL_CERT_FILE` and
  `REQUESTS_CA_BUNDLE` (the system bundle), beside `NODE_EXTRA_CA_CERTS`.
  They also make Python trust the broker's CA. `AWS_CA_BUNDLE` isn't set:
  the AWS CLI isn't in the guest.
- **Not done, on the Mac:** the broker should verify through the
  keychain (Go's default on macOS), and the AWS MCP proxy under uv
  likely needs `UV_NATIVE_TLS=1` or `SSL_CERT_FILE` in the supervisor's
  environment (both *unverified*).

## Step 4 as built

*2026-11-24. Unit tests; an image build from another base is*
unverified.

- `JIG_BASE_IMAGE` (default `ubuntu:26.04`) is what the base image builds
  on (`ARG BASE_IMAGE` in `image.Dockerfile`, part of the base image's
  tag) and the plain image a sandbox starts from without a built one.
  `Track.BaseImage` will set it.
- `guest/provision/`: `provision.sh` sources `lib.sh` (the settings and
  helpers), then `components/*.sh` in name order: `10-ca`, `20-base`,
  `30-sshd`, `40-git`, `50-broker`, `60-docker`, `70-claude`, `90-apply`.
  Each still runs only when `PROVISION_COMPONENTS` names it. The code is
  unchanged, only moved.
- `guest.Provision()` returns the files as a fresh map, so a track can
  replace a component (`components/20-base.sh` for a base without apt) or
  add one; `ProvisionHash` covers the whole set, and the base image
  records it as before.
- The supervisor writes the set to `/root/provision/` on every start
  (replacing the last, and removing the old `/root/provision.sh`); the
  image build copies it to `/opt/mono-jig/provision/`; the sbx stand-in
  `guest/Dockerfile` copies it too.
- Debian-specific still: `base` (apt, `useradd`), `ca` and `broker`
  (`update-ca-certificates`), `git` (gh and glab from apt), `apply`'s
  package install (apt, `dpkg -s`).

## Step 5 as built

*2026-11-24. Unit tests; opening VS Code on a sandbox through the new
code is* unverified.

- `agent.Agent` (`agent/agent.go`): `Components` (provision files it
  adds) and `ComponentNames`, `Guest` (its settings, and the
  instructions in its own file), `EditorExtensions`. `agent.ClaudeCode`
  owns `70-claude.sh` (moved from `guest/provision/components/`), the
  Anthropic environment, onboarding, `~/.claude/CLAUDE.md`, and
  `anthropic.claude-code`. A kiro-cli agent is another type with its own
  component and steering file.
- `editor.Editor` (`editor/editor.go`): `Find`, `Configure`, `Install`,
  `Open`, taking a `Profile` (the SSH config, the hosts, the agent's
  extensions). `editor.VSCode` holds what `remote.go` `code` and `setup`
  did: the `code` binary, `_vscode` and `_vscode-ext`, Remote-SSH.
  `VSCodeSettings` takes the agent's extensions instead of naming Claude
  Code's.
- `cmd/mono-jig/track.go` picks them (`theAgent`, `theEditor`) until
  step 7's `Track`. The provision set is `guest.Provision()` plus the
  agent's files (`provisionFiles`); the components run at the image build
  and a first start include the agent's.
- The sbx stand-in image (`guest/Dockerfile`) no longer installs Claude
  Code: its component isn't in `guest/` any more, and step 1's check
  doesn't need it.
- Left: the allowlist's `claude-code` and `anthropic` groups are still in
  `base.yaml`, not the agent's; the SSH config writers are still two
  (`editor/ssh.go` for sbx, `remote.go` `sshConfig`).

## Step 6 as built

*2026-11-24. Unit tests; creating, restarting and removing a sandbox
through the new code is* unverified.

- `sandbox.Backend` (`sandbox/sandbox.go`): `Open(Spec)` (create the
  first time, else start, applying only the environment), `Exists`,
  `Remove`. `sandbox.Guest`: `Exists`, `ReadFile`, `WriteFile`,
  `RemoveAll`, `Run` (a root script, its output and exit code), `Job` (a
  root script until it ends), `Stop`, `Kill`, `Wait`, `Close`. `Spec`
  has the image and sizes, environment, egress (allowlist entries),
  mounts, disks, the vsock route, files patched in at create, and the
  outbound proxy.
- `sandbox.Microsandbox` holds all the SDK code that was in
  `supervisor.go`, `brokermode.go`, `mounts.go`, `lifecycle.go` and
  `login.go`, with `EgressRules`. `cmd/mono-jig` no longer imports the
  SDK.
- The supervisor is three files: `brokerhost.go` (`startBroker`: the
  providers, the socket, the proxy in proxy mode), `spec.go` (the
  `Spec` from the config), `provision.go` (provisioning through
  `Guest`); `supervisor.go` keeps the config, the allowlist and `run`.
- Still microsandbox-specific outside `sandbox/`: the denial log
  (`denials.go` reads microsandbox's runtime log), `cp` (`msb copy`),
  SSH (`remote.go` `sshConfig`, `msb ssh`), `setup`'s `msb doctor` and
  `msb pull`, and loading images (`msb load`). Another runtime needs
  those behind `Backend` too.

## Step 7 so far

*2026-11-24. Unit tests.*

- The CLI is package `jig` (`jig/`, moved from `cmd/mono-jig/`), with
  `Track` (`Providers`, `Agent`, `Editor`, `Backend`, `BaseImage`),
  `Main(Track)` and `Personal()`. `cmd/mono-jig/main.go` is one line:
  `jig.Main(jig.Personal())`. A work track is a command like it with its
  own `Track`.
- `ProviderCLI` (was `cliProvider`) has exported fields, and `Settings`,
  `LoadSettings`, `SaveSettings`, `ProviderSettings` and `SetProvider`
  are exported, so a track's own provider can keep its settings in
  `settings.json`. The personal track's are the variables `GitLab`,
  `GitHub`, `Claude` and `AWS`.
- Not in `Track` yet: the name (`~/.mono-jig`, the Keychain items, the
  SSH host prefix, the CA's subject, image tags, `/etc/mono-jig`, the
  refusal text), the agent's namespace (`broker/policy.go`'s
  `agent/<user>/`), and the base allowlist layer.

Not done, because each needs a decision:

- the new repository's name and host, and the module path (`mono-jig`
  today, which only resolves inside this monorepo);
- how the history moves (`git filter-repo` of `mono-jig/`, or a fresh
  start);
- whether the personal track stays here, importing the new module, or
  moves too; and if it stays, how it pins the module (a tag, or a
  `replace`/`go.work` while both change);
- whether to rename `Track`'s package from `jig` and the commands from
  `mono-jig`.

## The PR marker

The broker decides whether the agent may change a pull or merge request
from its **source branch** alone, looked up from the provider
(`ExistingFromAgentBranch`); the title tag authorizes nothing. It's
only required on titles the agent sets, so a human reviewing the list
can tell. Its cost: a squash merge's default commit subject is the
title, on GitHub and GitLab, so the tag lands in the target branch's
history unless someone edits it out.

| Marker | Lands in the merged history? | Can it authorize? |
|---|---|---|
| Branch `agent/<user>/…` (now) | Only in a merge commit's default message (GitHub's "Merge pull request #n from owner/agent/…", GitLab's "Merge branch 'agent/…'"), never in squash or rebase commits | Yes: the provider reports it, and an open PR's source branch can't be changed |
| Title tag (now) | Yes, in a squash commit's subject | No, and it shouldn't: anyone can edit a title |
| A label (e.g. `agent`) | No | No, unless the broker also refuses to add or remove it on other PRs; a human with triage rights can set it on any PR |
| Draft status | No | No; humans toggle it |
| Author | No | No: the agent acts as the developer |

Recommendation: keep the branch as the only authority and drop the title
tag requirement. *Done 2026-11-24: the broker no longer checks titles,
and the agent's instructions no longer ask for the tag.* If a visible
flag is wanted beyond the branch name the provider already shows, have the agent add a label (`agent`) when it
opens the PR, treated as a hint only. Labels on GitHub's
`POST /repos/{o}/{r}/issues/{n}/labels` create a missing label, which
needs write access; GitLab creates missing labels from `labels=` on an
MR (both *unverified* for this broker's routes, which don't serve
labels today).

## Open questions (unverified)

- Kiro's remote-SSH settings and command-line flags.
- Where kiro-cli reads steering and instructions.
- Whether uv and the AWS MCP proxy fail under TLS inspection without
  `UV_NATIVE_TLS`.
- Whether microsandbox is allowed on work Macs.

## Sources

- This repo at 0491dc0: `broker/providers/providers.go`,
  `cmd/mono-jig/{supervisor,login,settings,remote,image}.go`,
  `editor/vscode.go`, `guest/provision.sh`, `guest/image.Dockerfile`,
  `broker/policy.go`, `allowlist/lists/base.yaml`.
- `docs/design.md`: the two tracks, the sandbox backend interface.
- Step 1: this repo after 4478cbb: `guest/config.go`,
  `guest/instructions/`, `cmd/mono-jig/guestconfig.go`,
  `guest/provision.sh` (`apply`); jq 1.8.2 on the Mac for its expressions.
- Step 2: this repo after fb4d052: `broker/providers/`,
  `cmd/mono-jig/{registry,login_*,settings,guestconfig}.go`.
- Step 3: this repo after 1bfe2c8: `cmd/mono-jig/{ca,image,supervisor}.go`,
  `guest/image.Dockerfile`, `guest/provision.sh` (`ca`); OpenSSL 4.1.0 and
  awk on the Mac for the split.
- Step 4: this repo after 2cba953: `guest/provision/`, `guest/provision.go`,
  `guest/image.Dockerfile`, `cmd/mono-jig/{image,supervisor}.go`.
- Step 5: this repo after d0cd229: `agent/`, `editor/{editor,vscode}.go`,
  `cmd/mono-jig/{track,remote,login,supervisor,image}.go`.
- Step 6: this repo after a65c128: `sandbox/`,
  `cmd/mono-jig/{brokerhost,spec,provision,supervisor}.go`.
- Step 7 so far: this repo after 106e927: `jig/`, `cmd/mono-jig/main.go`.
