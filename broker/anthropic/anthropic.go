// Package anthropic is the broker's Anthropic provider, served as
// anthropic.broker.internal. It forwards Claude Code's API calls with the
// user's credential, which never enters the sandbox: there, Claude Code
// holds a placeholder and ANTHROPIC_BASE_URL points here. It has no
// per-request rules; its job is custody of the credential, so that
// api.anthropic.com can leave the allowlist.
package anthropic

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"mcp-restrict/broker"
)

// Config configures the provider.
type Config struct {
	// Token returns the user's credential: an OAuth token from
	// `claude setup-token` (sk-ant-oat…), sent as a Bearer token, or an API
	// key, sent as x-api-key.
	Token  func() (string, error)
	API    *url.URL // https://api.anthropic.com
	Public *url.URL // https://anthropic.broker.internal
	Client *http.Client
	Log    *slog.Logger
}

// New returns the provider. Every request it serves:
//
//	any method  /v1/…   -> api.anthropic.com, with the credential
//	any method  /api/…  -> api.anthropic.com, with the credential
func New(c Config) *broker.Provider {
	api := &broker.Upstream{URL: c.API, Public: c.Public, Client: c.Client,
		NoCredential: "no Claude credential: run investigator login claude",
		Auth: func(h http.Header) error {
			tok, err := c.Token()
			if err != nil {
				return err
			}
			setAuth(h, tok)
			return nil
		}}
	return &broker.Provider{Name: "anthropic", Log: c.Log, Routes: []broker.Route{
		{Method: "*", Path: "/v1/{path...}", Op: "anthropic.call", Check: broker.Any, To: api},
		{Method: "*", Path: "/api/{path...}", Op: "anthropic.call", Check: broker.Any, To: api},
	}}
}

// oauthBeta is the beta flag the API wants with OAuth tokens (as Claude
// Code sends it; unverified that the API still requires it).
const oauthBeta = "oauth-2025-04-20"

// setAuth sets the credential in the form its kind needs.
func setAuth(h http.Header, tok string) {
	if !strings.HasPrefix(tok, "sk-ant-oat") {
		h.Set("X-Api-Key", tok)
		return
	}
	h.Set("Authorization", "Bearer "+tok)
	for _, v := range h.Values("Anthropic-Beta") {
		for _, f := range strings.Split(v, ",") {
			if strings.TrimSpace(f) == oauthBeta {
				return
			}
		}
	}
	h.Add("Anthropic-Beta", oauthBeta)
}
