package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	var last Denial
	off := drainDenials(src, 0, "s", &out, &last, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n := strings.Count(out.String(), "example.org"); n != 1 {
		t.Errorf("%d denials recorded, want 1 (duplicates folded): %s", n, out.String())
	}
	if fi, _ := os.Stat(src); off != 0 || fi.Size() != 0 {
		t.Errorf("not truncated: off %d, size %d", off, fi.Size())
	}
}

func TestGuestDir(t *testing.T) {
	for in, want := range map[string]string{
		"": "/home/agent", "repos/test": "/home/agent/repos/test",
		"/tmp/x/": "/tmp/x", "../etc": "/home/etc",
	} {
		var args []string
		if in != "" {
			args = []string{in}
		}
		if got := guestDir(args); got != want {
			t.Errorf("guestDir(%q) = %q, want %q", in, got, want)
		}
	}
}
