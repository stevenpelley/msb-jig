package providers

import (
	"context"
	"net/http"
	"strings"

	"github.com/stevenpelley/msb-jig/broker/npm"
	"github.com/stevenpelley/msb-jig/guest"
)

// NPM: npm registries, read-only, each with the user's token (or none).
// In server mode each is served as <host with dashes>.npm.broker.internal
// (registry-npmjs-org.npm.broker.internal), with the guest's npm pointed
// there; in proxy mode under its real host, with nothing to configure.
//
// Server mode has a gap proxy mode doesn't: a project's .npmrc that names
// a registry by its real URL for a scope (@scope:registry=https://registry.npmjs.org/)
// overrides the guest's settings and reaches for a host that's off the
// allowlist. The default registry is set in the environment, which beats
// a project's .npmrc.
type NPM struct {
	Registries []NPMRegistry // the first without Scopes is the default
}

// NPMRegistry is one registry the broker serves.
type NPMRegistry struct {
	Host     string   // registry.npmjs.org; the registry is https://<Host>
	Scopes   []string // @scope: the scopes it serves; none: the default registry
	TokenCmd string   // prints the user's token; empty: none sent
	URL      string   // tests: the registry, if not https://<Host>
}

// NPMJSHost is the public npm registry.
const NPMJSHost = "registry.npmjs.org"

func (NPM) Name() string { return "npm" }

// internal is a registry's name in server mode.
func (r NPMRegistry) internal() string {
	return strings.ReplaceAll(r.Host, ".", "-") + ".npm" + InternalSuffix
}

func (n NPM) Hosts(proxy bool) []string {
	var out []string
	for _, r := range n.Registries {
		if proxy {
			out = append(out, r.Host)
		} else {
			out = append(out, r.internal())
		}
	}
	return out
}

func (n NPM) Handlers(_ context.Context, b *Builder) (map[string]http.Handler, error) {
	out := map[string]http.Handler{}
	for _, r := range n.Registries {
		host := r.internal()
		if b.Proxy {
			host = r.Host
		}
		up := b.Upstreams(r.internal(), "npm")
		out[host] = b.Provider("npm", npm.Routes(up, npm.Config{URL: or(r.URL, "https://"+r.Host),
			Token: OptToken(r.TokenCmd), Public: up.Public}))
	}
	return out, nil
}

// NPMRCFile is the guest's global npmrc in server mode.
const NPMRCFile = "/etc/mono-jig/npmrc"

// Guest: in server mode, npm's default registry in the environment and
// the scopes' in a global npmrc (pnpm's use of it is unverified); in
// proxy mode, nothing but the instructions.
func (n NPM) Guest(proxy bool) guest.Config {
	var hosts []string
	for _, r := range n.Registries {
		hosts = append(hosts, r.Host)
	}
	c := guest.Config{Instructions: []string{guest.Instructions("npm", map[string]string{"Hosts": strings.Join(hosts, ", ")})}}
	if proxy {
		return c
	}
	const marker = "# mono-jig: npm registries through the broker (rewritten at every start)"
	rc := marker + "\n"
	c.Env = map[string]string{}
	for _, r := range n.Registries {
		u := "https://" + r.internal() + "/"
		if len(r.Scopes) == 0 {
			if _, set := c.Env["NPM_CONFIG_REGISTRY"]; !set {
				c.Env["NPM_CONFIG_REGISTRY"] = u
				rc += "registry=" + u + "\n"
			}
		}
		for _, s := range r.Scopes {
			rc += s + ":registry=" + u + "\n"
		}
	}
	c.Env["NPM_CONFIG_GLOBALCONFIG"] = NPMRCFile
	c.Files = []guest.File{{Path: NPMRCFile, Marker: marker, Content: rc}}
	return c
}
