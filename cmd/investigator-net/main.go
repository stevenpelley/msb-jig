// Command investigator-net is the per-sandbox network process
// (investigator-design.md, "Packaging: one process per sandbox").
//
//	investigator-net lima   -name <net> -lima-dir <dir> -state <dir> ...
//	    the VM's only network, on Lima's user-v2 sockets: DHCP, DNS for our
//	    names only, the filter on the gateway's port 3128 and the broker on
//	    its port 443. Nothing else is reachable.
//	investigator-net sbx -gitlab-listen 127.0.0.1:<port> -session-token-file <f> ...
//	    only the broker, in plain HTTP on the Mac's loopback, for an sbx
//	    sandbox that reaches it at host.docker.internal:<port> through sbx's
//	    proxy. Every request must carry the session token.
//	investigator-net filter -listen 127.0.0.1:3128 ...
//	    only the filter, on a TCP address (for testing tools against it).
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"mcp-restrict/broker"
	"mcp-restrict/filter"
	netstack "mcp-restrict/net"
)

type layerFlags []string

func (l *layerFlags) String() string     { return strings.Join(*l, ",") }
func (l *layerFlags) Set(v string) error { *l = append(*l, v); return nil }

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "investigator-net: "+f+"\n", a...)
	os.Exit(1)
}

// filterFlags are shared by both modes.
type filterFlags struct {
	profile, aclFile, upstream, denials *string
	layers                              layerFlags
}

func addFilterFlags(fs *flag.FlagSet) *filterFlags {
	f := &filterFlags{
		profile:  fs.String("profile", "investigator", "profile layer: "+strings.Join(filter.Profiles, " or ")),
		aclFile:  fs.String("acl", "", "an ACL file to use instead of the layers"),
		upstream: fs.String("upstream", "", `upstream proxy URL (http://[user:pw@]host:port), or "env" for $HTTPS_PROXY`),
		denials:  fs.String("denials", "", "append denials to this file, one JSON object per line"),
	}
	fs.Var(&f.layers, "layer", "an org or user layer file, in order (repeatable)")
	return f
}

func (f *filterFlags) proxy(log *slog.Logger) *filter.Proxy {
	acl, err := loadACL(*f.aclFile, *f.profile, f.layers)
	if err != nil {
		fail("%v", err)
	}
	p := &filter.Proxy{ACL: acl, Log: log}
	if u := *f.upstream; u != "" {
		if u == "env" {
			if u = os.Getenv("HTTPS_PROXY"); u == "" {
				u = os.Getenv("https_proxy")
			}
		}
		pu, err := url.Parse(u)
		if err != nil || pu.Scheme != "http" || pu.Host == "" {
			fail("upstream %q: want http://host:port", u)
		}
		p.Upstream = pu
	}
	if *f.denials != "" {
		fh, err := os.OpenFile(*f.denials, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fail("%v", err)
		}
		var mu sync.Mutex
		enc := json.NewEncoder(fh)
		p.OnDeny = func(d filter.Denial) { mu.Lock(); _ = enc.Encode(d); mu.Unlock() }
	}
	log.Info("filter ready", "entries", len(acl.Entries()), "upstream", p.Upstream.Redacted())
	return p
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: investigator-net lima|sbx|broker|filter [flags]")
		os.Exit(2)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	switch os.Args[1] {
	case "filter":
		fs := flag.NewFlagSet("filter", flag.ExitOnError)
		listen := fs.String("listen", "127.0.0.1:3128", "TCP address to serve the filter on")
		ff := addFilterFlags(fs)
		_ = fs.Parse(os.Args[2:])
		srv := &http.Server{Addr: *listen, Handler: ff.proxy(log), ReadHeaderTimeout: 30 * time.Second}
		log.Info("filter listening", "addr", *listen)
		fail("%v", srv.ListenAndServe())
	case "lima":
		lima(os.Args[2:], log)
	case "sbx":
		sbx(os.Args[2:], log)
	case "broker":
		brokerOnSocket(os.Args[2:], log)
	default:
		fail("unknown mode %q", os.Args[1])
	}
}

type brokerFlags struct {
	glUser, glURL, glTokenCmd, glMCPTokenCmd, anTokenCmd, anURL *string
}

func addBrokerFlags(fs *flag.FlagSet) *brokerFlags {
	return &brokerFlags{
		glUser:        fs.String("gitlab-user", "", "the GitLab user the agent acts for (no GitLab provider if empty)"),
		glURL:         fs.String("gitlab-url", "https://gitlab.com", "the GitLab the broker forwards to"),
		glTokenCmd:    fs.String("gitlab-token-command", "", "a command printing the user's GitLab token (default: $INVESTIGATOR_GITLAB_TOKEN)"),
		glMCPTokenCmd: fs.String("gitlab-mcp-token-command", "", "a command printing the token for GitLab's MCP server, a fine-grained PAT (default: $INVESTIGATOR_GITLAB_MCP_TOKEN, else the GitLab token)"),
		anTokenCmd:    fs.String("anthropic-token-command", "", "a command printing the Claude credential (`claude setup-token`'s OAuth token, or an API key); serves anthropic.broker.internal"),
		anURL:         fs.String("anthropic-url", "https://api.anthropic.com", "the Anthropic API the broker forwards to"),
	}
}

// providers builds the broker's providers by hostname; empty if none is configured.
func (b *brokerFlags) providers(log *slog.Logger) map[string]http.Handler {
	providers := map[string]http.Handler{}
	if *b.glUser != "" {
		up, err := url.Parse(*b.glURL)
		if err != nil || up.Scheme != "https" {
			fail("gitlab-url %q: want https://", *b.glURL)
		}
		const glHost = "gitlab.broker.internal"
		gl := &broker.GitLab{User: *b.glUser, Token: tokenSource(*b.glTokenCmd, "INVESTIGATOR_GITLAB_TOKEN"),
			Upstream: up, PublicHost: glHost, Client: broker.NewUpstreamClient(), Log: log}
		if *b.glMCPTokenCmd != "" || os.Getenv("INVESTIGATOR_GITLAB_MCP_TOKEN") != "" {
			gl.MCPToken = tokenSource(*b.glMCPTokenCmd, "INVESTIGATOR_GITLAB_MCP_TOKEN")
		}
		providers[glHost] = gl
	}
	if *b.anTokenCmd != "" {
		up, err := url.Parse(*b.anURL)
		if err != nil || up.Scheme != "https" {
			fail("anthropic-url %q: want https://", *b.anURL)
		}
		const anHost = "anthropic.broker.internal"
		providers[anHost] = &broker.Anthropic{Token: tokenSource(*b.anTokenCmd, ""), Upstream: up,
			PublicHost: anHost, Client: broker.NewUpstreamClient(), Log: log}
	}
	return providers
}

// serveBroker serves the providers over TLS on l, under the sandbox's CA.
func serveBroker(l net.Listener, providers map[string]http.Handler, state, name string, log *slog.Logger) *http.Server {
	ca, err := broker.LoadOrCreateCA(filepath.Join(state, "broker-ca"), name)
	if err != nil {
		fail("ca: %v", err)
	}
	hosts := make([]string, 0, len(providers))
	for h := range providers {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	// No WriteTimeout: model responses stream for minutes.
	srv := &http.Server{Handler: &broker.Server{Providers: providers, Log: log},
		ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(tls.NewListener(l, ca.TLSConfig(hosts))) }()
	log.Info("broker ready", "hosts", hosts, "ca", filepath.Join(state, "broker-ca", "ca.pem"))
	return srv
}

// brokerOnSocket serves the broker alone on a Unix socket, for a runtime
// that owns the VM's network and exposes host sockets to the guest
// (microsandbox's vsock routes). The socket's path is the sandbox's
// identity: it lives in the sandbox's 0700 state directory.
func brokerOnSocket(args []string, log *slog.Logger) {
	fs := flag.NewFlagSet("broker", flag.ExitOnError)
	name := fs.String("name", "", "the sandbox's name (the CA's name)")
	state := fs.String("state", "", "this sandbox's state directory (the broker's CA lives here)")
	sock := fs.String("socket", "", "the Unix socket to serve on (default <state>/broker.sock)")
	bf := addBrokerFlags(fs)
	_ = fs.Parse(args)
	if *name == "" || *state == "" {
		fail("broker: -name and -state are required")
	}
	if *sock == "" {
		*sock = filepath.Join(*state, "broker.sock")
	}
	providers := bf.providers(log)
	if len(providers) == 0 {
		fail("broker: no provider configured (-gitlab-user, -anthropic-token-command)")
	}
	if err := os.MkdirAll(*state, 0o700); err != nil {
		fail("%v", err)
	}
	if err := os.Chmod(*state, 0o700); err != nil {
		fail("%v", err)
	}
	_ = os.Remove(*sock)
	l, err := net.Listen("unix", *sock)
	if err != nil {
		fail("%v", err)
	}
	if err := os.Chmod(*sock, 0o600); err != nil {
		fail("%v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv := serveBroker(l, providers, *state, *name, log)
	log.Info("broker listening", "socket", *sock)
	<-ctx.Done()
	_ = srv.Close()
	_ = os.Remove(*sock)
	log.Info("broker down")
}

func lima(args []string, log *slog.Logger) {
	fs := flag.NewFlagSet("lima", flag.ExitOnError)
	name := fs.String("name", "", "the user-v2 network's name in Lima's networks.yaml")
	limaDir := fs.String("lima-dir", "", "the network's socket directory ($LIMA_HOME/_networks/<name>)")
	subnet := fs.String("subnet", "192.168.104.0/24", "the network's subnet, as in networks.yaml")
	state := fs.String("state", "", "this sandbox's state directory (the broker's CA lives here)")
	pidfile := fs.String("pidfile", "", "write our PID here (Lima's usernet_<name>.pid, until lima#5818)")
	networksYAML := fs.String("networks-yaml", "", "on exit, remove -name from this networks.yaml ($LIMA_HOME/_config/networks.yaml), so Lima can't start its own daemon for it")
	bf := addBrokerFlags(fs)
	ff := addFilterFlags(fs)
	_ = fs.Parse(args)
	if *name == "" || *limaDir == "" || *state == "" {
		fail("lima: -name, -lima-dir and -state are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	n, err := netstack.New(netstack.Config{Subnet: *subnet, Log: log})
	if err != nil {
		fail("%v", err)
	}
	defer n.Close()
	if err := n.ServeDNS(netstack.DefaultNames); err != nil {
		fail("dns: %v", err)
	}
	ln, err := n.ListenTCP(3128)
	if err != nil {
		fail("%v", err)
	}
	go func() { _ = (&http.Server{Handler: ff.proxy(log), ReadHeaderTimeout: 30 * time.Second}).Serve(ln) }()

	if providers := bf.providers(log); len(providers) > 0 {
		bl, err := n.ListenTCP(443)
		if err != nil {
			fail("%v", err)
		}
		serveBroker(bl, providers, *state, *name, log)
	}

	if err := n.ServeLima(ctx, *limaDir, *name); err != nil {
		fail("lima sockets: %v", err)
	}
	if *pidfile != "" {
		if err := os.WriteFile(*pidfile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			fail("pidfile: %v", err)
		}
		defer os.Remove(*pidfile)
	}
	fd, qemu, ep := netstack.SocketPaths(*limaDir, *name)
	log.Info("network up", "gateway", n.Gateway().String(), "fd", fd, "qemu", qemu, "ep", ep)
	<-ctx.Done()
	// First take the network out of Lima's config, then the sockets.
	if *networksYAML != "" {
		if err := dropNetwork(*networksYAML, *name); err != nil {
			log.Error("networks.yaml: network left defined", "file", *networksYAML, "err", err)
		} else {
			log.Info("networks.yaml: network removed", "file", *networksYAML)
		}
	}
	for _, p := range []string{fd, qemu, ep} {
		_ = os.Remove(p)
	}
	log.Info("network down")
}

func sbx(args []string, log *slog.Logger) {
	fs := flag.NewFlagSet("sbx", flag.ExitOnError)
	listen := fs.String("gitlab-listen", "127.0.0.1:18443", "where to serve the GitLab broker (loopback only)")
	public := fs.String("public-host", "", "host:port the sandbox uses (default host.docker.internal:<port>)")
	tokFile := fs.String("session-token-file", "", "the session token, created (0600) if missing")
	glUser := fs.String("gitlab-user", "", "the GitLab user the agent acts for")
	glURL := fs.String("gitlab-url", "https://gitlab.com", "the GitLab the broker forwards to")
	glTokenCmd := fs.String("gitlab-token-command", "", "a command printing the user's GitLab token (default: $INVESTIGATOR_GITLAB_TOKEN)")
	glMCPTokenCmd := fs.String("gitlab-mcp-token-command", "", "the same for GitLab's MCP server (default: $INVESTIGATOR_GITLAB_MCP_TOKEN, else the GitLab token)")
	_ = fs.Parse(args)
	if *glUser == "" || *tokFile == "" {
		fail("sbx: -gitlab-user and -session-token-file are required")
	}
	host, port, err := net.SplitHostPort(*listen)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		fail("sbx: -gitlab-listen must be a loopback address and port")
	}
	if *public == "" {
		*public = "host.docker.internal:" + port
	}
	session, err := sessionToken(*tokFile)
	if err != nil {
		fail("session token: %v", err)
	}
	up, err := url.Parse(*glURL)
	if err != nil || up.Scheme != "https" {
		fail("gitlab-url %q: want https://", *glURL)
	}
	gl := &broker.GitLab{User: *glUser, Token: tokenSource(*glTokenCmd, "INVESTIGATOR_GITLAB_TOKEN"),
		Upstream: up, PublicHost: *public, PublicScheme: "http", Client: broker.NewUpstreamClient(), Log: log}
	if *glMCPTokenCmd != "" || os.Getenv("INVESTIGATOR_GITLAB_MCP_TOKEN") != "" {
		gl.MCPToken = tokenSource(*glMCPTokenCmd, "INVESTIGATOR_GITLAB_MCP_TOKEN")
	}
	srv := &http.Server{Addr: *listen, Handler: &broker.Server{Only: gl, SessionToken: session, Log: log},
		ReadHeaderTimeout: 30 * time.Second}
	log.Info("broker listening (sbx mode)", "addr", *listen, "public", "http://"+*public, "token_file", *tokFile)
	fail("%v", srv.ListenAndServe())
}

// sessionToken reads the token, or makes a random one.
func sessionToken(file string) (string, error) {
	if b, err := os.ReadFile(file); err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 32 {
			return t, nil
		}
		return "", fmt.Errorf("%s: too short", file)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	t := hex.EncodeToString(b)
	return t, os.WriteFile(file, []byte(t+"\n"), 0o600)
}

// tokenSource returns the user's token from a command (run at most once a
// minute) or an environment variable.
func tokenSource(command, env string) func() (string, error) {
	if command == "" {
		return func() (string, error) {
			if t := os.Getenv(env); t != "" {
				return t, nil
			}
			return "", errors.New(env + " is not set")
		}
	}
	var mu sync.Mutex
	var tok string
	var at time.Time
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if tok != "" && time.Since(at) < time.Minute {
			return tok, nil
		}
		out, err := exec.Command("/bin/sh", "-c", command).Output()
		if err != nil {
			return "", fmt.Errorf("token command: %w", err)
		}
		tok, at = strings.TrimSpace(string(out)), time.Now()
		if tok == "" {
			return "", errors.New("token command printed nothing")
		}
		return tok, nil
	}
}

func loadACL(file, profile string, layers []string) (*filter.ACL, error) {
	var es []filter.Entry
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if es, err = filter.ParseACL(f); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
	} else {
		ls, err := filter.Layers(profile, layers...)
		if err != nil {
			return nil, err
		}
		if es, err = filter.Compile(ls); err != nil {
			return nil, err
		}
	}
	return filter.NewACL(es)
}
