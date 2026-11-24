// Package providers is the broker's provider registry. Start reviewing
// the broker here: each provider (gitlab.go, github.go, anthropic.go,
// aws.go) says what it's served under, which function holds its policy,
// and what the guest needs for it. That function (Routes, in each
// provider's package) defines the provider's upstreams, each with its
// credential, then its route table: every request it serves, the check
// run on it and where it goes. A request that matches no route is denied.
//
// The checks are next to Routes (checks.go and the like); the rules the
// providers share are in broker/policy.go, credential headers in
// broker/upstream.go.
//
// A track (docs/library-split.md) picks its providers: Config.Providers
// holds those that are on. A new one implements Provider.
package providers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"mono-jig/broker"
	"mono-jig/guest"
)

// Hostnames the sandbox uses for each provider in server mode. In proxy
// mode it uses the real ones, except AWSHost: the AWS MCP endpoint is the
// broker's own, with no real host behind it.
const (
	GitLabHost    = "gitlab.broker.internal"
	GitHubHost    = "github.broker.internal"
	AnthropicHost = "anthropic.broker.internal"
	AWSHost       = "aws.broker.internal"
)

// InternalSuffix ends the names the broker serves on its own behalf,
// which the guest points at its bridge.
const InternalSuffix = ".broker.internal"

// Provider is one service the broker can serve.
type Provider interface {
	// Name is its allowlist group (whose hosts leave the allowlist while
	// the broker serves it) and its mono-jig login name.
	Name() string
	// Hosts are the names it's served under: in server mode under
	// InternalSuffix; in proxy mode its real hosts, which the broker
	// terminates, or an internal name for an endpoint with no real host.
	Hosts(proxy bool) []string
	// Handlers serve it, by host (Hosts(b.Proxy)). Processes they start
	// stop when ctx ends.
	Handlers(ctx context.Context, b *Builder) (map[string]http.Handler, error)
	// Guest is what the guest needs for it: its tools' settings and its
	// section of the agent's instructions.
	Guest(proxy bool) guest.Config
}

// Config is the providers to serve, and how.
type Config struct {
	Providers []Provider // those that are on

	// Overrides for tests and mono-jig-sbx; empty means the default.
	Public func(host string) *url.URL // how the sandbox reaches host: https://<host>
	Client *http.Client               // the upstreams': broker.NewUpstreamClient()

	// Proxy: proxy mode (docs/transparent-broker.md). The providers are
	// served under their real hostnames (Hosts), Location headers aren't
	// rewritten, and GitHub and GitLab forward unmatched reads without the
	// credential.
	Proxy bool
}

// Get is the provider named name, or nil if it's off.
func (c Config) Get(name string) Provider {
	for _, p := range c.Providers {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// Hosts are the real hostnames proxy mode terminates: the CONNECT front
// end's and the CA's names. Internal names aren't among them.
func (c Config) Hosts() []string {
	var out []string
	for _, p := range c.Providers {
		for _, h := range p.Hosts(true) {
			if !strings.HasSuffix(h, InternalSuffix) {
				out = append(out, h)
			}
		}
	}
	return out
}

// Internal are the internal names served in this mode, which the guest
// points at the broker's bridge.
func (c Config) Internal() []string {
	var out []string
	for _, p := range c.Providers {
		for _, h := range p.Hosts(c.Proxy) {
			if strings.HasSuffix(h, InternalSuffix) {
				out = append(out, h)
			}
		}
	}
	return out
}

// Brokered names the providers that are on, as the allowlist's groups
// name them.
func (c Config) Brokered() []string {
	var out []string
	for _, p := range c.Providers {
		out = append(out, p.Name())
	}
	return out
}

// InternalOnly names the providers with no real host (AWS's): in proxy
// mode their groups still leave the allowlist, since the guest reaches
// them through the broker's own name.
func (c Config) InternalOnly() []string {
	var out []string
	for _, p := range c.Providers {
		if !slices.ContainsFunc(p.Hosts(true), func(h string) bool { return !strings.HasSuffix(h, InternalSuffix) }) {
			out = append(out, p.Name())
		}
	}
	return out
}

// Guest is what the guest needs for the providers, in order.
func (c Config) Guest() guest.Config {
	var g guest.Config
	for _, p := range c.Providers {
		g.Add(p.Guest(c.Proxy))
	}
	return g
}

// Build returns the providers' handlers by hostname. Processes they run
// (the AWS MCP proxy) stop when ctx ends; their files go in state.
func Build(ctx context.Context, c Config, state string, log *slog.Logger) (map[string]http.Handler, error) {
	b := &Builder{Proxy: c.Proxy, State: state, Log: log, public: c.Public, client: c.Client}
	if b.client == nil {
		b.client = broker.NewUpstreamClient()
	}
	if b.public == nil {
		b.public = func(host string) *url.URL { return &url.URL{Scheme: "https", Host: host} }
	}
	out := map[string]http.Handler{}
	for _, p := range c.Providers {
		hs, err := p.Handlers(ctx, b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name(), err)
		}
		for h, x := range hs {
			if _, dup := out[h]; dup {
				return nil, fmt.Errorf("%s: %s is served twice", p.Name(), h)
			}
			out[h] = x
		}
	}
	return out, nil
}

// Builder makes the providers' upstreams and handlers.
type Builder struct {
	Proxy  bool
	State  string // where a provider's files go
	Log    *slog.Logger
	public func(host string) *url.URL
	client *http.Client
}

// Upstreams are a provider's: reached by the sandbox at host (in server
// mode), with a missing credential sending the user to mono-jig login
// <login>. In proxy mode the sandbox reaches each upstream at its own URL:
// no Public, so no Location rewrite.
func (b *Builder) Upstreams(host, login string) broker.Upstreams {
	u := broker.Upstreams{Client: b.client, NoCredential: "no credential: run mono-jig login " + login}
	if !b.Proxy {
		u.Public = b.public(host)
	}
	return u
}

// Provider serves a route table.
func (b *Builder) Provider(name string, routes []broker.Route) http.Handler {
	return &broker.Provider{Name: name, Routes: routes, Log: b.Log}
}

// Token runs the token command (at most once a minute).
func Token(cmd string) broker.Token { return broker.TokenSource(cmd, "") }

// OptToken is Token, or nil without a command.
func OptToken(cmd string) broker.Token {
	if cmd == "" {
		return nil
	}
	return Token(cmd)
}

// InsteadOf points git's URLs for host at the broker's URL.
func InsteadOf(broker, host string) map[string][]string {
	return map[string][]string{"url." + broker + "/.insteadOf": {
		"https://" + host + "/", "git@" + host + ":", "ssh://git@" + host + "/", "http://" + host + "/",
	}}
}
