package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	m "github.com/superradcompany/microsandbox/sdk/go"
)

// Host folders in the sandbox. Every sandbox has an exchange folder,
// <state>/share on the Mac, at /mnt/host in the guest (and ~/host, a link
// to it), the one folder the agent writes on the Mac by default: nothing
// on the Mac runs from it. More folders are mounted with
// --mount host[:guest][:rw] when the sandbox is created (mono-jig up or open): microsandbox can't change
// a sandbox's mounts afterwards, so the list is saved in
// <state>/mounts.json, the supervisor applies it at create, and a sandbox
// that exists refuses a new one (mono-jig rm first). Guest paths are
// under /mnt: the agent's home is made at the first start, and a mount
// there before it would leave it root's. mono-jig cp copies single
// files either way without a mount.
//
// Mounts are read-only unless :rw says otherwise (docs/design.md, "Don't
// mount host files read-write"): in a writable repo the agent could plant
// .git/hooks, core.fsmonitor or .vscode/settings.json, which the Mac's own
// tools would later run outside the sandbox, with the user's credentials.

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

// parseMount reads host[:guest][:ro|:rw]: read-only unless :rw. The
// guest path defaults to /mnt/<the host folder's name>.
func parseMount(spec string) (Mount, error) {
	parts := strings.Split(spec, ":")
	mt := Mount{ReadOnly: true}
	if n := len(parts); n > 1 && (parts[n-1] == "ro" || parts[n-1] == "rw") {
		mt.ReadOnly = parts[n-1] == "ro"
		parts = parts[:n-1]
	}
	if len(parts) > 2 || parts[0] == "" {
		return mt, fmt.Errorf("--mount %s: want host[:guest][:ro|:rw]", spec)
	}
	host, err := filepath.Abs(expandHome(parts[0]))
	if err != nil {
		return mt, err
	}
	if fi, err := os.Stat(host); err != nil || !fi.IsDir() {
		return mt, fmt.Errorf("--mount %s: %s isn't a folder", spec, host)
	}
	mt.Host = host
	mt.Guest = path.Join(guestMounts, filepath.Base(host))
	if len(parts) == 2 {
		mt.Guest = path.Clean(parts[1])
	}
	if !strings.HasPrefix(mt.Guest, guestMounts+"/") || mt.Guest == shareGuest {
		return mt, fmt.Errorf("--mount %s: the guest path must be under %s/, and not %s", spec, guestMounts, shareGuest)
	}
	return mt, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, strings.TrimPrefix(p, "~"))
	}
	return p
}

// splitMounts takes the --mount flags out of args.
func splitMounts(args []string) (rest, specs []string) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--mount" && i+1 < len(args):
			i++
			specs = append(specs, args[i])
		case strings.HasPrefix(a, "--mount="):
			specs = append(specs, strings.TrimPrefix(a, "--mount="))
		default:
			rest = append(rest, a)
		}
	}
	return rest, specs
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

// saveMounts records the mounts for a sandbox about to be created.
func saveMounts(name, state string, specs []string) {
	var ms []Mount
	for _, s := range specs {
		mt, err := parseMount(s)
		if err != nil {
			fail("%v", err)
		}
		if slices.ContainsFunc(ms, func(o Mount) bool { return o.Guest == mt.Guest }) {
			fail("--mount: two folders at %s", mt.Guest)
		}
		if repo := inRepo(mt.Host); !mt.ReadOnly && repo != "" {
			fmt.Fprintf(os.Stderr, "warning: %s is mounted read-write and is in the git repository %s: the agent can write hooks and settings that git and your editor run on the Mac\n", mt.Host, repo)
		}
		ms = append(ms, mt)
	}
	if _, err := m.GetSandbox(context.Background(), name); err == nil {
		fail("%s exists, and its mounts were fixed when it was made: mono-jig rm %s, then again with --mount", name, name)
	} else if !m.IsKind(err, m.ErrSandboxNotFound) {
		fail("%v", err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		fail("%v", err)
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

// hostMounts are the bind mounts for the SDK, owned by the agent.
func hostMounts(ms []Mount) map[string]m.MountConfig {
	out := map[string]m.MountConfig{}
	for _, mt := range ms {
		out[mt.Guest] = m.Mount.Bind(mt.Host, m.MountOptions{
			Readonly: mt.ReadOnly,
			Owner:    &m.MountOwner{UID: guestUID, GID: guestUID},
		})
	}
	return out
}

// cp copies between the Mac and a sandbox with msb copy. A sandbox path
// is <name>:/absolute/path, or <name>:relative to the agent's home.
func cp(args []string) {
	if len(args) != 2 {
		usage()
	}
	var sandboxes int
	for i, a := range args {
		name, p, ok := strings.Cut(a, ":")
		if !ok || !nameRE.MatchString(name) {
			continue
		}
		sandboxes++
		if !path.IsAbs(p) {
			p = path.Join("/home", guestUser, p)
		}
		args[i] = name + ":" + p
	}
	if sandboxes != 1 {
		fail("cp: one of the two paths is in the sandbox, as <name>:<path>")
	}
	setRuntimePaths()
	c := exec.Command("msb", "copy", args[0], args[1])
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		fail("msb copy: %v", err)
	}
}
