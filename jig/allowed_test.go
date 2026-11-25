package jig

import (
	"slices"
	"testing"

	"github.com/stevenpelley/msb-jig/allowlist"
)

// The AWS group leaves the allowlist in both modes: in proxy mode only the
// AWS MCP Server's host is added, for the broker to answer. The forges'
// groups stay in proxy mode, with their brokered hosts.
func TestAllowedAWS(t *testing.T) {
	t.Setenv("JIG_STATE", t.TempDir())
	t.Setenv("JIG_LAYERS", "")
	defer func(old Track) { track = old }(track)
	track = Personal().withDefaults()
	has := func(es []allowlist.Entry, s string) bool {
		return slices.ContainsFunc(es, func(e allowlist.Entry) bool { return e.String() == s })
	}
	none, err := allowed(config{}.broker)
	if err != nil {
		t.Fatal(err)
	}
	if !has(none, "*.amazonaws.com:443") {
		t.Fatal("the base allowlist has no *.amazonaws.com: the test needs another host")
	}
	for _, proxy := range []bool{false, true} {
		es, err := allowed(allOn(proxy).broker)
		if err != nil {
			t.Fatal(err)
		}
		if has(es, "*.amazonaws.com:443") {
			t.Errorf("proxy=%v: the aws group stayed", proxy)
		}
		if has(es, "aws-mcp.us-east-1.api.aws:443") != proxy {
			t.Errorf("proxy=%v: the AWS MCP Server's host", proxy)
		}
		if has(es, "github.com:443") != proxy {
			t.Errorf("proxy=%v: github.com", proxy)
		}
	}
}
