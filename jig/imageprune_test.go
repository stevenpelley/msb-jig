package jig

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"3.354GB": 3354000000, "512.5MB": 512500000, "0B": 0, "12kB": 12000, "1.5GiB": 1610612736} {
		if got, ok := parseSize(in); !ok || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "N/A", "3 parsecs"} {
		if _, ok := parseSize(in); ok {
			t.Errorf("parseSize(%q) ok", in)
		}
	}
}

// The tags recorded, and pruning with nothing stale: no engine or
// microsandbox calls, the kept tags recorded.
func TestPruneImagesRecords(t *testing.T) {
	t.Setenv("JIG_STATE", t.TempDir())
	pruneImages("no-such-engine", []string{"localhost/mono-jig-base:a", "localhost/mono-jig:b"})
	pruneImages("no-such-engine", []string{"localhost/mono-jig:b", "localhost/mono-jig-base:a"})
	if got := loadBuiltImages(); !slices.Equal(got, []string{"localhost/mono-jig-base:a", "localhost/mono-jig:b"}) {
		t.Errorf("recorded %q", got)
	}
	t.Setenv("JIG_KEEP_IMAGES", "1")
	pruneImages("no-such-engine", []string{"localhost/mono-jig:c"})
	if got := loadBuiltImages(); len(got) != 3 {
		t.Errorf("with JIG_KEEP_IMAGES, recorded %q", got)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("JIG_STATE"), "images.json")); err != nil {
		t.Error(err)
	}
}
