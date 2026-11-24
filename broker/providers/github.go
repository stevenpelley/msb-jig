package providers

import (
	"context"
	"net/http"

	"msb-jig/broker/github"
	"msb-jig/guest"
)

// GitHub: git, REST and GraphQL, for one user. In server mode under
// GitHubHost, as gh calls a GitHub Enterprise Server; in proxy mode under
// WebHost (git) and APIHost (the API).
//
// WebHost and APIHost default to github.com and api.github.com. GitHub
// Enterprise Cloud with data residency (<sub>.ghe.com, the API at
// api.<sub>.ghe.com) has the same shape (unverified). GitHub Enterprise
// Server, with the API on the web host under /api/v3 and /api/graphql,
// doesn't fit the route tables yet.
type GitHub struct {
	User     string // the GitHub user the agent acts for
	TokenCmd string // prints the user's token
	WebHost  string // github.com
	APIHost  string // api.github.com

	// Tests: the upstreams, if not https://<WebHost> and https://<APIHost>.
	GitURL, APIURL string
}

func (GitHub) Name() string { return "github" }

// Web is the web host, default github.com.
func (g GitHub) Web() string { return or(g.WebHost, "github.com") }

// API is the API host, default api.github.com.
func (g GitHub) API() string { return or(g.APIHost, "api.github.com") }

func (g GitHub) Hosts(proxy bool) []string {
	if proxy {
		return []string{g.Web(), g.API()}
	}
	return []string{GitHubHost}
}

func (g GitHub) Handlers(_ context.Context, b *Builder) (map[string]http.Handler, error) {
	up := b.Upstreams(GitHubHost, "github")
	gc := github.Config{User: g.User, Token: Token(g.TokenCmd),
		GitURL: or(g.GitURL, "https://"+g.Web()), APIURL: or(g.APIURL, "https://"+g.API())}
	if b.Proxy {
		web, api := github.ProxyRoutes(up, gc)
		return map[string]http.Handler{g.Web(): b.Provider("github", web), g.API(): b.Provider("github", api)}, nil
	}
	return map[string]http.Handler{GitHubHost: b.Provider("github", github.ServerRoutes(up, gc))}, nil
}

// Guest: gh and git call the broker as a GitHub Enterprise Server (in
// proxy mode, the real host, which the broker terminates).
func (g GitHub) Guest(proxy bool) guest.Config {
	c := guest.Config{Instructions: []string{guest.Instructions("github", map[string]string{"User": g.User, "Host": g.Web()})}}
	switch {
	case proxy && g.Web() == "github.com":
		c.Env = map[string]string{"GH_TOKEN": "placeholder"}
	case proxy:
		// gh treats any other host as an enterprise one (unverified for ghe.com).
		c.Env = map[string]string{"GH_HOST": g.Web(), "GH_ENTERPRISE_TOKEN": "placeholder"}
	default:
		c.Env = map[string]string{"GH_HOST": GitHubHost, "GH_ENTERPRISE_TOKEN": "placeholder"}
		c.Git = InsteadOf("https://"+GitHubHost, g.Web())
	}
	return c
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
