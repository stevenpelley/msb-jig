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
	for _, s := range []string{"ARG PROVISION_COMPONENTS", "ARG PROVISION_HASH", imageFile} {
		if !strings.Contains(guest.ImageDockerfile, s) {
			t.Errorf("image.Dockerfile lacks %q", s)
		}
	}
	if baseTag() != baseTag() || !strings.HasPrefix(baseTag(), "localhost/mono-jig-base:") {
		t.Errorf("baseTag %q", baseTag())
	}
}
