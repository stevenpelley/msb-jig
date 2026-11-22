# Prototype plan: the personal track

The steps to build the personal version of the harness (see "Two tracks" in `investigator-design.md`): GitLab and AWS as providers, Claude Code as the agent, run directly in the sandbox or from VS Code over SSH into it, on a personal Mac. The sandbox is sbx first, then Lima: the plan isn't done until it runs without sbx, because work may not allow Docker accounts. Written 2026-11-22; nothing started yet.

## Who can test what

The agent works inside an sbx sandbox itself, so some checks need the user's Mac.

| Where | Can do | Can't do |
|---|---|---|
| **The agent's sandbox** | Build images; run Docker containers as stand-in sandboxes (Docker 29, arm64); SSH into them; run `git-http-backend` as a fake git upstream; unit- and integration-test the broker against fake upstreams | Run sbx (no nested sandboxes); run VS Code; reach the Mac's network; act as the user on GitLab (the proxy here injects the **agent's** GitLab PAT on gitlab.com, so live calls from here act as `<user>-agent`; ask first) |
| **The user's Mac** | Create real sbx sandboxes; run VS Code; run the broker with the user's own GitLab token; live tests against a throwaway GitLab project | — |

The agent writes scripts for each Mac step, and the user runs them and pastes the output back.

## Steps

### 0. Decisions

- **Language:** Go for the broker and the CLI: a single binary and easy git protocol parsing.
- **Policy:** plain Go (an allowed-operations table and predicate functions behind one `Decide` function), not Cedar. Revisit once the rules are enumerated ("Policy in code" in `investigator-design.md`).
- **Layout:** `mcp-restrict/{sandbox,broker,net,filter,cli,checks}/`. `sandbox/provision.sh` installs everything in the sandbox, so the sbx image and the Lima VM share it. `broker`, `filter` and `net` are separate Go packages, built into one binary, `investigator-net` (see "Packaging: one process per sandbox" in `investigator-design.md`).
- **Nothing on the Mac runs on its own:** no `launchd` jobs, no containers. `investigator-net` runs only while its VM runs.
- **sbx features used:** only those Lima can replace (the VM, Docker inside, domain policy). No sbx secrets or sbx MCP gateway for the harness.
- **Test target:** a throwaway GitLab project under the user's personal account, for live tests.

### 1. Sandbox image and SSH (agent, in its sandbox)

- `sandbox/provision.sh`, and a Dockerfile that runs it, giving a sandbox image with:
  - sshd: key-only, one user, and server-side refusal of forwarding (`AllowAgentForwarding no`, `X11Forwarding no`, `PermitTunnel no`, `AllowStreamLocalForwarding no`, `AllowTcpForwarding local`);
  - git, `glab`, the Docker client and Claude Code.
- The CLI's SSH setup: a per-sandbox key, a pinned host key, and its own SSH config file using a `ProxyCommand`.
- Test it against a Docker container, with `docker exec -i ... nc localhost 22` standing in for `sbx exec`. Check that:
  - `ssh -F <config>` connects with no host-key prompt;
  - the server refuses agent forwarding even when the client asks for it.
- Write `checks/bridges.sh`, to run in the editor's terminal. It reports:
  - `SSH_AUTH_SOCK` and `ssh-add -l`;
  - askpass and token environment variables;
  - whether `git credential fill` or `git ls-remote` against a private repo gets credentials from anywhere.

### 2. The real sandbox and VS Code (user, on the Mac, with the agent's script)

1. Create the sbx sandbox from the image, with no GitLab or GitHub secret for it. Try a `ProxyCommand` through `sbx exec`; if that fails, publish sshd on `127.0.0.1`.
2. Open `code --user-data-dir <state>/vscode --remote ssh-remote+investigator /workspace`, with the generated settings file.
3. Run `checks/bridges.sh` twice and paste the output back:
   - **Baseline:** default settings, signed into a throwaway account in that VS Code instance. This shows which bridges actually exist.
   - **Hardened:** our settings. Every bridge should show as closed.
4. Also check:
   - With Microsoft hosts denied, does the server install through `remote.SSH.localServerDownload: "always"`?
   - Does `Developer: Show Running Extensions` list Claude Code as running on the remote?
   - Does Claude Code sign in and work inside the sandbox, from the VS Code terminal and from its extension?

### 3. The broker's GitLab HTTP proxy (agent, in its sandbox)

- **Core:** the broker's own endpoints per provider, credential injection, the policy decision, logging. No TLS interception and no general egress filtering.
- **Git push check:** read the ref updates at the start of each `git-receive-pack` request and reject any ref outside `refs/heads/agent/<user>/`, answering with a receive-pack status line so git prints the reason. Test against a local `git-http-backend` in a container.
- **Serving:** one hostname per provider (`gitlab.broker.internal`, later `github.broker.internal`), HTTPS with a CA name-constrained to `*.broker.internal`, and the sandbox's `insteadOf` rules, empty credential helper and `glab` host settings ("How the agent uses git and gh" in `investigator-design.md`). Check whether git applies a `remote.origin.push` mapping to `git push origin <branch>`.
- **GitLab adapter:**
  - parse nested group paths, and normalize project IDs and URL-encoded paths;
  - allowlist REST and GraphQL operations, parsing the GraphQL document rather than trusting operation names;
  - check MR titles, allowing the `Draft:` prefix;
  - deny merging and approving;
  - inject the token in the right form: Basic for git, `PRIVATE-TOKEN` for the API.
  - Test against a fake upstream with recorded responses. Check that `glab` works against the broker as a self-hosted GitLab host.

### 3b. The domain filter and the netstack (agent, in its sandbox)

- The layered allowlist (base, profile `investigator` or `dev`, org, user) as data, grouped by tool or service.
- A CLI command that turns the layers into `sbx policy` rules (for the prototype), and into the ACL for the filter.
- **The filter:** smokescreen embedded as a Go library (check first that it's still maintained as one; otherwise another Go proxy with our ACL), serving an explicit HTTP proxy.
- **The netstack:** `investigator-net`'s gvisor-tap-vsock network, modelled on Lima's `gvproxy.go`. It serves Lima's three `user-v2` sockets, DNS for `proxy.internal` and `broker.internal` only, the filter on the gateway's port 3128 and the broker on its port 443, and forwards nothing else.
- Test it here with a fake guest: a second gvisor stack, or a QEMU-protocol client on the socket. Check that the proxy and broker answer, that other addresses and names fail, and that `go`, `npm`, `pip`, `curl` and `docker pull` work through the proxy. Record which tools ignore `HTTPS_PROXY`.

### 4. The hosted-MCP filter (agent, in its sandbox)

- Proxy the GitLab MCP server and apply policy per tool call:
  - deny `accept_merge_request`;
  - check `title` and `source_branch` in `save_merge_request`;
  - allow the read tools.
- First confirm whether the GitLab MCP server in use is hosted by GitLab.
- Test against a fake MCP server.

### 5. Live GitLab and the wiring (user, on the Mac)

1. Run `investigator-net` in its sbx mode (broker only, no netstack) on the Mac with the user's own GitLab token, listening on `127.0.0.1:<port>` with a per-session token. The CLI starts it and stops it with the sandbox.
2. The sandbox reaches it at `host.docker.internal:<port>`, which sbx's policy allows (`localhost:<port>`). The rest of sbx's policy comes from the generated allowlist (step 3b).
3. In the sandbox, git's `insteadOf`, `glab` and the MCP client all point at the broker.
4. From the VS Code terminal, and through Claude Code, against the throwaway project, check that:
   - a push to `agent/<user>/x` succeeds, and a push to `main` is rejected;
   - creating an MR is rejected without the title tag and allowed with it;
   - merging is rejected, through `glab` and through MCP;
   - a direct, anonymous clone of a public repo works without the broker.

### 6. The Lima backend (agent writes it, user runs it on the Mac)

The design isn't done until this works. Details are in "Leaving sbx: Lima" in `investigator-design.md`.

1. A Lima template: stock Ubuntu cloud image, `vmType: vz`, `mounts: []`, and `sandbox/provision.sh` as its provisioning step.
2. Enforce the route with option C: `investigator-net` (step 3b) serves a per-sandbox `user-v2` network, which on `vz` is the VM's only NIC. The filter and the broker are served inside it, and nothing else is reachable. No root needed. `HTTPS_PROXY=http://proxy.internal:3128` in the VM. Details and the Mac checks are in "Option C on Lima" (`investigator-design.md`):
   - On the Mac, start with plain `limactl usernet` run by hand as the "external" daemon, then swap in `investigator-net`.
   - Until lima#5818 (`external: true`) or lima#5813 (`daemon:`) merges: write our PID into `usernet_<name>.pid`, and have the CLI refuse to open the sandbox unless our daemon holds it. Otherwise Lima silently starts its own unfiltered daemon.
   - Use a guest with systemd 256 or later, so SSH runs over vsock.
   - Check: one NIC; the filter and the broker reachable, everything else unreachable by IP and by name; SSH works; a stopped daemon blocks `open`; stopping the VM stops `investigator-net`; after delete, `ifconfig`, `pfctl`, `networks.yaml` and `$LIMA_HOME/_networks/` are back as they were.
   - Fallbacks, in order: Tart with Softnet, option B (`nftables` inside the VM), option A (`pf` on the Mac).
3. Check that credentials come from Keychain and the CLI's own AWS files, never `~/.aws`.
4. Rerun steps 2 and 5 on Lima: VS Code over SSH, `checks/bridges.sh`, and the live GitLab checks.

### 7. After that

- **AWS:** the AWS MCP proxy behind the broker, with personal read-only roles.
- **The CLI:** `investigator setup`, `login`, `open`, `stop`, `logout`, `uninstall`, `allow`, `denied`, behind the sandbox backend interface.
- **Packaging:** a Homebrew tap with `investigator` and `investigator-net`, and Lima as a dependency.
- **Then the work track:** the GitHub adapter, Datadog and Wiz through the MCP filter, Kiro as the agent, and Kiro IDE. Rerun `checks/bridges.sh` under Kiro, whose remote support is its own.

Steps 1 and 3 are independent. Step 1 comes first, because the VS Code check on the Mac (step 2) is the biggest unknown.
