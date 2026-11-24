package broker

import (
	"net"
	"net/http"
	"strings"
)

// Server routes each request to the provider for its hostname, after the
// checks every provider shares: no absolute-form target, the TLS name
// matching the Host, no dot segments in the path (request.go), and no
// credential or method override from the sandbox.
type Server struct {
	// Providers by hostname, e.g. "gitlab.broker.internal".
	Providers map[string]http.Handler
}

// credentialHeaders are dropped from every incoming request, and again
// from every outgoing one: the sandbox holds only placeholders, and an
// Upstream's Auth sets the real credential.
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "Private-Token", "Job-Token", "Cookie", "X-Jig-Session", "X-Api-Key"}

// overrideHeaders would let a request routed as one method run upstream
// as another (Rack's MethodOverride and the like): dropped.
var overrideHeaders = []string{"X-Http-Method-Override", "X-Http-Method", "X-Method-Override"}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Absolute-form request targets would let a client name another host.
	if !strings.HasPrefix(r.RequestURI, "/") {
		HTTPError(w, http.StatusBadRequest, "absolute-form request URIs are not accepted")
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	// One listener serves several hostnames: the TLS name and the Host
	// header must agree.
	if r.TLS != nil && !strings.EqualFold(r.TLS.ServerName, host) {
		HTTPError(w, http.StatusMisdirectedRequest, "Host doesn't match the TLS server name")
		return
	}
	p, ok := s.Providers[host]
	if !ok {
		HTTPError(w, http.StatusNotFound, "no provider for host "+host)
		return
	}
	if why := badPath(r.URL.EscapedPath()); why != "" {
		HTTPError(w, http.StatusBadRequest, why)
		return
	}
	for _, h := range append(credentialHeaders, overrideHeaders...) {
		r.Header.Del(h)
	}
	p.ServeHTTP(w, r)
}

// Alias serves a real hostname, in proxy mode, with a provider whose
// routes were written for its broker.internal name: Path maps the
// request's path to the one those routes expect, so the provider sees
// exactly the requests it sees in server mode. api.github.com's
// /repos/... becomes /api/v3/repos/..., as gh sends it to a GitHub
// Enterprise Server. Server's checks run before, and the path checks
// again after.
type Alias struct {
	To   http.Handler
	Path func(path string) string
}

func (a Alias) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.URL.Path = a.Path(r.URL.Path)
	if r.URL.RawPath != "" {
		r.URL.RawPath = a.Path(r.URL.RawPath)
	}
	if why := badPath(r.URL.EscapedPath()); why != "" {
		HTTPError(w, http.StatusBadRequest, why)
		return
	}
	a.To.ServeHTTP(w, r)
}
