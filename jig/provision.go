package jig

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevenpelley/msb-jig/guest"
	"github.com/stevenpelley/msb-jig/sandbox"
)

// provision runs provision.sh as root: all of it on the first start (a
// marker in the guest records it), and on later starts only apply, which
// writes guestConfig (the environment, git's and glab's Settings, Claude
// Code's, the agent's instructions, mcpcli), so the guest follows the
// broker's providers. openSandbox sets the same environment on the
// sandbox itself. The script and the settings are written from this
// binary each time.
func provision(ctx context.Context, sb sandbox.Guest, c config, log *slog.Logger) error {
	done, err := sb.Exists(ctx, provisioned)
	if err != nil {
		return err
	}
	settings, err := guestConfig(c).JSON()
	if err != nil {
		return err
	}
	// The binary's provision files, replacing the last start's.
	if err := sb.RemoveAll(ctx, provisionDir); err != nil {
		return err
	}
	for name, body := range provisionFiles() {
		if err := sb.WriteFile(ctx, provisionDir+"/"+name, []byte(body)); err != nil {
			return err
		}
	}
	if err := sb.WriteFile(ctx, guest.ConfigFile, settings); err != nil {
		return err
	}
	if err := sb.WriteFile(ctx, guestCAFile, []byte(strings.Join(c.cas, ""))); err != nil {
		return err
	}
	// The hosts line every start: in proxy mode it removes the names an
	// older proxy-mode sandbox had (aws.broker.internal until 2026-11-25).
	first, comps := hostsLine(c), "ca apply"
	if !done {
		comps = "ca " + firstComponents(ctx, sb, c, log)
		log.Info("provisioning the guest (first start)", "components", comps)
		if !c.proxy {
			// Server mode's bridge (supervisor.go), before the components.
			first = `export DEBIAN_FRONTEND=noninteractive
if ! command -v socat >/dev/null; then
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends socat >/dev/null
fi
` + first
		}
	}
	script := fmt.Sprintf(`set -euo pipefail
%s
export SANDBOX_USER=%s SANDBOX_UID=%d PROVISION_COMPONENTS=%q BROKER_CA_FILE=%s GUEST_CONFIG=%s GUEST_CA=%s
rm -f /root/provision.sh  # the single script before provision/
bash %[11]s/provision.sh
home=$(getent passwd %[2]s | cut -d: -f6)
if [ -d %[8]s ] && [ ! -e "$home/host" ]; then ln -s %[8]s "$home/host"; fi
mkdir -p %[9]s && touch %[10]s
`, first, guestUser, guestUID, comps, guestCA, guest.ConfigFile, guestCAFile, shareGuest, filepath.Dir(provisioned), provisioned, provisionDir)
	out, code, err := sb.Run(ctx, script)
	if err != nil {
		return err
	}
	_, _ = io.WriteString(os.Stderr, out)
	if code != 0 {
		return fmt.Errorf("provision.sh exited %d", code)
	}
	return nil
}

// hostsLine points the broker's internal names (those of the providers
// that are on, in server mode) at the bridge, replacing an older line, so
// the names follow the providers. In proxy mode there are none: it only
// removes the line.
func hostsLine(c config) string {
	line := ""
	if names := c.broker.Internal(); len(names) > 0 {
		line = `\n127.0.0.1 ` + strings.Join(names, " ")
	}
	return fmt.Sprintf(`h=$(grep -v 'broker\.internal' /etc/hosts || true); printf '%%s%s\n' "$h" >/etc/hosts`, line)
}

// components are provision.sh's, for this sandbox.
func components(c config) string {
	comps := "base git broker"
	if c.dockerGiB > 0 {
		comps += " docker"
	}
	if guestSSHD() {
		comps += " sshd"
	}
	return comps + " " + addedComponents()
}

// imageComponents are the parts of provision.sh mono-jig build
// puts in the base image: those that depend only on the image.
func imageComponents() string {
	if guestSSHD() {
		return "base sshd git docker " + addedComponents()
	}
	return "base git docker " + addedComponents()
}

// imageFile is where the base image records its parts and provision.sh.
const imageFile = "/etc/mono-jig/image"

// firstComponents are the parts of provision.sh a sandbox's first start
// runs: those its image doesn't have (all, from a plain image), and the
// settings every start writes.
func firstComponents(ctx context.Context, sb sandbox.Guest, c config, log *slog.Logger) string {
	baked, hash := map[string]bool{}, ""
	if s, err := sb.ReadFile(ctx, imageFile); err == nil {
		baked, hash = parseImageFile(s)
	}
	if hash == "" {
		log.Info("the sandbox's image has nothing of mono-jig's (JIG_PLAIN_IMAGE or JIG_IMAGE): this first start installs everything, which takes minutes",
			"image", c.image)
	}
	if want := guest.ProvisionHash(provisionFiles()); hash != "" && hash != want {
		log.Warn("the image was built from other provision files; to bring it up to date: mono-jig build, then rm and create it again",
			"image", hash, "binary", want)
	}
	var comps []string
	for _, p := range strings.Fields(components(c)) {
		if !baked[p] {
			comps = append(comps, p)
		}
	}
	return strings.Join(append(comps, "apply"), " ")
}

// parseImageFile reads imageFile: components=<parts> and provision=<hash>.
func parseImageFile(s string) (map[string]bool, string) {
	baked, hash := map[string]bool{}, ""
	for _, line := range strings.Split(s, "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "components":
			for _, p := range strings.Fields(v) {
				baked[p] = true
			}
		case "provision":
			hash = strings.TrimSpace(v)
		}
	}
	return baked, hash
}
