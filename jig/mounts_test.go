package jig

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseMount(t *testing.T) {
	dir := t.TempDir()
	ok := []struct {
		spec string
		want Mount
	}{
		{dir + ":/mnt/src:ro", Mount{Host: dir, Guest: "/mnt/src", ReadOnly: true}},
		{dir + ":/mnt/src:rw", Mount{Host: dir, Guest: "/mnt/src"}},
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
		dir,                 // no guest path or mode: both required, as msb reads them
		dir + ":/mnt/src",   // no mode
		dir + ":ro",         // no guest path
		dir + ":/mnt/src:x", // not ro or rw
		dir + ":/mnt/src:ro,noexec",
		"data:/mnt/data:ro", // a named volume, to msb
		"~/src:/mnt/src:ro", // the same, unless the shell expanded it
		dir + "/missing:/mnt/m:ro",
		dir + ":/home/agent/src:ro", // not under /mnt
		dir + ":/mnt:ro",
		dir + ":/mnt/host:ro", // the exchange folder
		dir + ":/mnt/../etc:ro",
		dir + ":relative:ro",
		dir + ":/mnt/a:/mnt/b:ro",
	} {
		if got, err := parseMount(spec); err == nil {
			t.Errorf("parseMount(%q) = %+v, want an error", spec, got)
		}
	}
}

func TestCreateArgs(t *testing.T) {
	name, specs, err := createArgs([]string{"-v", "a", "box", "--volume", "b", "--volume=c"})
	if err != nil || name != "box" || !reflect.DeepEqual(specs, []string{"a", "b", "c"}) {
		t.Errorf("got %q %q %v", name, specs, err)
	}
	for _, args := range [][]string{nil, {"-v"}, {"box", "-v"}, {"box", "other"}, {"box", "--mount", "a"}} {
		if _, _, err := createArgs(args); err == nil {
			t.Errorf("createArgs(%q) accepted", args)
		}
	}
}

func TestRmArgs(t *testing.T) {
	for _, c := range []struct {
		args  []string
		force bool
	}{{[]string{"box"}, false}, {[]string{"-f", "box"}, true}, {[]string{"box", "--force"}, true}} {
		if name, force, err := rmArgs(c.args); err != nil || name != "box" || force != c.force {
			t.Errorf("rmArgs(%q) = %q %v %v", c.args, name, force, err)
		}
	}
	for _, args := range [][]string{nil, {"-f"}, {"a", "b"}, {"-x", "box"}} {
		if _, _, err := rmArgs(args); err == nil {
			t.Errorf("rmArgs(%q) accepted", args)
		}
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

func TestLoadBrokerMode(t *testing.T) {
	state := t.TempDir()
	if got := loadBrokerMode(state); got != modeServer {
		t.Errorf("default mode %q", got)
	}
	writeBrokerMode(state, modeProxy)
	if got := loadBrokerMode(state); got != modeProxy {
		t.Errorf("written proxy, read %q", got)
	}
}
