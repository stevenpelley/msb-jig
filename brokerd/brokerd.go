// Package brokerd sets up the broker as the per-sandbox processes run it:
// the providers from flags, and the TLS server under the sandbox's CA.
// investigator-net (sbx, a bare broker) and the investigator supervisor
// (microsandbox) share it.
package brokerd

import (
	"context"
	"crypto/tls"
	_ "embed"
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
	AWSHost       = "aws.broker.internal"
)

// AWSProxyScript is the aws-mcp-proxy wrapper around AWS's
// mcp-proxy-for-aws: it starts while the SSO login has expired, and signs
// again after the next login, without a restart.
//
//go:embed aws-mcp-proxy.py
var AWSProxyScript string

// Flags configure the providers. Empty user or command leaves a provider out.
type Flags struct {
	GitLabUser, GitLabURL, GitLabTokenCmd, GitLabMCPTokenCmd string
	AnthropicTokenCmd, AnthropicURL                          string
	// AWSProfiles are the proxy's --profile list; the first is the
	// default, the rest are chosen per call with aws_profile.
	AWSProfiles              []string
	AWSMCPURL, AWSMCPCommand string
}

// Register adds the provider flags to fs.
func (f *Flags) Register(fs *flag.FlagSet) {
	fs.StringVar(&f.GitLabUser, "gitlab-user", "", "the GitLab user the agent acts for (no GitLab provider if empty)")
	fs.StringVar(&f.GitLabURL, "gitlab-url", "https://gitlab.com", "the GitLab the broker forwards to")
	fs.StringVar(&f.GitLabTokenCmd, "gitlab-token-command", "", "a command printing the user's GitLab token (default: $INVESTIGATOR_GITLAB_TOKEN)")
	fs.StringVar(&f.GitLabMCPTokenCmd, "gitlab-mcp-token-command", "", "a command printing the token for GitLab's MCP server, a fine-grained PAT (default: $INVESTIGATOR_GITLAB_MCP_TOKEN, else the GitLab token)")
	fs.StringVar(&f.AnthropicTokenCmd, "anthropic-token-command", "", "a command printing the Claude credential (`claude setup-token`'s OAuth token, or an API key); serves "+AnthropicHost)
	fs.StringVar(&f.AnthropicURL, "anthropic-url", "https://api.anthropic.com", "the Anthropic API the broker forwards to")
	fs.Func("aws-profile", "an AWS profile for the AWS MCP proxy (repeat; the first is the default); serves "+AWSHost, func(v string) error {
		f.AWSProfiles = append(f.AWSProfiles, v)
		return nil
	})
	fs.StringVar(&f.AWSMCPURL, "aws-mcp-url", DefaultAWSMCPURL, "the AWS MCP Server")
	fs.StringVar(&f.AWSMCPCommand, "aws-mcp-command", "", "a shell command that runs a stdio MCP proxy (default: the embedded aws-mcp-proxy under uv, with the profiles)")
}

// DefaultAWSMCPURL is the AWS MCP Server's endpoint.
const DefaultAWSMCPURL = "https://aws-mcp.us-east-1.api.aws/mcp"

// Providers builds the providers by hostname; empty if none is configured.
// Processes they run (the AWS MCP proxy) stop when ctx ends; their files
// go in state.
func (f *Flags) Providers(ctx context.Context, state string, log *slog.Logger) (map[string]http.Handler, error) {
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
	if len(f.AWSProfiles) > 0 || f.AWSMCPCommand != "" {
		a, err := f.awsMCP(state, log)
		if err != nil {
			return nil, err
		}
		a.Start(ctx)
		providers[AWSHost] = a
	}
	return providers, nil
}

// awsMCP runs the AWS MCP proxy: the embedded wrapper, written to the
// state directory, under uv. Its stderr goes to <state>/aws-mcp.log.
func (f *Flags) awsMCP(state string, log *slog.Logger) (*broker.AWSMCP, error) {
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, err
	}
	argv := []string{"/bin/sh", "-c", "exec " + f.AWSMCPCommand}
	if f.AWSMCPCommand == "" {
		uv, err := exec.LookPath("uv")
		if err != nil {
			return nil, errors.New("the AWS MCP proxy needs uv on PATH (brew install uv)")
		}
		script := filepath.Join(state, "aws-mcp-proxy.py")
		if err := os.WriteFile(script, []byte(AWSProxyScript), 0o700); err != nil {
			return nil, err
		}
		argv = []string{uv, "run", "--script", "--quiet", script, f.AWSMCPURL, "--skip-auth", "--profile"}
		argv = append(argv, f.AWSProfiles...)
	}
	stderr, err := os.OpenFile(filepath.Join(state, "aws-mcp.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	log.Info("aws mcp proxy", "argv", argv)
	return &broker.AWSMCP{Command: argv, Stderr: stderr, Log: log}, nil
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
