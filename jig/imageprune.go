package jig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/stevenpelley/msb-jig/sandbox"
)

// Every base or Dockerfile change gets a new tag (baseTag, userTag), so
// old images would pile up in Docker or Podman and in microsandbox's
// cache, a few GB each: on 2026-11-27 a build on the Mac failed with
// ENOSPC. mono-jig records each tag it builds (images.json in the state
// directory) and, after a build, removes the recorded ones it no longer
// uses. It removes nothing it didn't build: not another track's (with its
// own state directory), not the user's. A microsandbox image a sandbox
// still uses stays, and stays recorded, until that sandbox is removed.
// The engine's build cache stays too (docker builder prune).

func imagesPath() string { return filepath.Join(stateRoot(), "images.json") }

func loadBuiltImages() []string {
	var tags []string
	b, err := os.ReadFile(imagesPath())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			warn("%v", err)
		}
		return nil
	}
	if err := json.Unmarshal(b, &tags); err != nil {
		warn("%s: %v", imagesPath(), err)
	}
	return tags
}

func saveBuiltImages(tags []string) {
	slices.Sort(tags)
	tags = slices.Compact(tags)
	b, _ := json.MarshalIndent(tags, "", "  ")
	if err := os.MkdirAll(stateRoot(), 0o700); err != nil {
		warn("%v", err)
		return
	}
	if err := os.WriteFile(imagesPath(), append(b, '\n'), 0o600); err != nil {
		warn("%v", err)
	}
}

// pruneImages records keep (the tags just built) and removes every other
// recorded tag from the engine and from microsandbox. JIG_KEEP_IMAGES=1
// only records them.
func pruneImages(engine string, keep []string) {
	old := loadBuiltImages()
	if os.Getenv("JIG_KEEP_IMAGES") == "1" {
		saveBuiltImages(append(old, keep...))
		return
	}
	stale := slices.DeleteFunc(slices.Clone(old), func(t string) bool { return slices.Contains(keep, t) })
	if len(stale) == 0 {
		saveBuiltImages(append(old, keep...))
		return
	}
	ctx := context.Background()
	engBefore, engOK := engineImageBytes(engine)
	msbBefore, msbErr := sandbox.MsbImageBytes(ctx)
	var left, inUse []string
	for _, t := range stale {
		engGone := removeEngineImage(engine, t)
		busy, err := sandbox.RemoveMsbImage(ctx, t)
		if err != nil {
			warn("removing %s from microsandbox: %v", t, err)
		}
		if busy {
			inUse = append(inUse, t)
		}
		if !engGone || busy || err != nil {
			left = append(left, t)
		}
	}
	saveBuiltImages(append(left, keep...))
	var freed []string
	if after, ok := engineImageBytes(engine); engOK && ok && engBefore > after {
		freed = append(freed, fmt.Sprintf("%s from %s", gb(engBefore-after), engine))
	}
	if after, err := sandbox.MsbImageBytes(ctx); msbErr == nil && err == nil && msbBefore > after {
		freed = append(freed, fmt.Sprintf("%s from microsandbox", gb(msbBefore-after)))
	}
	fmt.Printf("removed %d of %d old mono-jig images", len(stale)-len(left), len(stale))
	if len(freed) > 0 {
		fmt.Printf(", freeing %s", strings.Join(freed, " and "))
	}
	fmt.Println()
	if len(inUse) > 0 {
		fmt.Printf("kept for the sandboxes that use them, until they're removed: %s\n", strings.Join(inUse, ", "))
	}
}

// removeEngineImage untags t in the engine; whether it's gone.
func removeEngineImage(engine, t string) bool {
	if exec.Command(engine, "image", "inspect", t).Run() != nil {
		return true // already gone
	}
	out, err := exec.Command(engine, "rmi", t).CombinedOutput()
	if err != nil {
		warn("%s rmi %s: %v: %s", engine, t, err, strings.TrimSpace(string(out)))
		return false
	}
	return true
}

// engineImageBytes is the engine's own count of its images' disk use
// (system df), shared layers counted once.
func engineImageBytes(engine string) (int64, bool) {
	out, err := exec.Command(engine, "system", "df", "--format", "{{.Type}}\t{{.Size}}").Output()
	if err != nil {
		return 0, false
	}
	for _, l := range strings.Split(string(out), "\n") {
		if typ, size, ok := strings.Cut(l, "\t"); ok && strings.EqualFold(strings.TrimSpace(typ), "images") {
			return parseSize(strings.TrimSpace(size))
		}
	}
	return 0, false
}

var sizeRE = regexp.MustCompile(`^([0-9.]+)\s*([kKMGT]?i?B)$`)

// parseSize reads a size as docker and podman print one ("3.354GB",
// "512.5MB", "0B").
func parseSize(s string) (int64, bool) {
	m := sizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	mult := map[string]float64{"B": 1, "kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}[m[2]]
	if mult == 0 {
		return 0, false
	}
	return int64(f * mult), true
}

func gb(n int64) string { return fmt.Sprintf("%.2f GB", float64(n)/1e9) }

// warn reports a problem that doesn't stop the command.
func warn(f string, a ...any) { fmt.Fprintf(os.Stderr, "mono-jig: "+f+"\n", a...) }
