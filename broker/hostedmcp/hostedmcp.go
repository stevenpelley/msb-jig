// Package hostedmcp is the broker's provider for a hosted MCP server: one
// endpoint that speaks MCP's streamable HTTP and takes the user's token
// as a Bearer token (an OAuth access token, or a personal token). The
// server acts from its own side, where no route can see what a tool does,
// so the broker decides each call by the tool's name alone: the agent
// calls only the tools on a list (read-only ones, by the track's choice),
// and tools/list is filtered to them (docs/design.md, "Hosted MCP
// servers: filter tool calls").
package hostedmcp

import (
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/stevenpelley/msb-jig/broker"
	"github.com/stevenpelley/msb-jig/broker/mcp"
)

// Config is one server.
type Config struct {
	Name  string       // the provider's name, for messages
	URL   string       // the endpoint: https://mcp.example.org/v1/mcp, no query
	Token broker.Token // the user's token, sent as a Bearer token
	Tools []string     // the tools the agent may call; others are hidden and refused
}

// Check reports what's wrong with the configuration, if anything.
func (c Config) Check() error {
	u, err := url.Parse(c.URL)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
		return fmt.Errorf("the MCP server URL %q: want https://host/path", c.URL)
	case u.RawQuery != "" || u.Fragment != "" || u.User != nil:
		return fmt.Errorf("the MCP server URL %q: no query, fragment or user", c.URL)
	case len(c.Tools) == 0:
		return fmt.Errorf("no tools listed for the MCP server %s", c.URL)
	}
	return nil
}

// Routes is every request the server is served for: MCP's POST at the
// endpoint, each message checked (Checks.Call), and its GET (the
// server's event stream) and DELETE (the session's end). Anything else,
// OAuth discovery included, is denied: the sandbox holds no credential to
// discover. The configuration must pass Check.
func Routes(up broker.Upstreams, c Config) []broker.Route {
	u, err := url.Parse(c.URL)
	if err != nil {
		panic(err)
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	to := up.To(u.Scheme+"://"+u.Host, broker.Bearer(c.Token))
	ch := &Checks{Name: c.Name, tools: map[string]bool{}}
	for _, t := range c.Tools {
		ch.tools[t] = true
	}
	return []broker.Route{
		{Method: "POST", Path: path, Op: "mcp", Check: ch.Call, To: to},
		{Method: "GET DELETE", Path: path, Op: "mcp.session", Check: noQuery, To: to},
	}
}

// Checks hold the server's tool list.
type Checks struct {
	Name  string
	tools map[string]bool
}

// Call checks one MCP message: initialize, ping and notifications pass,
// tools/list is filtered to the list, and tools/call passes only for a
// tool on it. Other methods (resources, prompts, completions) are refused
// by mcp.Read, and so are URL parameters, which some servers read as
// settings (a toolset, say).
func (ch *Checks) Call(c *broker.Call) broker.Decision {
	m, d := mcp.Read(c)
	if !d.Allow {
		return d
	}
	switch m.Method {
	case "tools/list":
		c.Rewrite(mcp.FilterToolsList(func(name string) bool { return ch.tools[name] }))
		return broker.Allow()
	case "tools/call":
	default:
		return broker.Allow()
	}
	var call mcp.ToolCall
	if err := json.Unmarshal(m.Params, &call); err != nil {
		return broker.Deny("bad tools/call params")
	}
	mcp.DenyAsTool(c, m.ID, call.Name)
	c.Note("tool", call.Name)
	if !ch.tools[call.Name] {
		return broker.Deny("the %s MCP tool %s isn't on the broker's list (read-only tools only)", ch.Name, call.Name)
	}
	return broker.Allow()
}

func noQuery(c *broker.Call) broker.Decision {
	if c.R.URL.RawQuery != "" {
		return broker.Deny("URL parameters aren't accepted")
	}
	return broker.Allow()
}
