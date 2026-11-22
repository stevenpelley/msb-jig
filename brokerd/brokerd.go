// Package brokerd sets up the broker as the per-sandbox processes run it:
// the providers from flags, and the TLS server under the sandbox's CA.
// investigator-net (Lima, sbx) and investigator-msb (microsandbox) share it.
package brokerd

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mcp-restrict/broker"
)

// Hostnames the sandbox uses for each provider.
const (
	GitLabHost    = "gitlab.broker.internal"
	AnthropicHost = "anthropic.broker.internal"
)

// Flags configure the providers. Empty user or command leaves a provider out.
type Flags struct {
	GitLabUser, GitLabURL, GitLabTokenCmd, GitLabMCPTokenCmd string
	AnthropicTokenCmd, AnthropicURL                          string
}

// Register adds the provider flags to fs.
func (f *Flags) Register(fs *flag.FlagSet) {
	fs.StringVar(&f.GitLabUser, "gitlab-user", "", "the GitLab user the agent acts for (no GitLab provider if empty)")
	fs.StringVar(&f.GitLabURL, "gitlab-url", "https://gitlab.com", "the GitLab the broker forwards to")
	fs.StringVar(&f.GitLabTokenCmd, "gitlab-token-command", "", "a command printing the user's GitLab token (default: $INVESTIGATOR_GITLAB_TOKEN)")
	fs.StringVar(&f.GitLabMCPTokenCmd, "gitlab-mcp-token-command", "", "a command printing the token for GitLab's MCP server, a fine-grained PAT (default: $INVESTIGATOR_GITLAB_MCP_TOKEN, else the GitLab token)")
	fs.StringVar(&f.AnthropicTokenCmd, "anthropic-token-command", "", "a command printing the Claude credential (`claude setup-token`'s OAuth token, or an API key); serves "+AnthropicHost)
	fs.StringVar(&f.AnthropicURL, "anthropic-url", "https://api.anthropic.com", "the Anthropic API the broker forwards to")
}

// Providers builds the providers by hostname; empty if none is configured.
func (f *Flags) Providers(log *slog.Logger) (map[string]http.Handler, error) {
	providers := map[string]http.Handler{}
	if f.GitLabUser != "" {
		up, err := url.Parse(f.GitLabURL)
		if err != nil || up.Scheme != "https" {
			return nil, fmt.Errorf("gitlab-url %q: want https://", f.GitLabURL)
		}
		gl := &broker.GitLab{User: f.GitLabUser, Token: TokenSource(f.GitLabTokenCmd, "INVESTIGATOR_GITLAB_TOKEN"),
			Upstream: up, PublicHost: GitLabHost, Client: broker.NewUpstreamClient(), Log: log}
		if f.GitLabMCPTokenCmd != "" || os.Getenv("INVESTIGATOR_GITLAB_MCP_TOKEN") != "" {
			gl.MCPToken = TokenSource(f.GitLabMCPTokenCmd, "INVESTIGATOR_GITLAB_MCP_TOKEN")
		}
		providers[GitLabHost] = gl
	}
	if f.AnthropicTokenCmd != "" {
		up, err := url.Parse(f.AnthropicURL)
		if err != nil || up.Scheme != "https" {
			return nil, fmt.Errorf("anthropic-url %q: want https://", f.AnthropicURL)
		}
		providers[AnthropicHost] = &broker.Anthropic{Token: TokenSource(f.AnthropicTokenCmd, ""), Upstream: up,
			PublicHost: AnthropicHost, Client: broker.NewUpstreamClient(), Log: log}
	}
	return providers, nil
}

// CAFile is where the sandbox's CA certificate lives (its key is beside it).
func CAFile(state string) string { return filepath.Join(state, "broker-ca", "ca.pem") }

// Serve serves the providers over TLS on l, under the sandbox's CA, in the
// background. Close the returned server to stop.
func Serve(l net.Listener, providers map[string]http.Handler, state, name string, log *slog.Logger) (*http.Server, error) {
	ca, err := broker.LoadOrCreateCA(filepath.Join(state, "broker-ca"), name)
	if err != nil {
		return nil, fmt.Errorf("ca: %w", err)
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
	log.Info("broker ready", "hosts", hosts, "ca", CAFile(state))
	return srv, nil
}

// ListenUnix listens on a Unix socket only the user can reach: mode 0600,
// in a 0700 directory. A stale socket from a killed process is replaced.
func ListenUnix(sock string) (net.Listener, error) {
	dir := filepath.Dir(sock)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// TokenSource returns a token getter: the command's output, cached for a
// minute, or $env when there is no command.
func TokenSource(command, env string) func() (string, error) {
	if command == "" {
		return func() (string, error) {
			if env != "" {
				if t := os.Getenv(env); t != "" {
					return t, nil
				}
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
