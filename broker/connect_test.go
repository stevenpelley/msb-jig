package broker

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testPassword = "proxy-password"

// proxyEnv is a Proxy brokering github.com, whose splices all go to
// upstream (a stand-in for the CONNECT target).
type proxyEnv struct {
	addr   string
	ca     *CA
	dials  atomic.Int32
	seen   chan *http.Request // what the brokered provider got
	splice net.Listener       // where spliced connections go
}

func newProxyEnv(t *testing.T, upstream net.Listener) *proxyEnv {
	t.Helper()
	ca, err := NewCA("test", "github.com")
	if err != nil {
		t.Fatal(err)
	}
	e := &proxyEnv{ca: ca, seen: make(chan *http.Request, 10), splice: upstream}
	provider := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.seen <- r
		_, _ = io.WriteString(w, "brokered "+r.Proto)
	})
	p := NewProxy(&Server{Providers: map[string]http.Handler{"github.com": provider}}, ca, []string{"github.com"}, testPassword, nil)
	p.FirstWait = 200 * time.Millisecond
	p.Dial = func(network, _ string) (net.Conn, error) {
		e.dials.Add(1)
		if e.splice == nil {
			return nil, io.EOF
		}
		return net.Dial(network, e.splice.Addr().String())
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = p.Serve(l) }()
	t.Cleanup(func() { l.Close(); p.Close() })
	e.addr = l.Addr().String()
	return e
}

// client sends HTTPS through the proxy as microsandbox does: CONNECT with
// the password, then TLS to the guest's server name.
func (e *proxyEnv) client(password string, h2 bool, roots *x509.CertPool) *http.Client {
	pool := roots
	if pool == nil {
		pool = x509.NewCertPool()
		pool.AddCert(e.ca.Cert)
	}
	tr := &http.Transport{
		Proxy:             http.ProxyURL(&url.URL{Scheme: "http", User: url.UserPassword(ProxyUser, password), Host: e.addr}),
		TLSClientConfig:   &tls.Config{RootCAs: pool},
		ForceAttemptHTTP2: h2,
	}
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

// connect opens a raw tunnel through the proxy to target.
func (e *proxyEnv) connect(t *testing.T, target string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
	req.SetBasicAuth(ProxyUser, testPassword)
	req.Header.Set("Proxy-Authorization", req.Header.Get("Authorization"))
	req.Header.Del("Authorization")
	if err := req.Write(c); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	return c, br
}

func TestProxyTerminatesBrokeredNames(t *testing.T) {
	e := newProxyEnv(t, nil)
	for _, h2 := range []bool{false, true} {
		req, _ := http.NewRequest("GET", "https://github.com/o/r/info/refs", nil)
		req.Header.Set("Authorization", "token pasted-in")
		resp, err := e.client(testPassword, h2, nil).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := map[bool]string{false: "HTTP/1.1", true: "HTTP/2.0"}[h2]
		if string(b) != "brokered "+want {
			t.Errorf("h2=%v: got %q", h2, b)
		}
		r := <-e.seen
		if r.TLS == nil || r.TLS.ServerName != "github.com" || r.Header.Get("Authorization") != "" {
			t.Errorf("h2=%v: provider saw TLS %v, Authorization %q", h2, r.TLS, r.Header.Get("Authorization"))
		}
	}
	if n := e.dials.Load(); n != 0 {
		t.Errorf("brokered connections were spliced: %d dials", n)
	}
}

func TestProxySplicesOtherNames(t *testing.T) {
	var gotAuth atomic.Value
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "upstream")
	}))
	defer up.Close()
	e := newProxyEnv(t, up.Listener)
	roots := x509.NewCertPool()
	roots.AddCert(up.Certificate())
	c := e.client(testPassword, false, roots)
	// httptest's certificate is for example.com.
	req, _ := http.NewRequest("GET", "https://example.com/", nil)
	req.Header.Set("Authorization", "token theirs")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "upstream" || gotAuth.Load() != "token theirs" || e.dials.Load() != 1 {
		t.Errorf("splice: %q, upstream Authorization %v, dials %d", b, gotAuth.Load(), e.dials.Load())
	}
}

func TestProxyRefusesWithoutPassword(t *testing.T) {
	e := newProxyEnv(t, nil)
	for _, pw := range []string{"wrong", ""} {
		if _, err := e.client(pw, false, nil).Get("https://github.com/x"); err == nil {
			t.Errorf("password %q: no error", pw)
		}
	}
	c, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("no credential: %v %v", resp, err)
	}
	if len(e.seen) != 0 || e.dials.Load() != 0 {
		t.Error("an unauthenticated connection got through")
	}
}

func TestProxyRefusesTLSWithoutSNI(t *testing.T) {
	e := newProxyEnv(t, nil)
	c, _ := e.connect(t, "140.82.112.3:443")
	tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true}) // no ServerName: no SNI
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tc.Handshake(); err == nil {
		t.Error("handshake without SNI succeeded")
	}
	if e.dials.Load() != 0 {
		t.Error("a connection without SNI was spliced")
	}
}

func TestProxyPlainHTTP(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "plain "+r.Host)
	}))
	defer up.Close()
	e := newProxyEnv(t, up.Listener)
	get := func(host string) (int, string) {
		c, br := e.connect(t, "203.0.113.1:80")
		_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n")
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("GitHub.com"); code != http.StatusForbidden || !strings.Contains(body, "brokered") {
		t.Errorf("brokered host over HTTP: %d %q", code, body)
	}
	if e.dials.Load() != 0 {
		t.Error("a brokered host was spliced over HTTP")
	}
	if code, body := get("deb.example:80"); code != http.StatusOK || body != "plain deb.example:80" {
		t.Errorf("other host over HTTP: %d %q", code, body)
	}
}

func TestProxySplicesServerFirst(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			_, _ = io.WriteString(c, "SSH-2.0-test\r\n")
			c.Close()
		}
	}()
	e := newProxyEnv(t, l)
	_, br := e.connect(t, "203.0.113.1:22")
	line, err := br.ReadString('\n')
	if err != nil || line != "SSH-2.0-test\r\n" {
		t.Errorf("server-first: %q %v", line, err)
	}
}

func TestProxyRefusesOtherMethods(t *testing.T) {
	e := newProxyEnv(t, nil)
	resp, err := e.client(testPassword, false, nil).Transport.(*http.Transport).Clone().RoundTrip(
		&http.Request{Method: "GET", URL: &url.URL{Scheme: "http", Host: "github.com", Path: "/"}, Header: http.Header{}})
	if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("absolute-form GET to the proxy: %v %v", resp, err)
	}
}

func TestPeekSNIOversizeHello(t *testing.T) {
	// A record longer than the buffer can't be read: no SNI, refused.
	r := bufio.NewReaderSize(strings.NewReader("\x16\x03\x01\xff\xff"+strings.Repeat("a", 100)), 16)
	if sni := peekSNI(r); sni != "" {
		t.Errorf("got %q", sni)
	}
}

func TestCAWithRealNames(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir, "s", "github.com", "API.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{BrokerDomain, "github.com", "api.anthropic.com"}
	if strings.Join(ca.Cert.PermittedDNSDomains, " ") != strings.Join(want, " ") || !ca.Cert.PermittedDNSDomainsCritical {
		t.Errorf("constraint %q", ca.Cert.PermittedDNSDomains)
	}
	for _, ok := range []string{"github.com", "api.github.com", "api.anthropic.com", "aws.broker.internal"} {
		if _, err := ca.Leaf(ok); err != nil {
			t.Errorf("Leaf(%s): %v", ok, err)
		}
	}
	for _, bad := range []string{"gitlab.com", "evilgithub.com", "anthropic.com"} {
		if _, err := ca.Leaf(bad); err == nil {
			t.Errorf("Leaf(%s) signed", bad)
		}
	}
	// Kept: it must cover what's asked now.
	if _, err := LoadOrCreateCA(dir, "s", "github.com"); err != nil {
		t.Errorf("reload: %v", err)
	}
	if _, err := LoadOrCreateCA(dir, "s", "gitlab.com"); err == nil {
		t.Error("a kept CA that can't sign gitlab.com was accepted")
	}
}
