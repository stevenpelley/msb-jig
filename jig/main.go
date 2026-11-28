// Package jig is the harness CLI, as a library: a track (Track: its
// providers, agent, editor, VM runtime and base image) calls Main from its
// own command, as the personal track's mono-jig does with Personal
// (docs/library-split.md).
// A sandbox is a microsandbox VM owned by a supervisor process that also
// serves the broker (docs/microsandbox.md); the commands taking a <name>
// manage one. The commands are a supplement to msb, not a replacement:
// where msb has a command of the same name (create, start, stop, rm, ssh),
// ours means the same, plus the broker, and everything msb already does
// (ls, inspect, logs, exec, cp) is left to it. usage lists them.
package jig

import (
	"fmt"
	"os"
	"strings"

	"github.com/stevenpelley/msb-jig/allowlist"
)

// providerUsage are the providers' login lines.
func providerUsage() string {
	var b strings.Builder
	for _, p := range cliProviders() {
		u := p.Usage
		if u == "" {
			u = "login " + p.Name + "\n"
		}
		for _, line := range strings.SplitAfter(strings.TrimSuffix(u, "\n")+"\n", "\n") {
			if line != "" {
				b.WriteString("  " + line)
			}
		}
	}
	return b.String()
}

func usage() {
	ag, ed := agentCmd(), track.Editor.Name()
	fmt.Fprintln(os.Stderr, `usage: mono-jig <command> ...

Sandboxes. Make one with create; the commands that use it start it if
it's stopped. Where msb has a command of the same name, ours does the
same, and also runs the sandbox's broker:
  create <name> [-v <host>:<guest>:ro|rw]...
                          create the sandbox and start it. -v mounts a Mac folder
                          as msb create -v does, all three parts required: <host>
                          starts with / or ., <guest> is under /mnt. Every sandbox
                          also has ~/.mono-jig/<name>/share at /mnt/host (and ~/host)
  start <name>            start it and its broker, in the background
                          (log: ~/.mono-jig/<name>/run.log)
  `+pad(ag+" <name> [arg...]", 24)+`run `+ag+` in the sandbox, from the agent user's home
  `+pad(ed+" <name> [dir]", 24)+`open `+ed+` on the sandbox, on dir (relative to the agent
                          user's home; default the home)
  ssh <name> [cmd...]     a login shell as the agent user, or cmd from its home
  stop <name>             stop the sandbox and its broker
  rm [-f] <name>          remove the sandbox and its disks; -f stops it first

Setting up:
  setup                   check what's needed, set up the editor profile, build the base image
  login                   show what the broker uses for each provider (`+cliProviderNames()+`)
  login all               ask about each provider in turn and log in with the defaults
`+providerUsage()+`  logout [<provider>...]  forget them (all, if none named)
  build [-f <Dockerfile> [<context>]] [-engine docker|podman]
                          build the image new sandboxes start from: the base (git,
                          Docker, `+ag+`...), then your Dockerfile on it, which starts
                          ARG JIG_BASE and FROM ${JIG_BASE}. The Dockerfile is
                          recorded: each create builds it again (from the cache).
                          Without -f, the base alone. A sandbox keeps the image it
                          was created from

When something's wrong:
  denied <name>           hosts the sandbox was refused, most recent first
  allowlist               the hosts a new sandbox may reach (base, JIG_LAYERS,
                          ~/.mono-jig/allowlist.yaml)

Rarely:
  ca [<pem-file>...|--clear]
                          extra CA certificates the sandboxes trust, such as a
                          TLS-inspecting proxy's root; none given: show them
  ssh-config <name>       write and print its OpenSSH Host block, for ssh or
                          another editor (`+ed+` writes it itself)
  self uninstall [--yes]  remove mono-jig: every sandbox, its state, the Keychain
                          items and this command

Environment, read by create:
  JIG_BROKER_MODE=server|proxy
                          how the guest reaches the broker, default `+track.BrokerMode+`: server
                          (broker.internal names) or proxy (microsandbox's outbound
                          proxy; the providers' hosts by their own names)
  JIG_PLAIN_IMAGE=1       start from plain `+track.BaseImage+` and install everything in the
                          guest at the first start (minutes), without Docker or Podman
and by build and create: JIG_KEEP_IMAGES=1 keeps the images a build replaces.

Everything else is msb's (microsandbox):
  msb ls                  every sandbox, running or stopped
  msb inspect <name>      one sandbox's configuration and status
  msb logs <name>         its output; msb ps, msb metrics
  msb exec -u agent <name> -- <cmd>, msb cp <src> <name>:<path>
                          only while it's started (mono-jig start): msb starts a
                          stopped sandbox without its broker
Stop and remove sandboxes with mono-jig, not msb stop or msb rm: mono-jig
also stops the broker and clears the sandbox's state.`)
	os.Exit(2)
}

// pad left-aligns s in a column of width n, or ends the line when it's
// longer, as the usage's other entries do.
func pad(s string, n int) string {
	if len(s) < n {
		return s + strings.Repeat(" ", n-len(s))
	}
	return s + "\n" + strings.Repeat(" ", n+2)
}

// supervisorEnv names the sandbox whose supervisor this process is: start
// runs the same executable with it set, instead of a command of its own.
const supervisorEnv = "JIG_SUPERVISOR"

// Main runs the command line for track t.
func Main(t Track) {
	track = t.withDefaults()
	if name := os.Getenv(supervisorEnv); name != "" {
		os.Unsetenv(supervisorEnv)
		run(name, sandboxState(name))
		return
	}
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch {
	case cmd == track.Editor.Name():
		cmd = "editor"
	case cmd == agentCmd():
		cmd = "agent"
	case cmd == "editor" || cmd == "agent":
		usage()
	}
	switch cmd {
	case "setup":
		if len(args) != 0 {
			usage()
		}
		setup()
	case "login":
		login(args)
	case "logout":
		logout(args)
	case "build":
		build(args)
	case "ca":
		caCmd(args)
	case "allowlist":
		if len(args) != 0 {
			usage()
		}
		allowlistCmd()
	case "self":
		if len(args) == 0 || args[0] != "uninstall" {
			usage()
		}
		uninstall(args[1:])
	case "create":
		name, specs, err := createArgs(args)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mono-jig create:", err)
			usage()
		}
		create(name, specs)
	case "rm":
		name, force, err := rmArgs(args)
		if err != nil {
			usage()
		}
		rm(name, sandboxState(name), force)
	case "agent", "ssh":
		if len(args) == 0 {
			usage()
		}
		state := sandboxState(args[0])
		if cmd == "agent" {
			agentCLI(args[0], state, args[1:])
		} else {
			sshCmd(args[0], state, args[1:])
		}
	case "editor":
		if len(args) != 1 && len(args) != 2 {
			usage()
		}
		code(args[0], sandboxState(args[0]), args[1:])
	case "start", "stop", "ssh-config", "denied":
		if len(args) != 1 {
			usage()
		}
		name, state := args[0], sandboxState(args[0])
		switch cmd {
		case "start":
			start(name, state)
		case "stop":
			stop(name, state)
		case "ssh-config":
			fmt.Print(sshConfig(name, state))
		case "denied":
			denied(state)
		}
	default:
		usage()
	}
}

// createArgs reads create's arguments: one name, and -v or --volume
// mounts (-v spec, --volume spec, --volume=spec), in any order.
func createArgs(args []string) (name string, specs []string, err error) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-v" || a == "--volume":
			if i+1 == len(args) {
				return "", nil, fmt.Errorf("%s needs <host>:<guest>:ro|rw", a)
			}
			i++
			specs = append(specs, args[i])
		case strings.HasPrefix(a, "--volume="):
			specs = append(specs, strings.TrimPrefix(a, "--volume="))
		case strings.HasPrefix(a, "-"):
			return "", nil, fmt.Errorf("unknown flag %s", a)
		case name != "":
			return "", nil, fmt.Errorf("one sandbox name, not %s and %s", name, a)
		default:
			name = a
		}
	}
	if name == "" {
		return "", nil, fmt.Errorf("no sandbox name")
	}
	return name, specs, nil
}

// rmArgs reads rm's arguments: one name, and -f or --force.
func rmArgs(args []string) (name string, force bool, err error) {
	for _, a := range args {
		switch {
		case a == "-f" || a == "--force":
			force = true
		case strings.HasPrefix(a, "-") || name != "":
			return "", false, fmt.Errorf("rm [-f] <name>")
		default:
			name = a
		}
	}
	if name == "" {
		return "", false, fmt.Errorf("rm [-f] <name>")
	}
	return name, force, nil
}

// agentCmd is the command that runs the track's agent CLI.
func agentCmd() string { return track.Agent.Command() }

// commands are mono-jig's own commands, which the agent's and the
// editor's can't be.
var commands = []string{"create", "start", "ssh", "stop", "rm", "setup", "login", "logout", "build",
	"denied", "allowlist", "ca", "ssh-config", "self", "editor", "agent", "help"}

// allowlistCmd prints the hosts a new sandbox would reach: the base, the
// JIG_LAYERS files and the user's layer, less the hosts of the providers
// you're logged in to (allowed).
func allowlistCmd() {
	es, err := allowed(brokerConfig())
	if err != nil {
		fail("allowlist: %v", err)
	}
	fmt.Print(allowlist.Format(es))
}
