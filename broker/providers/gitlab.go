package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/stevenpelley/msb-jig/broker/gitlab"
	"github.com/stevenpelley/msb-jig/guest"
)

// GitLab: git, LFS, REST, GraphQL and GitLab's MCP server, for one user.
// In server mode under GitLabHost, as glab calls a self-hosted GitLab; in
// proxy mode under its URL's host.
type GitLab struct {
	User     string // the GitLab user the agent acts for
	URL      string // https://gitlab.com
	TokenCmd string // prints the user's token
	// MCPTokenCmd: a fine-grained PAT for GitLab's MCP server; default the
	// GitLab token.
	MCPTokenCmd string
}

func (GitLab) Name() string { return "gitlab" }

// host is the URL's host name, lower case, or "" if the URL is bad.
func (g GitLab) host() string {
	u, err := url.Parse(g.URL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func (g GitLab) Hosts(proxy bool) []string {
	if proxy {
		return []string{g.host()}
	}
	return []string{GitLabHost}
}

func (g GitLab) Handlers(_ context.Context, b *Builder) (map[string]http.Handler, error) {
	if u, err := url.Parse(g.URL); err != nil || u.Host == "" {
		return nil, fmt.Errorf("the GitLab URL %q: want https://host", g.URL)
	}
	p := b.Provider("gitlab", gitlab.Routes(b.Upstreams(GitLabHost, "gitlab"), gitlab.Config{
		User: g.User, URL: g.URL, Anonymous: b.Proxy, Token: Token(g.TokenCmd), MCPToken: OptToken(g.MCPTokenCmd)}))
	return map[string]http.Handler{g.Hosts(b.Proxy)[0]: p}, nil
}

// GlabMarker is the first line of glab's config, as provision.sh wrote it
// before guest.Config, so those files stay ours.
const GlabMarker = "# Written by mono-jig provision.sh; rewritten on every start."

// Guest: glab and git call the broker as a self-hosted GitLab (in proxy
// mode, GitLab's own host, which the broker terminates). The token is a
// placeholder; the broker injects the user's.
func (g GitLab) Guest(proxy bool) guest.Config {
	host := g.Hosts(proxy)[0]
	real := g.host()
	if u, err := url.Parse(g.URL); err == nil && u.Host != "" {
		real = u.Host // with its port, for git's URLs
	}
	c := guest.Config{
		Env: map[string]string{"GITLAB_HOST": host, "GITLAB_TOKEN": "placeholder", "GITLAB_API_PROTOCOL": "https"},
		// glab's own config too: auth status and clone read it, not
		// GITLAB_HOST. Only this host: glab checks every host listed (its
		// default gitlab.com fails with the placeholder), and https, since
		// glab repo clone would otherwise use SSH, which the sandbox lacks.
		Files: []guest.File{{
			Path: "~/.config/glab-cli/config.yml", Owner: "user", Mode: "0600", Marker: GlabMarker,
			Content: GlabMarker + "\nhost: " + host + "\ngit_protocol: https\ncheck_update: false\nhosts:\n    " +
				host + ":\n        api_protocol: https\n        git_protocol: https\n",
		}},
		Instructions: []string{guest.Instructions("gitlab", map[string]string{"User": g.User, "Host": real})},
	}
	if !proxy {
		// A URL rewritten to a name the broker has no certificate for
		// fails its TLS handshake, so only for a provider it serves.
		c.Git = InsteadOf("https://"+host, real)
	}
	return c
}
