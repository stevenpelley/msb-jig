// Command investigator-net is the per-sandbox network process
// (investigator-design.md, "Packaging: one process per sandbox").
//
//	investigator-net lima   -name <net> -lima-dir <dir> -state <dir> ...
//	    the VM's only network, on Lima's user-v2 sockets: DHCP, DNS for our
//	    names only, the filter on the gateway's port 3128 and the broker on
//	    its port 443. Nothing else is reachable.
//	investigator-net filter -listen 127.0.0.1:3128 ...
//	    only the filter, on a TCP address (for testing tools against it).
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
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
		fmt.Fprintln(os.Stderr, "usage: investigator-net lima|filter [flags]")
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
	default:
		fail("unknown mode %q", os.Args[1])
	}
}

func lima(args []string, log *slog.Logger) {
	fs := flag.NewFlagSet("lima", flag.ExitOnError)
	name := fs.String("name", "", "the user-v2 network's name in Lima's networks.yaml")
	limaDir := fs.String("lima-dir", "", "the network's socket directory ($LIMA_HOME/_networks/<name>)")
	subnet := fs.String("subnet", "192.168.104.0/24", "the network's subnet, as in networks.yaml")
	state := fs.String("state", "", "this sandbox's state directory (the broker's CA lives here)")
	pidfile := fs.String("pidfile", "", "write our PID here (Lima's usernet_<name>.pid, until lima#5818)")
	glUser := fs.String("gitlab-user", "", "the GitLab user the agent acts for (no broker if empty)")
	glURL := fs.String("gitlab-url", "https://gitlab.com", "the GitLab the broker forwards to")
	glTokenCmd := fs.String("gitlab-token-command", "", "a command printing the user's GitLab token (default: $INVESTIGATOR_GITLAB_TOKEN)")
	glMCPTokenCmd := fs.String("gitlab-mcp-token-command", "", "a command printing the token for GitLab's MCP server, a fine-grained PAT (default: $INVESTIGATOR_GITLAB_MCP_TOKEN, else the GitLab token)")
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

	if *glUser != "" {
		ca, err := broker.LoadOrCreateCA(filepath.Join(*state, "broker-ca"), *name)
		if err != nil {
			fail("ca: %v", err)
		}
		up, err := url.Parse(*glURL)
		if err != nil || up.Scheme != "https" {
			fail("gitlab-url %q: want https://", *glURL)
		}
		const glHost = "gitlab.broker.internal"
		gl := &broker.GitLab{User: *glUser, Token: tokenSource(*glTokenCmd, "INVESTIGATOR_GITLAB_TOKEN"),
			Upstream: up, PublicHost: glHost, Client: broker.NewUpstreamClient(), Log: log}
		if *glMCPTokenCmd != "" || os.Getenv("INVESTIGATOR_GITLAB_MCP_TOKEN") != "" {
			gl.MCPToken = tokenSource(*glMCPTokenCmd, "INVESTIGATOR_GITLAB_MCP_TOKEN")
		}
		bl, err := n.ListenTCP(443)
		if err != nil {
			fail("%v", err)
		}
		srv := &http.Server{Handler: &broker.Server{Providers: map[string]http.Handler{glHost: gl}, Log: log},
			ReadHeaderTimeout: 30 * time.Second}
		go func() { _ = srv.Serve(tls.NewListener(bl, ca.TLSConfig([]string{glHost}))) }()
		log.Info("broker ready", "hosts", []string{glHost}, "ca", filepath.Join(*state, "broker-ca", "ca.pem"))
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
	for _, p := range []string{fd, qemu, ep} {
		_ = os.Remove(p)
	}
	log.Info("network down")
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
