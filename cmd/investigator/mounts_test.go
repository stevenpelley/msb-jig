package main

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseMount(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Base(dir)
	ok := []struct {
		spec string
		want Mount
	}{
		{dir, Mount{Host: dir, Guest: "/mnt/" + base}},
		{dir + ":ro", Mount{Host: dir, Guest: "/mnt/" + base, ReadOnly: true}},
		{dir + ":/mnt/src", Mount{Host: dir, Guest: "/mnt/src"}},
		{dir + ":/mnt/a/../src:ro", Mount{Host: dir, Guest: "/mnt/src", ReadOnly: true}},
	}
	for _, c := range ok {
		got, err := parseMount(c.spec)
		if err != nil || got != c.want {
			t.Errorf("parseMount(%q) = %+v, %v; want %+v", c.spec, got, err, c.want)
		}
	}
	for _, spec := range []string{
		"",
		dir + "/missing",
		dir + ":/home/agent/src", // not under /mnt
		dir + ":/mnt",
		dir + ":/mnt/host", // the exchange folder
		dir + ":/mnt/../etc",
		dir + ":relative",
		dir + ":/mnt/a:/mnt/b",
	} {
		if got, err := parseMount(spec); err == nil {
			t.Errorf("parseMount(%q) = %+v, want an error", spec, got)
		}
	}
}

func TestSplitMounts(t *testing.T) {
	rest, specs := splitMounts([]string{"box", "--mount", "a", "src", "--mount=b:ro"})
	if !reflect.DeepEqual(rest, []string{"box", "src"}) || !reflect.DeepEqual(specs, []string{"a", "b:ro"}) {
		t.Errorf("got %q %q", rest, specs)
	}
}

func TestLoadMounts(t *testing.T) {
	state := t.TempDir()
	ms, err := loadMounts(state)
	want := []Mount{{Host: filepath.Join(state, "share"), Guest: shareGuest}}
	if err != nil || !reflect.DeepEqual(ms, want) {
		t.Errorf("loadMounts = %+v, %v; want %+v", ms, err, want)
	}
}
