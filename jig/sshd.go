package jig

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/stevenpelley/msb-jig/sandbox"
)

// The guest's OpenSSH sshd, for an editor that needs it (guestSSHD: Kiro,
// whose Remote SSH reaches its server through a Unix-socket forward, which
// msb ssh serve refuses). The sshd component installs it in the image
// without host keys; at every start sshdSetup gives the sandbox its own
// host key (made the first time), authorizes our key and pins the host
// key on the Mac; the supervisor's sshd job runs it.

// sshdConf is the microsandbox sandbox's drop-in, read before the sshd
// component's 10-mono-jig.conf (the first value sshd reads wins): only
// loopback (the editor reaches it by ssh -W through msb's SSH), and
// Unix-socket forwards toward the guest, which Kiro needs. Remote ones
// stay off: AllowStreamLocalForwarding local, like AllowTcpForwarding.
const sshdConf = `# Written by mono-jig's supervisor (jig/sshd.go).
ListenAddress 127.0.0.1
AllowStreamLocalForwarding local
`

// sshdJob runs sshd in the foreground as a supervisor job. A sandbox
// provisioned before sshd was added has none, nor a host key: then it
// sleeps (rm and create it again to add it).
const sshdJob = `if ! command -v sshd >/dev/null || [ ! -s /etc/ssh/ssh_host_ed25519_key ]; then exec sleep infinity; fi
install -d -m 0755 /run/sshd
exec "$(command -v sshd)" -D -e >>/var/log/sshd.log 2>&1`

var pubKeyRE = regexp.MustCompile(`^ssh-ed25519 [A-Za-z0-9+/]+={0,2}$`)

// authorizedKey is an OpenSSH public key's type and key, without its
// comment, checked so it can go into a shell script as it is.
func authorizedKey(pub string) (string, error) {
	f := strings.Fields(pub)
	if len(f) < 2 {
		return "", fmt.Errorf("not an OpenSSH public key")
	}
	k := f[0] + " " + f[1]
	if !pubKeyRE.MatchString(k) {
		return "", fmt.Errorf("not an ed25519 public key")
	}
	return k, nil
}

// sshdScript makes the host key the first time, writes sshdConf, installs
// the authorized key for the guest user and checks the configuration. It
// exits 3 if the guest has no sshd.
func sshdScript(authorized string) string {
	return fmt.Sprintf(`set -eu
command -v sshd >/dev/null || exit 3
[ -s /etc/ssh/ssh_host_ed25519_key ] || ssh-keygen -q -t ed25519 -N '' -C '' -f /etc/ssh/ssh_host_ed25519_key
install -d -m 0755 /etc/ssh/sshd_config.d /run/sshd
cat >/etc/ssh/sshd_config.d/05-mono-jig-microsandbox.conf <<'EOF'
%sEOF
home=$(getent passwd %[2]s | cut -d: -f6)
install -d -m 0700 -o %[2]s -g %[2]s "$home/.ssh"
printf '%%s\n' '%[3]s' >"$home/.ssh/authorized_keys"
chown %[2]s:%[2]s "$home/.ssh/authorized_keys"
chmod 0600 "$home/.ssh/authorized_keys"
sshd -t
`, sshdConf, guestUser, authorized)
}

// knownHostsLine pins the guest sshd's host key under the editor's host
// alias.
func knownHostsLine(alias, hostPub string) (string, error) {
	k, err := authorizedKey(hostPub)
	if err != nil {
		return "", fmt.Errorf("the guest's host key: %w", err)
	}
	return alias + " " + k + "\n", nil
}

// sshdSetup prepares the guest's sshd at a start, and pins its host key
// in <state>/sshd_known_hosts. A guest without sshd (provisioned before it
// was added) is reported, not an error.
func sshdSetup(ctx context.Context, sb sandbox.Guest, name, state string) error {
	pub, err := os.ReadFile(sandboxKey(name, state) + ".pub")
	if err != nil {
		return err
	}
	k, err := authorizedKey(string(pub))
	if err != nil {
		return fmt.Errorf("%s.pub: %w", sshFilesFor(state).key, err)
	}
	out, code, err := sb.Run(ctx, sshdScript(k))
	switch {
	case err != nil:
		return err
	case code == 3:
		return fmt.Errorf("the guest has no sshd, which the editor needs: it was provisioned before sshd was added; rm and create it again")
	case code != 0:
		return fmt.Errorf("setting up sshd (exit %d): %s", code, out)
	}
	hostPub, err := sb.ReadFile(ctx, "/etc/ssh/ssh_host_ed25519_key.pub")
	if err != nil {
		return err
	}
	line, err := knownHostsLine(sshHost(name), hostPub)
	if err != nil {
		return err
	}
	return os.WriteFile(sshFilesFor(state).sshdKnown, []byte(line), 0o600)
}
