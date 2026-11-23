// Package broker is the credential broker's core. The sandbox reaches each
// managed provider (GitLab, GitHub, Anthropic, AWS) on its own hostname
// under broker.internal; the broker holds the user's credentials and
// decides every request before forwarding it.
//
// Reading the policy: start at broker/providers, which lists the
// providers. Each provider's package has a routes.go with its route table:
// every request it serves, the check run on it, and where it goes with
// which credential. A request that matches no route is denied. The checks
// the providers share (branch namespace, title tag) are in policy.go.
//
// A request's way through: Server (server.go) picks the provider by
// hostname and drops any credential the sandbox sent; Provider.ServeHTTP
// (here) finds the first matching route, runs its check and logs the
// decision; the route's target (upstream.go for HTTP providers) injects
// the credential and forwards.
package broker

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// MaxBody is the most a check reads of a request body.
const MaxBody = 1 << 20

// Decision is a check's answer, logged with the request.
type Decision struct {
	Allow  bool
	Reason string
}

// Check decides one request. It may read the body (Call.ReadBody), look
// things up upstream, and set fields the log shows (Call.Note).
type Check func(c *Call) Decision

// Target serves an allowed request: an Upstream, or a provider's own.
type Target interface {
	Serve(w http.ResponseWriter, c *Call)
}

// Route is one kind of request a provider serves.
type Route struct {
	// Method is one method, several separated by spaces ("GET HEAD"), or
	// "*" for any.
	Method string
	// Path is matched against the request's escaped path, segment by
	// segment: a literal segment matches itself; {name} matches one
	// non-empty segment; {name...} (at most one per path) matches one or
	// more. Captured values are unescaped (Call.Var).
	Path string
	// Op names the request in the decision log.
	Op    string
	Check Check
	// To serves the request once allowed.
	To Target
	// Rewrite, if set, replaces a 200 response's body (an Upstream applies it).
	Rewrite func(*http.Response) (io.Reader, error)
}

// Provider serves one hostname from its route table.
type Provider struct {
	Name   string
	Routes []Route
	Log    *slog.Logger
}

// Call is one request on its way through a provider.
type Call struct {
	Provider string
	Op       string
	R        *http.Request
	// Body is what is forwarded. A check that reads or rewrites the body
	// replaces it.
	Body    io.Reader
	vars    map[string]string
	notes   []any
	rewrite func(*http.Response) (io.Reader, error)
	denied  func(w http.ResponseWriter, reason string)
}

// Var returns a value captured from the path.
func (c *Call) Var(name string) string { return c.vars[name] }

// Note adds key-value pairs to the decision log (repo, refs, tool…).
func (c *Call) Note(kv ...any) { c.notes = append(c.notes, kv...) }

// ReadBody reads the body (up to MaxBody) for a check, and leaves Body
// reading the same bytes for the forward.
func (c *Call) ReadBody() ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(c.Body, MaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBody {
		return nil, errTooLarge
	}
	c.Body = bytes.NewReader(b)
	return b, nil
}

type bodyError string

func (e bodyError) Error() string { return string(e) }

const errTooLarge = bodyError("request body too large")

// DenyWith sets how a denial is written, for protocols whose clients read
// errors in their own form (git's push status, JSON-RPC). The default is
// a 403 with a JSON message.
func (c *Call) DenyWith(f func(w http.ResponseWriter, reason string)) { c.denied = f }

// Rewrite sets a response rewrite from a check, for one that depends on
// the request.
func (c *Call) Rewrite(f func(*http.Response) (io.Reader, error)) { c.rewrite = f }

func (p *Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := &Call{Provider: p.Name, R: r, Body: r.Body}
	if r.Body == nil {
		c.Body = http.NoBody
	}
	rt, vars, ok := p.match(r)
	var d Decision
	switch {
	case !ok:
		c.Op = "unlisted"
		d = Deny("%s %s isn't a request the %s provider serves", r.Method, r.URL.EscapedPath(), p.Name)
	case vars == nil:
		c.Op = rt.Op
		d = Deny("bad path encoding")
	default:
		c.Op, c.vars, c.rewrite = rt.Op, vars, rt.Rewrite
		d = rt.Check(c)
	}
	p.log(c, d)
	if !d.Allow {
		if c.denied != nil {
			c.denied(w, d.Reason)
		} else {
			HTTPError(w, http.StatusForbidden, d.Reason)
		}
		return
	}
	rt.To.Serve(w, c)
}

// match returns the first route for r. vars is nil if the path matched
// but a segment didn't unescape.
func (p *Provider) match(r *http.Request) (*Route, map[string]string, bool) {
	segs := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	for i := range p.Routes {
		rt := &p.Routes[i]
		if !methodMatches(rt.Method, r.Method) {
			continue
		}
		raw, ok := matchPath(strings.Split(strings.TrimPrefix(rt.Path, "/"), "/"), segs)
		if !ok {
			continue
		}
		vars := map[string]string{}
		for k, v := range raw {
			u, err := url.PathUnescape(v)
			if err != nil {
				return rt, nil, true
			}
			vars[k] = u
		}
		return rt, vars, true
	}
	return nil, nil, false
}

func methodMatches(spec, m string) bool {
	if spec == "*" {
		return true
	}
	for _, s := range strings.Fields(spec) {
		if s == m {
			return true
		}
	}
	return false
}

// matchPath matches pattern segments against escaped path segments. A
// {name...} segment takes whatever lies between the segments before and
// after it (at least one), joined by "/".
func matchPath(pat, segs []string) (map[string]string, bool) {
	rest := -1
	for i, p := range pat {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "...}") {
			rest = i
		}
	}
	vars := map[string]string{}
	one := func(p, s string) bool {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			if s == "" {
				return false
			}
			vars[p[1:len(p)-1]] = s
			return true
		}
		return p == s
	}
	if rest < 0 {
		if len(pat) != len(segs) {
			return nil, false
		}
		for i := range pat {
			if !one(pat[i], segs[i]) {
				return nil, false
			}
		}
		return vars, true
	}
	after := len(pat) - rest - 1
	if len(segs) < len(pat) {
		return nil, false
	}
	for i := 0; i < rest; i++ {
		if !one(pat[i], segs[i]) {
			return nil, false
		}
	}
	for i := 0; i < after; i++ {
		if !one(pat[rest+1+i], segs[len(segs)-after+i]) {
			return nil, false
		}
	}
	mid := segs[rest : len(segs)-after]
	for _, s := range mid {
		if s == "" {
			return nil, false
		}
	}
	vars[strings.TrimSuffix(pat[rest][1:], "...}")] = strings.Join(mid, "/")
	return vars, true
}

func (p *Provider) log(c *Call, d Decision) {
	if p.Log == nil {
		return
	}
	args := []any{"provider", c.Provider, "op", c.Op, "method", c.R.Method, "path", c.R.URL.EscapedPath()}
	args = append(args, c.notes...)
	args = append(args, "allow", d.Allow, "reason", d.Reason)
	p.Log.Info("decision", args...)
}

// Answer is a Target that answers itself with a status and no body, for
// requests that need no upstream (an MCP session's DELETE).
type Answer int

func (a Answer) Serve(w http.ResponseWriter, _ *Call) { w.WriteHeader(int(a)) }
