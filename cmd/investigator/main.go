// Command investigator is the harness CLI. Only the pieces built so far are
// here; the full command set (setup, login, open, stop, ...) comes with the
// backends (prototype-plan.md step 7).
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
	fmt.Fprintln(os.Stderr, `usage: investigator <command> [flags]

commands:
  ssh-setup   generate a sandbox's SSH keys and write its SSH config
  allowlist   compile the allowlist layers into the filter's ACL or sbx policy commands`)
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
