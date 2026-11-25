package providers

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/stevenpelley/msb-jig/broker/awsmcp"
	"github.com/stevenpelley/msb-jig/guest"
)

// AWSURL is the broker's AWS MCP endpoint in server mode, for mcpcli and
// curl. In proxy mode it's the AWS MCP Server's own URL (awsmcp.DefaultURL).
const AWSURL = "https://" + AWSHost + awsmcp.Path

// AWSAPIHost is the AWS MCP Server's host, which proxy mode serves.
var AWSAPIHost = func() string {
	u, err := url.Parse(awsmcp.DefaultURL)
	if err != nil {
		panic(err)
	}
	return u.Host
}()

// MCPCLIDir is where mcpcli goes in the guest.
const MCPCLIDir = "/usr/local/lib/mono-jig/mcpcli"

// AWS: the AWS MCP Server, through AWS's stdio proxy on the Mac: under
// AWSHost in server mode, and in proxy mode under the server's own host,
// which the broker answers itself (SelfServed).
type AWS struct {
	Profiles []string // the first is the default
	Command  string   // tests: a stdio MCP server instead of AWS's proxy
}

func (AWS) Name() string { return "aws" }

func (AWS) Hosts(proxy bool) []string {
	if proxy {
		return []string{AWSAPIHost}
	}
	return []string{AWSHost}
}

// SelfServed: the broker answers AWS's host itself, through AWS's proxy.
func (AWS) SelfServed() {}

func awsURL(proxy bool) string {
	if proxy {
		return awsmcp.DefaultURL
	}
	return AWSURL
}

func (a AWS) Handlers(ctx context.Context, b *Builder) (map[string]http.Handler, error) {
	proxy, err := awsmcp.Start(ctx, awsmcp.Config{Profiles: a.Profiles, Command: a.Command, State: b.State, Log: b.Log})
	if err != nil {
		return nil, err
	}
	return map[string]http.Handler{a.Hosts(b.Proxy)[0]: b.Provider("aws", awsmcp.Routes(proxy))}, nil
}

// Guest: mcpcli, the agent's client for the broker's AWS MCP endpoint.
// The agent calls the AWS MCP Server from the shell (mcpcli or curl),
// never as native MCP tools, so it chooses what enters its context.
func (a AWS) Guest(proxy bool) guest.Config {
	p := slices.Concat(a.Profiles, []string{""})
	g := mcpcliGuest()
	g.Env = map[string]string{"MCP_GATEWAY_URL": awsURL(proxy)}
	g.Instructions = []string{guest.Instructions("aws", map[string]string{
		"URL": awsURL(proxy), "Profile": or(p[0], "?"), "Others": strings.TrimSpace(strings.Join(p[1:], " ")),
	})}
	return g
}
