package broker

import (
	"net"
	"net/http"
	"strings"
)

// Server routes each request to the provider for its hostname, after the
// checks every provider shares.
type Server struct {
	// Providers by hostname, e.g. "gitlab.broker.internal".
	Providers map[string]http.Handler
}

// credentialHeaders are dropped from every incoming request, and again
// from every outgoing one: the sandbox holds only placeholders, and an
// Upstream's Auth sets the real credential.
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "Private-Token", "Job-Token", "Cookie", "X-Jig-Session", "X-Api-Key"}

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
	for _, h := range credentialHeaders {
		r.Header.Del(h)
	}
	p.ServeHTTP(w, r)
}
