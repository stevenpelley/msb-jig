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
| `providers.Config` | its fields |
| `Hosts`, `Brokered`, `Build` | an `if` block each |
| `cmd/mono-jig/settings.go` | `gitlabSettings`, `githubSettings`, the Keychain names |
| `login.go` | `loginGitLab`, `loginGitHub`, … and the `switch` |
| `supervisor.go` | `brokerConfig`, `clientEnv`, `optionalEnv`, `brokerURLs`, `brokerNames` and `hostsLine`, the `BROKER_*` exports in `provision` |
| `provision.sh` | `broker-env` (insteadOf, `GH_HOST`, glab's config) and a section of the agent's instructions each |
| `allowlist/lists/base.yaml` | the provider groups |
| `cmd/mono-jig-sbx` | a second wiring of the same providers |

The guest's environment is written twice, in Go (`clientEnv`) and in
bash (`client_env`), and `TestClientEnvMatchesProvision` exists only to
keep them in step.

GitHub is fixed to github.com: `GitHubWebHost` and `GitHubAPIHost` are
constants, and login and the token command pass `--hostname github.com`.
GitHub Enterprise Cloud with data residency (`*.ghe.com`) or a GitHub
Enterprise Server needs the host configurable, as GitLab's URL is.

### Base image

- `image.Dockerfile` and `defaultImage` say `ubuntu:26.04`.
- `provision.sh` assumes Debian: `apt-get`, `update-ca-certificates`,
  `useradd`, glab from apt.
- A corporate base image, registry mirror or apt mirror means editing
  `provision.sh`.

### A corporate CA (TLS inspection)

Not handled anywhere. It matters in four places:

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

- `provision.sh`: the `claude` and `claude-env` components,
  `hasCompletedOnboarding`, `claude mcp add`.
- The instructions go to `~/.claude/CLAUDE.md`; kiro-cli reads steering
  files from elsewhere (*unverified*).
- `clientEnv`: `ANTHROPIC_BASE_URL`, `CLAUDE_CODE_OAUTH_TOKEN`.
- The editor settings name `anthropic.claude-code`.
- The allowlist groups `claude-code` and `anthropic`.

The rules in the instructions (push only under `agent/<user>/`) belong
to the providers; the agent says only where the file goes.

### Other things likely to change

- **The VM runtime.** Work may not allow microsandbox
  (`microsandbox.md`). `supervisor.go` mixes the SDK, the broker's start
  and provisioning; the [backend interface](design.md#the-sandbox-backend-interface)
  was never built.
- **The agent's namespace.** `broker/policy.go` fixes `agent/<user>/` and
  the title tag `[agent/<user>]`. The title tag also ends up in a
  squash-merged commit's subject; see [the PR marker](#the-pr-marker).
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
2. **The provider registry.** GitLab, GitHub, Anthropic and AWS on it,
   replacing the wiring in `providers.go`, `settings.go`, `login.go` and
   `supervisor.go`. GitHub's host configurable.
3. **Extra CAs**, at image build and every start, with `SSL_CERT_FILE`
   and `REQUESTS_CA_BUNDLE`. Small, and needed on the first day at work.
4. **The base image configurable** (`BASE_IMAGE`, `Track.BaseImage`);
   `provision.sh` split into components.
5. **Agent and Editor**, with Claude Code and VS Code as the first.
6. **The supervisor split**: broker host, `Backend`, provisioning.
7. **The module renamed and moved** to its own repo; the personal track
   is one `Track`.

Steps 1 to 3 carry most of the value; 5 and 6 make Kiro, kiro-cli and
another VM runtime replacements rather than forks.

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
tag requirement. If a visible flag is wanted beyond the branch name the
provider already shows, have the agent add a label (`agent`) when it
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
