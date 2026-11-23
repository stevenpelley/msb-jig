package main

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	m "github.com/superradcompany/microsandbox/sdk/go"

	"mcp-restrict/filter"
	"mcp-restrict/sandbox"
)

func TestEgressRules(t *testing.T) {
	rules := egressRules([]filter.Entry{{Pattern: "proxy.golang.org", Port: 443}, {Pattern: "*.ports.ubuntu.com", Port: 80}})
	if len(rules) != 2 {
		t.Fatalf("%d rules", len(rules))
	}
	for i, want := range []struct{ dest, port string }{{"proxy.golang.org", "443"}, {".ports.ubuntu.com", "80"}} {
		r := rules[i]
		if r.Action != m.PolicyActionAllow || r.Direction != m.PolicyDirectionEgress || r.Destination != want.dest ||
			r.Protocol != m.PolicyProtocolTCP || r.Port != want.port {
			t.Errorf("rule %d: %+v, want %v", i, r, want)
		}
	}
}

// The variables provision.sh writes for the components the supervisor
// provisions (client_env 80-83: git, broker, node CA, claude) are the ones
// clientEnv sets on the sandbox.
func TestClientEnvMatchesProvision(t *testing.T) {
	var want []string
	call := regexp.MustCompile(`(?m)^\s*client_env 8[0-3]-\S+((?:.*\\\n)*.*)$`)
	name := regexp.MustCompile(`([A-Z_]+)=`)
	for _, c := range call.FindAllStringSubmatch(sandbox.ProvisionScript, -1) {
		for _, n := range name.FindAllStringSubmatch(c[1], -1) {
			want = append(want, n[1])
		}
	}
	var c config
	c.broker.AnthropicTokenCmd = "true"
	var got []string
	for k := range clientEnv(c) {
		got = append(got, k)
	}
	sort.Strings(want)
	sort.Strings(got)
	if len(want) == 0 || strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("clientEnv sets %v; provision.sh writes %v", got, want)
	}
}
