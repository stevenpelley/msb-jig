package jig

import (
	"strings"
	"testing"
)

// The usage message lists the track's providers, each line indented.
func TestProviderUsage(t *testing.T) {
	defer func(old Track) { track = old }(track)
	track = Personal()
	track.Providers = append(track.Providers, ProviderCLI{Name: "extra"})
	u := providerUsage()
	for _, want := range []string{"  login gitlab [<user>]", "\n                          act for the user", "  login claude            store", "  login extra\n"} {
		if !strings.Contains(u, want) {
			t.Errorf("lacks %q:\n%s", want, u)
		}
	}
}
