package jig

import (
	"os"
	"slices"
	"testing"

	"mono-jig/broker"
)

// The spec carries the mounts (owned by the agent), Docker's disk, the
// vsock route, the broker's CA and, in proxy mode, the proxy.
func TestSandboxSpec(t *testing.T) {
	c := allOn(true)
	c.name, c.state, c.image = "s", t.TempDir(), "img"
	c.dockerGiB, c.proxyAddr = 64, "127.0.0.1:9"
	c.mounts = []Mount{{Host: "/h", Guest: "/mnt/h", ReadOnly: true}}
	if err := os.MkdirAll(c.state+"/broker-ca", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broker.CAFile(c.state), []byte("CA"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := sandboxSpec(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Mounts) != 1 || s.Mounts[0].UID != guestUID || !s.Mounts[0].ReadOnly || s.Disks[dockerDir] != 64 {
		t.Errorf("mounts %+v disks %v", s.Mounts, s.Disks)
	}
	if s.Files[guestCA] != "CA" || s.VsockPort != brokerPort || s.Proxy == nil || s.Proxy.Addr != "127.0.0.1:9" {
		t.Errorf("%+v", s)
	}
	if s.Env["GH_TOKEN"] != "placeholder" || !slices.Contains(s.EnvRemove, "GH_HOST") {
		t.Errorf("env %v, remove %v", s.Env, s.EnvRemove)
	}
	c.proxy, c.broker.Proxy = false, false
	if s, _ = sandboxSpec(c); s.Proxy != nil {
		t.Error("proxy in server mode")
	}
}
