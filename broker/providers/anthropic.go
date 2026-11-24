package providers

import (
	"context"
	"net/http"

	"mono-jig/broker/anthropic"
	"mono-jig/guest"
)

// AnthropicAPIHost is the Claude API's real host, which proxy mode
// terminates.
const AnthropicAPIHost = "api.anthropic.com"

// Anthropic: the Claude API, for the agent. Under AnthropicHost in server
// mode, AnthropicAPIHost in proxy mode. The agent's settings for it
// (Claude Code's) are the agent's, not the provider's.
type Anthropic struct {
	TokenCmd string // prints the user's token
	APIURL   string // tests: the API, if not https://api.anthropic.com
}

func (Anthropic) Name() string { return "anthropic" }

func (Anthropic) Hosts(proxy bool) []string {
	if proxy {
		return []string{AnthropicAPIHost}
	}
	return []string{AnthropicHost}
}

func (a Anthropic) Handlers(_ context.Context, b *Builder) (map[string]http.Handler, error) {
	p := b.Provider("anthropic", anthropic.Routes(b.Upstreams(AnthropicHost, "claude"),
		anthropic.Config{Token: Token(a.TokenCmd), APIURL: a.APIURL}))
	return map[string]http.Handler{a.Hosts(b.Proxy)[0]: p}, nil
}

func (Anthropic) Guest(bool) guest.Config { return guest.Config{} }
