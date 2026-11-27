package jig

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevenpelley/msb-jig/sandbox"
)

// existsBackend is a Backend whose sandboxes exist or don't.
type existsBackend struct {
	sandbox.Microsandbox
	exists bool
}

func (b existsBackend) Exists(context.Context, string) (bool, error) { return b.exists, nil }

// fakeTools puts docker and msb stand-ins first on PATH: docker records its
// calls and writes an image ID for --iidfile; msb has every image cached.
func fakeTools(t *testing.T) (calls func() string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	docker := `#!/bin/sh
echo "docker $*" >>` + log + `
while [ $# -gt 0 ]; do
  if [ "$1" = --iidfile ]; then echo sha256:abcdef0123456789abcdef >"$2"; fi
  shift
done
`
	for name, body := range map[string]string{"docker": docker, "msb": "#!/bin/sh\nexit 0\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("MSB_PATH", filepath.Join(dir, "msb"))
	return func() string { b, _ := os.ReadFile(log); return string(b) }
}

func withTrack(t *testing.T, exists bool) {
	t.Helper()
	old, oldPlain := track, plainImage
	t.Cleanup(func() { track, plainImage = old, oldPlain })
	track = Personal().withDefaults()
	track.Backend = existsBackend{exists: exists}
	t.Setenv("JIG_STATE", t.TempDir())
	t.Setenv("JIG_IMAGE", "")
	os.Unsetenv("JIG_IMAGE")
}

// A new sandbox's image is built on the Mac: the base image, then the
// user's Dockerfile, tagged by its ID; JIG_IMAGE carries it to the
// supervisor.
func TestNewSandboxImageBuilds(t *testing.T) {
	withTrack(t, false)
	calls := fakeTools(t)
	df := filepath.Join(t.TempDir(), "Dockerfile")
	if err := os.WriteFile(df, []byte("FROM x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := LoadSettings()
	s.ImageFile, s.ImageContext = df, filepath.Dir(df)
	SaveSettings(s)

	newSandboxImage("s")
	if got := os.Getenv("JIG_IMAGE"); got != "localhost/mono-jig:abcdef012345" {
		t.Errorf("JIG_IMAGE %q", got)
	}
	c := calls()
	if !strings.Contains(c, "-t "+baseTag()) || !strings.Contains(c, "-f "+df) || !strings.Contains(c, "docker tag sha256:abcdef0123456789abcdef localhost/mono-jig:abcdef012345") {
		t.Errorf("calls:\n%s", c)
	}
	if LoadSettings().Image != "localhost/mono-jig:abcdef012345" {
		t.Error("not recorded")
	}
}

// --plain-image starts from the plain base image, with no build; an
// existing sandbox keeps its image and builds nothing.
func TestNewSandboxImagePlainAndExisting(t *testing.T) {
	withTrack(t, false)
	calls := fakeTools(t)
	plainImage = true
	newSandboxImage("s")
	if got := os.Getenv("JIG_IMAGE"); got != baseImage() {
		t.Errorf("plain: JIG_IMAGE %q", got)
	}
	os.Unsetenv("JIG_IMAGE")
	plainImage = false
	track.Backend = existsBackend{exists: true}
	newSandboxImage("s")
	if os.Getenv("JIG_IMAGE") != "" || calls() != "" {
		t.Errorf("existing: JIG_IMAGE %q, calls %q", os.Getenv("JIG_IMAGE"), calls())
	}
}
