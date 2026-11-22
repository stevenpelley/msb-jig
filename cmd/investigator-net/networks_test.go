package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The format lima/sandbox.sh writes.
const twoNetworks = `# Written by mcp-restrict's lima/sandbox.sh: only running investigator-net networks.
networks:
  inv1:
    mode: user-v2
    gateway: 192.168.104.1
    netmask: 255.255.255.0
  inv2:
    mode: user-v2
    gateway: 192.168.105.1
    netmask: 255.255.255.0
`

func writeTemp(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "networks.yaml")
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDropNetworkKeepsOthers(t *testing.T) {
	p := writeTemp(t, twoNetworks)
	if err := dropNetwork(p, "inv1"); err != nil {
		t.Fatal(err)
	}
	got := read(t, p)
	want := `# Written by mcp-restrict's lima/sandbox.sh: only running investigator-net networks.
networks:
  inv2:
    mode: user-v2
    gateway: 192.168.105.1
    netmask: 255.255.255.0
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestDropNetworkLastLeavesEmptyMap(t *testing.T) {
	p := writeTemp(t, twoNetworks)
	for _, n := range []string{"inv2", "inv1"} {
		if err := dropNetwork(p, n); err != nil {
			t.Fatal(err)
		}
	}
	if got := read(t, p); !strings.Contains(got, "networks: {}") || strings.Contains(got, "inv1") || strings.Contains(got, "inv2") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestDropNetworkNoOp(t *testing.T) {
	if err := dropNetwork(filepath.Join(t.TempDir(), "missing.yaml"), "inv1"); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	p := writeTemp(t, twoNetworks)
	if err := dropNetwork(p, "inv9"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, p); got != twoNetworks {
		t.Fatalf("an absent entry changed the file:\n%s", got)
	}
	p = writeTemp(t, "networks: {}\n")
	if err := dropNetwork(p, "inv1"); err != nil {
		t.Fatal(err)
	}
}

func TestDropNetworkRefusesGarbage(t *testing.T) {
	p := writeTemp(t, "- a list\n")
	if err := dropNetwork(p, "inv1"); err == nil {
		t.Fatal("want an error for a file that isn't a mapping")
	}
}
