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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	m "github.com/superradcompany/microsandbox/sdk/go"

	"mcp-restrict/brokerd"
	"mcp-restrict/cli"
	"mcp-restrict/filter"
	"mcp-restrict/sandbox"
)

const (
	guestUser   = "agent"
	brokerPort  = 443 // the vsock port, and the guest's loopback port
	guestCA     = "/usr/local/share/ca-certificates/investigator-broker-src.crt"
	provisioned = "/var/lib/investigator/provisioned"
	// dockerDir is on its own ext4 disk: the root is overlayfs, and Docker's
	// overlay snapshots can't sit on overlayfs ("invalid argument").
	dockerDir = "/var/lib/docker"
	// brokerNames are pointed at the guest's bridge.
	brokerNames = "gitlab.broker.internal github.broker.internal anthropic.broker.internal"
)

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "investigator: "+f+"\n", a...)
	os.Exit(1)
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

// The sandbox commands run a sandbox on microsandbox, with the broker in
// the same process that owns the VM (microsandbox.md, "Tying the broker's
// lifecycle to the sandbox's").
//
// The supervisor (run) serves the broker on <state>/broker.sock and holds
// the sandbox through the microsandbox SDK, attached: if the process dies,
// the VM stops with it. The guest reaches the broker through a vsock route
// to that socket. Its other egress is microsandbox's policy, compiled from
// the filter's layers. Network rules and the vsock route are set when the
// sandbox is created; rm and up again to change them.
//
// Environment:
//
//	INV_STATE                 state directories (default ~/.investigator)
//	INV_IMAGE                 the OCI image (default ubuntu:26.04)
//	INV_MEMORY                MiB (default 4096)
//	INV_CPUS                  vCPUs (default 4)
//	INV_DISK                  root disk, GiB (default 32; sparse)
//	INV_DOCKER_DISK           the ext4 disk for /var/lib/docker, GiB (default 64; sparse); 0: no Docker
//	INV_PROFILE, INV_LAYERS   the allowlist's profile and extra layer files
//	INV_GITLAB_USER           the GitLab user the agent acts for; empty: no GitLab provider
//	INV_GITLAB_URL            default https://gitlab.com
//	INV_GITLAB_TOKEN_COMMAND  default: glab's own login
//	INV_CLAUDE_TOKEN_COMMAND  default: the Keychain item inv-claude, if it exists
func sandboxCmd(cmd, name string) {
	if !nameRE.MatchString(name) {
		fail("name must be lowercase letters, digits and dashes, at most 31 characters")
	}
	home, _ := os.UserHomeDir()
	state := filepath.Join(env("INV_STATE", filepath.Join(home, ".investigator")), name)
	setRuntimePaths()
	switch cmd {
	case "up":
		up(name, state)
	case "run":
		run(name, state)
	case "down":
		down(state)
	case "shell":
		shell(name)
	case "ssh-config":
		fmt.Print(sshConfig(name, state))
	case "code":
		code(name, state)
	case "rm":
		down(state)
		if err := m.RemoveSandbox(context.Background(), name); err != nil && !m.IsKind(err, m.ErrSandboxNotFound) {
			fail("remove: %v", err)
		}
		// The next sandbox of this name has a new host key.
		_ = os.Remove(filepath.Join(state, "known_hosts"))
		fmt.Println("removed", name, "(state kept in", state+")")
	}
}

// setRuntimePaths points the SDK at a Homebrew install, which it doesn't
// find on its own (it looks in ~/.microsandbox).
func setRuntimePaths() {
	if os.Getenv("MSB_PATH") != "" {
		return
	}
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".microsandbox", "bin", "msb")); err == nil {
		return
	}
	p, err := exec.LookPath("msb")
	if err != nil {
		return
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	libexec := filepath.Join(filepath.Dir(filepath.Dir(p)), "libexec")
	if _, err := os.Stat(filepath.Join(libexec, "msb")); err == nil {
		os.Setenv("MSB_PATH", filepath.Join(libexec, "msb"))
		if libs, _ := filepath.Glob(filepath.Join(libexec, "libkrunfw.*.dylib")); len(libs) > 0 {
			os.Setenv("MSB_LIBKRUNFW_PATH", libs[0])
		}
	}
}

// --- up, down: the supervisor in the background ---

func pidOf(state string) int {
	b, err := os.ReadFile(filepath.Join(state, "run.pid"))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return 0
	}
	return pid
}

func up(name, state string) {
	if pid := pidOf(state); pid != 0 {
		fmt.Printf("%s is already up (supervisor pid %d)\n", name, pid)
		return
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		fail("%v", err)
	}
	logPath := filepath.Join(state, "run.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fail("%v", err)
	}
	ready := filepath.Join(state, "ready")
	_ = os.Remove(ready)
	self, _ := os.Executable()
	c := exec.Command(self, "run", name)
	c.Stdout, c.Stderr = lf, lf
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		fail("%v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- c.Wait() }()
	fmt.Printf("starting %s (log: %s); the first start provisions the guest, which takes a few minutes\n", name, logPath)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	deadline := time.After(20 * time.Minute)
	for {
		select {
		case err := <-exited:
			tail(logPath, 20)
			fail("the supervisor exited: %v", err)
		case <-deadline:
			fail("not ready after 20 minutes; see %s", logPath)
		case <-tick.C:
			if _, err := os.Stat(ready); err == nil {
				fmt.Printf("%s is up (supervisor pid %d)\n  shell:   investigator shell %[3]s\n  VS Code: investigator code %[3]s\n  down:    investigator down %[3]s\n", name, c.Process.Pid, name)
				return
			}
		}
	}
}

func tail(path string, n int) {
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	fmt.Fprintln(os.Stderr, strings.Join(lines, "\n"))
}

func down(state string) {
	pid := pidOf(state)
	if pid == 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 90; i++ {
		if syscall.Kill(pid, 0) != nil {
			fmt.Println("down")
			return
		}
		time.Sleep(time.Second)
	}
	fail("the supervisor (pid %d) didn't stop in 90s", pid)
}

func shell(name string) {
	msb, err := exec.LookPath("msb")
	if err != nil {
		fail("msb not found")
	}
	fail("%v", syscall.Exec(msb, []string{"msb", "exec", "-t", "-u", guestUser, name, "--", "bash", "-l"}, os.Environ()))
}

// sshConfig writes the sandbox's Host block to <state>/ssh_config, and the
// file VS Code reads (<root>/ssh_config) that includes every sandbox's, and
// returns the block. The key is the sandbox's own; msb ssh authorize adds
// it for all sandboxes (microsandbox.md).
func sshConfig(name, state string) string {
	key := filepath.Join(state, "ssh_key")
	if _, err := os.Stat(key); err != nil {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "investigator-"+name, "-f", key).CombinedOutput(); err != nil {
			fail("ssh-keygen: %v: %s", err, out)
		}
		if out, err := exec.Command("msb", "ssh", "authorize", "--file", key+".pub").CombinedOutput(); err != nil {
			fail("msb ssh authorize: %v: %s", err, out)
		}
	}
	msb, err := exec.LookPath("msb")
	if err != nil {
		fail("msb not found")
	}
	// No agent or X11 forwarding: the Mac's credentials stay on the Mac.
	// The host key is new with each sandbox (rm removes known_hosts), so
	// the first one is accepted; it reaches us over msb's own channel.
	block := fmt.Sprintf(`Host %[1]s
  User %[2]s
  IdentityFile %[3]s
  IdentitiesOnly yes
  UserKnownHostsFile %[4]s
  StrictHostKeyChecking accept-new
  ProxyCommand %[5]s ssh serve %[6]s --stdio
  ForwardAgent no
  ForwardX11 no
  ClearAllForwardings no
`, sshHost(name), guestUser, key, filepath.Join(state, "known_hosts"), msb, name)
	if err := os.WriteFile(filepath.Join(state, "ssh_config"), []byte(block), 0o600); err != nil {
		fail("%v", err)
	}
	root := filepath.Dir(state)
	all := fmt.Sprintf("# Written by investigator: every sandbox's Host block.\nInclude %s\n", filepath.Join(root, "*", "ssh_config"))
	if err := os.WriteFile(filepath.Join(root, "ssh_config"), []byte(all), 0o600); err != nil {
		fail("%v", err)
	}
	return block
}

func sshHost(name string) string { return "investigator-" + name }

// code opens the harness's own VS Code instance (its own user data and
// extensions, under <root>/_vscode) on the sandbox, with the settings
// that close the credential bridges (cli.VSCodeSettings).
func code(name, state string) {
	if pidOf(state) == 0 {
		fail("%s isn't up: investigator up %s", name, name)
	}
	sshConfig(name, state)
	root := filepath.Dir(state)
	data, ext := filepath.Join(root, "_vscode"), filepath.Join(root, "_vscode-ext")
	if _, err := cli.WriteVSCodeSettings(data, filepath.Join(root, "ssh_config")); err != nil {
		fail("%v", err)
	}
	bin, err := exec.LookPath("code")
	if err != nil {
		fail("VS Code's code command isn't on PATH (VS Code: Shell Command: Install 'code' command in PATH)")
	}
	args := []string{"code", "--user-data-dir", data, "--extensions-dir", ext,
		"--remote", "ssh-remote+" + sshHost(name), "/home/" + guestUser}
	fmt.Println(strings.Join(args, " "))
	fail("%v", syscall.Exec(bin, args, os.Environ()))
}

// --- run: the supervisor ---

type config struct {
	name, state string
	broker      brokerd.Flags
	image       string
	memory      uint32
	cpus        uint8
	diskGiB     uint32 // the root disk
	dockerGiB   uint32 // 0: no Docker
	rules       []m.PolicyRule
}

func loadConfig(name, state string) config {
	c := config{name: name, state: state, image: env("INV_IMAGE", "ubuntu:26.04")}
	mem, err := strconv.ParseUint(env("INV_MEMORY", "4096"), 10, 32)
	if err != nil {
		fail("INV_MEMORY: %v", err)
	}
	cpus, err := strconv.ParseUint(env("INV_CPUS", "4"), 10, 8)
	if err != nil {
		fail("INV_CPUS: %v", err)
	}
	c.memory, c.cpus = uint32(mem), uint8(cpus)
	disk, err := strconv.ParseUint(env("INV_DISK", "32"), 10, 16)
	if err != nil || disk < 8 {
		fail("INV_DISK: want GiB, at least 8")
	}
	docker, err := strconv.ParseUint(env("INV_DOCKER_DISK", "64"), 10, 16)
	if err != nil {
		fail("INV_DOCKER_DISK: %v", err)
	}
	c.diskGiB, c.dockerGiB = uint32(disk), uint32(docker)

	user := env("INV_GITLAB_USER", "")
	if user != "" && !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`).MatchString(user) {
		fail("INV_GITLAB_USER isn't a GitLab username")
	}
	c.broker.GitLabUser = user
	c.broker.GitLabURL = env("INV_GITLAB_URL", "https://gitlab.com")
	c.broker.AnthropicURL = "https://api.anthropic.com"
	if user != "" {
		u, err := url.Parse(c.broker.GitLabURL)
		if err != nil {
			fail("INV_GITLAB_URL: %v", err)
		}
		c.broker.GitLabTokenCmd = env("INV_GITLAB_TOKEN_COMMAND",
			fmt.Sprintf("glab api --hostname %[1]s user >/dev/null && glab config get token --host %[1]s", u.Host))
	}
	claude, set := os.LookupEnv("INV_CLAUDE_TOKEN_COMMAND")
	if !set && exec.Command("security", "find-generic-password", "-s", "inv-claude").Run() == nil {
		claude = "security find-generic-password -s inv-claude -w"
	}
	c.broker.AnthropicTokenCmd = claude

	layers, err := filter.Layers(env("INV_PROFILE", "investigator"), strings.Fields(env("INV_LAYERS", ""))...)
	if err != nil {
		fail("%v", err)
	}
	entries, err := filter.Compile(layers)
	if err != nil {
		fail("%v", err)
	}
	c.rules = egressRules(entries)
	return c
}

// egressRules turns the filter's allowlist into microsandbox rules. A
// "*.suffix" entry becomes a domain-suffix rule. No DNS rule: without one,
// microsandbox's resolver answers only allowed names (NXDOMAIN for the
// rest), so DNS can't carry data out; an AllowDNS rule would resolve any
// name.
func egressRules(entries []filter.Entry) []m.PolicyRule {
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
	providers, err := c.broker.Providers(log)
	if err != nil {
		fail("%v", err)
	}
	sock := filepath.Join(state, "broker.sock")
	l, err := brokerd.ListenUnix(sock)
	if err != nil {
		fail("broker socket: %v", err)
	}
	defer os.Remove(sock)
	if len(providers) == 0 {
		log.Warn("no broker provider configured (INV_GITLAB_USER, INV_CLAUDE_TOKEN_COMMAND): the broker answers 404")
	}
	srv, err := brokerd.Serve(l, providers, state, name, log)
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
		for _, k := range []string{"ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN"} {
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
	ca, err := os.ReadFile(brokerd.CAFile(c.state))
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
			// SNI plus the sandbox's own DNS answers (microsandbox.md).
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
// /etc/environment for the broker, git and claude components. Neither file
// reaches a microsandbox session: msb ssh and exec run /bin/sh -c from the
// guest's init, without PAM or a login shell. The sandbox's own environment
// does: it is set at create and re-applied on every start (openSandbox).
// TestClientEnvMatchesProvision keeps the names in step.
func clientEnv(c config) map[string]string {
	env := map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GITLAB_HOST":         brokerd.GitLabHost, "GITLAB_TOKEN": "placeholder", "GITLAB_API_PROTOCOL": "https",
		"GH_HOST": "github.broker.internal", "GH_ENTERPRISE_TOKEN": "placeholder",
		"NODE_EXTRA_CA_CERTS": "/etc/ssl/certs/ca-certificates.crt",
	}
	if c.broker.AnthropicTokenCmd != "" {
		env["ANTHROPIC_BASE_URL"] = "https://" + brokerd.AnthropicHost
		env["CLAUDE_CODE_OAUTH_TOKEN"] = "placeholder"
	}
	return env
}

// provision runs provision.sh as root once per sandbox (a marker in the
// guest records it).
func provision(ctx context.Context, sb *m.Sandbox, c config, log *slog.Logger) error {
	if ok, err := sb.FS().Exists(ctx, provisioned); err == nil && ok {
		return nil
	}
	log.Info("provisioning the guest (first start)")
	if err := sb.FS().WriteString(ctx, "/root/provision.sh", sandbox.ProvisionScript); err != nil {
		return err
	}
	anthropic := ""
	if c.broker.AnthropicTokenCmd != "" {
		anthropic = "https://" + brokerd.AnthropicHost
	}
	script := fmt.Sprintf(`set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends socat >/dev/null
export SANDBOX_USER=%s PROVISION_COMPONENTS=%q
export BROKER_GITLAB_URL=https://gitlab.broker.internal BROKER_GITHUB_URL=https://github.broker.internal
export BROKER_GITLAB_USER=%q BROKER_GITLAB_MCP= BROKER_ANTHROPIC_URL=%q BROKER_CA_FILE=%s
%s
bash /root/provision.sh
mkdir -p %s && touch %s
`, guestUser, components(c), c.broker.GitLabUser, anthropic, guestCA, hostsLine(), filepath.Dir(provisioned), provisioned)
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

func hostsLine() string {
	return fmt.Sprintf(`grep -q 'broker.internal' /etc/hosts || echo '127.0.0.1 %s' >>/etc/hosts`, brokerNames)
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

// mounts are the sandbox's own disks: Docker's, if it has one.
func mounts(c config) map[string]m.MountConfig {
	if c.dockerGiB == 0 {
		return nil
	}
	return map[string]m.MountConfig{dockerDir: m.Mount.Owned(m.OwnedVolumeOptions{Kind: m.VolumeKindDisk, SizeMiB: c.dockerGiB * 1024})}
}

// components are provision.sh's, for this sandbox.
func components(c config) string {
	if c.dockerGiB > 0 {
		return "base git broker docker claude"
	}
	return "base git broker claude"
}
