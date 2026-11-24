// Command mono-jig-sbx serves the broker's code-host providers to a
// Docker sbx sandbox: plain HTTP on the Mac's loopback, one port per
// provider, which the sandbox reaches at host.docker.internal:<port>
// through sbx's own proxy. sbx has no private channel from the sandbox to
// the Mac, so every request must carry a session token (sbx injects it as
// the provider's credential). The providers and their rules are the same
// as under microsandbox (broker/providers).
//
//	mono-jig-sbx -session-token-file <f> -gitlab-user <u> [-gitlab-listen 127.0.0.1:18443] \
//	    [-github-user <u> -github-listen 127.0.0.1:18444]
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"mono-jig/broker"
	"mono-jig/broker/providers"
)

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "mono-jig-sbx: "+f+"\n", a...)
	os.Exit(1)
}

func main() {
	tokFile := flag.String("session-token-file", "", "the session token, created (0600) if missing")
	glUser := flag.String("gitlab-user", "", "the GitLab user the agent acts for (no GitLab if empty)")
	glURL := flag.String("gitlab-url", "https://gitlab.com", "the GitLab the broker forwards to")
	glListen := flag.String("gitlab-listen", "127.0.0.1:18443", "where to serve GitLab (loopback only)")
	glTokenCmd := flag.String("gitlab-token-command", "", "a command printing the user's GitLab token (default: $JIG_GITLAB_TOKEN)")
	glMCPTokenCmd := flag.String("gitlab-mcp-token-command", "", "the same for GitLab's MCP server (default: the GitLab token)")
	ghUser := flag.String("github-user", "", "the GitHub user the agent acts for (no GitHub if empty)")
	ghListen := flag.String("github-listen", "127.0.0.1:18444", "where to serve GitHub (loopback only)")
	ghTokenCmd := flag.String("github-token-command", "", "a command printing the user's GitHub token (default: $JIG_GITHUB_TOKEN)")
	flag.Parse()
	if *tokFile == "" || (*glUser == "" && *ghUser == "") {
		fail("-session-token-file and -gitlab-user or -github-user are required")
	}
	session, err := sessionToken(*tokFile)
	if err != nil {
		fail("session token: %v", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	errs := make(chan error, 2)
	// Each provider on its own listener, from the same route table as the
	// microsandbox broker (broker/providers).
	if *glUser != "" {
		up, err := url.Parse(*glURL)
		if err != nil || up.Scheme != "https" {
			fail("gitlab-url %q: want https://", *glURL)
		}
		pub := public(*glListen)
		p := build(providers.GitLab{User: *glUser, URL: *glURL, MCPTokenCmd: *glMCPTokenCmd,
			TokenCmd: tokenCmd(*glTokenCmd, "JIG_GITLAB_TOKEN")}, pub, log)[providers.GitLabHost]
		go func() { errs <- listen(*glListen, pub, p, session, log) }()
	}
	if *ghUser != "" {
		pub := public(*ghListen)
		p := build(providers.GitHub{User: *ghUser,
			TokenCmd: tokenCmd(*ghTokenCmd, "JIG_GITHUB_TOKEN")}, pub, log)[providers.GitHubHost]
		go func() { errs <- listen(*ghListen, pub, p, session, log) }()
	}
	fail("%v", <-errs)
}

// build builds one provider, reached by the sandbox at pub.
func build(p providers.Provider, pub *url.URL, log *slog.Logger) map[string]http.Handler {
	c := providers.Config{Providers: []providers.Provider{p}, Public: func(string) *url.URL { return pub }}
	ps, err := providers.Build(context.Background(), c, "", log)
	if err != nil {
		fail("%v", err)
	}
	return ps
}

// tokenCmd is the token command, else one printing the environment
// variable env.
func tokenCmd(cmd, env string) string {
	if cmd != "" {
		return cmd
	}
	return "printenv " + env
}

// public is how the sandbox reaches a listener: host.docker.internal on
// the same port, in plain HTTP.
func public(listen string) *url.URL {
	host, port, err := net.SplitHostPort(listen)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		fail("%s: listen on a loopback address and port", listen)
	}
	return &url.URL{Scheme: "http", Host: "host.docker.internal:" + port}
}

func listen(addr string, pub *url.URL, p http.Handler, session string, log *slog.Logger) error {
	srv := &http.Server{Addr: addr, ReadHeaderTimeout: 30 * time.Second,
		Handler: sessionCheck(session, &broker.Server{Providers: map[string]http.Handler{pub.Hostname(): p}})}
	log.Info("listening", "addr", addr, "public", pub.String())
	return srv.ListenAndServe()
}

// sessionCheck refuses requests without the session token, wherever the
// sandbox's tools put their credential: git's http.extraHeader
// (X-Jig-Session), glab's PRIVATE-TOKEN, a Bearer or "token"
// token, or a Basic password. broker.Server then drops all of them.
func sessionCheck(session string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cands := []string{r.Header.Get("X-Jig-Session"), r.Header.Get("Private-Token")}
		a := r.Header.Get("Authorization")
		for _, scheme := range []string{"Bearer ", "token "} {
			if t, ok := strings.CutPrefix(a, scheme); ok {
				cands = append(cands, t)
			}
		}
		if b, ok := strings.CutPrefix(a, "Basic "); ok {
			if raw, err := base64.StdEncoding.DecodeString(b); err == nil {
				if _, pw, ok := strings.Cut(string(raw), ":"); ok {
					cands = append(cands, pw)
				}
			}
		}
		for _, c := range cands {
			if c != "" && subtle.ConstantTimeCompare([]byte(c), []byte(session)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}
		broker.HTTPError(w, http.StatusUnauthorized, "missing or wrong session token")
	})
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
