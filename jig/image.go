package jig

import (
	"context"
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
	"strings"

	"github.com/stevenpelley/msb-jig/guest"
)

// Sandbox images. Every new sandbox starts from an image built on the
// Mac (newSandboxImage): up, open and run build it first, with Docker or
// Podman, which answer from their cache when nothing changed, so the guest
// never installs mono-jig's parts itself. mono-jig image build builds the
// same ahead of time:
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
// It loads the result into microsandbox's image cache (msb load) unless
// it's there already, and records it in settings.json, with your
// Dockerfile, which later builds use too. A first start runs only what
// the image doesn't have (firstComponents): the broker's CA and settings.
// --plain-image (or JIG_IMAGE) creates a sandbox from another image
// instead, such as plain Ubuntu (baseImage), with everything installed at
// its first start, for a Mac without Docker or Podman. A sandbox keeps the
// image it was created from: mono-jig rm, then open, to move one to a new
// image.

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
		fmt.Println("last built:", ref)
		if s := LoadSettings(); s.ImageFile != "" {
			fmt.Printf("new sandboxes build the base image, then %s (context %s)\n", s.ImageFile, s.ImageContext)
		} else {
			fmt.Println("new sandboxes build the base image alone")
		}
	case "reset":
		s := LoadSettings()
		s.ImageFile, s.ImageContext = "", ""
		SaveSettings(s)
		fmt.Println("new sandboxes build the base image alone (your Dockerfile is no longer used)")
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
	s := LoadSettings()
	if *file != "" {
		f, err1 := filepath.Abs(*file)
		c, err2 := filepath.Abs(ctxDir)
		if err := errors.Join(err1, err2); err != nil {
			fail("%v", err)
		}
		s.ImageFile, s.ImageContext = f, c
	} else {
		s.ImageFile, s.ImageContext = "", ""
	}
	SaveSettings(s)
	tag := buildImage(e, s.ImageFile, s.ImageContext)
	fmt.Printf("new sandboxes start from %s, built again (from the cache) when created\n(an existing sandbox keeps its image: mono-jig rm <name>, then open, to move it)\n", tag)
}

// plainImage: --plain-image, create the sandbox from JIG_IMAGE or the plain
// base image (baseImage) instead of building one on the Mac.
var plainImage bool

// splitPlainImage takes --plain-image out of args.
func splitPlainImage(args []string) (rest []string, plain bool) {
	for _, a := range args {
		if a == "--plain-image" {
			plain = true
		} else {
			rest = append(rest, a)
		}
	}
	return rest, plain
}

// newSandboxImage readies the image for a sandbox about to be created, and
// sets JIG_IMAGE to it for the supervisor (sandboxImage). For a sandbox
// that exists it does nothing: it keeps its image.
func newSandboxImage(name string) {
	ok, err := track.Backend.Exists(context.Background(), name)
	if err != nil {
		fail("%v", err)
	}
	if ok {
		if plainImage {
			fail("--plain-image is for a new sandbox; %s exists (mono-jig rm %s first)", name, name)
		}
		return
	}
	if plainImage {
		ref := env("JIG_IMAGE", baseImage())
		os.Setenv("JIG_IMAGE", ref)
		fmt.Printf("--plain-image: %s from %s; its first start installs everything (minutes)\n", name, ref)
		return
	}
	if v := os.Getenv("JIG_IMAGE"); v != "" {
		fmt.Printf("%s from JIG_IMAGE=%s\n", name, v)
		return
	}
	e := imageEngine("")
	if e == "" {
		fail("a new sandbox's image is built on the Mac with Docker or Podman, and neither is installed:\n" +
			"install one, or pass --plain-image to install everything in the guest at its first start (minutes)")
	}
	s := LoadSettings()
	if s.ImageFile != "" {
		if _, err := os.Stat(s.ImageFile); err != nil {
			fail("your Dockerfile %s: %v\n(mono-jig image reset to build the base image alone)", s.ImageFile, err)
		}
	}
	fmt.Printf("building %s's image with %s (from the cache when nothing changed)\n", name, e)
	os.Setenv("JIG_IMAGE", buildImage(e, s.ImageFile, s.ImageContext))
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
// loads the result into microsandbox unless it's there, and records it.
func buildImage(engine, file, ctxDir string) string {
	setRuntimePaths()
	tag := buildBase(engine)
	if file != "" {
		tag = buildUser(engine, file, ctxDir)
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

// buildUser builds file on the base image, tagged by the image's ID, so
// an unchanged build (from the cache) has the same tag and isn't loaded
// into microsandbox again, and a changed one gets a new tag (sandboxes made
// from the old one keep it).
func buildUser(engine, file, ctxDir string) string {
	idf, err := os.CreateTemp("", "mono-jig-iid-")
	if err != nil {
		fail("%v", err)
	}
	idf.Close()
	defer os.Remove(idf.Name())
	runCmd(engine, append(buildVerb(engine), "--platform", "linux/"+runtime.GOARCH,
		"--build-arg", "JIG_BASE="+baseTag(), "--iidfile", idf.Name(),
		"-f", file, ctxDir)...)
	b, err := os.ReadFile(idf.Name())
	if err != nil {
		fail("%v", err)
	}
	tag, err := userTag(string(b))
	if err != nil {
		fail("%s build: %v", engine, err)
	}
	runCmd(engine, "tag", strings.TrimSpace(string(b)), tag)
	return tag
}

// userTag names the user's image by its ID (sha256:<hex>, from --iidfile).
func userTag(id string) (string, error) {
	h := strings.TrimPrefix(strings.TrimSpace(id), "sha256:")
	if len(h) < 12 || strings.Trim(h, "0123456789abcdef") != "" {
		return "", fmt.Errorf("unexpected image ID %q", id)
	}
	return "localhost/mono-jig:" + h[:12], nil
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

// baseDockerfile is the base image's Dockerfile: a layer per baked
// component.
func baseDockerfile() string {
	return guest.ImageDockerfile(provisionFiles(), strings.Fields(imageComponents()))
}

// baseTag names the base image by everything it's built from, the
// extra CAs included.
func baseTag() string {
	h := sha256.New()
	for _, s := range slices.Concat([]string{baseDockerfile()}, baseArgs(), extraCAs()) {
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
	files := map[string]string{"Dockerfile": baseDockerfile(), "ca/README": "Extra CAs (mono-jig ca), one .crt each.\n"}
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
