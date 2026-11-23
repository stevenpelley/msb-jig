// Package editor is how an editor reaches a sandbox: per-sandbox SSH keys,
// the pinned host key and the CLI's own SSH config file (for sshd-based
// sandboxes: sbx), and the settings of the harness's own VS Code.
package editor

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"golang.org/x/crypto/ssh"
)

// SSHPaths are the files the CLI keeps for one sandbox, under
// <state>/sandboxes/<name>/ssh/.
type SSHPaths struct {
	Dir         string // the ssh directory itself (0700)
	ClientKey   string // id_ed25519: the per-sandbox client key
	KnownHosts  string // known_hosts: the pinned host key, nothing else
	Config      string // config: the file VS Code reads through remote.SSH.configFile
	SandboxDir  string // sandbox/: what install-keys.sh receives
	HostKeyPriv string // sandbox/ssh_host_ed25519_key
	AuthKeys    string // sandbox/authorized_keys
}

// PathsFor returns the SSH paths for a sandbox's state directory.
func PathsFor(sandboxState string) SSHPaths {
	d := filepath.Join(sandboxState, "ssh")
	return SSHPaths{
		Dir:         d,
		ClientKey:   filepath.Join(d, "id_ed25519"),
		KnownHosts:  filepath.Join(d, "known_hosts"),
		Config:      filepath.Join(d, "config"),
		SandboxDir:  filepath.Join(d, "sandbox"),
		HostKeyPriv: filepath.Join(d, "sandbox", "ssh_host_ed25519_key"),
		AuthKeys:    filepath.Join(d, "sandbox", "authorized_keys"),
	}
}

// HostAlias is the SSH host name the editor opens: ssh-remote+<alias>.
func HostAlias(sandbox string) string { return "investigator-" + sandbox }

// GenerateKeys creates the client keypair and the host keypair for a
// sandbox, unless they already exist. The host's public key is pinned in
// known_hosts under the host alias, so the editor's fingerprint check passes
// without a trust-on-first-use prompt.
func GenerateKeys(p SSHPaths, sandbox string) error {
	for _, d := range []string{p.Dir, p.SandboxDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	if _, err := os.Stat(p.ClientKey); err == nil {
		return nil // already generated; keep the pinned key stable
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	clientPub, err := writeKey(p.ClientKey, "investigator-client-"+sandbox)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p.ClientKey+".pub", ssh.MarshalAuthorizedKey(clientPub), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(p.AuthKeys, ssh.MarshalAuthorizedKey(clientPub), 0o600); err != nil {
		return err
	}

	hostPub, err := writeKey(p.HostKeyPriv, "investigator-host-"+sandbox)
	if err != nil {
		return err
	}
	line := HostAlias(sandbox) + " " + string(ssh.MarshalAuthorizedKey(hostPub))
	return os.WriteFile(p.KnownHosts, []byte(line), 0o600)
}

func writeKey(path, comment string) (ssh.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, err
	}
	return ssh.NewPublicKey(pub)
}

// HostEntry is one Host block in the CLI's SSH config.
type HostEntry struct {
	Alias        string
	User         string
	ProxyCommand string // how SSH reaches the sandbox (backend-specific)
	HostName     string // used only when ProxyCommand is empty
	Port         int
	Paths        SSHPaths
}

var configTmpl = template.Must(template.New("cfg").Parse(`# Written by investigator. Don't edit: regenerated on every open.
# Read by VS Code through remote.SSH.configFile; ~/.ssh/config is untouched.
Host {{.Alias}}
  User {{.User}}
{{- if .ProxyCommand}}
  HostName {{.Alias}}
  ProxyCommand {{.ProxyCommand}}
{{- else}}
  HostName {{.HostName}}
  Port {{.Port}}
{{- end}}
  HostKeyAlias {{.Alias}}
  IdentityFile "{{.Paths.ClientKey}}"
  IdentitiesOnly yes
  IdentityAgent none
  UserKnownHostsFile "{{.Paths.KnownHosts}}"
  GlobalKnownHostsFile /dev/null
  StrictHostKeyChecking yes
  UpdateHostKeys no
  PasswordAuthentication no
  KbdInteractiveAuthentication no
  ForwardAgent no
  ForwardX11 no
  ForwardX11Trusted no
  PermitLocalCommand no
  ServerAliveInterval 15
`))

// WriteConfig writes the SSH config file for one sandbox.
func WriteConfig(e HostEntry) error {
	if e.ProxyCommand == "" && e.HostName == "" {
		return errors.New("ssh config: need a ProxyCommand or a HostName")
	}
	if e.ProxyCommand != "" && strings.ContainsAny(e.ProxyCommand, "\n\r") {
		return errors.New("ssh config: ProxyCommand must be one line")
	}
	if e.Port == 0 {
		e.Port = 22
	}
	var b strings.Builder
	if err := configTmpl.Execute(&b, e); err != nil {
		return err
	}
	if err := os.WriteFile(e.Paths.Config, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("ssh config: %w", err)
	}
	return nil
}
