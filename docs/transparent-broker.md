# A transparent broker for code forges

Status (2026-11-24): a design, from reading microsandbox 0.7.16's source
and docs (the version `mono-jig` uses) and sbx v0.54.0's help text. The
microsandbox hook was tested on the user's Mac with a toy proxy on
2026-11-24 (see "What we checked"). Proxy mode is built, beside server
mode, as `mono-jig up|open <name> --broker proxy` (2026-11-24; see
"Proxy mode as built"); since 2026-11-25 it serves AWS at its own host
too, with no vsock route ("AWS at its own host"). Letting the broker decide all egress, with
microsandbox allowing everything but a few fixed rules, was investigated
and tested on 2026-11-24 but not built (see "Egress decided by the
broker").
"Verify" marks what's open, and *unverified* marks claims not confirmed
in a source.

**Verdict:**

- **microsandbox** can do this with features it already has. Its
  outbound proxy sends every guest TCP connection to a process on the Mac,
  after its own policy has passed it. If the broker *is* that proxy, it can:
  - take the connections whose TLS SNI is a forge host;
  - terminate them with its own CA;
  - apply its routes and inject the credential;
  - splice everything else to the destination microsandbox already checked.

  The guest needs one change: trust the broker's CA. We already install
  that CA today. It also needs no `/etc/hosts`, socat, `insteadOf`,
  `GH_HOST` or session token.
- **sbx** might do it through its global upstream-proxy setting, which
  accepts a PAC file. But whether its proxy hands us the guest's TLS or its
  own is unverified, the setting covers every sandbox, and the broker
  can't tell sandboxes apart. Worth one experiment, not a plan.
- **Rules** can become declarative once they're split three ways:
  - decoders, written in Go: they read a request the way the provider
    does;
  - lookups, declarative: fetches from the provider;
  - CEL expressions over both.

  The decoders are the part that matters most for security, and they stay
  code in a library.

## The goal

Intercept traffic to GitHub and GitLab, restrict routes and request
content (for example, fetch a PR to read its title before allowing an
update), and inject the user's credential. And do it as a feature of the
sandbox's network, so that the guest uses `github.com` and `gitlab.com`
as usual:

- remotes stay `https://github.com/owner/repo`;
- `gh` and `glab` use their default hosts;
- nothing in the guest names the broker.

The only guest change allowed is trusting a CA.

## What microsandbox already has

Read in `crates/network/lib` and `docs/` at v0.7.16.

| Feature | What it does | Where |
|---|---|---|
| Outbound proxy | One HTTP CONNECT, SOCKS4 or SOCKS5 proxy for all of a sandbox's outbound TCP. The address is `IP:port`, so a Unix socket isn't possible. Go SDK: `WithProxy(HTTPConnectProxy(addr))`. | `docs/networking/outbound-proxy.mdx`, `proxy/`, `engine/tcp/upstream.rs` |
| Proxy credentials | HTTP Basic on the CONNECT, with the password read from a *host* environment variable when the sandbox starts (`.Credentials(user, SecretSourceEnv(...))`). A 407 fails the connection; it's never retried without credentials. | `proxy/http_connect.rs` |
| Policy before the proxy | "Network policy is evaluated against the sandbox's actual destination before the proxy connection is opened." In the code, the SNI peek and the domain-rule check run first, then `connect_target.connect(…, outbound_proxy)`. Both the TLS bypass and the interception paths dial through the proxy too. | `engine/tcp/proxy.rs` `try_run`, `engine/tls/proxy.rs` `bypass_relay`, `intercept_relay` |
| What the proxy receives | `CONNECT <ip>:<port>`, the address microsandbox resolved and checked, never a name. Then the guest's bytes. Without interception, that's the guest's own ClientHello, SNI included. | `docs/networking/outbound-proxy.mdx` |
| Not proxied | DNS (microsandbox's forwarder answers it) and `host.microsandbox.internal`. HTTP CONNECT blocks non-DNS UDP, so QUIC falls back to TCP. | the same |
| SNI and DNS binding | A domain rule matches only if this sandbox resolved that name to that IP. A forged SNI to another IP fails. Intercepted HTTP must also have `Host` or `:authority` equal to the SNI. | `docs/security/network.mdx` ("SNI spoofing") |
| Strict mode | With strict on, TLS allowed by a hostname rule is denied unless microsandbox intercepts it (`strict_hostname_allow_is_opaque`). That's why `mono-jig` runs with strict off and without interception (`microsandbox.md`). | `engine/tcp/proxy.rs` |
| TLS interception | Its own CA, or one you give it (`TLSConfig.CACert`, `CAKey`), which microsandbox adds to the guest's trust store. Bypass patterns. Upstream verification against the Mac's roots, plus `ScopedUpstreamCACerts`: a CA trusted only for matching hosts. | `docs/networking/tls.mdx`, Go SDK `options.go` `TLSConfig` |
| Secret placeholders | The guest holds `$MSB_<VAR>`, and the real value replaces it on the way out to allowed hosts, gated on SNI, the DNS binding and Host. They can't express a rule per operation. | `docs/security/secrets.mdx` |
| Host sockets | A vsock port that leads to a Unix socket on the Mac. The guest has to speak `AF_VSOCK` itself, hence our socat bridge today. | `docs/networking/host-sockets.mdx` |

It has **no** custom DNS records, so `gitlab.com` can't be pointed
elsewhere. It has no "route this host to that socket" rule, and no
callback into our code for each request. None of these is needed: the
outbound proxy is the hook.

## The design on microsandbox

```
guest                         msb machine (Mac)                          broker (Mac, same process as the VM's owner)
git / gh / glab ──TLS──▶  DNS + policy + SNI check ──CONNECT ip:443──▶  CONNECT front end, Basic auth
 (trust broker CA)         strict off, no interception                    │
                                                                          ├─ SNI is brokered (gitlab.com, api.github.com…):
                                                                          │    terminate TLS (broker CA) → routes → credential → provider
                                                                          └─ anything else: splice to ip:443 unchanged
```

1. **The supervisor starts the broker's CONNECT front end** on
   `127.0.0.1:<random port>`, one per sandbox, and makes a random password.
   It passes both to `CreateSandbox`:
   `WithProxy(HTTPConnectProxy(addr).Credentials("jig", SecretSourceEnv("JIG_PROXY_PASSWORD")))`.
   The password lives in the supervisor's environment, never in the guest.
   Whether the `msb machine` child reads it from the supervisor's
   environment is *unverified*.
2. **The policy stays as today:** strict off, no interception, and the
   allowlist compiled into rules. Brokered hosts move from "dropped from
   the allowlist" to "allowed". microsandbox has to allow them so the guest
   can resolve them and so the connection reaches the proxy. The broker
   then decides what's actually allowed.
3. **For each connection,** the front end:
   - checks the Basic credential (wrong or missing: 407 and close);
   - reads the ClientHello's SNI;
   - if the SNI is brokered, terminates TLS with a certificate from the
     broker's CA and serves the request to `broker.Server`, keyed by the real
     hostname;
   - if the SNI names another host, opens a TCP connection to the `ip:port`
     in the CONNECT and copies bytes both ways. microsandbox has already
     applied its policy to that IP and SNI;
   - if there's **no SNI**, refuses the connection. microsandbox lets a
     connection with no SNI through to any IP the sandbox resolved
     an allowed name to, so a bare `https://<github's IP>` would otherwise
     reach GitHub without the broker. A token pasted into the guest would
     work there (checked; see below).
4. **The broker's own upstream connections** go out directly from the Mac
   (`NewUpstreamClient`, `HTTPS_PROXY`-aware), as they do today.

**Identity.** The port is loopback TCP, so any process on the Mac can
connect. But only microsandbox has the password, and the password
identifies the sandbox. That replaces the socket path's file permissions
with a secret held on the Mac. It's weaker than the 0600 socket only
against processes of the same user that can read the supervisor's
environment, and those can already read its memory.

**Failing closed.** If the broker is down, the CONNECT is refused and the
guest gets connection refused, for *all* egress, not just the forges.
That's safe, but louder than today, where a dead broker cost only the
forges. The broker already lives in the process that owns the VM, so
they stop together.

**The CA.** It's the broker's CA as now, name-constrained to the brokered
hosts instead of `*.broker.internal`: `github.com`, `api.github.com`,
`gitlab.com` and any other host we broker. That's still a CA the guest
trusts only for those names. The trust stores that need it (system,
Node's `NODE_EXTRA_CA_CERTS`, and so on) are the ones `provision.sh`
already fills.

### What we checked (2026-11-24)

On the user's Mac, with microsandbox 0.7.16 through the CLI:
- a throwaway `alpine/curl` sandbox, deny by default, strict off, no
  interception;
- `--proxy http://127.0.0.1:18990` with `--http-connect-username` and
  `--http-connect-password-env`;
- a toy CONNECT proxy, about 250 lines of Go. It checks the Basic
  password, logs the CONNECT target and the SNI (or the HTTP `Host`), and
  terminates TLS for `github.com` with its own CA, answering itself.
  Everything else it splices.

The sandbox and the proxy were removed afterwards.

| Check | Result |
|---|---|
| Allowed HTTPS, `api.github.com` (spliced) | 200. The proxy saw `CONNECT 140.82.114.6:443`, an IP not a name, then the guest's ClientHello with `sni="api.github.com"` |
| Brokered HTTPS, `github.com` with the toy CA trusted (`--cacert`) | Terminated. HTTP/2 by default (ALPN `h2`), HTTP/1.1 with `--http1.1`. The proxy saw `Host: github.com`, the path, and the guest's own `Authorization` header (for the real broker to strip) |
| The same without trusting the CA | curl refuses the certificate, as it should. The guest must trust the broker's CA |
| git (`git ls-remote https://github.com/…`, with `GIT_SSL_CAINFO`) | Reached the broker: `GET /owner/repo/info/refs?service=git-upload-pack` over h2, `git/2.54.1`, no credential |
| `apk add git` (HTTPS to `dl-cdn.alpinelinux.org`) | Spliced, and worked |
| Plain HTTP, `http://example.com` | Spliced. The proxy saw `CONNECT <ip>:80` and `GET / HTTP/1.1`, `Host: example.com` |
| A denied host (`gitlab.com`) | Reset in the guest. **Never reached the proxy** |
| Forged SNI: `github.com` sent to proxy.golang.org's IP, and the reverse | Both reset by microsandbox. Never reached the proxy |
| HTTP domain fronting: `Host: github.com` to `example.com`'s IP over port 80 | Empty reply. microsandbox opened the CONNECT, then sent nothing and closed |
| HTTPS to a GitHub IP the sandbox never resolved, no SNI | Reset. Never reached the proxy |
| **HTTPS to the IP the sandbox resolved `github.com` to, no SNI** | **Allowed.** The proxy saw `sni=""`, the toy spliced it, and GitHub answered 301 to a request carrying a "pasted" token. Documented behavior ("connections without SNI still need a live binding"). Hence the broker's no-SNI rule above |
| Proxy down | Every connection reset. Fails closed |
| Wrong password (proxy restarted with another) | The proxy answered 407; the guest saw a reset. Fails closed |

Then through the Go SDK (2026-11-24, SDK v0.7.16), a few lines that
create an `alpine:3.20` sandbox with
`WithProxy(HTTPConnectProxy(addr).Credentials("jig", SecretSourceEnv("JIG_PROXY_PASSWORD")))`
and run `wget https://api.github.com/zen` in it:

| Check | Result |
|---|---|
| The password set in the creating process's environment | `msb machine` read it: the toy proxy saw the CONNECT, spliced it, and `wget` printed GitHub's zen |
| The wrong password in that environment | The proxy answered 407; `wget` failed (`Connection reset by peer`) |

So the supervisor sets the password in its own environment before it
creates or starts the sandbox.

Still open:
- UDP other than DNS. HTTP CONNECT blocks it, per the docs; we didn't test
  what breaks. (Later checked: none gets out; see "Egress decided by the
  broker".)

### The variant with strict mode on (not recommended)

If strict mode is ever wanted, microsandbox must intercept every
hostname-allowed TLS connection. Then:

- the guest trusts microsandbox's CA, which it installs itself, so the
  guest needs no change at all;
- microsandbox opens its own TLS to the upstream through our proxy, which
  sees `CONNECT ip:443` plus microsandbox's ClientHello;
- for brokered names, the broker terminates that TLS with a certificate
  that microsandbox verifies through
  `ScopedUpstreamCACerts{gitlab.com: broker-ca.pem}`.

The costs are those `microsandbox.md` lists for interception: every
client trusts a CA that can sign any name, pinned clients must be
bypassed, there's no QUIC, and #2013 is open. It also adds a second TLS
hop, and microsandbox would see every request in plaintext.
*Unverified:* that a scoped upstream CA works through the outbound
proxy, and which ALPN microsandbox offers upstream.

## What changes in the broker

**Removed:**

- `*.broker.internal`, `/etc/hosts`, the socat bridge and the vsock route
  for the forges;
- git `insteadOf`, `GH_HOST`/`GITLAB_HOST`, and the MCP URL pointing at the
  broker;
- the session token in `mono-jig-sbx`;
- rewriting `Location` headers and LFS `href`s to the broker's host
  (stripping the LFS `header` stays);
- T3 in `enforcement.md`, a token the agent pastes in: it gets stripped
  like any other credential, so that case closes.

**Added:**

- **The CONNECT front end:** Basic auth, an SNI peek, then a dispatch
  that terminates or splices. A few hundred lines.
  - Plain HTTP (`CONNECT ip:80`): read the `Host` header. A brokered host
    gets 403, because the credential is never sent in cleartext. Anything
    else is spliced.
- **Providers keyed by real hostnames.** `broker.Server` already checks
  that `Host` matches the provider. That stays, and the SNI must equal the
  `Host`.
- **A third outcome, anonymous forwarding.** Today `github.com` is allowed
  directly for public clones and Go modules. Once it's brokered, every
  such request reaches the broker. Unmatched reads must be forwarded
  *without* the credential, rather than denied, or `go get` and public
  clones break. Writes that don't match stay denied. This decision
  belongs in each provider's route table, with a default stated at the
  top.
- **The full list of forge hosts.** Each must be brokered, allowed
  anonymously, or denied:

  | Host | Choice |
  |---|---|
  | `github.com` | brokered (git, web) |
  | `api.github.com` | brokered (REST, GraphQL) |
  | `uploads.github.com` | brokered (release assets) |
  | `codeload.github.com` | anonymous (archives) |
  | `raw.githubusercontent.com` | anonymous |
  | `objects.githubusercontent.com` | anonymous (signed URLs) |
  | `gitlab.com` | brokered (git, REST, GraphQL, MCP) |
  | `registry.gitlab.com` | brokered later, or anonymous |

  *Unverified* that this list is complete. Signed-URL hosts remain the
  exfiltration channel AGENTS.md already notes.
- **HTTP/2.** `gh` negotiates h2 through ALPN, and Go's TLS server answers
  it. Routes and checks already work on `http.Request`, so they don't
  depend on the protocol version, but the tests should cover h2 too.

**The guest still needs** placeholder tokens for the CLIs that refuse to
start without one: `GH_TOKEN`, `GITLAB_TOKEN`, and for Claude Code
`CLAUDE_CODE_OAUTH_TOKEN`. They aren't secrets; the broker drops them.
Plain git needs nothing. The broker injects the credential, so git never
sees a 401 and never prompts. So the honest claim is "a CA and some dummy
variables".

**Anthropic and AWS** can move to the same path: `api.anthropic.com`
becomes brokered, with no `ANTHROPIC_BASE_URL`. The AWS MCP proxy, the
broker's own service with no real host behind it, can keep the vsock
route, or that route can stay just for it.

## sbx

From `sbx settings ls` and `sbx policy … --help`, v0.54.0:

- `proxy.sandbox` is the upstream proxy for sandbox egress: a URL, a PAC
  source, `system` or `direct`. It applies to **all** sandboxes and needs a
  daemon restart. A PAC file could send only the forge hosts to the broker
  and everything else `DIRECT`.
- sbx's proxy already intercepts TLS and injects `sbx secret` credentials.
  Its rules now include HTTP method, path and query (`--method`, `--path`,
  `--query`), but they're static: no lookups, no request bodies.
- Its policy help says some connections aren't decrypted. Which ones, and
  whether a forge host with no secret is one of them, is *unverified*.

How that could play out:

- **If sbx doesn't decrypt the forge hosts,** the guest's own TLS should
  arrive at our proxy through the tunnel. Then the broker works as on
  microsandbox, with sbx's CA in the guest plus ours.
- **If sbx decrypts them,** it opens its own TLS to the upstream through us.
  It would trust our certificate only if it uses the Mac's trust store.
  The `tls.allowNegativeSerial` setting "for TLS-inspecting proxies"
  suggests it does. That would mean adding a name-constrained CA to the
  Mac's System keychain. That's a host-wide change, and I wouldn't make
  it.

Either way, there's one broker for every sandbox, with no way to tell
them apart, and with sandbox-scoped policy left to sbx. Keep `mono-jig-sbx`
as it is unless the experiment below comes out cleanly.

## Declarative rules

Today a provider is a Go route table, `[]broker.Route{Method, Path, Op,
Check, To}`, with checks in Go. A rich rule needs three kinds of thing:

1. **Decoders (Go, a library).** They turn a request into facts, read the
   way the provider reads it:
   - REST params merged as Rails does, refusing `_method` and repeated
     keys;
   - JSON keys compared ignoring case;
   - GraphQL operations;
   - git receive-pack ref updates;
   - MCP tool calls;
   - GitLab quick actions.

   A rule is only as safe as the decoder that feeds it. This is the code
   that the 2026-11-23 review found the bypasses in, so it stays reviewed
   Go, one decoder per protocol.
2. **Lookups (declarative).** A named fetch from the provider with the
   user's credential, and the fields to keep from its response.
   `mrSource` and `sameProject` are already this shape.
3. **Rules (CEL).** A method and path pattern, a decoder, lookups, and a
   CEL expression over `req`, the lookups and `user`. CEL isn't
   Turing-complete, it's typed and side-effect free, and `cel-go` is
   mature. Kubernetes ValidatingAdmissionPolicy and Envoy's RBAC filter use
   it for this kind of check. OPA/Rego (the Envoy ext_authz + OPA pattern)
   is the alternative, but it's harder to read next to a route.

```yaml
provider: gitlab
hosts: [gitlab.com]
credential: {keychain: gitlab, header: private-token}
unmatched: {read: anonymous, write: deny}
routes:
  - match: PUT /api/v4/projects/{pid}/merge_requests/{iid}
    decode: rest-params
    lookup:
      mr: GET /api/v4/projects/{pid}/merge_requests/{iid} -> [source_branch, title]
    allow: >
      mr.source_branch.startsWith(ns) &&
      !has(req.params.merge_when_pipeline_succeeds) &&
      !req.quickActions
    reason: only the agent's own MRs, and no merging
  - match: POST /{group...}/{project}.git/git-receive-pack
    check: agent-refs          # a Go check, registered by name
```

A check too complicated for CEL, such as a push or a GraphQL mutation
table, stays Go, registered by name and referenced from the YAML. The
file is still the one place that lists every route of a provider, which
keeps the guardrails easy to find.

### As a library

- **`broker` (the core):**
  - `Server` with its shared checks;
  - `Route`, `Call`, `Decision`;
  - the decoders;
  - credential custody (`Token`, `Upstream`);
  - the CA;
  - the new CONNECT front end: `ServeProxy(l net.Listener, password
    string, providers map[string]http.Handler, splice func(ip) bool)`.
- **`broker/rules` (the loader):** builds `[]Route` from YAML+CEL and the
  registry of named Go checks.
- **The microsandbox glue:** one function returning the `CreateSandbox`
  options (`WithProxy`, the policy rules for the brokered hosts) and the
  CA file the guest needs. Then any Go program that creates a microsandbox
  sandbox gets the broker with a few lines.

The library keeps the route table and the checks as the unit of review.
The YAML is a second way to write a table, not a second engine.

## Both modes, chosen at create (the plan, 2026-11-24)

Keep the broker as a server (today's mode) and add the CONNECT proxy as a
second mode, one per sandbox. The choice is a flag at create:
`mono-jig up --broker=server|proxy`, default `server`. microsandbox fixes
the network settings at create anyway (`msb modify` can't change them in
0.7.16). Server mode stays because `mono-jig-sbx` needs it, and nothing it
uses changes. The route tables and checks are shared: proxy mode is a new
front end and new host names, not a second broker.

Estimate: about 600–800 lines with tests, mostly new code beside the
broker.

| Piece | Server mode (today) | Proxy mode (new) | Size |
|---|---|---|---|
| Flag | default | `--broker=proxy`, saved in the sandbox's state like the mounts (`mounts.go` `saveMounts`) and read on every start | ~30 lines |
| Front end | `broker.ServeTLS` on `<state>/broker.sock` (`broker/listen.go`) | New `broker/connect.go`:<br>• the Basic password;<br>• the SNI peek;<br>• terminate brokered names, splice the rest;<br>• no SNI: refuse;<br>• plain HTTP: refuse brokered names by `Host`, splice the rest.<br>Terminated connections feed one `http.Server` (with `broker.Server`) through a channel listener, not one server per connection as in the toy. | ~250 lines + tests |
| Host names | `*.broker.internal` (`providers.go`: `GitLabHost`, …; `serve()` derives the provider's name from the suffix) | Real names, through an alias map in front of the same providers (below) | ~80 lines + tests |
| CA | name-constrained to `.broker.internal` (`ca.go`; `TLSConfig(hosts)`) | Constrained to the brokered real names | ~30 lines |
| Supervisor (`cmd/mono-jig/supervisor.go`) | vsock route to `broker.sock` (`WithVsock`, port `brokerPort`); the allowlist drops brokered hosts | Listen on `127.0.0.1:0`; `WithProxy(HTTPConnectProxy(addr).Credentials("jig", SecretSourceEnv(...)))` with a random password set in the supervisor's environment; brokered hosts *allowed* in microsandbox's policy | ~80 lines |
| Guest (`guest/provision.sh broker-env`, written from `supervisor.go`'s `export BROKER_…` lines) | `/etc/hosts`, socat, `insteadOf`, `GITLAB_HOST`, `ANTHROPIC_BASE_URL` | `BROKER_MODE=proxy`: skip those and set placeholder `GH_TOKEN` and `GITLAB_TOKEN`; GitLab's MCP URL becomes `https://gitlab.com/api/v4/mcp`; update the agent's instructions (`~/.claude/CLAUDE.md`) | ~50 lines of shell |
| AWS MCP | `aws.broker.internal` over vsock | Unchanged. It's the broker's own service with no real host, so proxy mode keeps the vsock route, `/etc/hosts` and socat for it alone | 0 |

### The parts that need care

1. **GitHub is one broker host and two real hosts.** Today
   `github.broker.internal` serves:
   - git directly (`/{owner}/{repo}/info/refs`, `git-upload-pack`,
     `git-receive-pack`, to `github.com`);
   - `/api/v3/...` for REST, to `api.github.com` with `Strip = "/api/v3"`;
   - `/api/graphql`, to `api.github.com/graphql`.

   This is in `broker/github/routes.go`. In proxy mode, `gh` calls
   `api.github.com/repos/...` and `/graphql`, without the prefix. Don't
   write a second route table: two tables drift, and doubling the routes
   doubles the review. Put an alias map in front:

   ```
   github.com         -> github provider, path unchanged
   api.github.com     -> github provider: "/graphql" -> "/api/graphql", otherwise prefix "/api/v3"
   gitlab.com         -> gitlab provider, path unchanged
   api.anthropic.com  -> anthropic provider, path unchanged
   ```

   The provider then sees exactly today's requests. The prefix is added
   before `broker.Server`'s path checks run, and those checks must still
   run on the rewritten path.

   (Built that way first, then replaced on 2026-11-24: the alias left
   one table serving both hosts, so in proxy mode `github.com/api/v3/...`
   reached the API with the credential, and the reader had to work out
   which routes applied in which mode. Now each mode builds only its own
   tables, from the same route lists with the path prefix as a
   parameter, so they can't drift; see "Proxy mode as built".)
2. **Rewriting `Location` must be off in proxy mode.** `Upstream`
   rewrites upstream `Location` headers to `Public` plus `Strip`
   (`broker/upstream.go`). Behind the alias map, that would produce
   `https://api.github.com/api/v3/...`. In proxy mode the upstream URLs are
   already what the guest should follow. GitLab's LFS `href` rewrite
   (`RewriteLFSBatch`, from `API.URL` to `API.Public`) is a no-op when
   `Public` is `https://gitlab.com`, but it needs `Public` set, not nil.
   Test every route under both naming schemes.
3. **Anonymous forwarding.** This is needed in proxy mode only: public
   clones from `github.com` and `gitlab.com` now reach the broker, and
   unmatched requests are denied today. Each provider gets a stated
   default at the top of its route table: unmatched reads are forwarded
   without the credential, and unmatched writes are denied. It's about 40
   lines in `broker`, but it's a policy change and has to be visible
   where reviewers look.
4. **The password through the Go SDK.** This is unchecked: whether the
   `msb machine` that the SDK starts reads `SecretSourceEnv` from the
   supervisor's environment. The CLI read it from its own environment
   (checked). If the SDK doesn't, proxy mode needs another way to hand
   over the password. Check this first.

## Proxy mode as built (2026-11-24)

`mono-jig up|open <name> --broker proxy` makes a proxy-mode sandbox;
without the flag it's server mode, as before. The mode is saved in
`<state>/broker-mode` and fixed at create: `up --broker server` on a
proxy-mode sandbox fails and says to `rm` first. `rm` also deletes the
broker's CA, so the next sandbox of that name gets one constrained to
what its mode needs.

| Piece | Where | What it does |
|---|---|---|
| Front end | `broker/connect.go` (`Proxy`) | Basic auth (user `jig`). Peeks the first bytes: TLS with a brokered SNI is terminated and handed to one `http.Server` (h2 and HTTP/1.1) through a channel listener; other SNI is spliced to the CONNECT's `ip:port`; TLS with no SNI, or a ClientHello longer than one record, is refused; plain HTTP whose first request has a brokered `Host` gets 403, or is refused if its headers can't be read; anything else, including server-first protocols after 3 s, is spliced. One log line per connection (`"msg":"connect"`) |
| Host names | `broker/providers` (`Config.Proxy`, `Hosts`) | The providers are keyed by their real names: the GitLab URL's host, `github.com`, `api.github.com`, `api.anthropic.com`. `aws.broker.internal` stays. GitHub builds only the active mode's tables (`github.ServerRoutes`, `ProxyRoutes`): in server mode one table under `/api/v3`, `/api/graphql` and git; in proxy mode `github.com` gets git and anonymous reads, `api.github.com` the API at GitHub's own paths. Both come from the same route lists, with the REST prefix as a parameter. (Until 2026-11-24 a path alias mapped `api.github.com` onto the server-mode table.) GitLab's paths are the same in both modes |
| Location | `broker/upstream.go` | `Upstreams.Public` is nil in proxy mode, so `Location` and `Link` aren't rewritten. GitLab's LFS `href` rewrite is skipped too; the `header` is still removed |
| Anonymous reads | `github/routes.go`, `gitlab/routes.go` | In proxy mode only, the last route (of `github.com`'s table, for GitHub) forwards a GET or HEAD that no earlier route matched to the provider without the credential (`anonymous.read`). Unmatched writes are still denied |
| CA | `broker/ca.go` | `NewCA`/`LoadOrCreateCA` take extra permitted domains. A kept CA must cover them, or the supervisor stops and says to `rm` |
| Supervisor | `jig/supervisor.go` (`cmd/mono-jig/` until 2026-11-24), `brokerhost.go`, `brokermode.go` | Listens on `127.0.0.1` on a port chosen at create and saved in `<state>/proxy-port`, since microsandbox keeps the proxy address with the sandbox. Makes a new password on every start and sets it in its own environment (`JIG_PROXY_PASSWORD`). The allowlist keeps the forge groups and adds the brokered hosts on 443; only AWS's group is dropped (`providers.SelfServed`). Since 2026-11-25 no vsock route, socat bridge or `/etc/hosts` names (below) |
| Guest | each provider's `Guest(proxy)` in `broker/providers`, and the agent's in `agent.ClaudeCode` (until 2026-11-24 `guest/provision.sh` with `BROKER_MODE=proxy`) | No `insteadOf`, `GH_HOST` or `ANTHROPIC_BASE_URL`. `GH_TOKEN`, `GITLAB_TOKEN` and `CLAUDE_CODE_OAUTH_TOKEN` are placeholders. `GITLAB_HOST` is the real host. glab's config names it |

Until 2026-11-25 the vsock route, `broker.sock`, socat and the
`/etc/hosts` line stayed in proxy mode for AWS alone, at
`aws.broker.internal`; see "AWS at its own host" below.

### End to end (2026-11-24)

On the user's Mac, with microsandbox 0.7.16:
- a proxy-mode sandbox from `ubuntu:26.04`, provisioned from scratch;
- the user's `gh` and `glab` logins;
- a dummy Anthropic key, since no Claude token was in the Keychain;
- no AWS.

Only reads and refused writes were sent to the forges. The sandbox was
removed afterwards.

| Check | Result |
|---|---|
| Provisioning (apt over HTTP, the Claude Code installer over HTTPS) | Worked, all spliced through the broker |
| Guest environment | `GH_TOKEN`, `GITLAB_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN` placeholders, `GITLAB_HOST=gitlab.com`; no `insteadOf`; `/etc/hosts` has only `aws.broker.internal` |
| `gh api user`, `gh repo view`, `gh pr list` (REST and GraphQL on `api.github.com`) | The user's login, through the alias. Re-run after the per-mode tables replaced it (below) |
| `git ls-remote`, `git clone` from `github.com`; `git ls-remote` from `gitlab.com` | Worked, with the credential injected |
| `curl` a GitHub archive URL | 302 to `codeload.github.com` (`anonymous.read`) |
| `curl -X POST https://github.com/login/oauth/access_token` | 403, "isn't a request the github provider serves" |
| `gh api -X PUT .../pulls/1/merge` | 403, "merging is never allowed through the broker" |
| `curl https://api.github.com/user` with a pasted `Authorization: token ...` | Answered as the user: the pasted token was dropped |
| `glab api user`, `glab repo view`; a GitLab raw file | The user's login; the raw file 200 (anonymous) |
| `curl https://api.anthropic.com/v1/messages` | Anthropic's `invalid x-api-key`: terminated, and the dummy key was injected |
| `claude -p hi` | Its calls (`/api/claude_cli/bootstrap`, `/v1/messages`) reached the broker and were forwarded through Node with the broker's CA trusted. "Not logged in", from the dummy key. *Unverified* with a real token |
| `https://proxy.golang.org/` | 200, spliced |
| `openssl s_client -noservername` to the IP `github.com` resolved to | Refused by the broker ("no server name") |
| `http://github.com/` | 403 from microsandbox's policy (`network_policy_denied`): port 80 isn't allowed, so it never reached the broker |
| Certificate the guest sees for `github.com` | Issued by `mono-jig broker CA (<name>)`; curl, git, OpenSSL, Go (`gh`, `glab`) and Node accepted it under the name constraint |
| `down`, then `up` without the flag | Same port, new password, `gh` and `glab` work again |
| `up --broker server` on the proxy-mode sandbox | Refused, with the `rm` hint |

Re-run on 2026-11-24 after the GitHub tables were split by mode, in
fresh sandboxes (one per mode, removed afterwards). The proxy-mode
checks above all gave the same results. New:

| Check | Result |
|---|---|
| `GET github.com/api/v3/user` (proxy mode) | 404 from github.com, sent as `anonymous.read`, without the credential |
| `POST github.com/api/graphql` (proxy mode) | 403: `github.com`'s table has no API |
| `POST api.github.com/<owner>/<repo>/git-upload-pack` (proxy mode) | 403: `api.github.com`'s table has no git |
| Server mode: `gh api user`, `gh repo view`, `gh pr list`, `git ls-remote`, `glab api user` | Worked through `github.broker.internal`; a merge and an unlisted read were refused |

A server-mode sandbox, made the same day with the same binary, still had
`GH_HOST`, `GITLAB_HOST` and `ANTHROPIC_BASE_URL` pointing at
`broker.internal`. `gh` and `glab` worked there too, and an unlisted read
was still denied (403).

Not covered: AWS in proxy mode, Claude Code with a real token, pushes
and merge-request writes (the routes are the same as server mode's and are
unit-tested), LFS, and UDP.

### AWS at its own host (2026-11-25)

Proxy mode now serves the AWS MCP Server under its own host,
`aws-mcp.us-east-1.api.aws` (`awsmcp.DefaultURL`), so the guest needs
no vsock route, socat bridge or `/etc/hosts` names. `aws.broker.internal`
had kept them for AWS alone, since microsandbox has no DNS records of its
own and its proxy receives `CONNECT <ip>:<port>`: a made-up name can't
reach the broker that way.

- `providers.AWS.Hosts(true)` is the real host. The broker terminates it
  like a forge host, then answers it itself through AWS's stdio proxy on
  the Mac (which keeps per-call `aws_profile`); it never forwards there.
  `$MCP_GATEWAY_URL` in the guest is the server's own URL.
- `providers.SelfServed` marks such a provider: its allowlist group
  (`aws`, `*.amazonaws.com`) still leaves in proxy mode, and only its
  host is added on 443.
- `providers.Config.Validate` refuses an internal name in proxy mode, and
  `Build` runs it.
- The supervisor creates a proxy-mode sandbox without the vsock route,
  doesn't listen on `broker.sock`, doesn't run the bridge job or install
  socat, and every start removes an older sandbox's `broker.internal`
  line from `/etc/hosts`.
- A proxy-mode sandbox made before this keeps its egress rules and CA,
  neither of which names the new host: the supervisor stops at the CA
  check and says to `rm`.

Unit-tested (`broker/awsmcp`, `broker/providers`, `jig`); checked in a
sandbox on the Mac on 2026-11-27 (below).

### End to end with every provider (2026-11-27)

On the user's Mac, with microsandbox 0.8.2 and msb-jig `c5ad4cb`
(mono-jig pinned to it):
- a new proxy-mode sandbox (the default), from an image built on the Mac
  with Docker (cached from an earlier run): `up` took 7s, of which the
  first start's `ca broker apply` took 2.6s;
- the user's GitHub, GitLab, Claude and AWS logins (`mono-jig login all`).

The user's own check script ran these, and all passed. Only reads and
one refused write were sent.

| Check | Result |
|---|---|
| Guest plumbing | No `broker.internal` line in `/etc/hosts`, no socat, no `broker.sock` on the Mac; no `GH_HOST`, `ANTHROPIC_BASE_URL` or `insteadOf`; the broker's CA trusted |
| `https://proxy.golang.org/` | 200, spliced |
| `github.com`: certificate, `gh api user`, `git ls-remote` | Issued by the broker's CA; the user's login; the credential injected |
| `gh api -X PUT .../pulls/2/merge` | 403 from the broker |
| `glab api user`, `git ls-remote` from GitLab | The user's login; the credential injected |
| `MCP_GATEWAY_URL` | `https://aws-mcp.us-east-1.api.aws/mcp`, AWS's own URL |
| `aws-mcp.us-east-1.api.aws`: certificate, `mcpcli tools` | Issued by the broker's CA; the `aws___` tools listed |
| `curl https://sts.amazonaws.com/` | Refused (`mono-jig denied` lists it) |
| `claude -p` with the broker's token | Answered |

`mono-jig denied` also listed `http-intake.logs.us5.datadoghq.com`,
Claude Code's telemetry (Datadog), refused because it's on no allow list.

Not covered: pushes and merge-request writes, LFS, UDP, and server mode
on 0.8.2.

## Egress decided by the broker (investigated 2026-11-24, not built)

The idea: in proxy mode, microsandbox allows nearly everything and the
broker decides. The allow list is then one the supervisor can change
while the sandbox runs (microsandbox fixes its rules at create, #1538),
and every denial is logged with its name, with no debug-log follower
(`docs/microsandbox.md`, #691). Counts per domain per period and the
most recent time would be worked out from `denials.jsonl` when asked.

### What microsandbox does today that we'd have to do

- **The name and the IP.** microsandbox allows a domain rule only for an
  IP the sandbox resolved that name to. With everything allowed, the
  guest can send `SNI: github.com` to any IP. The proxy would ignore the
  CONNECT's IP and dial the SNI (or `Host`) name itself, on the same port.
- **The Mac's own addresses.** The proxy dials from the Mac, so it must
  refuse loopback, private and link-local addresses, checked on the
  address it actually dials (a name can resolve to 127.0.0.1).
- **Connections with no name** (SSH, raw TCP): only an IP and port.
  Denied unless an IP or CIDR rule allows them.
- **DNS.** With everything allowed, every name resolves, and DNS is open
  as a channel out again, which is why the `AllowDNS` rule was removed
  (`docs/microsandbox.md`). The answer is a resolver of our own, below.
- **Encrypted ClientHello** shows only a front name, such as a CDN's.
  curl, Go and git don't send it.

### DNS through a resolver on the Mac

From microsandbox 0.7.16's source and docs:
- `NetworkConfig.DNS.Nameservers` takes `ip:port` entries, fixed at
  create. A query to the gateway (the guest's only nameserver) goes to
  them, from the Mac, not through the outbound proxy. A source comment
  expects the configured upstream "on the host's loopback"
  (`engine/dns/forwarder.rs`, `client_for`).
- A query aimed at another resolver (`dig @8.8.8.8`) is sent there
  directly if the egress rules allow that resolver's IP on port 53;
  otherwise microsandbox answers NXDOMAIN (`decide_upstream_with_platform`).
- Lookups are matched only by `*`, `host`, domain and domain-suffix rules,
  never by IP, CIDR or group rules like `public`
  (`model/policy/types.rs`, `evaluate_dns_query_inner`). So "deny `public`
  port 53" stops direct resolvers without touching gateway lookups.
- Rebinding protection (private addresses dropped from answers) still
  applies, which is harmless here.
- UDP datagrams are dropped when the proxy is HTTP CONNECT
  (`engine/udp/relay.rs`, `select_upstream`).
- A connection whose destination microsandbox rewrites (the host alias,
  `host.microsandbox.internal`) goes out directly, not through the proxy
  (`proxy/types.rs`, `select_for_destination`).

That gives these fixed rules (first match wins), with all name decisions
in the supervisor's resolver and proxy:

| # | Rule | Why |
|---|---|---|
| 1 | allow `host`, UDP and TCP, port 53 | gateway lookups reach our resolver |
| 2 | deny `host`, `loopback`, `private`, `link-local`, `metadata`, `multicast` | the Mac and the LAN; the host alias skips the proxy |
| 3 | deny `public`, UDP and TCP, port 53 | no resolver but ours |
| 4 | deny `*`, UDP | already dropped by HTTP CONNECT; stated anyway |
| — | default egress: allow | all other TCP goes to the proxy |

### What we checked (2026-11-24)

On the user's Mac, through the Go SDK v0.7.16: three throwaway
`alpine:3.20` sandboxes with the rules above, strict off, and the
nameserver set to a test resolver on `127.0.0.1:<port>`. The resolver
logged each question, answered NXDOMAIN itself for names containing
"blocked", and forwarded the rest to the Mac's resolver. A plain CONNECT
proxy logged and spliced, and a listener on the Mac's loopback stood in
for a local service. All were removed afterwards.

| Check | Result |
|---|---|
| Lookups through the gateway, UDP and TCP (`nslookup`, `dig`, `dig +tcp`) | Reached our resolver, which logged each name; the answers came back |
| A name our resolver refuses | The guest got our NXDOMAIN |
| `dig @8.8.8.8`, UDP and TCP | NXDOMAIN from microsandbox for a name that exists. Our resolver never saw it |
| HTTP and HTTPS (`wget`, `apk add`) | Looked up through our resolver, then `CONNECT <ip>:80` and `:443` through the proxy |
| Other TCP (`1.1.1.1:443`, `:8443`) | Through the proxy |
| TCP to `1.1.1.1:853` (DNS over TLS) | Refused by microsandbox, never reaching the proxy, though no rule names 853. *Why is unverified*; it looks like its DNS-over-TLS handling refusing without TLS inspection. Clients fall back to port 53 |
| The host alias over TCP, with rule 2 | Refused |
| **The host alias over TCP, without `host` in rule 2** | **Reached the listener on the Mac's loopback**, and the proxy never saw it. Rule 2 is required |
| The host alias, looked up | Still resolves (`172.16.0.9`) |
| A private IP (`10.1.2.3`) | Refused |
| UDP other than DNS (port 5353, NTP) | Nothing got out |

Not checked: IPv6 (the sandbox had none), the resolver being down, and
whether a fixed loopback upstream avoids #1998.

### Is it worth it?

It works, but it moves the guardrail. Today microsandbox denies by
default, and the broker can only narrow what it allows: a bug in the
broker exposes brokered hosts, not egress. Here, microsandbox allows by
default and our code is the only fence: a bug in the resolver's or the
proxy's decision, or in the address checks, opens egress. And the fixed
rules rest on microsandbox behavior that the docs don't state (the host
alias skipping the proxy, port 853), seen at one version; an upgrade
could change it quietly.

What it buys is changing the list without `mono-jig rm`, and better
denial logs. So it's worth building only if recreating a sandbox for a
new host is a real cost in practice. If it is:
- keep the decision small and in one package, shared by the resolver
  and the proxy, failing closed;
- turn this check into a script in `checks/` and run it on every
  microsandbox upgrade.

## Order of work

1. **Check the hook on the Mac.** Done 2026-11-24; see "What we checked".
2. **Check the password through the Go SDK.** Done 2026-11-24.
3. **Front end and alias map**, with tests. Done.
4. **The flag, the supervisor, the CA and `provision.sh`.** Done.
5. **Anonymous forwarding.** Done.
6. **End to end.** Done 2026-11-24, except for AWS and a real Claude
   token; see "End to end".
7. Later:
   - extract the library and the YAML loader;
   - **sbx, optional:** one experiment with `proxy.sandbox` set to a PAC
     file, to see whose TLS arrives.

## Open questions

- Should the broker see *all* egress? It adds a byte copy per connection
  and makes the broker a single point of failure for the sandbox. The
  alternative is a PAC-like split inside microsandbox (proxy only some
  destinations), which doesn't exist in 0.7.16.
- The CONNECT names an IP, not a host. For spliced traffic that doesn't
  matter. For brokered traffic, the broker dials the provider by name
  itself. Should it also check that the CONNECT's IP is one of the
  provider's? microsandbox's DNS binding already ensures it, so probably
  not.
- Name constraints with real public names: Go's verifier and OpenSSL
  honour them, but check Node and Java against a CA constrained to
  `github.com` and `gitlab.com`.

## Sources

- microsandbox v0.7.16 (Go module cache):
  - `docs/networking/outbound-proxy.mdx`, `tls.mdx`, `dns.mdx`,
    `host-sockets.mdx`;
  - `docs/security/network.mdx`, `secrets.mdx`;
  - `crates/network/lib/engine/tcp/proxy.rs` (`try_run`,
    `strict_hostname_allow_is_opaque`), `engine/tcp/upstream.rs`,
    `engine/tls/proxy.rs`, `proxy/http_connect.rs`, `proxy/types.rs`;
  - `engine/dns/forwarder.rs`, `engine/udp/relay.rs`,
    `model/policy/types.rs`;
  - `sdk/go` `options.go` (`WithProxy`, `HTTPConnectProxy`, `TLSConfig`).
- sbx v0.54.0: `sbx settings ls`, `sbx policy --help`,
  `sbx policy allow network --help`, `sbx secret --help`.
- This repository: `docs/microsandbox.md` (strict mode, the vsock route),
  `docs/design.md` ("On sbx", the broker), `cmd/mono-jig-sbx/main.go`.
