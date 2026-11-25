package jig

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"time"

	"github.com/stevenpelley/msb-jig/guest"
)

// Sandbox images. mono-jig image build builds, with Docker or Podman
// on the Mac:
//
//   - the base image, localhost/mono-jig-base:<hash>, from the
//     Dockerfile and provision/ embedded in this binary
//     (guest/image.Dockerfile): baseImage (Ubuntu) with provision.sh's parts that depend
//     only on the image (imageComponents: packages, the agent user, git,
//     gh, glab, Docker, Claude Code). The engine caches it, so it's rebuilt
//     only when provision.sh changes;
//   - then, with -f, your Dockerfile on top of it (FROM
//     $JIG_BASE, passed as a build argument).
//
// It loads the result into microsandbox's image cache (msb load) and
// records it in settings.json; new sandboxes start from it. Their first
// start runs only what the image doesn't have (firstComponents): the
// broker's CA and settings, and Claude's and AWS's. A plain image
// (JIG_IMAGE=ubuntu:26.04, or baseImage) still works, with everything installed at the
// first start. A sandbox keeps the image it was created from: mono-jig
// rm, then open, to move one to a new image.

// baseImage is the image the base image builds on, and the plain image
// sandboxes start from without one: JIG_BASE_IMAGE, default the track's. Its
// packages must suit provision/'s components (base uses apt).
func baseImage() string { return env("JIG_BASE_IMAGE", track.BaseImage) }

// sandboxImage is the image new sandboxes start from: JIG_IMAGE, else the
// last image mono-jig image build made, else plain Ubuntu. local is
// true for a built image, which msb can't pull.
func sandboxImage() (ref string, local bool) {
	if v := os.Getenv("JIG_IMAGE"); v != "" {
		return v, false
	}
	if s := LoadSettings(); s.Image != "" {
		return s.Image, true
	}
	return baseImage(), false
}

const starterDockerfile = `# Your sandbox image, on top of mono-jig's base: Ubuntu with the agent
# user, git, gh, glab, Docker and Claude Code. Build it with
#   mono-jig image build -f <this file>
# which builds the base first and passes it in. Hosts your tools need at
# run time go in ~/.mono-jig/allowlist.yaml.
ARG JIG_BASE
FROM ${JIG_BASE}
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ripgrep \
 && rm -rf /var/lib/apt/lists/*
`

func imageCmd(args []string) {
	if len(args) == 0 {
		usage()
	}
	switch args[0] {
	case "init":
		imageInit(args[1:])
	case "build":
		imageBuild(args[1:])
	case "show":
		ref, _ := sandboxImage()
		fmt.Println(ref)
	case "reset":
		s := LoadSettings()
		s.Image = ""
		SaveSettings(s)
		fmt.Println("new sandboxes start from", baseImage(), "(everything installed at the first start)")
	default:
		usage()
	}
}

// imageInit writes a starter Dockerfile.
func imageInit(args []string) {
	path := "Dockerfile"
	if len(args) == 1 {
		path = args[0]
	} else if len(args) > 1 {
		usage()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fail("%v", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, os.ErrExist) {
		fail("%s exists; not overwriting it", path)
	} else if err != nil {
		fail("%v", err)
	}
	if _, err := f.WriteString(starterDockerfile); err != nil {
		fail("%v", err)
	}
	if err := f.Close(); err != nil {
		fail("%v", err)
	}
	fmt.Printf("wrote %s; add your tools, then: mono-jig image build -f %s\n", path, path)
}

func imageBuild(args []string) {
	fs := flag.NewFlagSet("image build", flag.ExitOnError)
	file := fs.String("f", "", "your Dockerfile, FROM $JIG_BASE (default: the base image alone)")
	engine := fs.String("engine", "", "docker or podman (default: whichever is installed, docker first)")
	_ = fs.Parse(args)
	ctxDir := filepath.Dir(*file)
	switch {
	case fs.NArg() == 1 && *file != "":
		ctxDir = fs.Arg(0)
	case fs.NArg() != 0:
		usage()
	}
	e := imageEngine(*engine)
	if e == "" {
		fail("neither docker nor podman is installed")
	}
	tag := buildImage(e, *file, ctxDir)
	fmt.Printf("new sandboxes start from %s\n(an existing sandbox keeps its image: mono-jig rm <name>, then open, to move it)\n", tag)
}

// imageEngine is the engine to build with: the one named, else docker or
// podman, whichever is installed; "" if none.
func imageEngine(named string) string {
	if named != "" {
		if named != "docker" && named != "podman" {
			fail("-engine: docker or podman")
		}
		return named
	}
	for _, e := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(e); err == nil {
			return e
		}
	}
	return ""
}

// buildImage builds the base image, then file on it if there is one,
// loads the result into microsandbox and records it.
func buildImage(engine, file, ctxDir string) string {
	setRuntimePaths()
	tag := buildBase(engine)
	if file != "" {
		// A new tag each build: sandboxes made from an earlier build keep it.
		tag = "localhost/mono-jig:" + time.Now().Format("20060102-150405")
		runCmd(engine, append(buildVerb(engine), "--platform", "linux/"+runtime.GOARCH,
			"--build-arg", "JIG_BASE="+baseTag(),
			"-t", tag, "-f", file, ctxDir)...)
	}
	if exec.Command("msb", "image", "inspect", tag).Run() == nil {
		fmt.Println(tag, "is already in microsandbox's cache")
	} else {
		fmt.Printf("%s save %s | msb load -t %s\n", engine, tag, tag)
		if err := pipe(exec.Command(engine, "save", tag), exec.Command("msb", "load", "-t", tag)); err != nil {
			fail("loading the image into microsandbox: %v", err)
		}
	}
	s := LoadSettings()
	s.Image = tag
	SaveSettings(s)
	return tag
}

// baseArgs are the base image's build arguments.
func baseArgs() []string {
	return []string{
		"SANDBOX_USER=" + guestUser,
		"SANDBOX_UID=" + strconv.Itoa(guestUID),
		"PROVISION_COMPONENTS=" + imageComponents(),
		"PROVISION_HASH=" + guest.ProvisionHash(provisionFiles()),
		"BASE_IMAGE=" + baseImage(),
	}
}

// baseTag names the base image by everything it's built from, the
// extra CAs included.
func baseTag() string {
	h := sha256.New()
	for _, s := range slices.Concat([]string{guest.ImageDockerfile}, baseArgs(), extraCAs()) {
		fmt.Fprintf(h, "%d:%s", len(s), s)
	}
	return "localhost/mono-jig-base:" + hex.EncodeToString(h.Sum(nil)[:6])
}

// buildBase builds the base image from the embedded Dockerfile and
// provision/, and the extra CAs (ca/), in a temporary context directory.
func buildBase(engine string) string {
	dir, err := os.MkdirTemp("", "mono-jig-base-")
	if err != nil {
		fail("%v", err)
	}
	defer os.RemoveAll(dir)
	files := map[string]string{"Dockerfile": guest.ImageDockerfile, "ca/README": "Extra CAs (mono-jig ca), one .crt each.\n"}
	for name, body := range provisionFiles() {
		files["provision/"+name] = body
	}
	for i, ca := range extraCAs() {
		files[fmt.Sprintf("ca/%d.crt", i+1)] = ca
	}
	for name, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			fail("%v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			fail("%v", err)
		}
	}
	tag := baseTag()
	a := append(buildVerb(engine), "--platform", "linux/"+runtime.GOARCH, "-t", tag)
	for _, b := range baseArgs() {
		a = append(a, "--build-arg", b)
	}
	runCmd(engine, append(a, dir)...)
	return tag
}

// buildVerb starts an image build. Docker's gets --load: with a buildx
// builder on the docker-container driver (Docker Desktop can make it the
// default) the result otherwise stays in the build cache, and docker save
// can't find it (seen 2026-11-25). With the docker driver --load is the
// default anyway. A Dockerfile FROM the base image (image build -f) still
// can't see it on the docker-container driver, which reads only registries.
func buildVerb(engine string) []string {
	if engine == "docker" {
		return []string{"build", "--load"}
	}
	return []string{"build"}
}

// runCmd runs a command with our output, and fails if it does.
func runCmd(name string, args ...string) {
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	fmt.Println(c.String())
	if err := c.Run(); err != nil {
		fail("%s %s: %v", name, args[0], err)
	}
}

// pipe runs a | b, with a's stdout into b's stdin and both stderrs ours.
func pipe(a, b *exec.Cmd) error {
	r, w := io.Pipe()
	a.Stdout, a.Stderr = w, os.Stderr
	b.Stdin, b.Stdout, b.Stderr = r, os.Stdout, os.Stderr
	if err := b.Start(); err != nil {
		return err
	}
	errA := a.Run()
	w.CloseWithError(errA)
	errB := b.Wait()
	if errA != nil {
		return fmt.Errorf("%s: %w", a.Args[0], errA)
	}
	return errB
}
