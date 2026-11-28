// Command ssh-setup writes a sandbox's SSH keys and config, and the VS
// Code settings, for checks/step1-docker.sh, which stands a container in
// for the sandbox: the editor package's half of mono-jig's SSH, without a
// VM. It was mono-jig ssh-setup until the CLI kept to the commands a user
// needs (2026-11-28).
//
//	go run ./checks/ssh-setup -name <name> -state <dir> -proxy-command '<cmd>'
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stevenpelley/msb-jig/agent"
	"github.com/stevenpelley/msb-jig/editor"
)

func main() {
	name := flag.String("name", "", "sandbox name")
	user := flag.String("user", "agent", "user in the sandbox")
	proxy := flag.String("proxy-command", "", "ProxyCommand that reaches the sandbox's sshd (e.g. 'docker exec -i <ctr> nc localhost 22')")
	host := flag.String("host", "", "host name or address, when there's no ProxyCommand")
	port := flag.Int("port", 22, "port, with -host")
	state := flag.String("state", "", "state directory")
	flag.Parse()
	if *name == "" || *state == "" {
		fmt.Fprintln(os.Stderr, "ssh-setup: -name and -state are required")
		os.Exit(2)
	}
	p := editor.PathsFor(filepath.Join(*state, "sandboxes", *name))
	if err := editor.GenerateKeys(p, *name); err != nil {
		fail(err)
	}
	e := editor.HostEntry{Alias: editor.HostAlias(*name), User: *user, ProxyCommand: *proxy, HostName: *host, Port: *port, Paths: p}
	if err := editor.WriteConfig(e); err != nil {
		fail(err)
	}
	vs, err := editor.WriteVSCodeSettings(filepath.Join(*state, "vscode"), p.Config, []string{e.Alias}, agent.ClaudeCode{}.EditorExtensions())
	if err != nil {
		fail(err)
	}
	fmt.Printf("config=%s\nhost=%s\nsandbox-keys=%s\nvscode-settings=%s\n", p.Config, e.Alias, p.SandboxDir, vs)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "ssh-setup:", err)
	os.Exit(1)
}
