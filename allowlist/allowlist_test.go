package allowlist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEntry(t *testing.T) {
	good := map[string]string{"Example.COM": "example.com:443", "a.b:80": "a.b:80", "*.example.com": "*.example.com:443", "x-y.io:443": "x-y.io:443"}
	for in, want := range good {
		e, err := ParseEntry(in)
		if err != nil || e.String() != want {
			t.Errorf("%q: %v %v, want %s", in, e, err, want)
		}
	}
	for _, in := range []string{"", "1.2.3.4", "::1", "[::1]:443", "a.b:22", "*.com", "a.*.b", "*", "a_b.com", "-a.com", "0x7f.1", "a.123", "a..b", "a.b:"} {
		if e, err := ParseEntry(in); err == nil {
			t.Errorf("%q accepted as %v", in, e)
		}
	}
}

func TestMatches(t *testing.T) {
	cases := []struct {
		entry, host string
		port        int
		ok          bool
	}{
		{"example.com", "example.com", 443, true},
		{"example.com", "www.example.com", 443, false},
		{"example.com", "example.com", 80, false},
		{"*.example.net", "a.example.net", 443, true},
		{"*.example.net", "a.b.example.net", 443, true},
		{"*.example.net", "example.net", 443, false},
		{"*.example.net", "badexample.net", 443, false},
		{"plain.example.org:80", "plain.example.org", 80, true},
	}
	for _, c := range cases {
		e, _ := ParseEntry(c.entry)
		if e.Matches(c.host, c.port) != c.ok {
			t.Errorf("%s allows %s:%d = %v", c.entry, c.host, c.port, !c.ok)
		}
	}
}

func allows(es []Entry, host string, port int) (Entry, bool) {
	for _, e := range es {
		if e.Matches(host, port) {
			return e, true
		}
	}
	return Entry{}, false
}

func TestBase(t *testing.T) {
	ls, err := Layers()
	if err != nil {
		t.Fatal(err)
	}
	cat, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	// Every group is in the base layer, so none is dead.
	inBase := map[string]bool{}
	for _, g := range ls[0].Groups {
		inBase[g] = true
	}
	for g := range cat {
		if !inBase[g] {
			t.Errorf("group %s isn't in base.yaml", g)
		}
	}

	all, err := Compile(ls)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"claude.ai", "proxy.golang.org", "registry.npmjs.org", "pypi.org",
		"api.anthropic.com", "github.com", "api.github.com", "gitlab.com", "s3.us-east-1.amazonaws.com"} {
		if _, ok := allows(all, h, 443); !ok {
			t.Errorf("without providers, %s isn't allowed", h)
		}
	}
	if _, ok := allows(all, "archive.ubuntu.com", 80); !ok {
		t.Error("apt's plain http isn't allowed")
	}
	if _, ok := allows(all, "pastebin.com", 443); ok {
		t.Error("pastebin.com allowed")
	}

	// With every provider brokered, their hosts leave; downloads stay.
	brokered, err := Compile(ls, "anthropic", "github", "gitlab", "aws")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"api.anthropic.com", "github.com", "api.github.com", "uploads.github.com", "gitlab.com", "s3.us-east-1.amazonaws.com"} {
		if e, ok := allows(brokered, h, 443); ok {
			t.Errorf("brokered, %s is still allowed by %s/%s", h, e.Layer, e.Group)
		}
	}
	for _, h := range []string{"codeload.github.com", "objects.githubusercontent.com", "registry.gitlab.com", "claude.ai"} {
		if _, ok := allows(brokered, h, 443); !ok {
			t.Errorf("brokered, %s isn't allowed", h)
		}
	}
}

func TestLayerFiles(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "user.yaml")
	os.WriteFile(user, []byte("groups: [mine]\nhosts: [extra.example.com]\ndefine:\n  mine:\n    hosts: ['*.mine.example']\n"), 0o600)
	ls, err := Layers(user)
	if err != nil {
		t.Fatal(err)
	}
	es, err := Compile(ls)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := allows(es, "x.mine.example", 443); !ok || e.Group != "mine" || e.Layer != user {
		t.Errorf("user group: %v %v", e, ok)
	}
	if _, ok := allows(es, "extra.example.com", 443); !ok {
		t.Error("user host not allowed")
	}
	sbx := FormatSbx(es)
	if !strings.Contains(sbx, "archive.ubuntu.com:80") || !strings.Contains(sbx, "*.mine.example:443") {
		t.Errorf("sbx = %s", sbx)
	}

	bad := filepath.Join(dir, "bad.yaml")
	for _, body := range []string{"groups: [nosuch]\n", "hosts: [1.2.3.4]\n", "hostz: [a.com]\n", "define:\n  go:\n    hosts: [a.com]\n"} {
		os.WriteFile(bad, []byte(body), 0o600)
		ls, err := Layers(bad)
		if err == nil {
			_, err = Compile(ls)
		}
		if err == nil {
			t.Errorf("layer %q accepted", body)
		}
	}
}
