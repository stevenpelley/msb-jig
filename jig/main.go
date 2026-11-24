// Package jig is the harness CLI, as a library: a track (Track: its
// providers, agent, editor, VM runtime and base image) calls Main from its
// own command, as the personal track's mono-jig does with Personal
// (docs/library-split.md).
// A sandbox is a microsandbox VM owned by a supervisor process that also
// serves the broker (docs/microsandbox.md); the commands taking a <name>
// manage one.
//
//	mono-jig setup              check the Mac, write the harness's VS Code profile, build the base image
//	mono-jig login [provider]   record GitLab, GitHub, Claude and AWS for the broker (login.go); alone: what's recorded
//	mono-jig logout [provider]  forget them, and remove their Keychain items
//	mono-jig image build        your own sandbox image, from a Dockerfile (image.go)
//	mono-jig open <name> [dir]  start the sandbox if needed (creating it the first time), then VS Code on dir
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
//	mono-jig code <name> [dir]  VS Code on the sandbox, with the harness's Settings
//	mono-jig ssh-config <name>  write and print the sandbox's OpenSSH Host block
//	mono-jig denied <name>      the hosts its egress policy refused (denials.jsonl)
//	mono-jig rm <name>          down, then remove the sandbox and its disks (the state directory stays)
//	mono-jig allowlist          the hosts a new sandbox may reach
//	mono-jig ssh-setup          SSH keys and config for an sshd-based sandbox (sbx, checks/step1-docker.sh)
package jig

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stevenpelley/msb-jig/allowlist"
	"github.com/stevenpelley/msb-jig/editor"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: mono-jig <command> ...

  setup                   check what's needed, set up the harness's VS Code, build the base image
  login                   show what the broker uses for GitLab, GitHub, Claude and AWS
  login all               ask about each provider in turn and log in with the defaults
  login gitlab [<user>] [--url https://host] [--keychain]
                          act for the user of your glab login's token, or one you paste
                          (given <user>, check that it's theirs)
  login github [<user>] [--host <host>] [--keychain]
                          the same on github.com (or <host>, with its API at
                          api.<host>), with your gh login's token
  login claude            store a token from claude setup-token
  login aws [<profile>...]
                          use these AWS profiles (signing in with SSO if needed); none: choose from a list
  logout [gitlab|github|claude|aws]
                          forget them (all, if none named)
  ca [<pem-file>...|--clear]
                          extra CA certificates the sandboxes trust, such as a
                          TLS-inspecting proxy's root; none given: show them
  open <name> [dir]       start the sandbox (creating it the first time) and open VS Code
                          on dir (relative to the sandbox user's home; default: home)
  open <name> [dir] --mount host[:guest][:rw] ...
                          when creating it, also mount these Mac folders (guest path under
                          /mnt; default /mnt/<folder name>). Every sandbox has
                          ~/.mono-jig/<name>/share at /mnt/host (and ~/host)
  open <name> [dir] --broker server|proxy
                          when creating it, how the guest reaches the broker: server (the
                          default; broker.internal names) or proxy (microsandbox's outbound
                          proxy; gitlab.com, github.com, api.anthropic.com by their own names)
  cp <src> <dst>          copy between the Mac and a sandbox; a sandbox path is
                          <name>:<path>, relative to the agent's home unless absolute
  stop <name>             stop it
  denied <name>           hosts the sandbox was refused, most recent first
  image init [file]       write a starter Dockerfile (default ./Dockerfile)
  image build [-f Dockerfile [context]] [-engine docker|podman]
                          build the base image (git, Docker, Claude Code...), then your
                          Dockerfile on it; new sandboxes start from the result
  image show|reset        the image new sandboxes start from; back to plain Ubuntu
  uninstall [--yes]       remove every sandbox, its state and the Keychain items

sandboxes, beneath open and stop:
  up <name>          start the sandbox and its broker in the background (creates it the first time;
                     --mount and --broker as for open)
  run <name>         the same, in the foreground
  down <name>        stop it
  shell <name>       a login shell as the agent user
  code <name> [dir]  open VS Code on it
  ssh-config <name>  write and print its OpenSSH Host block
  rm <name>          stop and remove it, with its disks

other:
  allowlist [-format list|sbx]  the hosts a new sandbox may reach (base, JIG_LAYERS, ~/.mono-jig/allowlist.yaml)
  ssh-setup [flags]  SSH keys and config for an sshd-based sandbox (sbx, checks/step1-docker.sh)`)
	os.Exit(2)
}

// Main runs the command line for track t.
func Main(t Track) {
	track = t
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
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
	case "up", "run", "down", "shell", "code", "open", "stop", "ssh-config", "denied", "rm":
		cmd := os.Args[1]
		args, specs := splitMounts(os.Args[2:])
		args, mode := splitBrokerMode(args)
		dir := cmd == "code" || cmd == "open"
		if len(args) != 1 && (!dir || len(args) != 2) {
			usage()
		}
		if (len(specs) > 0 || mode != "") && cmd != "up" && cmd != "open" {
			usage()
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
