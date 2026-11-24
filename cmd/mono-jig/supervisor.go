package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"mono-jig/allowlist"
	"mono-jig/broker/providers"
	"mono-jig/sandbox"
)

// The supervisor (mono-jig run): one process per sandbox that serves
// the broker and owns the VM (docs/microsandbox.md, "Tying the broker's
// lifecycle to the sandbox's").
//
// It serves the broker on <state>/broker.sock (brokerhost.go) and holds
// the sandbox through the backend (theBackend; sandbox.Microsandbox, the
// SDK, attached: if the process dies, the VM stops with it), from the
// spec in spec.go, then provisions it (provision.go). The guest reaches the broker through a vsock route to
// that socket. Its other egress is microsandbox's policy, compiled from
// the allowlist. In proxy mode (brokermode.go) the broker is also
// microsandbox's outbound proxy, on a loopback port: every connection
// the policy allows goes through it, and it terminates the providers'
// real hosts. Network rules, mounts, the vsock route and the proxy are
// set when the sandbox is created; rm and open again to change them.
//
// The providers come from mono-jig login (settings.json and the
// Keychain; registry.go and login_<provider>.go); the JIG_* variables
// below override them.
//
// Environment:
//
//	JIG_STATE                 state directories (default ~/.mono-jig)
//	JIG_IMAGE                 the OCI image (default: the one mono-jig image build made, else JIG_BASE_IMAGE)
//	JIG_BASE_IMAGE            what the base image builds on (default ubuntu:26.04; base uses apt)
//	JIG_MEMORY                MiB (default 4096)
//	JIG_CPUS                  vCPUs (default 4)
//	JIG_DISK                  root disk, GiB (default 32; sparse)
//	JIG_DOCKER_DISK           the ext4 disk for /var/lib/docker, GiB (default 64; sparse); 0: no Docker
//	JIG_LAYERS                more allowlist layer files (an org's), before the user's (<state root>/allowlist.yaml)
//	JIG_GITLAB_USER           the GitLab user the agent acts for; empty: no GitLab provider
//	JIG_GITLAB_URL            default https://gitlab.com
//	JIG_GITLAB_TOKEN_COMMAND  default: glab's own login, or the Keychain item mono-jig-gitlab
//	JIG_GITHUB_USER           the GitHub user the agent acts for; empty: no GitHub provider
//	JIG_GITHUB_HOST           default github.com; the API is at api.<host>
//	JIG_GITHUB_TOKEN_COMMAND  default: gh's own login, or the Keychain item mono-jig-github
//	JIG_CLAUDE_TOKEN_COMMAND  default: the Keychain item mono-jig-claude, if it exists
//	JIG_AWS_PROFILES          AWS profiles for the AWS MCP proxy, the first the default, space-separated; empty: no AWS
//	JIG_AWS_MCP_COMMAND       default: the embedded aws-mcp-proxy wrapper under uv

const (
	guestUser   = "agent"
	brokerPort  = 443 // the vsock port, and the guest's loopback port
	guestCA     = "/usr/local/share/ca-certificates/mono-jig-broker-src.crt"
	provisioned = "/var/lib/mono-jig/provisioned"
	// provisionDir is where the supervisor writes provision/, every start.
	provisionDir = "/root/provision"
	// dockerDir is on its own ext4 disk: the root is overlayfs, and Docker's
	// overlay snapshots can't sit on overlayfs ("invalid argument").
	dockerDir = "/var/lib/docker"
)

type config struct {
	name, state string
	broker      providers.Config
	image       string
	memory      uint32
	cpus        uint8
	diskGiB     uint32 // the root disk
	dockerGiB   uint32 // 0: no Docker
	egress      []allowlist.Entry
	mounts      []Mount  // host folders (mounts.go)
	proxy       bool     // the broker's mode is proxy (brokermode.go)
	cas         []string // extra CA certificates, PEM (ca.go)
	proxyAddr   string   // in proxy mode, where the broker's proxy listens
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
	c.cas = extraCAs()
	c.proxy = loadBrokerMode(state) == modeProxy
	c.broker.Proxy = c.proxy
	entries, err := allowed(c.broker)
	if err != nil {
		fail("%v", err)
	}
	c.egress = entries
	if c.mounts, err = loadMounts(state); err != nil {
		fail("%v", err)
	}
	return c
}

// userAllowlist is the user's own allowlist layer: hosts to add to the
// base (allowlist/lists/base.yaml), in the layer format.
func userAllowlist() string { return filepath.Join(stateRoot(), "allowlist.yaml") }

// allowed compiles the allowlist: the base, the JIG_LAYERS files, then the
// user's own layer if there is one. The hosts of the providers the broker
// serves are left out. In proxy mode they stay, and the providers' real
// hosts are added on 443: microsandbox must allow them for their
// connections to reach the broker, which decides. Only AWS's go, since
// the agent reaches AWS through the broker's own endpoint.
func allowed(b providers.Config) ([]allowlist.Entry, error) {
	files := strings.Fields(env("JIG_LAYERS", ""))
	if _, err := os.Stat(userAllowlist()); err == nil {
		files = append(files, userAllowlist())
	}
	layers, err := allowlist.Layers(files...)
	if err != nil {
		return nil, err
	}
	if !b.Proxy {
		return allowlist.Compile(layers, b.Brokered()...)
	}
	es, err := allowlist.Compile(layers, b.InternalOnly()...)
	if err != nil {
		return nil, err
	}
	for _, h := range b.Hosts() {
		e := allowlist.Entry{Pattern: h, Port: 443, Layer: "broker", Group: "proxy"}
		if !slices.ContainsFunc(es, func(o allowlist.Entry) bool { return o.String() == e.String() }) {
			es = append(es, e)
		}
	}
	return es, nil
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
	sock, closeBroker, err := startBroker(ctx, &c, log)
	if err != nil {
		fail("%v", err)
	}
	defer closeBroker()

	// Denials from this run start here (denials.go).
	var logStart int64
	if fi, err := os.Stat(runtimeLog(name)); err == nil {
		logStart = fi.Size()
	}
	sb, created, err := openSandbox(ctx, theBackend, c)
	if err != nil {
		fail("sandbox: %v", err)
	}
	defer sb.Close()
	log.Info("sandbox running", "name", name, "created", created)

	if err := provision(ctx, sb, c, log); err != nil {
		_ = sb.Stop(context.Background(), 30*time.Second)
		fail("provision: %v", err)
	}
	stopped := make(chan struct{})
	go func() {
		res, err := sb.Wait(context.Background())
		log.Info("sandbox stopped", "result", res, "err", err)
		close(stopped)
	}()
	// The bridge to the broker, and dockerd: root exec jobs, restarted if
	// they end (there's no systemd; the guest's init is microsandbox's).
	jobs := map[string]string{"bridge": hostsLine(c) + fmt.Sprintf(
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
		if err := sb.Stop(sctx, 30*time.Second); err != nil {
			log.Warn("stop failed; killing", "err", err)
			_ = sb.Kill(context.Background())
		}
		cancel()
		<-stopped
	}
	jobsDone.Wait()
	log.Info("down")
}

// keep runs cmd as a root job, so the agent user can't replace it, and
// restarts it until the sandbox stops or ctx ends.
func keep(ctx context.Context, sb sandbox.Guest, log *slog.Logger, stopped <-chan struct{}, name, cmd string) {
	for {
		code, err := sb.Job(ctx, cmd)
		err = errors.Join(err, fmt.Errorf("exit %d", code))
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
