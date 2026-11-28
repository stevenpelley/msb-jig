package jig

// The commands that set mono-jig up and take it down: setup, login,
// logout, self uninstall (docs/design.md, "Install and uninstall"). The
// sandbox commands are in lifecycle.go.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// --- setup ---

// setup checks the Mac for what mono-jig needs, writes the harness's
// VS Code profile (settings, and the Remote-SSH extension in its own
// extensions directory) and builds the base image (image.go), or pulls
// plain Ubuntu without Docker or Podman.
func setup() {
	setRuntimePaths()
	ok := true
	need := func(bin, why, fix string) bool {
		if _, err := exec.LookPath(bin); err != nil {
			fmt.Printf("  missing  %-6s %s: %s\n", bin, why, fix)
			return false
		}
		fmt.Printf("  ok       %-6s %s\n", bin, why)
		return true
	}
	fmt.Println("required:")
	if need("msb", "runs the sandboxes", "brew install superradcompany/tap/microsandbox") {
		if out, err := exec.Command("msb", "doctor").CombinedOutput(); err != nil {
			fmt.Printf("  msb doctor failed:\n%s", out)
			ok = false
		}
	} else {
		ok = false
	}
	_, edErr := track.Editor.Find()
	if edErr != nil {
		fmt.Printf("  missing  %-6s %v\n", track.Editor.Name(), edErr)
	} else {
		fmt.Printf("  ok       %-6s the editor\n", track.Editor.Name())
	}
	ok = ok && edErr == nil
	fmt.Println("optional, for login:")
	need("glab", "GitLab with your own glab login", "brew install glab, or login gitlab --keychain")
	need("gh", "GitHub with your own gh login", "brew install gh, or login github --keychain")
	need("claude", "claude setup-token makes the Claude token", "https://claude.com/claude-code")
	need("aws", "AWS SSO login", "brew install awscli")
	need("uv", "runs AWS's MCP proxy", "brew install uv")

	root := stateRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		fail("%v", err)
	}
	if err := track.Editor.Configure(root, editorProfile(root)); err != nil {
		fail("%v", err)
	}
	if edErr == nil {
		// The harness's instance has its own extensions (for VS Code,
		// Remote-SSH only: the agent's go on the remote).
		if err := track.Editor.Install(root); err != nil {
			fmt.Printf("  installing %s's extensions failed: %v\n", track.Editor.Name(), err)
			ok = false
		}
	}
	image, local := sandboxImage()
	if e := imageEngine(""); ok && image == baseImage() && os.Getenv("JIG_IMAGE") == "" && e != "" {
		fmt.Println("building the base image with", e, "(mono-jig build)")
		image, local = buildImage(e, "", ""), true
	} else if ok && image == baseImage() {
		fmt.Println("no docker or podman: new sandboxes need one to build their image, or JIG_PLAIN_IMAGE=1\n" +
			"  (plain Ubuntu, everything installed at the first start)")
	}
	if ok && !local {
		fmt.Println("pulling", image)
		c := exec.Command("msb", "pull", image)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			fail("msb pull %s: %v", image, err)
		}
	}
	if !ok {
		fail("setup isn't complete: fix the missing pieces above and run it again")
	}
	fmt.Printf("set up in %s\nnext: mono-jig login all, then mono-jig create <name>\n", root)
}

// --- login, logout ---

// Ask prints a prompt and reads one line from stdin. It reads a byte at
// a time, so nothing typed after the line is taken from the commands
// that run next (glab, gh, aws sso login).
func Ask(prompt string) string {
	fmt.Print(prompt)
	var b []byte
	one := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(one)
		if n == 1 && one[0] != '\n' {
			b = append(b, one[0])
			continue
		}
		if n == 1 || err != nil {
			return strings.TrimSpace(string(b))
		}
	}
}

// ReadSecret reads a line from the terminal without echo.
func ReadSecret(prompt string) string {
	fmt.Print(prompt)
	echo := func(on bool) {
		a := "-echo"
		if on {
			a = "echo"
		}
		c := exec.Command("stty", a)
		c.Stdin = os.Stdin
		_ = c.Run()
	}
	echo(false)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	echo(true)
	fmt.Println()
	if err != nil && line == "" {
		fail("no input")
	}
	return strings.TrimSpace(line)
}

var tokenRE = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{8,1000}$`)

// The helpers below are for a track's own ProviderCLI: its Login asks
// with Ask and ReadSecret, keeps a token with KeychainStore, and its
// Broker reads it back with KeychainCommand (a token command, run by the
// broker). Fail ends the command with a message.

// KeychainHas reports whether the Keychain has a generic password for
// service.
func KeychainHas(service string) bool {
	return exec.Command("security", "find-generic-password", "-s", service).Run() == nil
}

// KeychainStore writes a generic password through security's own
// command reader, so the token never appears in a process's arguments.
func KeychainStore(service, account, secret string) {
	if !tokenRE.MatchString(secret) {
		fail("that doesn't look like a token")
	}
	// One item per service: the broker reads the first match.
	KeychainDelete(service)
	c := exec.Command("security", "-i")
	c.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -s %s -a %s -l %s -w %s\n", service, account, service, secret))
	out, err := c.CombinedOutput()
	// security -i exits 0 whatever its commands did: read it back.
	back, rerr := exec.Command("security", "find-generic-password", "-s", service, "-w").Output()
	if err != nil || rerr != nil || strings.TrimSpace(string(back)) != secret {
		fail("storing %s in the Keychain failed: %v %s", service, err, out)
	}
}

// KeychainCommand is the token command that prints service's password.
func KeychainCommand(service string) string {
	return "security find-generic-password -s " + service + " -w"
}

// secretRE is what keychainUpdate takes: a token's characters, which
// security's command reader can't misread, up to 16 KiB.
var secretRE = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)

// keychainUpdate writes service's password in place (-U), for a token
// rotated while the broker runs: readers never see the item missing. It
// returns an error rather than failing, as the broker calls it.
func keychainUpdate(service, secret string) error {
	if len(secret) < 8 || len(secret) > 16384 || !secretRE.MatchString(secret) {
		return fmt.Errorf("the token for %s has characters the Keychain helper doesn't take", service)
	}
	c := exec.Command("security", "-i")
	c.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a mono-jig -l %s -w %s\n", service, service, secret))
	out, err := c.CombinedOutput()
	back, rerr := keychainRead(service)
	if err != nil || rerr != nil || back != secret {
		return fmt.Errorf("storing %s in the Keychain failed: %v %s", service, err, out)
	}
	return nil
}

// keychainRead is service's password.
func keychainRead(service string) (string, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-w").Output()
	return strings.TrimSpace(string(out)), err
}

// KeychainDelete removes every generic password for service.
func KeychainDelete(service string) {
	for KeychainHas(service) {
		if exec.Command("security", "delete-generic-password", "-s", service).Run() != nil {
			fail("deleting %s from the Keychain", service)
		}
	}
}

// --- uninstall ---

// uninstall (self uninstall, as msb self uninstall removes msb) removes
// every sandbox, the state directory (with the VS Code profile and
// settings), the Keychain items, the sandboxes' SSH keys from
// microsandbox, and this executable, unless Homebrew installed it (then
// brew uninstall does). microsandbox and msb's image cache stay.
func uninstall(args []string) {
	yes := slices.Equal(args, []string{"--yes"})
	if len(args) > 0 && !yes {
		usage()
	}
	setRuntimePaths()
	root := stateRoot()
	names := sandboxNames(root)
	self, brew := selfPath()
	fmt.Printf("This removes:\n  the sandboxes %s, with their disks and everything in them\n  %s\n  the Keychain items %s\n  their SSH keys from microsandbox\n",
		strings.Join(names, " "), root, strings.Join(keychainItems(), " "))
	if self != "" && !brew {
		fmt.Printf("  %s\n", self)
	}
	if !yes {
		fmt.Print("type yes to go on: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			fail("nothing removed")
		}
	}
	for _, n := range names {
		down(filepath.Join(root, n))
		if err := track.Backend.Remove(context.Background(), n); err != nil {
			fail("removing %s: %v", n, err)
		}
		fmt.Println("removed", n)
	}
	if err := pruneAuthorizedKeys(func(c string) bool { return strings.HasPrefix(c, "mono-jig-") }); err != nil {
		fail("%v", err)
	}
	for _, k := range keychainItems() {
		KeychainDelete(k)
	}
	if err := os.RemoveAll(root); err != nil {
		fail("%v", err)
	}
	fmt.Println("removed", root, "and the Keychain items")
	switch {
	case brew:
		fmt.Println("to finish: brew uninstall mono-jig (and microsandbox, if nothing else uses it)")
	case self != "":
		if err := os.Remove(self); err != nil {
			fail("%v", err)
		}
		fmt.Println("removed", self)
	}
}

// selfPath is this executable, with links resolved, and whether Homebrew
// installed it ("" if it can't be found).
func selfPath() (path string, brew bool) {
	p, err := os.Executable()
	if err != nil {
		return "", false
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p, strings.Contains(p, "/Cellar/")
}

// sandboxNames are the sandboxes with a state directory under root.
func sandboxNames(root string) []string {
	es, _ := os.ReadDir(root)
	var names []string
	for _, e := range es {
		if e.IsDir() && nameRE.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names
}

// authorizedKeys is microsandbox's SSH authorization file (msb ssh authorize).
func authorizedKeys() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(env("MSB_HOME", filepath.Join(home, ".microsandbox")), "ssh", "authorized_keys")
}

// pruneAuthorizedKeys drops the keys whose comment matches.
func pruneAuthorizedKeys(match func(comment string) bool) error {
	p := authorizedKeys()
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var keep []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		f := strings.Fields(l)
		if len(f) >= 3 && match(f[len(f)-1]) {
			continue
		}
		keep = append(keep, l)
	}
	out := strings.Join(keep, "\n")
	if out != "" {
		out += "\n"
	}
	return os.WriteFile(p, []byte(out), 0o600)
}
