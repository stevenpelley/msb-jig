package jig

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	nameRE       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	gitlabUserRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)
	githubUserRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)
	// hostRE is a DNS name of two labels or more, lower case.
	hostRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
)

// Fail prints a message and exits with status 1.
func Fail(f string, a ...any) { fail(f, a...) }

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "mono-jig: "+f+"\n", a...)
	os.Exit(1)
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

// sandboxCmd runs the commands that take a sandbox name. The supervisor
// (run) is in supervisor.go; the editor's access (ssh-config, code) in
// remote.go.
func sandboxCmd(cmd, name string, args []string) {
	if !nameRE.MatchString(name) {
		fail("name must be lowercase letters, digits and dashes, at most 31 characters")
	}
	state := filepath.Join(stateRoot(), name)
	setRuntimePaths()
	switch cmd {
	case "up":
		up(name, state)
	case "run":
		run(name, state)
	case "down":
		down(state)
	case "shell":
		shell(name)
	case "ssh-config":
		fmt.Print(sshConfig(name, state))
	case "code":
		code(name, state, args)
	case "open":
		open(name, state, args)
	case "stop":
		stop(name, state)
	case "denied":
		denied(state)
	case "rm":
		down(state)
		if err := track.Backend.Remove(context.Background(), name); err != nil {
			fail("remove: %v", err)
		}
		// The next sandbox of this name has a new host key, and gets a new
		// key of ours, authorized again (sshConfig).
		_ = os.Remove(filepath.Join(state, "known_hosts"))
		if err := pruneAuthorizedKeys(func(c string) bool { return c == sshHost(name) }); err != nil {
			fail("%v", err)
		}
		_ = os.Remove(filepath.Join(state, "ssh_key"))
		_ = os.Remove(filepath.Join(state, "ssh_key.pub"))
		// The next sandbox of this name mounts only what its --mount says.
		_ = os.Remove(mountsFile(state))
		// And its broker's mode, by --broker or the default.
		_ = os.Remove(brokerModeFile(state))
		_ = os.Remove(proxyPortFile(state))
		// And the broker's CA, which only that sandbox trusted: the next
		// one gets its own, constrained to the names its mode needs.
		_ = os.RemoveAll(filepath.Join(state, "broker-ca"))
		fmt.Println("removed", name, "(state kept in", state+")")
	}
}

// setRuntimePaths points the SDK at a Homebrew install, which it doesn't
// find on its own (it looks in ~/.microsandbox).
func setRuntimePaths() {
	if os.Getenv("MSB_PATH") != "" {
		return
	}
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".microsandbox", "bin", "msb")); err == nil {
		return
	}
	p, err := exec.LookPath("msb")
	if err != nil {
		return
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	libexec := filepath.Join(filepath.Dir(filepath.Dir(p)), "libexec")
	if _, err := os.Stat(filepath.Join(libexec, "msb")); err == nil {
		os.Setenv("MSB_PATH", filepath.Join(libexec, "msb"))
		if libs, _ := filepath.Glob(filepath.Join(libexec, "libkrunfw.*.dylib")); len(libs) > 0 {
			os.Setenv("MSB_LIBKRUNFW_PATH", libs[0])
		}
	}
}

// --- up, down: the supervisor in the background ---

func pidOf(state string) int {
	b, err := os.ReadFile(filepath.Join(state, "run.pid"))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return 0
	}
	return pid
}

func up(name, state string) {
	if pid := pidOf(state); pid != 0 {
		fmt.Printf("%s is already up (supervisor pid %d)\n", name, pid)
		return
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		fail("%v", err)
	}
	logPath := filepath.Join(state, "run.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fail("%v", err)
	}
	ready := filepath.Join(state, "ready")
	_ = os.Remove(ready)
	self, _ := os.Executable()
	c := exec.Command(self, "run", name)
	c.Stdout, c.Stderr = lf, lf
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		fail("%v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- c.Wait() }()
	fmt.Printf("starting %s (log: %s); the first start provisions the guest, which takes a few minutes\n", name, logPath)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	deadline := time.After(20 * time.Minute)
	for {
		select {
		case err := <-exited:
			tail(logPath, 20)
			fail("the supervisor exited: %v", err)
		case <-deadline:
			fail("not ready after 20 minutes; see %s", logPath)
		case <-tick.C:
			if _, err := os.Stat(ready); err == nil {
				fmt.Printf("%s is up (supervisor pid %d)\n  shell:   mono-jig shell %[3]s\n  VS Code: mono-jig code %[3]s\n  down:    mono-jig down %[3]s\n", name, c.Process.Pid, name)
				return
			}
		}
	}
}

func tail(path string, n int) {
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	fmt.Fprintln(os.Stderr, strings.Join(lines, "\n"))
}

func down(state string) {
	pid := pidOf(state)
	if pid == 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 90; i++ {
		if syscall.Kill(pid, 0) != nil {
			fmt.Println("down")
			return
		}
		time.Sleep(time.Second)
	}
	fail("the supervisor (pid %d) didn't stop in 90s", pid)
}

func shell(name string) {
	msb, err := exec.LookPath("msb")
	if err != nil {
		fail("msb not found")
	}
	fail("%v", syscall.Exec(msb, []string{"msb", "exec", "-t", "-u", guestUser, name, "--", "bash", "-l"}, os.Environ()))
}
