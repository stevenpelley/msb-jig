// Command investigator-net serves the broker or the filter on their own,
// outside the investigator supervisor (which runs the broker for a
// microsandbox sandbox itself). Its Lima mode, the VM's whole network on
// Lima's user-v2 sockets, was removed when microsandbox replaced Lima
// (2026-11-22; microsandbox.md).
//
//	investigator-net broker -name <n> -state <dir> [-socket <path>] ...
//	    only the broker, on a Unix socket.
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
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"mcp-restrict/broker"
	"mcp-restrict/brokerd"
	"mcp-restrict/filter"
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
		fmt.Fprintln(os.Stderr, "usage: investigator-net sbx|broker|filter [flags]")
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
	case "sbx":
		sbx(os.Args[2:], log)
	case "broker":
		brokerOnSocket(os.Args[2:], log)
	default:
		fail("unknown mode %q", os.Args[1])
	}
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
	var bf brokerd.Flags
	bf.Register(fs)
	_ = fs.Parse(args)
	if *name == "" || *state == "" {
		fail("broker: -name and -state are required")
	}
	if *sock == "" {
		*sock = filepath.Join(*state, "broker.sock")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	providers, err := bf.Providers(ctx, *state, log)
	if err != nil {
		fail("%v", err)
	}
	if len(providers) == 0 {
		fail("broker: no provider configured (-gitlab-user, -anthropic-token-command, -aws-profile)")
	}
	l, err := brokerd.ListenUnix(*sock)
	if err != nil {
		fail("%v", err)
	}
	srv, err := brokerd.Serve(l, providers, *state, *name, log)
	if err != nil {
		fail("%v", err)
	}
	log.Info("broker listening", "socket", *sock)
	<-ctx.Done()
	_ = srv.Close()
	_ = os.Remove(*sock)
	log.Info("broker down")
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
	gl := &broker.GitLab{User: *glUser, Token: brokerd.TokenSource(*glTokenCmd, "INVESTIGATOR_GITLAB_TOKEN"),
		Upstream: up, PublicHost: *public, PublicScheme: "http", Client: broker.NewUpstreamClient(), Log: log}
	if *glMCPTokenCmd != "" || os.Getenv("INVESTIGATOR_GITLAB_MCP_TOKEN") != "" {
		gl.MCPToken = brokerd.TokenSource(*glMCPTokenCmd, "INVESTIGATOR_GITLAB_MCP_TOKEN")
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
