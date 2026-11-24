package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	m "github.com/superradcompany/microsandbox/sdk/go"

	"mono-jig/allowlist"
	"mono-jig/broker"
	"mono-jig/broker/awsmcp"
	"mono-jig/broker/providers"
	"mono-jig/guest"
)

// The supervisor (mono-jig run): one process per sandbox that serves
// the broker and owns the VM (docs/microsandbox.md, "Tying the broker's
// lifecycle to the sandbox's").
//
// It serves the broker on <state>/broker.sock and holds the sandbox
// through the microsandbox SDK, attached: if the process dies, the VM
// stops with it. The guest reaches the broker through a vsock route to
// that socket. Its other egress is microsandbox's policy, compiled from
// the allowlist. Network rules, mounts and the vsock route are set when
// the sandbox is created; rm and open again to change them.
//
// The providers come from mono-jig login (settings.json and the
// Keychain, login.go); the JIG_* variables below override them.
//
// Environment:
//
//	JIG_STATE                 state directories (default ~/.mono-jig)
//	JIG_IMAGE                 the OCI image (default: the one mono-jig image build made, else ubuntu:26.04)
//	JIG_MEMORY                MiB (default 4096)
//	JIG_CPUS                  vCPUs (default 4)
//	JIG_DISK                  root disk, GiB (default 32; sparse)
//	JIG_DOCKER_DISK           the ext4 disk for /var/lib/docker, GiB (default 64; sparse); 0: no Docker
//	JIG_LAYERS                more allowlist layer files (an org's), before the user's (<state root>/allowlist.yaml)
//	JIG_GITLAB_USER           the GitLab user the agent acts for; empty: no GitLab provider
//	JIG_GITLAB_URL            default https://gitlab.com
//	JIG_GITLAB_TOKEN_COMMAND  default: glab's own login, or the Keychain item mono-jig-gitlab
//	JIG_GITHUB_USER           the GitHub user the agent acts for (github.com); empty: no GitHub provider
//	JIG_GITHUB_TOKEN_COMMAND  default: gh's own login, or the Keychain item mono-jig-github
//	JIG_CLAUDE_TOKEN_COMMAND  default: the Keychain item mono-jig-claude, if it exists
//	JIG_AWS_PROFILES          AWS profiles for the AWS MCP proxy, the first the default, space-separated; empty: no AWS
//	JIG_AWS_MCP_COMMAND       default: the embedded aws-mcp-proxy wrapper under uv

const (
	guestUser   = "agent"
	brokerPort  = 443 // the vsock port, and the guest's loopback port
	guestCA     = "/usr/local/share/ca-certificates/mono-jig-broker-src.crt"
	provisioned = "/var/lib/mono-jig/provisioned"
	// dockerDir is on its own ext4 disk: the root is overlayfs, and Docker's
	// overlay snapshots can't sit on overlayfs ("invalid argument").
	dockerDir = "/var/lib/docker"
	// brokerNames are pointed at the guest's bridge.
	brokerNames = providers.GitLabHost + " " + providers.GitHubHost + " " + providers.AnthropicHost + " " + providers.AWSHost
	// mcpcliDir is where the supervisor writes mcpcli, on every start.
	mcpcliDir = "/usr/local/lib/mono-jig/mcpcli"
)

type config struct {
	name, state string
	broker      providers.Config
	image       string
	memory      uint32
	cpus        uint8
	diskGiB     uint32 // the root disk
	dockerGiB   uint32 // 0: no Docker
	rules       []m.PolicyRule
	mounts      []Mount // host folders (mounts.go)
}

func loadConfig(name, state string) config {
	c := config{name: name, state: state}
	c.image, _ = sandboxImage()
	mem, err := strconv.ParseUint(env("JIG_MEMORY", "4096"), 10, 32)
	if err != nil {
		fail("JIG_MEMORY: %v", err)
	}
	cpus, err := strconv.ParseUint(env("JIG_CPUS", "4"), 10, 8)
	if err != nil {
		fail("JIG_CPUS: %v", err)
	}
	c.memory, c.cpus = uint32(mem), uint8(cpus)
	disk, err := strconv.ParseUint(env("JIG_DISK", "32"), 10, 16)
	if err != nil || disk < 8 {
		fail("JIG_DISK: want GiB, at least 8")
	}
	docker, err := strconv.ParseUint(env("JIG_DOCKER_DISK", "64"), 10, 16)
	if err != nil {
		fail("JIG_DOCKER_DISK: %v", err)
	}
	c.diskGiB, c.dockerGiB = uint32(disk), uint32(docker)
	c.broker = brokerConfig()
	entries, err := allowed(c.broker)
	if err != nil {
		fail("%v", err)
	}
	c.rules = egressRules(entries)
	if c.mounts, err = loadMounts(state); err != nil {
		fail("%v", err)
	}
	return c
}

// brokerConfig is the providers mono-jig login recorded, under the
// JIG_* variables.
func brokerConfig() providers.Config {
	var b providers.Config
	s := loadSettings()
	gl := gitlabSettings{URL: "https://gitlab.com", Token: "glab"}
	if s.GitLab != nil {
		gl = *s.GitLab
	}
	user := env("JIG_GITLAB_USER", gl.User)
	if user != "" && !gitlabUserRE.MatchString(user) {
		fail("the GitLab user %q isn't a GitLab username", user)
	}
	b.GitLabUser = user
	b.GitLabURL = env("JIG_GITLAB_URL", gl.URL)
	if user != "" {
		u, err := url.Parse(b.GitLabURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			fail("the GitLab URL %q: want https://host", b.GitLabURL)
		}
		b.GitLabTokenCmd = env("JIG_GITLAB_TOKEN_COMMAND", gitlabTokenCommand(gl, u.Host))
	}
	gh := githubSettings{Token: "gh"}
	if s.GitHub != nil {
		gh = *s.GitHub
	}
	b.GitHubUser = env("JIG_GITHUB_USER", gh.User)
	if b.GitHubUser != "" {
		if !githubUserRE.MatchString(b.GitHubUser) {
			fail("the GitHub user %q isn't a GitHub username", b.GitHubUser)
		}
		b.GitHubTokenCmd = env("JIG_GITHUB_TOKEN_COMMAND", githubTokenCommand(gh))
	}
	claude, set := os.LookupEnv("JIG_CLAUDE_TOKEN_COMMAND")
	if !set && keychainHas(keychainClaude) {
		claude = "security find-generic-password -s " + keychainClaude + " -w"
	}
	b.AnthropicTokenCmd = claude
	var profiles []string
	if s.AWS != nil {
		profiles = s.AWS.Profiles
	}
	b.AWSProfiles = strings.Fields(env("JIG_AWS_PROFILES", strings.Join(profiles, " ")))
	b.AWSMCPCommand = env("JIG_AWS_MCP_COMMAND", "")
	return b
}

// userAllowlist is the user's own allowlist layer: hosts to add to the
// base (allowlist/lists/base.yaml), in the layer format.
func userAllowlist() string { return filepath.Join(stateRoot(), "allowlist.yaml") }

// allowed compiles the allowlist: the base, the JIG_LAYERS files, then the
// user's own layer if there is one. The hosts of the providers the broker
// serves are left out.
func allowed(b providers.Config) ([]allowlist.Entry, error) {
	files := strings.Fields(env("JIG_LAYERS", ""))
	if _, err := os.Stat(userAllowlist()); err == nil {
		files = append(files, userAllowlist())
	}
	layers, err := allowlist.Layers(files...)
	if err != nil {
		return nil, err
	}
	return allowlist.Compile(layers, b.Brokered()...)
}

// egressRules turns the allowlist into microsandbox rules. A
// "*.suffix" entry becomes a domain-suffix rule. No DNS rule: without one,
// microsandbox's resolver answers only allowed names (NXDOMAIN for the
// rest), so DNS can't carry data out; an AllowDNS rule would resolve any
// name.
func egressRules(entries []allowlist.Entry) []m.PolicyRule {
	var rules []m.PolicyRule
	for _, e := range entries {
		dest := e.Pattern
		if s, ok := strings.CutPrefix(dest, "*."); ok {
			dest = "." + s
		}
		rules = append(rules, m.PolicyRule{Action: m.PolicyActionAllow, Direction: m.PolicyDirectionEgress,
			Destination: dest, Protocol: m.PolicyProtocolTCP, Port: strconv.Itoa(e.Port)})
	}
	return rules
}

func run(name, state string) {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	c := loadConfig(name, state)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(state, 0o700); err != nil {
		fail("%v", err)
	}
	// One supervisor per sandbox.
	lock, err := os.OpenFile(filepath.Join(state, "run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fail("%v", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fail("another supervisor holds %s", lock.Name())
	}
	pidFile := filepath.Join(state, "run.pid")
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
	defer os.Remove(pidFile)
	defer os.Remove(filepath.Join(state, "ready"))

	// The broker first: the guest's route needs the socket to exist.
	providers, err := providers.Build(ctx, c.broker, state, log)
	if err != nil {
		fail("%v", err)
	}
	sock := filepath.Join(state, "broker.sock")
	l, err := broker.ListenUnix(sock)
	if err != nil {
		fail("broker socket: %v", err)
	}
	defer os.Remove(sock)
	if len(providers) == 0 {
		log.Warn("no broker provider configured (mono-jig login): the broker answers 404")
	}
	srv, err := broker.ServeTLS(l, providers, state, name, log)
	if err != nil {
		fail("%v", err)
	}
	defer srv.Close()

	// Denials from this run start here (denials.go).
	var logStart int64
	if fi, err := os.Stat(runtimeLog(name)); err == nil {
		logStart = fi.Size()
	}
	sb, created, err := openSandbox(ctx, c)
	if err != nil {
		fail("sandbox: %v", err)
	}
	defer sb.Close()
	log.Info("sandbox running", "name", name, "created", created)

	if err := provision(ctx, sb, c, log); err != nil {
		_ = sb.StopWithTimeout(context.Background(), 30*time.Second)
		fail("provision: %v", err)
	}
	stopped := make(chan struct{})
	go func() {
		res, err := sb.WaitUntilStopped(context.Background())
		log.Info("sandbox stopped", "result", res, "err", err)
		close(stopped)
	}()
	// The bridge to the broker, and dockerd: root exec jobs, restarted if
	// they end (there's no systemd; the guest's init is microsandbox's).
	jobs := map[string]string{"bridge": hostsLine() + fmt.Sprintf(
		"\nexec socat TCP-LISTEN:%d,bind=127.0.0.1,fork,reuseaddr VSOCK-CONNECT:2:%d", brokerPort, brokerPort)}
	if c.dockerGiB > 0 {
		// A sandbox provisioned before Docker was added has no dockerd.
		jobs["dockerd"] = "command -v dockerd >/dev/null || exec sleep infinity\n" +
			"rm -f /var/run/docker.pid; exec dockerd >>/var/log/dockerd.log 2>&1"
	}
	var jobsDone sync.WaitGroup
	jobsDone.Add(1)
	go func() { defer jobsDone.Done(); followDenials(ctx, name, state, logStart, log, stopped) }()
	for name, cmd := range jobs {
		jobsDone.Add(1)
		go func() { defer jobsDone.Done(); keep(ctx, sb, log, stopped, name, cmd) }()
	}
	_ = os.WriteFile(filepath.Join(state, "ready"), nil, 0o600)
	log.Info("ready", "name", name, "socket", sock)

	select {
	case <-stopped:
		log.Info("the sandbox stopped on its own; closing the broker")
	case <-ctx.Done():
		log.Info("stopping the sandbox")
		sctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		if err := sb.StopWithTimeout(sctx, 30*time.Second); err != nil {
			log.Warn("stop failed; killing", "err", err)
			_ = sb.Kill(context.Background())
		}
		cancel()
		<-stopped
	}
	jobsDone.Wait()
	log.Info("down")
}

// openSandbox starts the named sandbox, creating it the first time. Either
// way this process owns it: attached, not detached.
func openSandbox(ctx context.Context, c config) (*m.Sandbox, bool, error) {
	h, err := m.GetSandbox(ctx, c.name)
	switch {
	case err == nil:
		if s := h.Status(); s == m.SandboxStatusRunning || s == m.SandboxStatusStarting {
			// Left running by a supervisor that was killed before the parent
			// watch fired, or started by hand: stop it, so we own it.
			if err := exec.Command("msb", "stop", "-t", "30", c.name).Run(); err != nil {
				return nil, false, fmt.Errorf("%s is %s and couldn't be stopped: %w", c.name, s, err)
			}
		}
		// The env can change after create (unlike the egress rules), but
		// only while stopped: apply the current one, so a sandbox from an
		// older supervisor, or whose Claude setting changed, matches it.
		env := clientEnv(c)
		var gone []string
		for _, k := range optionalEnv {
			if _, ok := env[k]; !ok {
				gone = append(gone, k)
			}
		}
		if _, err := h.Modify(ctx, m.ModifyOptions{Env: env, EnvRemove: gone, Policy: m.ModificationPolicyNextStart}); err != nil {
			return nil, false, fmt.Errorf("setting the environment: %w", err)
		}
		sb, err := m.StartSandbox(ctx, c.name)
		return sb, false, err
	case m.IsKind(err, m.ErrSandboxNotFound):
	default:
		return nil, false, err
	}
	ca, err := os.ReadFile(broker.CAFile(c.state))
	if err != nil {
		return nil, false, err
	}
	mode := uint32(0o644)
	sb, err := m.CreateSandbox(ctx, c.name,
		m.WithImage(c.image),
		m.WithMemory(c.memory),
		m.WithCPUs(c.cpus),
		m.WithRootDisk(m.RootDisk.Managed(c.diskGiB*1024)),
		m.WithNetwork(&m.NetworkConfig{
			DefaultEgress:  m.PolicyActionDeny,
			DefaultIngress: m.PolicyActionDeny,
			Rules:          c.rules,
			// Strict mode denies HTTPS it can't inspect, i.e. every allowed
			// host without TLS interception. Off, hostname rules match on
			// SNI plus the sandbox's own DNS answers (docs/microsandbox.md).
			DisableStrict: true,
		}),
		m.WithEnv(clientEnv(c)),
		// Debug: the only level that records denied names (denials.go).
		m.WithLogLevel(m.LogLevelDebug),
		m.WithMounts(mounts(c)),
		m.WithVsock(m.VsockRoute{HostSocket: filepath.Join(c.state, "broker.sock"), Port: brokerPort}),
		m.WithPatches(m.Patch.Text(guestCA, string(ca), m.PatchOptions{Mode: &mode, Replace: true})),
	)
	return sb, true, err
}

// clientEnv is what provision.sh writes to /etc/profile.d and
// /etc/environment for the broker, git, claude and aws-env components. Neither file
// reaches a microsandbox session: msb ssh and exec run /bin/sh -c from the
// guest's init, without PAM or a login shell. The sandbox's own environment
// does: it is set at create and re-applied on every start (openSandbox).
// TestClientEnvMatchesProvision keeps the names in step.
func clientEnv(c config) map[string]string {
	env := map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"NODE_EXTRA_CA_CERTS": "/etc/ssl/certs/ca-certificates.crt",
	}
	if c.broker.GitLabUser != "" {
		env["GITLAB_HOST"], env["GITLAB_TOKEN"], env["GITLAB_API_PROTOCOL"] = providers.GitLabHost, "placeholder", "https"
	}
	if c.broker.GitHubUser != "" {
		env["GH_HOST"], env["GH_ENTERPRISE_TOKEN"] = providers.GitHubHost, "placeholder"
	}
	if c.broker.AnthropicTokenCmd != "" {
		env["ANTHROPIC_BASE_URL"] = "https://" + providers.AnthropicHost
		env["CLAUDE_CODE_OAUTH_TOKEN"] = "placeholder"
	}
	if c.broker.AWSOn() {
		env["MCP_GATEWAY_URL"] = awsURL
	}
	return env
}

// optionalEnv are the clientEnv variables a provider adds: removed from a
// sandbox whose provider has gone.
var optionalEnv = []string{"GITLAB_HOST", "GITLAB_TOKEN", "GITLAB_API_PROTOCOL", "GH_HOST", "GH_ENTERPRISE_TOKEN",
	"ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN", "MCP_GATEWAY_URL"}

// awsURL is the broker's AWS MCP endpoint, for mcpcli and curl.
const awsURL = "https://" + providers.AWSHost + awsmcp.Path

// provision runs provision.sh as root: all of it on the first start (a
// marker in the guest records it), and on later starts only broker-env,
// claude-env and aws-env, so git's and glab's settings, Claude Code's, the
// agent's instructions and mcpcli follow the broker's providers (clientEnv does the same for the sandbox's
// own env). The script and mcpcli are written from this binary each time.
func provision(ctx context.Context, sb *m.Sandbox, c config, log *slog.Logger) error {
	done, err := sb.FS().Exists(ctx, provisioned)
	if err != nil {
		return err
	}
	if err := sb.FS().WriteString(ctx, "/root/provision.sh", guest.ProvisionScript); err != nil {
		return err
	}
	if err := sb.FS().Mkdir(ctx, mcpcliDir); err != nil {
		return err
	}
	for name, body := range guest.MCPCLI {
		if err := sb.FS().WriteString(ctx, mcpcliDir+"/"+name, body); err != nil {
			return err
		}
	}
	aws := ""
	if c.broker.AWSOn() {
		aws = awsURL
	}
	gitlab, github := "", ""
	if c.broker.GitLabUser != "" {
		gitlab = "https://" + providers.GitLabHost
	}
	if c.broker.GitHubUser != "" {
		github = "https://" + providers.GitHubHost
	}
	anthropic := ""
	if c.broker.AnthropicTokenCmd != "" {
		anthropic = "https://" + providers.AnthropicHost
	}
	first, comps := "", "broker-env claude-env aws-env"
	if !done {
		comps = firstComponents(ctx, sb, c, log)
		log.Info("provisioning the guest (first start)", "components", comps)
		first = `export DEBIAN_FRONTEND=noninteractive
if ! command -v socat >/dev/null; then
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends socat >/dev/null
fi
` + hostsLine()
	}
	script := fmt.Sprintf(`set -euo pipefail
%s
export SANDBOX_USER=%s SANDBOX_UID=%d PROVISION_COMPONENTS=%q
export BROKER_GITLAB_URL=%q BROKER_GITHUB_URL=%q
export BROKER_GITLAB_USER=%q BROKER_GITHUB_USER=%q BROKER_GITLAB_MCP= BROKER_ANTHROPIC_URL=%q BROKER_CA_FILE=%s
export BROKER_AWS_URL=%q BROKER_AWS_PROFILES=%q MCPCLI_DIR=%s
bash /root/provision.sh
home=$(getent passwd %[2]s | cut -d: -f6)
if [ -d %[14]s ] && [ ! -e "$home/host" ]; then ln -s %[14]s "$home/host"; fi
mkdir -p %[15]s && touch %[16]s
`, first, guestUser, guestUID, comps, gitlab, github, c.broker.GitLabUser, c.broker.GitHubUser, anthropic, guestCA,
		aws, strings.Join(c.broker.AWSProfiles, " "), mcpcliDir, shareGuest, filepath.Dir(provisioned), provisioned)
	h, err := sb.ShellStream(ctx, script, m.WithExecUser("root"))
	if err != nil {
		return err
	}
	defer h.Close()
	out, err := h.Collect(ctx)
	if err != nil {
		return err
	}
	_, _ = io.WriteString(os.Stderr, out.Stdout()+out.Stderr())
	if out.ExitCode() != 0 {
		return fmt.Errorf("provision.sh exited %d", out.ExitCode())
	}
	return nil
}

// hostsLine points the broker's names at the bridge, replacing an older
// line, so names added since the sandbox was made are there too.
func hostsLine() string {
	return fmt.Sprintf(`h=$(grep -v 'broker\.internal' /etc/hosts || true); printf '%%s\n127.0.0.1 %s\n' "$h" >/etc/hosts`, brokerNames)
}

// keep runs cmd as a root exec job, so the agent user can't replace it,
// and restarts it until the sandbox stops or ctx ends.
func keep(ctx context.Context, sb *m.Sandbox, log *slog.Logger, stopped <-chan struct{}, name, cmd string) {
	for {
		h, err := sb.ShellStream(ctx, cmd, m.WithExecUser("root"))
		if err == nil {
			code, werr := h.Wait(ctx)
			h.Close()
			err = errors.Join(werr, fmt.Errorf("exit %d", code))
		}
		select {
		case <-ctx.Done():
			return
		case <-stopped:
			return
		case <-time.After(2 * time.Second):
			log.Warn("job ended; restarting", "job", name, "err", err)
		}
	}
}

// mounts are the host folders (mounts.go) and the sandbox's own disks:
// Docker's, if it has one.
func mounts(c config) map[string]m.MountConfig {
	ms := hostMounts(c.mounts)
	if c.dockerGiB > 0 {
		ms[dockerDir] = m.Mount.Owned(m.OwnedVolumeOptions{Kind: m.VolumeKindDisk, SizeMiB: c.dockerGiB * 1024})
	}
	return ms
}

// components are provision.sh's, for this sandbox.
func components(c config) string {
	if c.dockerGiB > 0 {
		return "base git broker docker claude aws-env"
	}
	return "base git broker claude aws-env"
}

// imageComponents are the parts of provision.sh mono-jig image build
// puts in the base image: those that depend only on the image.
const imageComponents = "base git docker claude"

// imageFile is where the base image records its parts and provision.sh.
const imageFile = "/etc/mono-jig/image"

// firstComponents are the parts of provision.sh a sandbox's first start
// runs: those its image doesn't have (all, from a plain image), and the
// settings every start writes.
func firstComponents(ctx context.Context, sb *m.Sandbox, c config, log *slog.Logger) string {
	baked, hash := map[string]bool{}, ""
	if s, err := sb.FS().ReadString(ctx, imageFile); err == nil {
		baked, hash = parseImageFile(s)
	}
	if hash != "" && hash != guest.ProvisionHash() {
		log.Warn("the image was built from another provision.sh; to bring it up to date: mono-jig image build, then rm and open",
			"image", hash, "binary", guest.ProvisionHash())
	}
	var comps []string
	for _, p := range strings.Fields(components(c)) {
		if !baked[p] {
			comps = append(comps, p)
		}
	}
	// claude includes claude-env, which a baked claude needs at start.
	return strings.Join(append(comps, "broker-env", "claude-env", "aws-env"), " ")
}

// parseImageFile reads imageFile: components=<parts> and provision=<hash>.
func parseImageFile(s string) (map[string]bool, string) {
	baked, hash := map[string]bool{}, ""
	for _, line := range strings.Split(s, "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "components":
			for _, p := range strings.Fields(v) {
				baked[p] = true
			}
		case "provision":
			hash = strings.TrimSpace(v)
		}
	}
	return baked, hash
}
