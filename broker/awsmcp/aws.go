// Package awsmcp is the broker's AWS provider, served as
// aws.broker.internal: the AWS MCP Server, as MCP over plain HTTP for curl
// and mcpcli (the agent never gets it as a native MCP server). Behind it
// runs AWS's own stdio proxy (mcp-proxy-for-aws, through the embedded
// aws-mcp-proxy wrapper) on the Mac, which holds the credentials, signs
// with SigV4 and switches profiles per call (aws_profile). Like the
// Anthropic provider it has no per-call rules: the profiles' IAM roles
// limit what the agent can do.
package awsmcp

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"mcp-restrict/broker"
	"mcp-restrict/broker/mcp"
)

// Path is the endpoint.
const Path = "/mcp"

// DefaultURL is the AWS MCP Server's endpoint.
const DefaultURL = "https://aws-mcp.us-east-1.api.aws/mcp"

// ProxyScript is the aws-mcp-proxy wrapper around AWS's mcp-proxy-for-aws:
// it starts while the SSO login has expired, and signs again after the
// next login, without a restart.
//
//go:embed aws-mcp-proxy.py
var ProxyScript string

// Config configures the provider.
type Config struct {
	// Profiles are the proxy's --profile list; the first is the default,
	// the rest are chosen per call with aws_profile.
	Profiles []string
	URL      string // DefaultURL
	// Command, if set, is a shell command that runs a stdio MCP proxy
	// instead of the embedded one (for tests).
	Command string
	State   string // the proxy's script and log (aws-mcp.log) go here
	Log     *slog.Logger
}

// New starts the proxy (until ctx ends) and returns the provider. Every
// request it serves:
//
//	POST    /mcp  one JSON-RPC message: initialize, ping, tools/list,
//	              tools/call, notifications -> the stdio proxy
//	DELETE  /mcp  the end of a session: answered 200 (sessions hold nothing)
func New(ctx context.Context, c Config) (*broker.Provider, error) {
	argv, err := c.argv()
	if err != nil {
		return nil, err
	}
	stderr, err := os.OpenFile(filepath.Join(c.State, "aws-mcp.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	c.Log.Info("aws mcp proxy", "argv", argv)
	b := &Bridge{Command: argv, Stderr: stderr, Log: c.Log}
	b.Start(ctx)
	return &broker.Provider{Name: "aws", Log: c.Log, Routes: []broker.Route{
		{Method: "POST", Path: Path, Op: "aws.mcp", Check: call, To: b},
		{Method: "DELETE", Path: Path, Op: "aws.mcp.session", Check: broker.Any, To: broker.Answer(200)},
	}}, nil
}

// call allows any MCP method on mcp.Methods, and notes the tool and
// profile for the log.
func call(c *broker.Call) broker.Decision {
	m, d := mcp.Read(c)
	if !d.Allow || m.Method != "tools/call" {
		return d
	}
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			Profile string `json:"aws_profile"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil || p.Name == "" {
		return broker.Deny("bad tools/call params")
	}
	c.Note("tool", p.Name, "aws_profile", p.Arguments.Profile)
	return d
}

// argv is the proxy's command: the embedded wrapper, written to the state
// directory, under uv; or Command.
func (c Config) argv() ([]string, error) {
	if err := os.MkdirAll(c.State, 0o700); err != nil {
		return nil, err
	}
	if c.Command != "" {
		return []string{"/bin/sh", "-c", "exec " + c.Command}, nil
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		return nil, errors.New("the AWS MCP proxy needs uv on PATH (brew install uv)")
	}
	script := filepath.Join(c.State, "aws-mcp-proxy.py")
	if err := os.WriteFile(script, []byte(ProxyScript), 0o700); err != nil {
		return nil, err
	}
	url := c.URL
	if url == "" {
		url = DefaultURL
	}
	return append([]string{uv, "run", "--script", "--quiet", script, url, "--skip-auth", "--profile"}, c.Profiles...), nil
}
