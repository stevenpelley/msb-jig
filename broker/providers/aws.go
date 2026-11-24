package providers

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/stevenpelley/msb-jig/broker/awsmcp"
	"github.com/stevenpelley/msb-jig/guest"
)

// AWSURL is the broker's AWS MCP endpoint, for mcpcli and curl.
const AWSURL = "https://" + AWSHost + awsmcp.Path

// MCPCLIDir is where mcpcli goes in the guest.
const MCPCLIDir = "/usr/local/lib/mono-jig/mcpcli"

// AWS: the AWS MCP Server, through AWS's stdio proxy on the Mac, under
// AWSHost in both modes.
type AWS struct {
	Profiles []string // the first is the default
	Command  string   // tests: a stdio MCP server instead of AWS's proxy
}

func (AWS) Name() string { return "aws" }

func (AWS) Hosts(bool) []string { return []string{AWSHost} }

func (a AWS) Handlers(ctx context.Context, b *Builder) (map[string]http.Handler, error) {
	proxy, err := awsmcp.Start(ctx, awsmcp.Config{Profiles: a.Profiles, Command: a.Command, State: b.State, Log: b.Log})
	if err != nil {
		return nil, err
	}
	return map[string]http.Handler{AWSHost: b.Provider("aws", awsmcp.Routes(proxy))}, nil
}

// Guest: mcpcli, the agent's client for the broker's AWS MCP endpoint.
// The agent calls the AWS MCP Server from the shell (mcpcli or curl),
// never as native MCP tools, so it chooses what enters its context.
func (a AWS) Guest(bool) guest.Config {
	p := slices.Concat(a.Profiles, []string{""})
	g := mcpcliGuest()
	g.Env = map[string]string{"MCP_GATEWAY_URL": AWSURL}
	g.Instructions = []string{guest.Instructions("aws", map[string]string{
		"URL": AWSURL, "Profile": or(p[0], "?"), "Others": strings.TrimSpace(strings.Join(p[1:], " ")),
	})}
	return g
}
