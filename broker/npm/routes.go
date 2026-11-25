// Package npm is the broker's npm registry provider: read-only access to
// a registry (registry.npmjs.org, or another that speaks its protocol),
// with the user's token, so private packages install while nothing can be
// published, unpublished, tagged or deprecated, and no login or token can
// be made from the sandbox.
package npm

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/stevenpelley/msb-jig/broker"
)

// Config is one registry.
type Config struct {
	URL   string       // the registry, e.g. https://registry.npmjs.org
	Token broker.Token // the user's token, sent as a Bearer token; nil: none
	// Public is the registry as the sandbox reaches it in server mode
	// (https://<name>.broker.internal): the tarball URLs in package
	// documents are rewritten to it. nil in proxy mode.
	Public *url.URL
}

// Routes is every request the registry serves: reads, npm audit's
// advisory lookup, and nothing that writes. A request matching no route
// is denied.
func Routes(up broker.Upstreams, c Config) []broker.Route {
	auth := broker.NoAuth
	if c.Token != nil {
		auth = broker.Bearer(c.Token)
	}
	reg := up.To(c.URL, auth)
	const readOnly = "the npm registry is read-only through the broker: no publish, unpublish, deprecate, dist-tag or access changes"
	const account = "the registry's account, login and token endpoints aren't served: a login from the sandbox would put a credential in it"
	var docs broker.Target = reg
	if c.Public != nil {
		docs = rewriteDocs{reg, reg.URL.Scheme + "://" + reg.URL.Host + "/", c.Public.Scheme + "://" + c.Public.Host + "/"}
	}
	return []broker.Route{
		// The registry's own endpoints, under /-/: those that read, and
		// npm audit's lookup (a POST that only reads); the rest (login,
		// users, tokens, profile, access, orgs, teams, staging, trust,
		// OIDC exchange) refused.
		{Method: "GET HEAD", Path: "/-/package/{pkg}/dist-tags", Op: "npm.read", Check: broker.Read, To: reg},
		{Method: "GET HEAD", Path: "/-/v1/search", Op: "npm.read", Check: broker.Read, To: reg},
		{Method: "GET HEAD", Path: "/-/ping", Op: "npm.read", Check: broker.Read, To: reg},
		{Method: "GET HEAD", Path: "/-/whoami", Op: "npm.read", Check: broker.Read, To: reg},
		{Method: "GET HEAD", Path: "/-/npm/v1/keys", Op: "npm.read", Check: broker.Read, To: reg},
		{Method: "POST", Path: "/-/npm/v1/security/advisories/bulk", Op: "npm.audit", Check: broker.Any, To: reg},
		{Method: "GET HEAD", Path: "/-/package/{path...}", Op: "npm.access", Check: broker.Never(readOnly)},
		{Method: "*", Path: "/-/{path...}", Op: "npm.account", Check: broker.Never(account)},

		// Package documents (/<pkg>, /@scope%2fpkg) and tarballs
		// (/<pkg>/-/<file>.tgz; GitHub Packages' /download/…).
		{Method: "GET HEAD", Path: "/{pkg}", Op: "npm.read", Check: broker.Read, To: docs},
		{Method: "GET HEAD", Path: "/{path...}", Op: "npm.read", Check: broker.Read, To: reg},
		{Method: "*", Path: "/{path...}", Op: "npm.write", Check: broker.Never(readOnly)},
	}
}

// rewriteDocs serves a package document with the registry's URLs in it
// (its tarballs') pointed at the broker, so the client fetches those
// through it too.
type rewriteDocs struct {
	reg      *broker.Upstream
	from, to string
}

func (r rewriteDocs) Serve(w http.ResponseWriter, c *broker.Call) {
	c.Rewrite(func(resp *http.Response) (io.Reader, error) {
		if !strings.Contains(resp.Header.Get("Content-Type"), "json") {
			return resp.Body, nil
		}
		return replacing(resp.Body, `"`+r.from, `"`+r.to), nil
	})
	r.reg.Serve(w, c)
}

// replacing streams src with every old replaced by new.
func replacing(src io.Reader, old, new string) io.Reader {
	return &replacer{src: src, old: []byte(old), new: []byte(new), buf: make([]byte, 64<<10)}
}

type replacer struct {
	src      io.Reader
	old, new []byte
	buf      []byte
	carry    []byte // read, not yet matched: may begin a match
	out      []byte // ready to return
	err      error  // src's, once it's done
}

func (r *replacer) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		n, err := r.src.Read(r.buf)
		data := append(r.carry, r.buf[:n]...)
		r.carry = nil
		if err != nil {
			r.out, r.err = bytes.ReplaceAll(data, r.old, r.new), err
			continue
		}
		var out []byte
		for {
			i := bytes.Index(data, r.old)
			if i < 0 {
				break
			}
			out = append(append(out, data[:i]...), r.new...)
			data = data[i+len(r.old):]
		}
		// No match is left in data: one that continues in the next read
		// starts in its last len(old)-1 bytes.
		keep := min(len(r.old)-1, len(data))
		r.out = append(out, data[:len(data)-keep]...)
		r.carry = append([]byte(nil), data[len(data)-keep:]...)
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}
