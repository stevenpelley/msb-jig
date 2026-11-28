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

// sandboxState checks a sandbox's name and returns its state directory.
// It also points the SDK at msb, which every sandbox command needs.
func sandboxState(name string) string {
	if !nameRE.MatchString(name) {
		fail("name must be lowercase letters, digits and dashes, at most 31 characters")
	}
	setRuntimePaths()
	return filepath.Join(stateRoot(), name)
}

// exists reports whether microsandbox has the sandbox.
func exists(name string) bool {
	ok, err := track.Backend.Exists(context.Background(), name)
	if err != nil {
		fail("%v", err)
	}
	return ok
}

// create records what's fixed when a sandbox is made (its mounts and the
// broker's mode, which microsandbox can't change afterwards), then starts
// it: the supervisor creates it, from an image built on the Mac first.
func create(name string, specs []string) {
	state := sandboxState(name)
	ms := parseMounts(specs)
	mode := env("JIG_BROKER_MODE", track.BrokerMode)
	if mode != modeServer && mode != modeProxy {
		fail("JIG_BROKER_MODE=%s: want server or proxy", mode)
	}
	if exists(name) {
		fail("%s exists: mono-jig start %s, or mono-jig rm %s and create it again", name, name, name)
	}
	// What an earlier sandbox of this name left, which a new one mustn't
	// inherit.
	clearSandboxState(name, state)
	if err := os.MkdirAll(state, 0o700); err != nil {
		fail("%v", err)
	}
	writeMounts(state, ms)
	writeBrokerMode(state, mode)
	startSupervisor(name, state)
}

// start starts a sandbox that exists.
func start(name, state string) {
	if pid := pidOf(state); pid != 0 {
		fmt.Printf("%s is already started (supervisor pid %d)\n", name, pid)
		return
	}
	ensureStarted(name, state)
}

// ensureStarted starts the sandbox unless it's started; it doesn't
// create one.
func ensureStarted(name, state string) {
	if pidOf(state) != 0 {
		return
	}
	if !exists(name) {
		fail("there's no sandbox %s: mono-jig create %s", name, name)
	}
	startSupervisor(name, state)
}

// rm removes the sandbox, and what its state directory holds for it
// alone. The rest stays: its log, its denials and the exchange folder.
func rm(name, state string, force bool) {
	if pidOf(state) != 0 {
		if !force {
			fail("%s is started: mono-jig stop %s first, or rm -f", name, name)
		}
		down(state)
	}
	if err := track.Backend.Remove(context.Background(), name); err != nil {
		fail("remove: %v", err)
	}
	clearSandboxState(name, state)
	fmt.Println("removed", name, "(state kept in", state+")")
}

// clearSandboxState removes the files that belong to one sandbox of this
// name, so the next one starts afresh.
func clearSandboxState(name, state string) {
	// The next sandbox of this name has a new host key, and gets a new
	// key of ours, authorized again (sshConfig).
	_ = os.Remove(filepath.Join(state, "known_hosts"))
	_ = os.Remove(sshFilesFor(state).sshdKnown)
	_ = os.Remove(sshFilesFor(state).msbConfig)
	if err := pruneAuthorizedKeys(func(c string) bool { return c == sshHost(name) }); err != nil {
		fail("%v", err)
	}
	_ = os.Remove(filepath.Join(state, "ssh_key"))
	_ = os.Remove(filepath.Join(state, "ssh_key.pub"))
	// The next sandbox of this name mounts only what its create says.
	_ = os.Remove(mountsFile(state))
	// And its broker's mode, by JIG_BROKER_MODE or the default.
	_ = os.Remove(brokerModeFile(state))
	_ = os.Remove(proxyPortFile(state))
	// And the broker's CA, which only that sandbox trusted: the next
	// one gets its own, constrained to the names its mode needs.
	_ = os.RemoveAll(filepath.Join(state, "broker-ca"))
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

// --- the supervisor in the background ---

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

// startSupervisor runs this executable again as the sandbox's
// supervisor (supervisorEnv), in its own session, logging to run.log, and
// waits until the sandbox is ready.
func startSupervisor(name, state string) {
	if err := os.MkdirAll(state, 0o700); err != nil {
		fail("%v", err)
	}
	newSandboxImage(name) // the supervisor inherits JIG_IMAGE
	logPath := filepath.Join(state, "run.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fail("%v", err)
	}
	ready := filepath.Join(state, "ready")
	_ = os.Remove(ready)
	self, _ := os.Executable()
	c := exec.Command(self)
	c.Env = append(os.Environ(), supervisorEnv+"="+name)
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
				fmt.Printf("%s is started (supervisor pid %d)\n  shell:   mono-jig ssh %[3]s\n  agent:   mono-jig %[4]s %[3]s\n  editor:  mono-jig %[5]s %[3]s\n  stop:    mono-jig stop %[3]s\n",
					name, c.Process.Pid, name, agentCmd(), track.Editor.Name())
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
			fmt.Println("stopped")
			return
		}
		time.Sleep(time.Second)
	}
	fail("the supervisor (pid %d) didn't stop in 90s", pid)
}

// stop stops the supervisor, and with it the sandbox.
func stop(name, state string) {
	if pidOf(state) == 0 {
		fmt.Println(name, "isn't started")
		return
	}
	down(state)
}

// sshCmd runs a login shell as the guest user, or cmd from the user's
// home, starting the sandbox first if it's stopped.
func sshCmd(name, state string, cmd []string) {
	msb, err := exec.LookPath("msb")
	if err != nil {
		fail("msb not found")
	}
	ensureStarted(name, state)
	fail("%v", syscall.Exec(msb, sshArgv(name, cmd, isTerminal(os.Stdin)), os.Environ()))
}

// sshArgv is the msb command line sshCmd runs: a tty for the shell, and
// for a command when stdin is a terminal.
func sshArgv(name string, cmd []string, tty bool) []string {
	if len(cmd) == 0 {
		return []string{"msb", "exec", "-t", "-u", guestUser, name, "--", "bash", "-l"}
	}
	argv := []string{"msb", "exec"}
	if tty {
		argv = append(argv, "-t")
	}
	return append(append(argv, "-u", guestUser, name, "--", "bash", "-lc", `cd && exec "$0" "$@"`), cmd...)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// agentCLI runs the track's agent CLI in the sandbox with args, from the
// guest user's home, in a login shell (for its PATH). It starts the
// sandbox first if it's stopped.
func agentCLI(name, state string, args []string) {
	msb, err := exec.LookPath("msb")
	if err != nil {
		fail("msb not found")
	}
	ensureStarted(name, state)
	fail("%v", syscall.Exec(msb, agentArgv(name, args), os.Environ()))
}

// agentArgv is the msb command line agentCLI runs.
func agentArgv(name string, args []string) []string {
	return sshArgv(name, append([]string{agentCmd()}, args...), true)
}
