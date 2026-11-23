package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

func TestParseDenial(t *testing.T) {
	for _, c := range []struct {
		line, method, host string
		port               int
	}{
		{`2026-11-22T23:39:36.768489Z DEBUG microsandbox_network::engine::dns::forwarder: DNS query denied by network policy domain=example.org`, "dns", "example.org", 0},
		{`2026-11-22T23:39:36.765763Z DEBUG microsandbox_network::engine::tcp::proxy: TCP egress denied by strict hostname policy sni="proxy.golang.org" dst=142.251.179.141:443`, "connect", "proxy.golang.org", 443},
	} {
		d, ok := parseDenial(c.line+"\n", "s")
		if !ok || d.Method != c.method || d.Host != c.host || d.Port != c.port || d.Time.IsZero() || d.Client != "s" {
			t.Errorf("%s: %+v %v", c.line, d, ok)
		}
	}
	for _, l := range []string{
		`2026-11-22T23:39:36.653164Z DEBUG microsandbox_network::engine::dns::forwarder: DNS query family is inactive for this sandbox domain=proxy.golang.org family=Ipv6`,
		`2026-11-22T23:39:36.756974Z DEBUG msb_krun_devices::virtio::mmio[block]: interrupt: signal_used_queue`,
	} {
		if _, ok := parseDenial(l, "s"); ok {
			t.Errorf("parsed %s", l)
		}
	}
}

func TestDrainDenialsTruncates(t *testing.T) {
	src := filepath.Join(t.TempDir(), "runtime.log")
	deny := "2026-11-22T23:39:36.768489Z DEBUG microsandbox_network::engine::dns::forwarder: DNS query denied by network policy domain=example.org\n"
	noise := strings.Repeat("2026-11-22T23:39:36.756974Z DEBUG msb_krun_devices::virtio::mmio[block]: interrupt: signal_used_queue\n", maxRuntimeLog/100+1000)
	if err := os.WriteFile(src, []byte(deny+deny+noise+"partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var last filter.Denial
	off := drainDenials(src, 0, "s", &out, &last, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n := strings.Count(out.String(), "example.org"); n != 1 {
		t.Errorf("%d denials recorded, want 1 (duplicates folded): %s", n, out.String())
	}
	if fi, _ := os.Stat(src); off != 0 || fi.Size() != 0 {
		t.Errorf("not truncated: off %d, size %d", off, fi.Size())
	}
}
