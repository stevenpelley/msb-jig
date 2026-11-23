// Command investigator is the harness CLI. A sandbox is a microsandbox VM
// owned by a supervisor process that also serves the broker
// (microsandbox.md); the commands taking a <name> manage one.
//
//	investigator setup              check the Mac, write the harness's VS Code profile, pull the image
//	investigator login [provider]   record GitLab, Claude and AWS for the broker (login.go); alone: what's recorded
//	investigator logout [provider]  forget them, and remove their Keychain items
//	investigator open <name> [dir]  start the sandbox if needed (creating it the first time), then VS Code on dir
//	investigator stop <name>        stop it
//	investigator uninstall          remove every sandbox, the state directory and the Keychain items
//
// And beneath them, for the checks:
//
//	investigator up <name>          start the supervisor in the background (creates and provisions the sandbox once)
//	investigator run <name>         the supervisor, in the foreground
//	investigator down <name>        stop the supervisor, and with it the sandbox
//	investigator shell <name>       a login shell as the agent user
//	investigator code <name> [dir]  VS Code on the sandbox, with the harness's settings
//	investigator ssh-config <name>  write and print the sandbox's OpenSSH Host block
//	investigator denied <name>      the hosts its egress policy refused (denials.jsonl)
//	investigator rm <name>          down, then remove the sandbox and its disks (the state directory stays)
//	investigator allowlist          compile the allowlist layers
//	investigator ssh-setup          SSH keys and config for an sshd-based sandbox (sbx, checks/step1-docker.sh)
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mcp-restrict/cli"
	"mcp-restrict/filter"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: investigator <command> ...

  setup                   check what's needed, set up the harness's VS Code, pull the image
  login                   show what the broker uses for GitLab, Claude and AWS
  login gitlab <user> [--url https://host] [--keychain]
                          act for <user>, with your glab login's token, or one you paste
  login claude            store a token from claude setup-token
  login aws <profile>...  use these AWS profiles (signing in with SSO if needed)
  logout [gitlab|claude|aws]
                          forget them (all, if none named)
  open <name> [dir]       start the sandbox (creating it the first time) and open VS Code
                          on dir (relative to the sandbox user's home; default: home)
  stop <name>             stop it
  denied <name>           hosts the sandbox was refused, most recent first
  uninstall [--yes]       remove every sandbox, its state and the Keychain items

sandboxes, beneath open and stop:
  up <name>          start the sandbox and its broker in the background (creates it the first time)
  run <name>         the same, in the foreground
  down <name>        stop it
  shell <name>       a login shell as the agent user
  code <name> [dir]  open VS Code on it
  ssh-config <name>  write and print its OpenSSH Host block
  rm <name>          stop and remove it, with its disks

other:
  allowlist [flags]  compile the allowlist layers into the filter's ACL or sbx policy commands
  ssh-setup [flags]  SSH keys and config for an sshd-based sandbox (sbx, checks/step1-docker.sh)`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "ssh-setup":
		sshSetup(os.Args[2:])
	case "allowlist":
		allowlist(os.Args[2:])
	case "setup":
		if len(os.Args) != 2 {
			usage()
		}
		setup()
	case "login":
		login(os.Args[2:])
	case "logout":
		logout(os.Args[2:])
	case "uninstall":
		uninstall(os.Args[2:])
	case "up", "run", "down", "shell", "code", "open", "stop", "ssh-config", "denied", "rm":
		dir := os.Args[1] == "code" || os.Args[1] == "open"
		if len(os.Args) != 3 && (!dir || len(os.Args) != 4) {
			usage()
		}
		sandboxCmd(os.Args[1], os.Args[2], os.Args[3:])
	default:
		usage()
	}
}

func defaultState() string {
	if d := os.Getenv("INVESTIGATOR_STATE"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "investigator")
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
	p := cli.PathsFor(filepath.Join(*state, "sandboxes", *name))
	if err := cli.GenerateKeys(p, *name); err != nil {
		fmt.Fprintln(os.Stderr, "ssh-setup:", err)
		os.Exit(1)
	}
	e := cli.HostEntry{Alias: cli.HostAlias(*name), User: *user, ProxyCommand: *proxy, HostName: *host, Port: *port, Paths: p}
	if err := cli.WriteConfig(e); err != nil {
		fmt.Fprintln(os.Stderr, "ssh-setup:", err)
		os.Exit(1)
	}
	vs, err := cli.WriteVSCodeSettings(filepath.Join(*state, "vscode"), p.Config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ssh-setup:", err)
		os.Exit(1)
	}
	fmt.Printf("config=%s\nhost=%s\nsandbox-keys=%s\nvscode-settings=%s\n", p.Config, e.Alias, p.SandboxDir, vs)
	fmt.Printf("open with: code --user-data-dir %q --extensions-dir %q --remote ssh-remote+%s /home/%s\n",
		filepath.Join(*state, "vscode"), filepath.Join(*state, "vscode-ext"), e.Alias, *user)
}

// layerFlags collects repeated -layer flags.
type layerFlags []string

func (l *layerFlags) String() string     { return strings.Join(*l, ",") }
func (l *layerFlags) Set(v string) error { *l = append(*l, v); return nil }

func allowlist(args []string) {
	fs := flag.NewFlagSet("allowlist", flag.ExitOnError)
	profile := fs.String("profile", "investigator", "profile layer: "+strings.Join(filter.Profiles, " or "))
	format := fs.String("format", "acl", "acl (the filter's ACL file) or sbx (sbx policy commands)")
	var layers layerFlags
	fs.Var(&layers, "layer", "an org or user layer file, in order (repeatable)")
	_ = fs.Parse(args)
	ls, err := filter.Layers(*profile, layers...)
	if err == nil {
		var es []filter.Entry
		if es, err = filter.Compile(ls); err == nil {
			switch *format {
			case "acl":
				fmt.Print(filter.FormatACL(es))
			case "sbx":
				fmt.Print(filter.FormatSbx(es))
			default:
				err = fmt.Errorf("unknown format %q", *format)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "allowlist:", err)
		os.Exit(1)
	}
}
