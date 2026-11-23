package main

// The user-facing commands: setup, login, logout, open, stop, uninstall
// (investigator-design.md, "Install and uninstall"). They sit on the
// sandbox commands in sandbox.go; up, down and rm stay for the checks.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	m "github.com/superradcompany/microsandbox/sdk/go"

	"mcp-restrict/cli"
)

// --- setup ---

// setup checks the Mac for what investigator needs, writes the harness's
// VS Code profile (settings, and the Remote-SSH extension in its own
// extensions directory) and pulls the sandbox image.
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
	hasCode := need("code", "VS Code's command line", "in VS Code, Shell Command: Install 'code' command in PATH")
	ok = ok && hasCode
	fmt.Println("optional, for login:")
	need("glab", "GitLab with your own glab login", "brew install glab, or login gitlab --keychain")
	need("claude", "claude setup-token makes the Claude token", "https://claude.com/claude-code")
	need("aws", "AWS SSO login", "brew install awscli")
	need("uv", "runs AWS's MCP proxy", "brew install uv")

	root := stateRoot()
	data, ext := filepath.Join(root, "_vscode"), filepath.Join(root, "_vscode-ext")
	if err := os.MkdirAll(root, 0o700); err != nil {
		fail("%v", err)
	}
	if _, err := cli.WriteVSCodeSettings(data, filepath.Join(root, "ssh_config")); err != nil {
		fail("%v", err)
	}
	if hasCode {
		// The harness's instance has its own extensions: Remote-SSH only
		// (Claude Code's goes on the remote, remote.SSH.defaultExtensions).
		c := exec.Command("code", "--user-data-dir", data, "--extensions-dir", ext,
			"--install-extension", "ms-vscode-remote.remote-ssh")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			fmt.Println("  installing Remote-SSH failed:", err)
			ok = false
		}
	}
	if ok {
		image := env("INV_IMAGE", "ubuntu:26.04")
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
	fmt.Printf("set up in %s\nnext: investigator login gitlab|claude|aws, then investigator open <name>\n", root)
}

// --- login, logout ---

func login(args []string) {
	if len(args) == 0 {
		loginStatus()
		return
	}
	switch args[0] {
	case "gitlab":
		loginGitLab(args[1:])
	case "claude":
		loginClaude(args[1:])
	case "aws":
		loginAWS(args[1:])
	default:
		usage()
	}
	fmt.Println("sandboxes pick this up when they next start (investigator stop, then open)")
}

func loginStatus() {
	s := loadSettings()
	if s.GitLab != nil {
		fmt.Printf("gitlab  %s on %s, token from %s\n", s.GitLab.User, s.GitLab.URL, tokenSource(*s.GitLab))
	} else {
		fmt.Println("gitlab  not logged in (investigator login gitlab <user>)")
	}
	if keychainHas(keychainClaude) {
		fmt.Printf("claude  token in the Keychain (%s)\n", keychainClaude)
	} else {
		fmt.Println("claude  not logged in (investigator login claude): the agent signs in inside the sandbox")
	}
	if s.AWS != nil && len(s.AWS.Profiles) > 0 {
		fmt.Printf("aws     profiles %s (the first is the default)\n", strings.Join(s.AWS.Profiles, " "))
	} else {
		fmt.Println("aws     not logged in (investigator login aws <profile>...)")
	}
}

func tokenSource(g gitlabSettings) string {
	if g.Token == "keychain" {
		return "the Keychain (" + keychainGitLab + ")"
	}
	return "your glab login"
}

// loginGitLab records the user the agent acts for, and where the broker
// gets their token: glab's login on the Mac (logging in if needed), or a
// token pasted into the Keychain. It checks the token belongs to the user.
func loginGitLab(args []string) {
	var g gitlabSettings
	g.URL, g.Token = "https://gitlab.com", "glab"
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--keychain":
			g.Token = "keychain"
		case a == "--url" && i+1 < len(args):
			i++
			g.URL = strings.TrimRight(args[i], "/")
		case strings.HasPrefix(a, "-"):
			usage()
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 || !gitlabUserRE.MatchString(rest[0]) {
		usage()
	}
	g.User = rest[0]
	u, err := url.Parse(g.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" {
		fail("--url: want https://host")
	}
	if g.Token == "keychain" {
		tok := readSecret(fmt.Sprintf("a GitLab token for %s on %s (api scope; input hidden): ", g.User, u.Host))
		keychainStore(keychainGitLab, u.Host, tok)
	} else {
		if _, err := exec.LookPath("glab"); err != nil {
			fail("glab isn't installed: brew install glab, or use --keychain")
		}
		if exec.Command("glab", "auth", "status", "--hostname", u.Host).Run() != nil {
			fmt.Println("glab isn't logged in to", u.Host+": glab auth login")
			c := exec.Command("glab", "auth", "login", "--hostname", u.Host)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				fail("glab auth login: %v", err)
			}
		}
	}
	out, err := exec.Command("sh", "-c", gitlabTokenCommand(g, u.Host)).Output()
	if err != nil {
		fail("reading the token back: %v", err)
	}
	who, err := gitlabUser(g.URL, strings.TrimSpace(string(out)))
	if err != nil {
		fail("checking the token: %v", err)
	}
	if !strings.EqualFold(who, g.User) {
		fail("the token belongs to %s, not %s", who, g.User)
	}
	s := loadSettings()
	s.GitLab = &g
	saveSettings(s)
	fmt.Printf("gitlab: the agent acts for %s on %s, with the token from %s\n", g.User, u.Host, tokenSource(g))
}

// gitlabUser is the username a token belongs to.
func gitlabUser(base, token string) (string, error) {
	req, err := http.NewRequest("GET", base+"/api/v4/user", nil)
	if err != nil {
		return "", err
	}
	// Bearer, as the broker sends it: glab's login is OAuth, which PRIVATE-TOKEN refuses.
	req.Header.Set("Authorization", "Bearer "+token)
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s/api/v4/user: %s", base, resp.Status)
	}
	var u struct{ Username string }
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return "", err
	}
	return u.Username, nil
}

// loginClaude stores a long-lived Claude token, from claude setup-token
// run on the Mac, in the Keychain. The broker sends it for the sandbox.
func loginClaude(args []string) {
	if len(args) != 0 {
		usage()
	}
	fmt.Println("Run `claude setup-token` in another terminal on this Mac and sign in; it prints a token.")
	tok := readSecret("paste the token (input hidden): ")
	if !strings.HasPrefix(tok, "sk-ant-") {
		fail("that isn't a Claude token (sk-ant-…)")
	}
	keychainStore(keychainClaude, "investigator", tok)
	fmt.Println("claude: token stored in the Keychain as", keychainClaude)
}

// loginAWS records the profiles for the AWS MCP proxy and runs aws sso
// login for each that uses SSO. The profiles and the SSO cache are the
// user's own, in ~/.aws (prototype-plan.md step 7).
func loginAWS(args []string) {
	if len(args) == 0 {
		usage()
	}
	if _, err := exec.LookPath("aws"); err != nil {
		fail("the AWS CLI isn't installed: brew install awscli")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		fmt.Println("note: uv isn't installed, and runs AWS's MCP proxy: brew install uv")
	}
	out, err := exec.Command("aws", "configure", "list-profiles").Output()
	if err != nil {
		fail("aws configure list-profiles: %v", err)
	}
	profiles := strings.Fields(string(out))
	for _, p := range args {
		if strings.HasPrefix(p, "-") {
			usage()
		}
		if !slices.Contains(profiles, p) {
			fail("no AWS profile %s in your AWS config (aws configure list-profiles)", p)
		}
		if exec.Command("aws", "sts", "get-caller-identity", "--profile", p).Run() == nil {
			fmt.Println("aws:", p, "is signed in")
			continue
		}
		fmt.Println("aws: signing in", p)
		c := exec.Command("aws", "sso", "login", "--profile", p)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			fail("aws sso login --profile %s: %v", p, err)
		}
	}
	s := loadSettings()
	s.AWS = &awsSettings{Profiles: args}
	saveSettings(s)
	fmt.Printf("aws: profiles %s (the first is the default)\n", strings.Join(args, " "))
}

// logout forgets a provider: the settings, and its Keychain item. It
// leaves the user's own logins alone (glab's, AWS's SSO session).
func logout(args []string) {
	which := args
	if len(which) == 0 {
		which = []string{"gitlab", "claude", "aws"}
	}
	s := loadSettings()
	for _, p := range which {
		switch p {
		case "gitlab":
			if s.GitLab != nil && s.GitLab.Token == "glab" {
				fmt.Println("gitlab: forgotten; your own glab login stays (glab auth logout to end it)")
			} else {
				fmt.Println("gitlab: forgotten")
			}
			s.GitLab = nil
			keychainDelete(keychainGitLab)
		case "claude":
			keychainDelete(keychainClaude)
			fmt.Println("claude: token removed from the Keychain; revoke it at claude.ai, Settings, Claude Code, if you're done with it")
		case "aws":
			s.AWS = nil
			fmt.Println("aws: forgotten; your SSO session stays (aws sso logout ends it for every tool)")
		default:
			usage()
		}
	}
	saveSettings(s)
	fmt.Println("running sandboxes keep the old setting until they stop")
}

// readSecret reads a line from the terminal without echo.
func readSecret(prompt string) string {
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

func keychainHas(service string) bool {
	return exec.Command("security", "find-generic-password", "-s", service).Run() == nil
}

// keychainStore writes a generic password through security's own
// command reader, so the token never appears in a process's arguments.
func keychainStore(service, account, secret string) {
	if !tokenRE.MatchString(secret) {
		fail("that doesn't look like a token")
	}
	// One item per service: the broker reads the first match.
	keychainDelete(service)
	c := exec.Command("security", "-i")
	c.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -s %s -a %s -l %s -w %s\n", service, account, service, secret))
	out, err := c.CombinedOutput()
	// security -i exits 0 whatever its commands did: read it back.
	back, rerr := exec.Command("security", "find-generic-password", "-s", service, "-w").Output()
	if err != nil || rerr != nil || strings.TrimSpace(string(back)) != secret {
		fail("storing %s in the Keychain failed: %v %s", service, err, out)
	}
}

func keychainDelete(service string) {
	for keychainHas(service) {
		if exec.Command("security", "delete-generic-password", "-s", service).Run() != nil {
			fail("deleting %s from the Keychain", service)
		}
	}
}

// --- open, stop ---

// open starts the sandbox if it isn't up, creating it the first time,
// then opens VS Code on it.
func open(name, state string, args []string) {
	if pidOf(state) == 0 {
		up(name, state)
	}
	code(name, state, args)
}

func stop(name, state string) {
	if pidOf(state) == 0 {
		fmt.Println(name, "isn't running")
		return
	}
	down(state)
}

// --- uninstall ---

// uninstall removes every sandbox, the state directory (with the VS Code
// profile and settings), the Keychain items and the sandboxes' SSH keys
// from microsandbox. Homebrew's packages and msb's image cache stay.
func uninstall(args []string) {
	yes := slices.Equal(args, []string{"--yes"})
	if len(args) > 0 && !yes {
		usage()
	}
	setRuntimePaths()
	root := stateRoot()
	names := sandboxNames(root)
	fmt.Printf("This removes:\n  the sandboxes %s, with their disks and everything in them\n  %s\n  the Keychain items %s and %s\n  their SSH keys from microsandbox\n",
		strings.Join(names, " "), root, keychainGitLab, keychainClaude)
	if !yes {
		fmt.Print("type yes to go on: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			fail("nothing removed")
		}
	}
	for _, n := range names {
		down(filepath.Join(root, n))
		if err := m.RemoveSandbox(context.Background(), n); err != nil && !m.IsKind(err, m.ErrSandboxNotFound) {
			fail("removing %s: %v", n, err)
		}
		fmt.Println("removed", n)
	}
	if err := pruneAuthorizedKeys(func(c string) bool { return strings.HasPrefix(c, "investigator-") }); err != nil {
		fail("%v", err)
	}
	keychainDelete(keychainGitLab)
	keychainDelete(keychainClaude)
	if err := os.RemoveAll(root); err != nil {
		fail("%v", err)
	}
	fmt.Println("removed", root, "and the Keychain items")
	fmt.Println("to finish: brew uninstall investigator (and microsandbox, if nothing else uses it)")
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
