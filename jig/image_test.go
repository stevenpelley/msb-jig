package jig

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stevenpelley/msb-jig/guest"
)

func TestParseImageFile(t *testing.T) {
	baked, hash := parseImageFile("components=base git docker claude\nprovision=abc123\n")
	want := map[string]bool{"base": true, "git": true, "docker": true, "claude": true}
	if !reflect.DeepEqual(baked, want) || hash != "abc123" {
		t.Errorf("got %v %q", baked, hash)
	}
	if baked, hash := parseImageFile(""); len(baked) != 0 || hash != "" {
		t.Errorf("empty: got %v %q", baked, hash)
	}
}

// The base image bakes only parts the supervisor would otherwise run at
// the first start, and its Dockerfile records them for it.
func TestImageComponents(t *testing.T) {
	all := strings.Fields(components(config{dockerGiB: 1}))
	for _, p := range strings.Fields(imageComponents()) {
		if !strings.Contains(" "+strings.Join(all, " ")+" ", " "+p+" ") {
			t.Errorf("%s is baked but not a first-start component", p)
		}
	}
	df := baseDockerfile()
	for _, s := range []string{"ARG PROVISION_COMPONENTS", "ARG PROVISION_HASH", imageFile} {
		if !strings.Contains(df, s) {
			t.Errorf("image.Dockerfile lacks %q", s)
		}
	}
	if baseTag() != baseTag() || !strings.HasPrefix(baseTag(), "localhost/mono-jig-base:") {
		t.Errorf("baseTag %q", baseTag())
	}
}

// The base image's Dockerfile has a layer per baked component, in
// provision.sh's order, each copying only its own file; the arguments
// that change with any provision file come after them all.
func TestBaseDockerfileLayers(t *testing.T) {
	df := baseDockerfile()
	if strings.Contains(df, "COMPONENT LAYERS") || strings.Contains(df, "COPY provision/ ") {
		t.Fatalf("not expanded:\n%s", df)
	}
	last := -1
	for _, name := range strings.Fields(imageComponents()) {
		i := strings.Index(df, "PROVISION_COMPONENTS="+name+" ")
		if i < 0 || i < last {
			t.Errorf("%s: layer missing or out of order:\n%s", name, df)
		}
		last = i
	}
	if n, want := strings.Count(df, "\nRUN SANDBOX_USER="), len(strings.Fields(imageComponents())); n != want {
		t.Errorf("%d component layers, want %d", n, want)
	}
	if !strings.Contains(df, "COPY provision/components/70-claude.sh /opt/mono-jig/provision/components/\n") {
		t.Errorf("claude's layer copies more than its file:\n%s", df)
	}
	if strings.Index(df, "ARG PROVISION_HASH") < last {
		t.Errorf("PROVISION_HASH before a component layer: any provision change would rebuild it")
	}
	files := map[string]string{"components/20-base.sh": "", "components/50-broker.sh": "", "lib.sh": ""}
	if g := guest.ImageDockerfile(files, []string{"base"}); strings.Contains(g, "50-broker.sh") || !strings.Contains(g, "20-base.sh") {
		t.Errorf("only the named components get layers:\n%s", g)
	}
}

// Docker's builds load the result into its image store (a buildx
// docker-container builder otherwise keeps it only in the build cache);
// Podman's don't take the flag.
func TestBuildVerb(t *testing.T) {
	if got := buildVerb("docker"); len(got) != 2 || got[1] != "--load" {
		t.Errorf("docker: %v", got)
	}
	if got := buildVerb("podman"); len(got) != 1 {
		t.Errorf("podman: %v", got)
	}
}
