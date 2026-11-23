package broker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Upstream forwards allowed requests to a provider's server with the
// user's credential. It is where credentials enter a request: Auth sets
// them, after every credential header the sandbox sent was dropped.
type Upstream struct {
	URL *url.URL // e.g. https://api.github.com
	// Strip is a path prefix the sandbox's form has and the upstream's
	// doesn't ("/api/v3" for GitHub's API); Path, if set, replaces the
	// whole path ("/graphql").
	Strip, Path string
	// Auth sets the credential on the outgoing request.
	Auth func(h http.Header) error
	// Public is the provider as the sandbox reaches it
	// (https://github.broker.internal). Location and Link headers that
	// point at the upstream are rewritten to it, so clients follow them
	// back through the broker.
	Public *url.URL
	Client *http.Client // NewUpstreamClient()
	// NoCredential is the message when Auth fails (no login).
	NoCredential string
}

// Upstreams makes a provider's upstreams, which share how the sandbox
// reaches the provider, the HTTP client and the message for a missing
// credential.
type Upstreams struct {
	Public       *url.URL
	Client       *http.Client
	NoCredential string
}

// To is an upstream at rawURL, with the credential auth sets. The URLs
// come from the broker's checked configuration: a bad one panics.
func (u Upstreams) To(rawURL string, auth func(http.Header) error) *Upstream {
	t, err := url.Parse(rawURL)
	if err != nil || t.Host == "" {
		panic("upstream URL " + rawURL)
	}
	return &Upstream{URL: t, Auth: auth, Public: u.Public, Client: u.Client, NoCredential: u.NoCredential}
}

// Token returns the user's credential for a provider (a token command's
// output, TokenSource).
type Token func() (string, error)

// Basic is an Upstream's Auth: HTTP Basic, user and token, as git sends it.
func Basic(user string, tok Token) func(http.Header) error {
	return func(h http.Header) error {
		t, err := tok()
		if err != nil {
			return err
		}
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+t)))
		return nil
	}
}

// Bearer is an Upstream's Auth: the token as a Bearer token.
func Bearer(tok Token) func(http.Header) error {
	return func(h http.Header) error {
		t, err := tok()
		if err != nil {
			return err
		}
		h.Set("Authorization", "Bearer "+t)
		return nil
	}
}

// hopHeaders aren't forwarded in either direction.
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

// NewUpstreamClient returns the client upstreams use: it never follows
// redirects (they go back to the sandbox's client, which follows them
// through the broker), and it uses the Mac's proxy settings.
func NewUpstreamClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = http.ProxyFromEnvironment
	return &http.Client{
		Transport:     t,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (u *Upstream) Serve(w http.ResponseWriter, c *Call) {
	resp, err := u.Do(c)
	if err != nil {
		HTTPError(w, err.(upstreamError).code, err.Error())
		return
	}
	defer resp.Body.Close()
	var body io.Reader = resp.Body
	if c.rewrite != nil && resp.StatusCode == http.StatusOK {
		if body, err = c.rewrite(resp); err != nil {
			HTTPError(w, http.StatusBadGateway, "upstream response: "+err.Error())
			return
		}
	}
	u.CopyResponse(w, resp, body)
}

type upstreamError struct {
	code int
	msg  string
}

func (e upstreamError) Error() string { return e.msg }

// Do sends the call upstream with the credential and returns the response.
func (u *Upstream) Do(c *Call) (*http.Response, error) {
	out, err := u.outbound(c)
	if err != nil {
		return nil, upstreamError{http.StatusBadGateway, "bad upstream request"}
	}
	if err := u.Auth(out.Header); err != nil {
		msg := u.NoCredential
		if msg == "" {
			msg = "no credential: run investigator login"
		}
		return nil, upstreamError{http.StatusServiceUnavailable, msg}
	}
	resp, err := u.Client.Do(out)
	if err != nil {
		return nil, upstreamError{http.StatusBadGateway, "upstream: " + err.Error()}
	}
	return resp, nil
}

// outbound builds the upstream request: the sandbox's path and query on
// the upstream URL, its headers minus hop-by-hop and credential headers.
func (u *Upstream) outbound(c *Call) (*http.Request, error) {
	r := c.R
	t := *u.URL
	switch {
	case u.Path != "":
		t.Path, t.RawPath = u.Path, ""
	default:
		t.Path = strings.TrimSuffix(u.URL.Path, "/") + strings.TrimPrefix(r.URL.Path, u.Strip)
		t.RawPath = ""
		if r.URL.RawPath != "" {
			t.RawPath = strings.TrimSuffix(u.URL.EscapedPath(), "/") + strings.TrimPrefix(r.URL.RawPath, u.Strip)
		}
	}
	q := r.URL.Query()
	q.Del("private_token")
	q.Del("access_token")
	t.RawQuery = q.Encode()
	out, err := http.NewRequestWithContext(r.Context(), r.Method, t.String(), c.Body)
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
	// and rewrites can parse them.
	out.Header.Del("Accept-Encoding")
	out.Header.Del("Content-Length")
	switch b := c.Body.(type) {
	case *bytes.Reader:
		out.ContentLength = int64(b.Len()) // a body a check read
	default:
		switch {
		case c.Body == http.NoBody:
			out.ContentLength = 0
		case c.Body == r.Body && r.ContentLength > 0:
			out.ContentLength = r.ContentLength // forwarded unchanged
		default:
			out.ContentLength = -1
		}
	}
	return out, nil
}

// CopyResponse writes an upstream response back to the sandbox.
func (u *Upstream) CopyResponse(w http.ResponseWriter, resp *http.Response, body io.Reader) {
	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	for _, h := range append(hopHeaders, "Set-Cookie", "Content-Length") {
		w.Header().Del(h)
	}
	if u.Public != nil {
		from := u.URL.Scheme + "://" + u.URL.Host
		to := u.Public.Scheme + "://" + u.Public.Host + u.Strip
		if loc := resp.Header.Get("Location"); loc != "" {
			if lu, err := url.Parse(loc); err == nil && strings.EqualFold(lu.Host, u.URL.Host) {
				w.Header().Set("Location", to+lu.RequestURI())
			}
		}
		if l := resp.Header.Get("Link"); l != "" {
			w.Header().Set("Link", strings.ReplaceAll(l, "<"+from+"/", "<"+to+"/"))
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(FlushWriter{w}, body)
}

// FlushWriter flushes after every write, so streams (model responses, SSE)
// reach the sandbox as they arrive.
type FlushWriter struct{ W http.ResponseWriter }

func (f FlushWriter) Write(p []byte) (int, error) {
	n, err := f.W.Write(p)
	if fl, ok := f.W.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// HTTPError answers with a JSON message.
func HTTPError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": "investigator broker: " + msg})
}
