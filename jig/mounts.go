package jig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Host folders in the sandbox. Every sandbox has an exchange folder,
// <state>/share on the Mac, at /mnt/host in the guest (and ~/host, a link
// to it), the one folder the agent writes on the Mac by default: nothing
// on the Mac runs from it. More folders are mounted with
// mono-jig create -v <host>:<guest>:ro|rw: microsandbox can't change a
// sandbox's mounts afterwards, so the list is saved in
// <state>/mounts.json and the supervisor applies it at create. Guest
// paths are under /mnt: the agent's home is made at the first start, and
// a mount there before it would leave it root's. msb cp copies single
// files either way without a mount.
//
// -v is msb create's flag, and every spec we accept means the same to
// msb, so the guest path and the mode are required, with no defaults.
// msb mounts read-write unless told :ro; before 2026-11-28 ours were
// read-only unless told :rw (docs/design.md, "Don't mount host files
// read-write": in a writable repo the agent could plant .git/hooks,
// core.fsmonitor or .vscode/settings.json, which the Mac's own tools
// would later run outside the sandbox, with the user's credentials).
// With the mode required, a read-write mount is always asked for by
// name. What we don't support (a named volume, other options) is
// refused, not read another way.

const (
	guestMounts = "/mnt"
	shareGuest  = guestMounts + "/host"
	// guestUID is the agent user's, fixed so mounted files are the agent's.
	guestUID = 2000
)

// Mount is one host folder in the guest.
type Mount struct {
	Host     string `json:"host"`
	Guest    string `json:"guest"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// parseMount reads <host>:<guest>:ro|rw, as msb create -v does. The host
// path starts with / or . (anything else is a named volume to msb).
func parseMount(spec string) (Mount, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return Mount{}, fmt.Errorf("-v %s: want <host>:<guest>:ro|rw", spec)
	}
	if parts[2] != "ro" && parts[2] != "rw" {
		return Mount{}, fmt.Errorf("-v %s: the mode is ro or rw, and nothing else", spec)
	}
	if !strings.HasPrefix(parts[0], "/") && !strings.HasPrefix(parts[0], ".") {
		return Mount{}, fmt.Errorf("-v %s: the host folder starts with / or . (to msb, anything else names a volume, which mono-jig doesn't mount)", spec)
	}
	host, err := filepath.Abs(parts[0])
	if err != nil {
		return Mount{}, err
	}
	if fi, err := os.Stat(host); err != nil || !fi.IsDir() {
		return Mount{}, fmt.Errorf("-v %s: %s isn't a folder", spec, host)
	}
	mt := Mount{Host: host, Guest: path.Clean(parts[1]), ReadOnly: parts[2] == "ro"}
	if !strings.HasPrefix(mt.Guest, guestMounts+"/") || mt.Guest == shareGuest {
		return Mount{}, fmt.Errorf("-v %s: the guest path must be under %s/, and not %s", spec, guestMounts, shareGuest)
	}
	return mt, nil
}

// inRepo returns the folder holding .git at or above dir, or "".
func inRepo(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

func mountsFile(state string) string { return filepath.Join(state, "mounts.json") }

// parseMounts reads create's -v specs, failing on the first bad one.
func parseMounts(specs []string) []Mount {
	var ms []Mount
	for _, s := range specs {
		mt, err := parseMount(s)
		if err != nil {
			fail("%v", err)
		}
		if slices.ContainsFunc(ms, func(o Mount) bool { return o.Guest == mt.Guest }) {
			fail("-v: two folders at %s", mt.Guest)
		}
		if repo := inRepo(mt.Host); !mt.ReadOnly && repo != "" {
			fmt.Fprintf(os.Stderr, "warning: %s is mounted read-write and is in the git repository %s: the agent can write hooks and settings that git and your editor run on the Mac\n", mt.Host, repo)
		}
		ms = append(ms, mt)
	}
	return ms
}

// writeMounts records the mounts for a sandbox about to be created.
func writeMounts(state string, ms []Mount) {
	if len(ms) == 0 {
		return
	}
	b, _ := json.MarshalIndent(ms, "", "  ")
	if err := os.WriteFile(mountsFile(state), append(b, '\n'), 0o600); err != nil {
		fail("%v", err)
	}
}

// loadMounts is what saveMounts recorded, and the exchange folder.
func loadMounts(state string) ([]Mount, error) {
	share := filepath.Join(state, "share")
	if err := os.MkdirAll(share, 0o700); err != nil {
		return nil, err
	}
	ms := []Mount{{Host: share, Guest: shareGuest}}
	b, err := os.ReadFile(mountsFile(state))
	if errors.Is(err, os.ErrNotExist) {
		return ms, nil
	} else if err != nil {
		return nil, err
	}
	var more []Mount
	if err := json.Unmarshal(b, &more); err != nil {
		return nil, fmt.Errorf("%s: %w", mountsFile(state), err)
	}
	return append(ms, more...), nil
}
