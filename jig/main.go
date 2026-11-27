// Package jig is the harness CLI, as a library: a track (Track: its
// providers, agent, editor, VM runtime and base image) calls Main from its
// own command, as the personal track's mono-jig does with Personal
// (docs/library-split.md).
// A sandbox is a microsandbox VM owned by a supervisor process that also
// serves the broker (docs/microsandbox.md); the commands taking a <name>
// manage one.
//
//	mono-jig setup              check the Mac, write the editor profile, build the base image
//	mono-jig login [provider]   record the track's providers for the broker (login.go); alone: what's recorded
//	mono-jig logout [provider]  forget them, and remove their Keychain items
//	mono-jig image build        your own sandbox image, from a Dockerfile (image.go)
//	mono-jig open <name> [dir]  start the sandbox if needed (creating it the first time), then the editor on dir
//	mono-jig stop <name>        stop it
//	mono-jig cp <src> <dst>     copy files between the Mac and a sandbox (mounts.go: host folders)
//	mono-jig uninstall          remove every sandbox, the state directory and the Keychain items
//
// And beneath them, for the checks:
//
//	mono-jig up <name>          start the supervisor in the background (creates and provisions the sandbox once)
//	mono-jig run <name>         the supervisor, in the foreground
//	mono-jig down <name>        stop the supervisor, and with it the sandbox
//	mono-jig shell <name>       a login shell as the agent user
//	mono-jig <agent> <name> ... the agent's CLI (Agent.Command: claude, kiro-cli) in the sandbox, from ~
//	mono-jig <editor> <name> [dir]  the editor (Editor.Name: vscode, kiro) on the sandbox, with the harness's Settings
//	mono-jig ssh-config <name>  write and print the sandbox's OpenSSH Host block
//	mono-jig denied <name>      the hosts its egress policy refused (denials.jsonl)
//	mono-jig rm <name>          down, then remove the sandbox and its disks (the state directory stays)
//	mono-jig allowlist          the hosts a new sandbox may reach
//	mono-jig ssh-setup          SSH keys and config for an sshd-based sandbox (sbx, checks/step1-docker.sh)
package jig

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevenpelley/msb-jig/allowlist"
	"github.com/stevenpelley/msb-jig/editor"
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
	fmt.Fprintln(os.Stderr, `usage: mono-jig <command> ...

  setup                   check what's needed, set up the editor profile, build the base image
  login                   show what the broker uses for each provider (`+cliProviderNames()+`)
  login all               ask about each provider in turn and log in with the defaults
`+providerUsage()+`  logout [<provider>...]  forget them (all, if none named)
  ca [<pem-file>...|--clear]
                          extra CA certificates the sandboxes trust, such as a
                          TLS-inspecting proxy's root; none given: show them
  open <name> [dir]       start the sandbox (creating it the first time) and open the editor
                          on dir (relative to the sandbox user's home; default: home)
  open <name> [dir] --mount host[:guest][:rw] ...
                          when creating it, also mount these Mac folders (guest path under
                          /mnt; default /mnt/<folder name>). Every sandbox has
                          ~/.mono-jig/<name>/share at /mnt/host (and ~/host)
  open <name> [dir] --broker server|proxy
                          when creating it, how the guest reaches the broker: server
                          (broker.internal names) or proxy (microsandbox's outbound proxy;
                          the providers' hosts by their own names); default `+track.BrokerMode+`
  open <name> [dir] --plain-image
                          when creating it, start from plain `+track.BaseImage+` and install
                          everything in the guest (minutes), instead of building its image
                          on the Mac with Docker or Podman, the default (cached; seconds
                          when nothing changed)
  cp <src> <dst>          copy between the Mac and a sandbox; a sandbox path is
                          <name>:<path>, relative to the agent's home unless absolute
  stop <name>             stop it
  `+agentCmd()+` <name> [arg...]
                          run `+agentCmd()+` in the sandbox, from the agent user's home
                          (starting the sandbox, or creating it, first if needed)
  denied <name>           hosts the sandbox was refused, most recent first
  image init [file]       write a starter Dockerfile (default ./Dockerfile)
  image build [-f Dockerfile [context]] [-engine docker|podman]
                          build the base image (git, Docker, Claude Code...), then your
                          Dockerfile on it, which every new sandbox's build then uses too
                          (new sandboxes build it anyway; this does it ahead of time)
  image show|reset        what new sandboxes build; back to the base image alone
  uninstall [--yes]       remove every sandbox, its state and the Keychain items

sandboxes, beneath open and stop:
  up <name>          start the sandbox and its broker in the background (creates it the first time;
                     --mount, --broker and --plain-image as for open)
  run <name>         the same, in the foreground
  down <name>        stop it
  shell <name>       a login shell as the agent user
  `+track.Editor.Name()+` <name> [dir]  open the editor on it
  ssh-config <name>  write and print its OpenSSH Host block
  rm <name>          stop and remove it, with its disks

other:
  allowlist [-format list|sbx]  the hosts a new sandbox may reach (base, JIG_LAYERS, ~/.mono-jig/allowlist.yaml)
  ssh-setup [flags]  SSH keys and config for an sshd-based sandbox (sbx, checks/step1-docker.sh)`)
	os.Exit(2)
}

// Main runs the command line for track t.
func Main(t Track) {
	track = t.withDefaults()
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	switch {
	case cmd == track.Editor.Name():
		cmd = "editor"
	case cmd == agentCmd():
		cmd = "agent"
	case cmd == "editor" || cmd == "agent":
		usage()
	}
	switch cmd {
	case "ssh-setup":
		sshSetup(os.Args[2:])
	case "allowlist":
		allowlistCmd(os.Args[2:])
	case "setup":
		if len(os.Args) != 2 {
			usage()
		}
		setup()
	case "login":
		login(os.Args[2:])
	case "logout":
		logout(os.Args[2:])
	case "image":
		imageCmd(os.Args[2:])
	case "ca":
		caCmd(os.Args[2:])
	case "uninstall":
		uninstall(os.Args[2:])
	case "cp":
		cp(os.Args[2:])
	case "up", "run", "down", "shell", "editor", "agent", "open", "stop", "ssh-config", "denied", "rm":
		args := os.Args[2:]
		var specs []string
		var mode string
		if cmd == "agent" {
			// The rest are the agent's own arguments.
			if len(args) == 0 {
				usage()
			}
		} else {
			args, specs = splitMounts(args)
			args, mode = splitBrokerMode(args)
			args, plainImage = splitPlainImage(args)
			dir := cmd == "editor" || cmd == "open"
			if len(args) != 1 && (!dir || len(args) != 2) {
				usage()
			}
			if (len(specs) > 0 || mode != "" || plainImage) && cmd != "up" && cmd != "open" {
				usage()
			}
		}
		if mode == "" && (cmd == "up" || cmd == "open" || cmd == "agent") && track.BrokerMode != modeServer {
			setRuntimePaths()
			if ok, err := track.Backend.Exists(context.Background(), args[0]); err == nil && !ok {
				mode = track.BrokerMode // a new sandbox: the track's default
			}
		}
		if mode != "" {
			setRuntimePaths()
			saveBrokerMode(args[0], filepath.Join(stateRoot(), args[0]), mode)
		}
		if len(specs) > 0 {
			setRuntimePaths()
			saveMounts(args[0], filepath.Join(stateRoot(), args[0]), specs)
		}
		sandboxCmd(cmd, args[0], args[1:])
	default:
		usage()
	}
}

// agentCmd is the command that runs the track's agent CLI.
func agentCmd() string { return track.Agent.Command() }

// commands are mono-jig's own commands, which the agent's and the
// editor's can't be.
var commands = []string{"ssh-setup", "allowlist", "setup", "login", "logout", "image", "ca", "uninstall", "cp",
	"up", "run", "down", "shell", "editor", "agent", "open", "stop", "ssh-config", "denied", "rm", "help"}

func defaultState() string {
	if d := os.Getenv("JIG_STATE"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "mono-jig")
}

func sshSetup(args []string) {
	fs := flag.NewFlagSet("ssh-setup", flag.ExitOnError)
	name := fs.String("name", "", "sandbox name")
	user := fs.String("user", "agent", "user in the sandbox")
	proxy := fs.String("proxy-command", "", "ProxyCommand that reaches the sandbox's sshd (e.g. 'docker exec -i <ctr> nc localhost 22')")
	host := fs.String("host", "", "host name or address, when there's no ProxyCommand")
	port := fs.Int("port", 22, "port, with -host")
	state := fs.String("state", defaultState(), "state directory")
	_ = fs.Parse(args)
	if *name == "" {
		fmt.Fprintln(os.Stderr, "ssh-setup: -name is required")
		os.Exit(2)
	}
	p := editor.PathsFor(filepath.Join(*state, "sandboxes", *name))
	if err := editor.GenerateKeys(p, *name); err != nil {
		fmt.Fprintln(os.Stderr, "ssh-setup:", err)
		os.Exit(1)
	}
	e := editor.HostEntry{Alias: editor.HostAlias(*name), User: *user, ProxyCommand: *proxy, HostName: *host, Port: *port, Paths: p}
	if err := editor.WriteConfig(e); err != nil {
		fmt.Fprintln(os.Stderr, "ssh-setup:", err)
		os.Exit(1)
	}
	vs, err := editor.WriteVSCodeSettings(filepath.Join(*state, "vscode"), p.Config, []string{e.Alias}, track.Agent.EditorExtensions())
	if err != nil {
		fmt.Fprintln(os.Stderr, "ssh-setup:", err)
		os.Exit(1)
	}
	fmt.Printf("config=%s\nhost=%s\nsandbox-keys=%s\nvscode-settings=%s\n", p.Config, e.Alias, p.SandboxDir, vs)
	fmt.Printf("open with: code --user-data-dir %q --extensions-dir %q --remote ssh-remote+%s /home/%s\n",
		filepath.Join(*state, "vscode"), filepath.Join(*state, "vscode-ext"), e.Alias, *user)
}

// allowlistCmd prints the hosts a new sandbox would reach: the base, the
// JIG_LAYERS files and the user's layer, less the hosts of the providers
// you're logged in to (allowed).
func allowlistCmd(args []string) {
	fs := flag.NewFlagSet("allowlist", flag.ExitOnError)
	format := fs.String("format", "list", "list (one host per line, with its layer and group) or sbx (sbx policy commands)")
	_ = fs.Parse(args)
	es, err := allowed(brokerConfig())
	if err == nil {
		switch *format {
		case "list":
			fmt.Print(allowlist.Format(es))
		case "sbx":
			fmt.Print(allowlist.FormatSbx(es))
		default:
			err = fmt.Errorf("unknown format %q", *format)
		}
	}
	if err != nil {
		fail("allowlist: %v", err)
	}
}
