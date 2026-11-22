package main

import (
	"testing"

	m "github.com/superradcompany/microsandbox/sdk/go"

	"mcp-restrict/filter"
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
