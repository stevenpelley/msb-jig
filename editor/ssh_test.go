package editor

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestGenerateKeysPinsHostKey(t *testing.T) {
	p := PathsFor(t.TempDir())
	if err := GenerateKeys(p, "demo"); err != nil {
		t.Fatal(err)
	}
	kh, err := os.ReadFile(p.KnownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(kh), "investigator-demo ssh-ed25519 ") {
		t.Fatalf("known_hosts = %q", kh)
	}
	// The pinned key is the public half of the key the sandbox receives.
	priv, _ := os.ReadFile(p.HostKeyPriv)
	signer, err := ssh.ParsePrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kh), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))) {
		t.Fatal("known_hosts doesn't pin the generated host key")
	}
	for _, f := range []string{p.ClientKey, p.HostKeyPriv, p.AuthKeys, p.KnownHosts} {
		st, _ := os.Stat(f)
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", f, st.Mode().Perm())
		}
	}

	// A second call keeps the keys, so the pin stays stable.
	if err := GenerateKeys(p, "demo"); err != nil {
		t.Fatal(err)
	}
	kh2, _ := os.ReadFile(p.KnownHosts)
	if string(kh2) != string(kh) {
		t.Fatal("host key changed on second call")
	}
}

func TestWriteConfig(t *testing.T) {
	p := PathsFor(t.TempDir())
	if err := GenerateKeys(p, "demo"); err != nil {
		t.Fatal(err)
	}
	err := WriteConfig(HostEntry{Alias: HostAlias("demo"), User: "agent", ProxyCommand: "docker exec -i demo nc localhost 22", Paths: p})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p.Config)
	cfg := string(b)
	for _, want := range []string{"Host investigator-demo", "ForwardAgent no", "IdentitiesOnly yes", "IdentityAgent none", "StrictHostKeyChecking yes", "ProxyCommand docker exec"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "RemoteForward") {
		t.Error("config has a RemoteForward")
	}
	if err := WriteConfig(HostEntry{Alias: "x", User: "u", ProxyCommand: "a\nb", Paths: p}); err == nil {
		t.Error("multi-line ProxyCommand accepted")
	}
}
