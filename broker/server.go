package broker

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Server routes each request to the provider for its hostname, after the
// checks every provider shares.
type Server struct {
	// Providers by hostname, e.g. "gitlab.broker.internal".
	Providers map[string]http.Handler
	// SessionToken, if set, must accompany every request (sbx mode, where
	// the broker listens on a Mac port). Empty on Lima, where the network
	// is the sandbox's identity.
	SessionToken string
	Log          *slog.Logger
}

// credentialHeaders are dropped from every incoming request: the sandbox
// holds only placeholders, and the adapter injects the real credential.
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "Private-Token", "Job-Token", "Cookie", "X-Investigator-Session"}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Absolute-form request targets would let a client name another host.
	if !strings.HasPrefix(r.RequestURI, "/") {
		httpError(w, http.StatusBadRequest, "absolute-form request URIs are not accepted")
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	// One port serves several hostnames: the TLS name and the Host header must agree.
	if r.TLS != nil && !strings.EqualFold(r.TLS.ServerName, host) {
		httpError(w, http.StatusMisdirectedRequest, "Host doesn't match the TLS server name")
		return
	}
	p, ok := s.Providers[host]
	if !ok {
		httpError(w, http.StatusNotFound, "no provider for host "+host)
		return
	}
	if s.SessionToken != "" && !s.sessionOK(r) {
		httpError(w, http.StatusUnauthorized, "missing or wrong session token")
		return
	}
	for _, h := range credentialHeaders {
		r.Header.Del(h)
	}
	p.ServeHTTP(w, r)
}

// sessionOK accepts the per-session token wherever the sandbox's tools put
// their placeholder: git's http.extraHeader, glab's PRIVATE-TOKEN, a Bearer
// token, or a Basic password.
func (s *Server) sessionOK(r *http.Request) bool {
	cands := []string{r.Header.Get("X-Investigator-Session"), r.Header.Get("Private-Token")}
	a := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(a, "Bearer "); ok {
		cands = append(cands, t)
	}
	if t, ok := strings.CutPrefix(a, "token "); ok {
		cands = append(cands, t)
	}
	if b, ok := strings.CutPrefix(a, "Basic "); ok {
		if raw, err := base64.StdEncoding.DecodeString(b); err == nil {
			if _, pw, ok := strings.Cut(string(raw), ":"); ok {
				cands = append(cands, pw)
			}
		}
	}
	for _, c := range cands {
		if c != "" && subtle.ConstantTimeCompare([]byte(c), []byte(s.SessionToken)) == 1 {
			return true
		}
	}
	return false
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": "investigator broker: " + msg})
}

// --- forwarding ---

// hopHeaders aren't forwarded in either direction.
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

// NewUpstreamClient returns the client every adapter uses: it never follows
// redirects (they go back to the sandbox's client, which follows them
// through the broker or the filter), and it uses the Mac's proxy settings,
// so the broker chains to an upstream proxy where there is one.
func NewUpstreamClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = http.ProxyFromEnvironment
	return &http.Client{
		Transport:     t,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// outbound builds the upstream request for r, with body as its body.
func outbound(r *http.Request, upstream *url.URL, body io.Reader) (*http.Request, error) {
	u := *upstream
	u.Path = strings.TrimSuffix(upstream.Path, "/") + r.URL.Path
	u.RawPath = ""
	if r.URL.RawPath != "" {
		u.RawPath = strings.TrimSuffix(upstream.EscapedPath(), "/") + r.URL.RawPath
	}
	q := r.URL.Query()
	q.Del("private_token")
	q.Del("access_token")
	u.RawQuery = q.Encode()
	out, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), body)
	if err != nil {
		return nil, err
	}
	for k, vs := range r.Header {
		out.Header[k] = append([]string(nil), vs...)
	}
	for _, h := range append(hopHeaders, credentialHeaders...) {
		out.Header.Del(h)
	}
	// Let the transport negotiate compression, so responses arrive decoded
	// and the adapters can parse the ones they rewrite.
	out.Header.Del("Accept-Encoding")
	out.Header.Del("Content-Length")
	switch {
	case body == http.NoBody || body == nil:
		out.ContentLength = 0
	case body == r.Body && r.ContentLength > 0:
		out.ContentLength = r.ContentLength // forwarded unchanged
	default:
		out.ContentLength = -1
	}
	return out, nil
}

// copyResponse writes an upstream response back to the sandbox, with
// Location headers that point at the upstream rewritten to the broker.
func copyResponse(w http.ResponseWriter, resp *http.Response, upstream *url.URL, publicHost string, body io.Reader) {
	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	for _, h := range append(hopHeaders, "Set-Cookie", "Content-Length") {
		w.Header().Del(h)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		if lu, err := url.Parse(loc); err == nil && strings.EqualFold(lu.Host, upstream.Host) {
			lu.Scheme, lu.Host = "https", publicHost
			w.Header().Set("Location", lu.String())
		}
	}
	w.WriteHeader(resp.StatusCode)
	if body == nil {
		body = resp.Body
	}
	_, _ = io.Copy(flushWriter{w}, body)
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
