// Package anthropic is the broker's Anthropic provider, served as
// anthropic.broker.internal. It forwards Claude Code's API calls with the
// user's credential, which never enters the sandbox: there, Claude Code
// holds a placeholder and ANTHROPIC_BASE_URL points here. It has no
// per-request rules; its job is custody of the credential, so that
// api.anthropic.com can leave the allowlist.
package anthropic

import (
	"net/http"
	"strings"

	"mono-jig/broker"
)

// Routes is every request the Anthropic provider serves: its upstream,
// with the user's credential, then its routes. No per-request rules, but
// one: the credential stays in custody.
func Routes(up broker.Upstreams, c Config) []broker.Route {
	api := up.To(c.api(), auth(c.Token))

	return []broker.Route{
		// OAuth account endpoints: reads only (Claude Code reads the
		// profile and roles). A write there can mint a lasting credential,
		// such as Claude Code's create_api_key at login, which would put
		// one in the sandbox. Which writes exist is unverified, so all are
		// refused.
		{Method: "GET HEAD", Path: "/api/oauth/{path...}", Op: "anthropic.account", Check: broker.Read, To: api},
		{Method: "*", Path: "/api/oauth/{path...}", Op: "anthropic.account", Check: broker.Never("writes to the account's OAuth endpoints could mint a credential in the sandbox")},

		{Method: "*", Path: "/v1/{path...}", Op: "anthropic.call", Check: broker.Any, To: api},
		{Method: "*", Path: "/api/{path...}", Op: "anthropic.call", Check: broker.Any, To: api},
	}
}

// Config is what the provider needs from mono-jig login.
type Config struct {
	Token  broker.Token // the user's credential (the Keychain)
	APIURL string       // tests: the API, if not https://api.anthropic.com
}

func (c Config) api() string {
	if c.APIURL != "" {
		return c.APIURL
	}
	return "https://api.anthropic.com"
}

// auth sends the user's credential: an OAuth token from
// `claude setup-token` (sk-ant-oat…) as a Bearer token, or an API key as
// x-api-key.
func auth(tok broker.Token) func(http.Header) error {
	return func(h http.Header) error {
		t, err := tok()
		if err != nil {
			return err
		}
		setAuth(h, t)
		return nil
	}
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
