# Keeping Docker out of the agent's sandbox

*Considered 2026-11-27, after [openshell.md](openshell.md). Not researched
beyond what's stated; nothing was built or tried.*

**Decision (user, 2026-11-27): stay on microsandbox, with Docker in the
guest.** Our workflows are too tightly tied to Docker to move it out of the
sandbox. This doc records the alternative in case that changes.

## Why consider it

Docker in the sandbox is the only reason the harness needs a VM per
sandbox, with a guest we control (`openshell.md`). If the agent never ran
dockerd, other sandboxes would do: OpenShell on Docker or Podman, Claude
Code's bubblewrap and Seatbelt sandbox, process sandboxes generally.

## Three tiers

### 1. Services in the sandbox, for unit tests

Most test suites need a database or search engine, not Docker itself.

- **Installed in the image:** Postgres and MongoDB from packages, OpenSearch
  from its tarball (it bundles a JVM), started on demand.
- **In-process stand-ins:** PGlite (Postgres compiled to WebAssembly),
  `mongodb-memory-server`, moto (AWS), embedded-postgres (JVM).
- **Costs:**
  - Versions and extensions may differ from production; PostGIS is the
    usual gap.
  - OpenSearch wants 1–2 GB of memory. The monorepo's 4 GB VM already
    runs out under load.
  - Suites that use **Testcontainers** need a change in each repo: use a
    service URL from the environment (say `TEST_PG_URL`) when it's set,
    and start a container only when it isn't.

### 2. A narrow job runner outside the sandbox

This is the piece that replaces Docker in the sandbox for integration
tests of the deployed image.

- **The interface:** the agent asks for one thing, "run this repo's
  integration-test target (say `make integration-test`) at commit X", and
  gets back the logs and the exit code. There's no Docker socket and no
  Docker API.
- **The runner:** a broker-side service starts a fresh VM per job and runs
  the target there with Docker. That VM can be a microsandbox VM, where
  Docker already works (`microsandbox.md`, "Docker in the guest").
- **The boundary:** the job runs code the agent wrote, so the VM is the
  boundary:
  - no credentials;
  - no host directories shared in;
  - egress through our filter;
  - thrown away after the job.

  An escape from the job's containers lands in an empty, disposable VM.
- **Why narrow:** a Docker API proxy would have to filter bind mounts,
  `--privileged`, host networking and the rest (OpenShell #113's fourth
  approach). A single "run this target" request leaves nothing to filter.
- **Hosted versions:**
  - Testcontainers Cloud gives existing Testcontainers suites a remote
    engine with no code change.
  - Remote BuildKit builds images away from the agent:
    `docker buildx create --driver remote`, Docker Build Cloud, or Depot.

  Both send code and build context to a third party.
- **Local daemonless builders don't help in OpenShell:** Buildah and
  rootless BuildKit need user namespaces, and Kaniko needs root. Both are
  blocked there.

With this in place, Docker remains a VM-isolated workload, just not the
one the agent lives in. OpenShell's supervisor middleware, or our broker,
would be where the agent's "run" request is handled.

### 3. GitHub Actions as the final check

The agent pushes, opens the PR and watches it (`gh run watch`,
`gh run view --log-failed`). What the broker must handle:

- **Secrets:** workflows on a PR from a branch in the same repository get
  the repository's secrets. Fork PRs don't.
- **Workflow edits:** the agent could push a change to `.github/workflows/`
  that prints those secrets. There are two ways to stop that:
  - the broker's receive-pack checks refuse pushes that touch workflow
    files;
  - or secrets go into GitHub environments with required reviewers, so a
    person approves before a job gets them.
- **Egress:** the Actions API read routes. Log downloads probably redirect
  to a separate blob-storage host, which would also need allowing
  (*unverified*).
- **Speed:** each round trip takes minutes, and the agent can't debug
  interactively. The agent's instructions should say to run tiers 1 and 2
  first and use CI to confirm.

## Smaller options

- **`container-structure-test`'s tar driver** checks an image's files,
  metadata and entrypoint without a container runtime. It's cheap, but it
  doesn't test the image running.
- **Running the image's root filesystem as plain processes:** unpack the
  image and run its entrypoint directly. It's fragile and skips networking
  and isolation; don't rely on it.

## Why we're not doing it now

- Our workflows assume Docker in the sandbox throughout: Testcontainers
  suites, `docker compose` stacks, image builds the agent iterates on.
  Tier 1 means a change in every repository, and tier 2 means a new
  service.
- microsandbox already runs dockerd in the guest, behind the broker.

## When to look again

- Most of our repositories can run their unit tests without Docker
  (tier 1), so only image integration tests still need it.
- We want a sandbox that can't host Docker (OpenShell, bubblewrap,
  Seatbelt) for other reasons: management, unattended agents, Linux or
  Kubernetes hosts.
- Building the tier 2 runner becomes worth it on its own, for example to
  keep Docker workloads out of the agent's VM even on microsandbox.
