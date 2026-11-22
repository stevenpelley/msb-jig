package filter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func mustACL(t *testing.T, specs ...string) *ACL {
	t.Helper()
	var es []Entry
	for _, s := range specs {
		e, err := ParseEntry(s)
		if err != nil {
			t.Fatal(err)
		}
		es = append(es, e)
	}
	a, err := NewACL(es)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestParseEntry(t *testing.T) {
	good := map[string]string{"Example.COM": "example.com:443", "a.b:80": "a.b:80", "*.example.com": "*.example.com:443", "x-y.io:443": "x-y.io:443"}
	for in, want := range good {
		e, err := ParseEntry(in)
		if err != nil || e.String() != want {
			t.Errorf("%q: %v %v, want %s", in, e, err, want)
		}
	}
	for _, in := range []string{"", "1.2.3.4", "::1", "[::1]:443", "a.b:22", "*.com", "a.*.b", "*", "a_b.com", "-a.com", "0x7f.1", "a.123", "a..b", "a.b:"} {
		if e, err := ParseEntry(in); err == nil {
			t.Errorf("%q accepted as %v", in, e)
		}
	}
}

func TestACL(t *testing.T) {
	a := mustACL(t, "example.com", "*.example.net", "plain.example.org:80")
	cases := []struct {
		host string
		port int
		ok   bool
	}{
		{"example.com", 443, true},
		{"www.example.com", 443, false},
		{"example.com", 80, false},
		{"a.example.net", 443, true},
		{"a.b.example.net", 443, true},
		{"example.net", 443, false},
		{"badexample.net", 443, false},
		{"plain.example.org", 80, true},
		{"plain.example.org", 443, false},
	}
	for _, c := range cases {
		if _, ok := a.Allows(c.host, c.port); ok != c.ok {
			t.Errorf("%s:%d = %v, want %v", c.host, c.port, ok, c.ok)
		}
	}
	if _, err := NewACL(nil); err == nil {
		t.Error("empty ACL accepted")
	}
}

func TestPublicAddr(t *testing.T) {
	pub := []string{"1.1.1.1", "8.8.8.8", "2606:4700::1111", "64:ff9b::808:808", "::ffff:8.8.8.8"}
	priv := []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.5.2", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "224.0.0.1", "255.255.255.255", "::1", "::", "fd00::1", "fe80::1",
		"::ffff:127.0.0.1", "64:ff9b::7f00:1", "64:ff9b::a00:1", "192.0.0.1"}
	for _, s := range pub {
		if !PublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s judged non-public", s)
		}
	}
	for _, s := range priv {
		if PublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s judged public", s)
		}
	}
}

func TestLayers(t *testing.T) {
	for _, prof := range Profiles {
		ls, err := Layers(prof)
		if err != nil {
			t.Fatal(err)
		}
		es, err := Compile(ls)
		if err != nil {
			t.Fatalf("%s: %v", prof, err)
		}
		a, err := NewACL(es)
		if err != nil {
			t.Fatal(err)
		}
		for _, hp := range []struct {
			h string
			p int
		}{{"claude.ai", 443}, {"archive.ubuntu.com", 80}, {"ports.ubuntu.com", 80}, {"proxy.golang.org", 443}} {
			if _, ok := a.Allows(hp.h, hp.p); !ok {
				t.Errorf("%s: %s:%d not allowed", prof, hp.h, hp.p)
			}
		}
		for _, h := range []string{"pastebin.com", "api.anthropic.com"} { // the API goes through the broker
			if _, ok := a.Allows(h, 443); ok {
				t.Errorf("%s allows %s", prof, h)
			}
		}
	}
	if _, err := Layers("nope"); err == nil {
		t.Error("unknown profile accepted")
	}

	dir := t.TempDir()
	user := filepath.Join(dir, "user.yaml")
	os.WriteFile(user, []byte("groups: [mine]\nhosts: [extra.example.com]\ndefine:\n  mine:\n    hosts: ['*.mine.example']\n"), 0o600)
	ls, err := Layers("investigator", user)
	if err != nil {
		t.Fatal(err)
	}
	es, err := Compile(ls)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := NewACL(es)
	if e, ok := a.Allows("x.mine.example", 443); !ok || e.Group != "mine" || e.Layer != user {
		t.Errorf("user group: %v %v", e, ok)
	}
	if _, ok := a.Allows("extra.example.com", 443); !ok {
		t.Error("user host not allowed")
	}
	// The ACL file round-trips, and the sbx form lists every entry.
	back, err := ParseACL(strings.NewReader(FormatACL(es)))
	if err != nil || len(back) != len(es) {
		t.Fatalf("round trip: %d of %d, %v", len(back), len(es), err)
	}
	sbx := FormatSbx(es)
	if !strings.Contains(sbx, "archive.ubuntu.com:80") || !strings.Contains(sbx, "*.mine.example:443") {
		t.Errorf("sbx = %s", sbx)
	}

	bad := filepath.Join(dir, "bad.yaml")
	for _, body := range []string{"groups: [nosuch]\n", "hosts: [1.2.3.4]\n", "hostz: [a.com]\n", "define:\n  go:\n    hosts: [a.com]\n"} {
		os.WriteFile(bad, []byte(body), 0o600)
		ls, err := Layers("dev", bad)
		if err == nil {
			_, err = Compile(ls)
		}
		if err == nil {
			t.Errorf("layer %q accepted", body)
		}
	}
}

// --- the proxy ---

type env struct {
	proxy      *Proxy
	addr       string // the proxy's listen address
	tlsTarget  *httptest.Server
	httpTarget *httptest.Server
	mu         sync.Mutex
	denials    []Denial
	dialed     []string
}

// newEnv starts a proxy whose resolver maps every name to 127.0.0.1 and
// whose dialer sends port 443 to an HTTPS test server and port 80 to an
// HTTP one, so the tests use real names and the real ports.
func newEnv(t *testing.T, acl *ACL, configure func(*Proxy)) *env {
	t.Helper()
	e := &env{}
	e.tlsTarget = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "tls %s %s", r.Host, r.URL.Path)
	}))
	e.httpTarget = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "http %s %s pa=%q conn=%q", r.Host, r.URL.Path, r.Header.Get("Proxy-Authorization"), r.Header.Get("X-Hop"))
	}))
	t.Cleanup(e.tlsTarget.Close)
	t.Cleanup(e.httpTarget.Close)
	e.proxy = &Proxy{
		ACL: acl,
		Resolve: func(_ context.Context, h string) ([]netip.Addr, error) {
			if h == "nxdomain.example.com" {
				return nil, errors.New("no such host")
			}
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		AddrOK: func(a netip.Addr) bool { return a == netip.MustParseAddr("127.0.0.1") },
		Dial:   e.dial,
		OnDeny: func(d Denial) { e.mu.Lock(); e.denials = append(e.denials, d); e.mu.Unlock() },
	}
	if configure != nil {
		configure(e.proxy)
	}
	srv := httptest.NewServer(e.proxy)
	t.Cleanup(srv.Close)
	e.addr = srv.Listener.Addr().String()
	return e
}

func (e *env) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	e.mu.Lock()
	e.dialed = append(e.dialed, addr)
	e.mu.Unlock()
	_, port, _ := net.SplitHostPort(addr)
	switch port {
	case "443":
		addr = e.tlsTarget.Listener.Addr().String()
	case "80":
		addr = e.httpTarget.Listener.Addr().String()
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func (e *env) client() *http.Client {
	pu, _ := url.Parse("http://" + e.addr)
	tr := e.tlsTarget.Client().Transport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(pu)
	return &http.Client{Transport: tr}
}

func get(t *testing.T, c *http.Client, u string) (int, string, error) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

// raw sends one request line and headers to the proxy and returns the
// status line and body.
func raw(t *testing.T, addr, reqLine string, headers ...string) (string, string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "%s\r\n%s\r\n", reqLine, strings.Join(append(headers, ""), "\r\n"))
	method, _, _ := strings.Cut(reqLine, " ")
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}
	if method == http.MethodConnect && resp.StatusCode == 200 {
		return resp.Status, "" // a tunnel: no body to read
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.Status, string(b)
}

func TestProxyDirect(t *testing.T) {
	e := newEnv(t, mustACL(t, "example.com", "example.com:80", "nxdomain.example.com"), nil)
	c := e.client()

	// httptest's certificate is for example.com, so TLS through CONNECT verifies.
	code, body, err := get(t, c, "https://example.com/x")
	if err != nil || code != 200 || body != "tls example.com /x" {
		t.Fatalf("CONNECT: %d %q %v", code, body, err)
	}
	if e.dialed[len(e.dialed)-1] != "127.0.0.1:443" {
		t.Errorf("dialed %v, want the checked address", e.dialed)
	}

	// Plain HTTP: absolute form, hop-by-hop and proxy headers dropped.
	st, body := raw(t, e.addr, "GET http://example.com/p HTTP/1.1", "Host: example.com", "Proxy-Authorization: Basic eDp5",
		"Connection: X-Hop", "X-Hop: 1")
	if !strings.HasPrefix(st, "200") || body != `http example.com /p pa="" conn=""` {
		t.Errorf("plain http: %s %q", st, body)
	}

	denied := []struct{ name, line, want string }{
		{"not listed", "CONNECT evil.example.org:443 HTTP/1.1", "not on the allowlist"},
		{"subdomain of an exact entry", "CONNECT www.example.com:443 HTTP/1.1", "not on the allowlist"},
		{"ip literal", "CONNECT 127.0.0.1:443 HTTP/1.1", "not addresses"},
		{"ipv6 literal", "CONNECT [::1]:443 HTTP/1.1", "not addresses"},
		{"other port", "CONNECT example.com:22 HTTP/1.1", "ports 80 and 443"},
		{"http to a port-443-only entry", "GET http://nxdomain.example.com/ HTTP/1.1", "not on the allowlist"},
		{"https absolute form", "GET https://example.com/ HTTP/1.1", "only http://"},
	}
	for _, d := range denied {
		st, body := raw(t, e.addr, d.line, "Host: x")
		if !strings.HasPrefix(st, "403") && !strings.HasPrefix(st, "400") || !strings.Contains(body, d.want) {
			t.Errorf("%s: %s %q", d.name, st, body)
		}
	}
	st, body = raw(t, e.addr, "CONNECT example.com:443 HTTP/1.1", "Host: example.com:443", "X-Upstream-Https-Proxy: evil.example.org:3128")
	if !strings.HasPrefix(st, "403") || !strings.Contains(body, "upstream proxy") {
		t.Errorf("client-chosen upstream: %s %q", st, body)
	}
	st, _ = raw(t, e.addr, "GET / HTTP/1.1", "Host: example.com")
	if !strings.HasPrefix(st, "400") {
		t.Errorf("origin form: %s", st)
	}
	st, _ = raw(t, e.addr, "CONNECT nxdomain.example.com:443 HTTP/1.1", "Host: nxdomain.example.com:443")
	if !strings.HasPrefix(st, "502") {
		t.Errorf("unresolvable: %s", st)
	}
	if len(e.denials) < len(denied) || e.denials[0].Host != "evil.example.org" || e.denials[0].Port != 443 {
		t.Errorf("denials: %+v", e.denials)
	}
}

// With the real address check, a name that resolves to loopback (the Mac's
// own services) is refused even though it's on the ACL.
func TestProxyRefusesPrivateAddresses(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "192.168.1.10", "169.254.169.254", "::1", "::ffff:10.0.0.1"} {
		e := newEnv(t, mustACL(t, "example.com", "example.com:80"), func(p *Proxy) {
			p.AddrOK = nil
			p.Resolve = func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr(a)}, nil }
		})
		st, body := raw(t, e.addr, "CONNECT example.com:443 HTTP/1.1", "Host: example.com:443")
		if !strings.HasPrefix(st, "403") || !strings.Contains(body, "non-public") {
			t.Errorf("%s: %s %q", a, st, body)
		}
		st, body = raw(t, e.addr, "GET http://example.com/ HTTP/1.1", "Host: example.com")
		if !strings.HasPrefix(st, "403") || !strings.Contains(body, "non-public") {
			t.Errorf("%s, plain http: %s %q", a, st, body)
		}
		if len(e.dialed) != 0 {
			t.Errorf("%s: dialed %v", a, e.dialed)
		}
	}
	// A mixed answer dials only the public address.
	e := newEnv(t, mustACL(t, "example.com"), func(p *Proxy) {
		p.AddrOK = nil
		p.Resolve = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("93.184.215.14")}, nil
		}
	})
	raw(t, e.addr, "CONNECT example.com:443 HTTP/1.1", "Host: example.com:443")
	if len(e.dialed) != 1 || e.dialed[0] != "93.184.215.14:443" {
		t.Errorf("mixed answer dialed %v", e.dialed)
	}
}

func TestProxyUpstream(t *testing.T) {
	// The upstream is another filter in direct mode, behind a check of the
	// Proxy-Authorization header. It allows example.com but not deny.example.com.
	up := newEnv(t, mustACL(t, "example.com", "example.com:80", "allowed-here.example.com"), nil)
	var seen []string
	var mu sync.Mutex
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.Host+" "+r.Header.Get("Proxy-Authorization"))
		mu.Unlock()
		if r.Header.Get("Proxy-Authorization") != "Basic dTpwdw==" {
			http.Error(w, "auth", http.StatusProxyAuthRequired)
			return
		}
		up.proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(upSrv.Close)
	upURL, _ := url.Parse("http://u:pw@" + upSrv.Listener.Addr().String())

	e := newEnv(t, mustACL(t, "example.com", "example.com:80", "deny.example.com"), func(p *Proxy) {
		p.Upstream = upURL
		p.Resolve = func(context.Context, string) ([]netip.Addr, error) {
			t.Error("upstream mode resolved a name")
			return nil, errors.New("no")
		}
		p.Dial = (&net.Dialer{}).DialContext
	})
	c := e.client()
	code, body, err := get(t, c, "https://example.com/u")
	if err != nil || code != 200 || body != "tls example.com /u" {
		t.Fatalf("CONNECT via upstream: %d %q %v", code, body, err)
	}
	code, body, err = get(t, c, "http://example.com/h")
	if err != nil || code != 200 || !strings.HasPrefix(body, "http example.com /h") {
		t.Fatalf("http via upstream: %d %q %v", code, body, err)
	}
	// Our ACL allows it, the upstream refuses it: reported as a denial.
	st, body := raw(t, e.addr, "CONNECT deny.example.com:443 HTTP/1.1", "Host: deny.example.com:443")
	if !strings.HasPrefix(st, "403") || !strings.Contains(body, "upstream proxy refused") {
		t.Errorf("upstream refusal: %s %q", st, body)
	}
	// Our ACL refuses before the upstream sees anything.
	n := len(seen)
	raw(t, e.addr, "CONNECT allowed-here.example.com:443 HTTP/1.1", "Host: x")
	if len(seen) != n {
		t.Errorf("denied request reached the upstream: %v", seen[n:])
	}
	if !strings.HasPrefix(seen[0], "CONNECT example.com:443 Basic ") {
		t.Errorf("upstream saw %v", seen)
	}
}
