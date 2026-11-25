# Writing a track

*2026-11-24, at msb-jig after 16c1ad9 (Go 1.26, msb 0.7.16); updated the
same day for a work track (branch work-track-support): GitHub owners,
the npm provider, kiro-cli, `Track.BrokerMode`. The personal track is
the only one run in a sandbox; a work track is built and unit-tested,
and everything below about running one is* unverified.*

A **track** is one harness command built on this library: its providers,
agent, editor, VM runtime, base image, built-in allowlist, and its own
provisioning and guest settings (`jig.Track`). The personal track is
`jig.Personal()`. This page is how to write another, such as a work
track, and what you can't change yet.

## Where a track lives

In its **own repository**, importing this module. Work names (hosts,
services, accounts, internal tools) never go into msb-jig: it is public.
Anything a work track needs from the library that isn't specific to work
is a change here, made with placeholder names (`example.org`,
`provider "example"`).

```go
// cmd/<name>/main.go in the track's repository
package main

import "github.com/stevenpelley/msb-jig/jig"

func main() { jig.Main(myTrack()) }
```

```sh
go mod init <module>
go get github.com/stevenpelley/msb-jig@<commit>   # a commit, not @main: the proxy caches @main
go mod tidy
go build -o ~/bin/<name> ./cmd/<name>
```

While changing both at once, a local `replace github.com/stevenpelley/msb-jig => ../msb-jig`
in the track's `go.mod` (not committed) builds against a checkout.

The command can have any name, but the library still calls itself
`mono-jig` (see [Fixed for now](#fixed-for-now)).

## The Track

```go
type Track struct {
    Providers  []jig.ProviderCLI    // login offers these, in order; the broker serves those logged in
    Agent      agent.Agent          // required
    Editor     editor.Editor        // required
    Backend    sandbox.Backend      // required
    BaseImage  string               // default ubuntu:26.04; JIG_BASE_IMAGE overrides
    Allowlist  []allowlist.Layer    // built-in layers; default allowlist.Base() alone
    Components map[string]string    // provision files added or replaced
    Guest      guest.Config         // the track's own guest settings, every start
    BrokerMode string               // "server" (default) or "proxy": a new sandbox's mode without --broker
}
```

Start from `jig.Personal()` and change fields, or build one from
scratch. The library's pieces are all exported for reuse:

| Axis | Interface | Library's own |
|---|---|---|
| Providers | `providers.Provider` (broker) + `jig.ProviderCLI` (login) | `jig.GitLab`, `jig.GitHub`, `jig.Claude`, `jig.AWS`, `jig.NPM` |
| Agent | `agent.Agent` | `agent.ClaudeCode{}`, `agent.KiroCLI{}` |
| Editor | `editor.Editor` | `editor.VSCode{}` |
| VM runtime | `sandbox.Backend`, `sandbox.Guest` | `sandbox.Microsandbox{}` |
| Allowlist | YAML layers | `allowlist.Base()` |
| Provisioning | shell components | `guest.Provision()` |

Each section below says what one axis does and how to change it.

## Providers

A provider is a service the agent reaches only through the broker, which
holds the user's credential on the Mac and checks each request. It has
two halves.

### The broker half: `providers.Provider`

```go
type Provider interface {
    Name() string                 // its allowlist group and login name
    Hosts(proxy bool) []string    // names it's served under
    Handlers(ctx context.Context, b *providers.Builder) (map[string]http.Handler, error)
    Guest(proxy bool) guest.Config
}
```

- **Name** is also the allowlist group whose hosts leave the allowlist
  while the broker serves it. Define that group in the track's layer
  with `provider: <name>` (below).
- **Hosts**: in server mode (the default) a name under
  `providers.InternalSuffix` (`example.broker.internal`); the library maps
  it to the broker in the guest's `/etc/hosts`. In proxy mode
  (`docs/transparent-broker.md`) the service's real hosts, or the
  internal name again for an endpoint with no real host (AWS's).
- **Handlers** builds the route table. `b.Upstreams(host, login)` makes
  upstreams that drop the sandbox's credential headers and set the
  user's; `b.Provider(name, routes)` serves the table, logging each
  decision. A request matching no route is denied.
- **Guest** is what the sandbox needs: the tools' environment (with a
  placeholder token), their config files, git's `insteadOf`
  (`providers.InsteadOf`), and the provider's section of the agent's
  instructions (Markdown). Nothing secret goes here.

A route is a method, a path pattern, a check and a target:

```go
func Routes(up broker.Upstreams, tok broker.Token) []broker.Route {
    api := up.To("https://api.example.org", func(h http.Header) error {
        t, err := tok()
        if err != nil {
            return err
        }
        h.Set("Authorization", "Bearer "+t)
        return nil
    })
    return []broker.Route{
        {Method: "GET HEAD", Path: "/v1/{path...}", Op: "example.read", Check: broker.Read, To: api},
        {Method: "POST", Path: "/v1/items/{id}/comments", Op: "example.comment", Check: myCheck, To: api},
    }
}
```

`broker.Read`, `broker.Any`, `broker.Never(reason)` are the simple
checks; a `broker.Check` can read the body (`Call.ReadBody`), look
things up upstream and return `broker.Allow()` or `broker.Deny(...)`.
The shared rules (the agent's branch namespace) are in
`broker/policy.go`. Read a library provider before writing one:

- `broker/anthropic`: the smallest, custody of a credential only.
- `broker/gitlab/mcp.go`: a **hosted MCP server**, deciding each
  `tools/call` by tool name and arguments, and filtering `tools/list`
  (`broker/mcp`). A vendor's hosted MCP server is a provider like this.
- `broker/github`, `broker/gitlab`: REST and GraphQL with per-request
  checks, and git's smart HTTP (`broker/gitproto`).
- `broker/providers/*.go`: the `Provider` wrappers around those.

Test a route table with `broker/brokertest`, as the library's do.

The library's providers may serve a work track as they are:
`providers.GitHub` takes a host (`login github --host`, API at
`api.<host>`; GitHub Enterprise Cloud with data residency has that
shape, *unverified*), and `providers.GitLab` a URL. GitHub Enterprise
Server (API under `/api/v3` on the web host) doesn't fit yet.

`providers.GitHub.Owners` limits the user's credential to some owners'
repositories (an organisation's): other owners' are read without it and
never written (`broker/github/owners.go`, which lists what counts, and
the limit of GraphQL's check). A track sets it by wrapping
`jig.GitHub.Broker`:

```go
gh := jig.GitHub
gh.Broker = func(s jig.Settings) providers.Provider {
    p := jig.GitHub.Broker(s)
    if p == nil {
        return nil
    }
    g := p.(providers.GitHub)
    g.Owners = []string{"example-org"}
    return g
}
```

`providers.NPM` serves npm registries read-only, each with its own token
command and scopes (`broker/npm`); `jig.NPM` logs in to
registry.npmjs.org with a granular token in the Keychain (`npm login`'s
last two hours). A track adds a registry (another host, for `@scope`) the
same way, wrapping `jig.NPM.Broker`, and defines its host's allowlist
group with `provider: npm`. In server mode a project's `.npmrc` naming a
registry's real URL for a scope bypasses the guest's settings; proxy
mode doesn't have that gap (`Track.BrokerMode: "proxy"`).

`providers.MCPServer` serves a vendor's hosted MCP server (streamable
HTTP, a Bearer token) with the agent limited to a list of tools, which
`tools/list` is filtered to (`broker/hostedmcp`). Tools are decided by
name alone, so list only those that read. A track makes one per server,
with its own `ProviderCLI`, whose token command is the vendor's CLI (an
OAuth access token it refreshes) or a Keychain item:

```go
providers.MCPServer{
    ID:       "example",                        // login example; no allowlist group needed
    Title:    "Example",                        // in the agent's instructions
    URL:      "https://mcp.example.org/v1/mcp",
    Tools:    []string{"search_items", "get_item"},
    TokenCmd: "example-cli auth token",
}
```

The guest gets the URL as the sandbox reaches it in `$EXAMPLE_MCP_URL`
(`example.mcp.broker.internal` in server mode, the real host in proxy
mode) and mcpcli, with instructions to call it as
`MCP_GATEWAY_URL=$EXAMPLE_MCP_URL mcpcli …`. Only the endpoint's POST,
GET and DELETE are served, without URL parameters; OAuth discovery
isn't.

`providers.Jira` serves one Jira Cloud site's REST API read-only
(`broker/jira`): GETs under `/rest/api/2|3/` and `/rest/agile/1.0/`, and
the POSTs that only read (searches, bulk fetches, JQL and expression
checks). The sandbox calls it as the site (`$JIRA_URL`:
`jira.broker.internal`, or the site's host in proxy mode); the broker
sends each call to Atlassian's gateway (`api.atlassian.com/ex/jira/<cloudId>`),
where scoped API tokens work, with Basic `email:token`. `jig.Jira` is
its login: the site, its cloud ID (from the site's public
`/_edge/tenant_info`), the account's email, and a scoped read-only token
in the Keychain (`mono-jig-jira`), checked with `GET /rest/api/3/myself`
before it's stored.

`jig.OAuthMCP` is a hosted MCP server's login for a server no vendor CLI
holds a token for: the harness is the OAuth client (`oauth/`). `login
<id>` finds the authorization server from the server's 401 (RFC 9728,
RFC 8414), registers a public client once (RFC 7591, a loopback
redirect on a port kept for later logins), signs in in the Mac's browser
(authorization code, PKCE S256, `resource`), keeps the refresh token in
the Keychain (`mono-jig-<id>`) and the client in `settings.json`, then
lists the server's tools against the track's list. Its broker provider
is a `providers.MCPServer` whose token is an `oauth.Source`: the access
token in memory until a minute before it expires, then refreshed, a
rotated refresh token written back in place, with a file lock so two
sandboxes' brokers never spend one refresh token twice.

```go
jig.OAuthMCP{
    ID:     "example",
    Title:  "Example",
    URL:    "https://mcp.example.org/",
    Scopes: []string{"read:all", "offline_access"}, // read scopes, and a refresh token
    Tools:  []string{"search_items", "get_item"},
}.CLI()
```

Servers that make confidential clients (a client secret) aren't
supported. Logout removes the refresh token; the registered client stays
at the authorization server.

### The login half: `jig.ProviderCLI`

```go
jig.ProviderCLI{
    Name:     "example",                // login example; its key in settings.json
    Keychain: "mono-jig-example",       // its Keychain item, if any: logout and uninstall delete it
    Usage:    "login example           store a token for Example\n",
    Login:    func(args []string) { ... },
    Status:   func(s jig.Settings) string { ... },  // "" if not logged in
    Forgotten: func(s jig.Settings) string { return "forgotten" },
    Broker:   func(s jig.Settings) providers.Provider { ... }, // nil if off
}
```

- **Login** asks and stores: `jig.Ask`, `jig.ReadSecret`,
  `jig.KeychainStore`, `jig.Fail`. Non-secret settings (a user name, a
  URL) go in `settings.json` with `s := jig.LoadSettings()`,
  `s.SetProvider(name, v)`, `jig.SaveSettings(s)`.
- **Broker** turns the settings into the provider, or nil.
  `jig.ProviderSettings[T](s, name)` reads them back. The credential is
  a **token command** the broker runs (at most once a minute):
  `jig.KeychainCommand(item)`, or the user's own CLI (`glab auth token`,
  a password manager's, an SSO helper's). Allow an environment variable
  override (`JIG_<NAME>_TOKEN_COMMAND`) for the checks, as the library's
  do.
- `Usage` lines after the first are indented 24 spaces.

`jig/login_claude.go` is the smallest example; `login_gitlab.go` reads
the user's own CLI login.

## Agent

```go
type Agent interface {
    Name() string
    Components() map[string]string   // provision files that install it
    Guest(b providers.Config, instructions []string) guest.Config
    EditorExtensions() []string       // installed on the remote by the editor
}
```

- **Components**: `components/NN-<name>.sh`, run at the image build and
  a sandbox's first start (`agent/70-claude.sh`). See
  [Provisioning](#base-image-and-provisioning).
- **Guest**: its environment and settings for the providers that are on
  (`b.Get("anthropic")`), and the instructions, already rendered
  (a header, then each provider's section, then the track's), written
  where the agent reads them. Claude Code's go in `~/.claude/CLAUDE.md`.
- Its hosts (installer, sign-in) go in the track's allowlist.

`agent.KiroCLI` is kiro-cli (`agent/70-kiro.sh`: a pinned release,
checked by SHA-256, in `/usr/local/bin`): its instructions in
`~/.kiro/steering/mono-jig.md`, every capability allowed in
`~/.kiro/settings/permissions.yaml`, self-update off. It signs in inside
the sandbox (`kiro-cli login --use-device-flow`), so its credential is
the sandbox's; holding it in the broker, like `anthropic` for Claude
Code, is *unverified*. Its hosts (`cli.kiro.dev`,
`prod.download.cli.kiro.dev`, `app.kiro.dev`, `*.us-east-1.kiro.dev`,
`q.us-east-1.amazonaws.com`, and the sign-in's) go in the track's
allowlist. That kiro-cli always includes a steering file without front
matter is *unverified*.

## Editor

```go
type Editor interface {
    Name() string
    Find() (string, error)                                   // its command, or how to get it
    Configure(root string, p editor.Profile) error           // write its profile's settings
    Install(root string) error                               // at setup: its remote extension
    Open(root string, p editor.Profile, host, dir string) error
}
```

The editor runs on the Mac as its own instance (settings and extensions
under the state directory) and reaches the sandbox over SSH, with the
SSH config and host names in `Profile`. `editor.VSCodeSettings` holds
the settings that close the editor's credential bridges
(`docs/design.md`); an editor forked from VS Code may reuse them.
`editor.Kiro` is Kiro IDE 1.5 with its built-in Remote SSH (remote
authority `kiro-ssh`): `kiroRemote.sshConfigFile` points it at a
flattened copy of the SSH config (`editor.FlattenSSHConfig`), and
`editor.KiroSettings` holds the VS Code settings Kiro has. Kiro
downloads its server from the guest, so a track using it allows Kiro
IDE's download and sign-in hosts. The settings and flags were read from
Kiro 1.5.11's app bundle and code (2026-11-25); `--remote` with
`editor.KiroAuthority` (`kiro-ssh+config-<alias in hex>`) opens a
configured host.
An editor that implements `editor.GuestSSHD` (Kiro) gets OpenSSH's sshd
in the image and every sandbox (`jig/sshd.go`): its Host block reaches
the guest's sshd with `ssh -W 127.0.0.1:22` through a second block for
msb's SSH (`<state>/msb_ssh_config`, which the editor's config doesn't
include), because Kiro needs Unix-socket forwards and `msb ssh serve`
refuses them.

## VM runtime

`sandbox.Backend` creates, starts and removes sandboxes from a
`sandbox.Spec` (image, sizes, environment, egress allowlist, mounts,
disks, the vsock route to the broker, files, outbound proxy);
`sandbox.Guest` is a running one (files, root scripts, stop). Only
`sandbox.Microsandbox` exists. A few commands still call microsandbox
directly: `denied` (its log), `cp` (`msb copy`), SSH (`msb ssh`),
`setup` (`msb doctor`, `msb pull`) and loading images (`msb load`).
Another runtime needs those behind the interface first.

## Base image and provisioning

Provisioning is shell components in `guest/provision/components/`, run
by `provision.sh` in name order; each runs only when named. The image
build (`mono-jig image build`) bakes `base git docker` and the added
ones into the base image; a sandbox's first start runs what the image
lacks; every start runs `ca` (extra CAs) and `apply` (`guest.Config`).

| Component | Does | Debian-specific |
|---|---|---|
| `10-ca` | trusts the extra CAs (`mono-jig ca`) | `update-ca-certificates` |
| `20-base` | packages, the sandbox user | apt, `useradd` |
| `30-sshd` | sshd (sbx only) | apt |
| `40-git` | git, gh, glab | apt |
| `50-broker` | trusts the broker's CA | `update-ca-certificates` |
| `60-docker` | Docker | apt |
| `90-apply` | applies `guest.Config` | apt for `Packages` |

`Track.Components` adds or replaces files by path:

- **A new file** (`components/65-tools.sh`) is a new component, named
  from the file (`tools`), run at the image build and first start after
  the library's, in name order with the agent's.
- **A replaced file** (`components/20-base.sh`, for a base image without
  apt) runs when the library's would.

A component is sourced by `provision.sh` as root, with `lib.sh`'s
helpers. It starts `if want <name>; then`, logs with `log`, and must be
idempotent. `$SANDBOX_USER` is the agent's user. The provision files'
hash is part of the base image's tag, so changing a component rebuilds
it at the next `image build`; sandboxes already created keep theirs
until removed.

`BaseImage` is what the base image builds on, Debian or Ubuntu unless
`base` is replaced; the first start also installs `socat` with apt
before running anything (`jig/provision.go`), so a non-Debian base needs
that changed too. A user's own image goes on top (`image init`, `image
build -f`).

## Guest settings

`Track.Guest` is a `guest.Config` applied at every start, after the
providers': `Env` (package mirrors such as `GOPROXY`, `PIP_INDEX_URL`),
`Packages` (apt), `Files` (text, a link, or JSON merged in), `Git`, and
`Instructions` (a section of the agent's instructions, after the
providers'). Environment values can't contain spaces or quotes. Files
with a `Marker` first line are rewritten each start and removed when
no longer listed. Use it for settings that change without an image
rebuild; use a component to install software.

## Allowlist

Everything the sandbox reaches directly, beyond the broker, is on the
allowlist. It's built from layers, each a YAML file:

```yaml
description: Example Corp
groups: [go, docs]                 # turn on groups (allowlist/lists/*.yaml, or defined below)
hosts: [artifacts.example.org]     # add hosts ("*.suffix", ":80")
define:
  example:                         # a provider's group: leaves the allowlist while the broker serves it
    description: Example's API
    provider: example
    hosts: [api.example.org]
```

`Track.Allowlist` is the built-in layers, in order; then come the
files in `JIG_LAYERS`, then the user's `~/.mono-jig/allowlist.yaml`.
Keep the library's base and add an organisation's layer:

```go
//go:embed org.yaml
var orgYAML []byte

Allowlist: []allowlist.Layer{allowlist.Base(), allowlist.MustParseLayer("org", orgYAML)},
```

or leave `allowlist.Base()` out for a stricter base: it turns on sbx's
balanced groups and the personal providers' and Claude Code's. The
groups in `allowlist/lists/` stay available either way. `mono-jig
allowlist` prints the result; `mono-jig denied <name>` shows what was
refused.

A provider's group's hosts leave the allowlist while the broker serves
it, from every group and layer that lists them exactly; a wildcard
elsewhere stays (the `aws` group's `*.amazonaws.com` leaves, but a layer's
exact `q.us-east-1.amazonaws.com` doesn't).

## Corporate CA

Not a track field: it differs per Mac. `mono-jig ca <pem>...` records a
TLS-inspecting proxy's root; it's trusted at the image build and every
start, with `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE` and
`NODE_EXTRA_CA_CERTS` set. On the Mac, Go verifies through the System
keychain; the AWS MCP proxy under uv may need `UV_NATIVE_TLS=1`
(both *unverified*). See `docs/library-split.md`, Step 3.

## Fixed for now

What a track can't change without a library change:

- **The name `mono-jig`**: the state directory (`~/.mono-jig`, or
  `JIG_STATE`), the Keychain items' prefix, the SSH host prefix, image
  tags, `/etc/mono-jig`, the broker CA's subject, the usage and refusal
  text, and the instructions' header. Harmless on a Mac with one track;
  two on one Mac would need a `Track.Name`.
- **The agent's namespace**: the agent pushes only to
  `agent/<user>/…` (`broker/policy.go`).
- **The instructions' header** (`guest/instructions/header.md`).
- **The Debian assumptions** listed under [Provisioning](#base-image-and-provisioning).
- **The microsandbox calls** listed under [VM runtime](#vm-runtime).

## Checklist for a new track

1. A repository and `cmd/<name>/main.go` as above, building with
   `jig.Personal()` unchanged.
2. `mono-jig ca` if the network inspects TLS; `setup`; `image build`.
3. The allowlist layer: the organisation's package mirrors and hosts.
4. `Track.Guest`: mirror settings for the package managers.
5. Providers: the library's where they fit (GitHub with a host), then
   one new provider at a time, each with tests from `brokertest`.
6. The agent and editor, if not Claude Code and VS Code.
7. Components for tools that need installing.
8. `mono-jig open` a sandbox; `mono-jig denied` for what's missing.

## Sources

- This repository: `jig/track.go`, `jig/registry.go`, `jig/login.go`,
  `broker/providers/providers.go`, `broker/broker.go`, `agent/agent.go`,
  `editor/editor.go`, `sandbox/sandbox.go`, `guest/config.go`,
  `guest/provision/`, `allowlist/layers.go`.
- `docs/library-split.md`: how the seams were made, and the open
  questions about Kiro and kiro-cli.
