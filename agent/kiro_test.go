package agent

import (
	"strings"
	"testing"

	"github.com/stevenpelley/msb-jig/broker/providers"
	"github.com/stevenpelley/msb-jig/guest"
)

func TestKiroCLI(t *testing.T) {
	k := KiroCLI{}
	for p := range k.Components() {
		if n := guest.ComponentName(p); n != "kiro" {
			t.Errorf("component %s is named %q, want kiro", p, n)
		}
	}
	if !strings.Contains(kiroComponent, "sha256sum -c") {
		t.Error("the component doesn't check the download's SHA-256")
	}
	g := k.Guest(providers.Config{}, []string{"## A\n", "## B\n"})
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	files := map[string]guest.File{}
	for _, f := range g.Files {
		files[f.Path] = f
	}
	if f := files["~/.kiro/steering/mono-jig.md"]; f.Content != KiroMarker+"\n## A\n## B\n" || f.Owner != "user" {
		t.Errorf("steering file %+v", f)
	}
	if f := files["~/.kiro/settings/permissions.yaml"]; !strings.Contains(f.Content, "capability: all\n    effect: allow") {
		t.Errorf("permissions %+v", f)
	}
	if f := files["~/.kiro/settings/cli.json"]; f.Merge["app.disableAutoupdates"] != true {
		t.Errorf("cli.json %+v", f)
	}
	if g := k.Guest(providers.Config{}, nil); len(g.Files) != 2 {
		t.Errorf("no instructions: %d files", len(g.Files))
	}
}
