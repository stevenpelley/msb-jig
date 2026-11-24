package providers

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/stevenpelley/msb-jig/broker/hostedmcp"
	"github.com/stevenpelley/msb-jig/guest"
)

// MCPServer: a hosted MCP server that takes a Bearer token, with the
// agent limited to a list of tools (broker/hostedmcp). A track makes one
// per server. In server mode it's served under <ID>.mcp.broker.internal,
// in proxy mode under its URL's host. The agent reaches it with mcpcli or
// curl at the URL in $<NAME>_MCP_URL.
type MCPServer struct {
	ID       string   // its Name: allowlist group and login name; a-z, 0-9 and -
	Title    string   // its name in the agent's instructions; default ID
	URL      string   // the endpoint: https://mcp.example.org/v1/mcp
	Tools    []string // the tools the agent may call
	TokenCmd string   // prints the user's token
	Upstream string   // tests: the server's origin, if not URL's
}

var mcpNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

func (m MCPServer) Name() string { return m.ID }

func (m MCPServer) internal() string { return m.ID + ".mcp" + InternalSuffix }

// host is the URL's host name, lower case, or "" if the URL is bad.
func (m MCPServer) host() string {
	u, err := url.Parse(m.URL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func (m MCPServer) Hosts(proxy bool) []string {
	if proxy {
		return []string{m.host()}
	}
	return []string{m.internal()}
}

func (m MCPServer) config() (hostedmcp.Config, error) {
	c := hostedmcp.Config{Name: m.ID, URL: m.URL, Tools: m.Tools, Token: Token(m.TokenCmd)}
	if !mcpNameRE.MatchString(m.ID) {
		return c, fmt.Errorf("the MCP server's name %q: want a-z, 0-9 and -", m.ID)
	}
	if err := c.Check(); err != nil {
		return c, err
	}
	if m.Upstream != "" {
		u, _ := url.Parse(m.URL)
		c.URL = strings.TrimSuffix(m.Upstream, "/") + u.EscapedPath()
	}
	return c, nil
}

func (m MCPServer) Handlers(_ context.Context, b *Builder) (map[string]http.Handler, error) {
	c, err := m.config()
	if err != nil {
		return nil, err
	}
	p := b.Provider(m.ID, hostedmcp.Routes(b.Upstreams(m.internal(), m.ID), c))
	return map[string]http.Handler{m.Hosts(b.Proxy)[0]: p}, nil
}

// EnvName is the guest's variable holding the server's URL as the
// sandbox reaches it: DATADOG_MCP_URL for "datadog".
func (m MCPServer) EnvName() string {
	return strings.ToUpper(strings.ReplaceAll(m.ID, "-", "_")) + "_MCP_URL"
}

// Guest: the URL in $<NAME>_MCP_URL, mcpcli, and the instructions.
func (m MCPServer) Guest(proxy bool) guest.Config {
	u, _ := url.Parse(m.URL)
	sandboxURL := "https://" + m.Hosts(proxy)[0] + u.EscapedPath()
	g := mcpcliGuest()
	g.Env = map[string]string{m.EnvName(): sandboxURL}
	g.Instructions = []string{guest.Instructions("mcpserver", map[string]string{
		"Title": or(m.Title, m.ID), "Env": m.EnvName(), "URL": sandboxURL,
		"Tools": "`" + strings.Join(slices.Sorted(slices.Values(m.Tools)), "`, `") + "`",
	})}
	return g
}

// mcpcliGuest is mcpcli, the agent's MCP client, in the guest: the same
// files for every provider that needs it (guest.Config.Add keeps one).
func mcpcliGuest() guest.Config {
	g := guest.Config{
		Packages: []string{"python3", "python3-venv"},
		Files:    []guest.File{{Path: "/usr/local/bin/mcpcli", Link: MCPCLIDir + "/mcpcli"}},
	}
	for _, name := range slices.Sorted(maps.Keys(guest.MCPCLI)) {
		g.Files = append(g.Files, guest.File{Path: MCPCLIDir + "/" + name, Mode: "0755", Content: guest.MCPCLI[name]})
	}
	return g
}
