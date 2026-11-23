package broker

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testHost = "gitlab.broker.internal"

func TestServerChecks(t *testing.T) {
	ca, err := NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	var gotAuth []string
	p := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization")+r.Header.Get("Private-Token")+r.Header.Get("Cookie"))
	})
	srv := httptest.NewUnstartedServer(&Server{Providers: map[string]http.Handler{testHost: p}})
	srv.TLS = ca.TLSConfig([]string{testHost})
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	addr := srv.Listener.Addr().String()
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	get := func(mod func(*http.Request)) int {
		req, _ := http.NewRequest("GET", "https://"+addr+"/x", nil)
		req.Host = testHost
		req.Header.Set("Authorization", "Bearer placeholder")
		req.Header.Set("Private-Token", "placeholder")
		req.Header.Set("Cookie", "a=b")
		mod(req)
		c.Transport.(*http.Transport).TLSClientConfig.ServerName = testHost
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(func(*http.Request) {}); code != http.StatusOK || gotAuth[0] != "" {
		t.Errorf("credentials reached the provider: %d %q", code, gotAuth)
	}
	if code := get(func(r *http.Request) { r.Host = "github.broker.internal" }); code != http.StatusMisdirectedRequest {
		t.Errorf("Host/SNI mismatch: %d", code)
	}
	// Absolute-form request URI, sent raw.
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: testHost})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET http://evil.example/ HTTP/1.1\r\nHost: "+testHost+"\r\nConnection: close\r\n\r\n")
	line, _ := io.ReadAll(io.LimitReader(conn, 64))
	if !bytes.HasPrefix(line, []byte("HTTP/1.1 400")) {
		t.Errorf("absolute-form: %q", line)
	}
	// Unknown SNI gets no certificate.
	if _, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: "gitlab.com"}); err == nil {
		t.Error("handshake succeeded for gitlab.com")
	}
}

func TestMatchPath(t *testing.T) {
	cases := []struct {
		pat, path string
		ok        bool
		vars      map[string]string
	}{
		{"/a/{x}/c", "/a/b/c", true, map[string]string{"x": "b"}},
		{"/a/{x}/c", "/a//c", false, nil},
		{"/a/{x}/c", "/a/b/c/d", false, nil},
		{"/api/v4/{path...}", "/api/v4/projects/1", true, map[string]string{"path": "projects/1"}},
		{"/api/v4/{path...}", "/api/v4", false, nil},
		{"/{repo...}/info/refs", "/g/s/p.git/info/refs", true, map[string]string{"repo": "g/s/p.git"}},
		{"/{repo...}/info/refs", "/info/refs", false, nil},
		{"/{repo...}/info/refs", "/g//p/info/refs", false, nil},
		{"/p/{id}/b/{branch}", "/p/g%2Fp/b/agent%2Fu%2Fx", true, map[string]string{"id": "g%2Fp", "branch": "agent%2Fu%2Fx"}},
	}
	for _, c := range cases {
		vars, ok := matchPath(splitPath(c.pat), splitPath(c.path))
		if ok != c.ok {
			t.Errorf("%s ~ %s = %v", c.pat, c.path, ok)
			continue
		}
		for k, v := range c.vars {
			if vars[k] != v {
				t.Errorf("%s ~ %s: %s = %q, want %q", c.pat, c.path, k, vars[k], v)
			}
		}
	}
}

func splitPath(p string) []string {
	var out []string
	for _, s := range bytes.Split([]byte(p[1:]), []byte("/")) {
		out = append(out, string(s))
	}
	return out
}

// A provider denies what no route lists, unescapes captures, and runs the
// first matching route only.
func TestProviderRoutes(t *testing.T) {
	var served []string
	to := targetFunc(func(w http.ResponseWriter, c *Call) { served = append(served, c.Op+" "+c.Var("b")) })
	p := &Provider{Name: "t", Routes: []Route{
		{Method: "POST", Path: "/x/{b}", Op: "never", Check: Never("no")},
		{Method: "GET HEAD", Path: "/x/{b}", Op: "read", Check: Read, To: to},
		{Method: "GET", Path: "/x/{b}", Op: "shadowed", Check: Read, To: to},
	}}
	do := func(method, path string) int {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w.Code
	}
	if code := do("GET", "/x/a%2Fb"); code != 200 || len(served) != 1 || served[0] != "read a/b" {
		t.Errorf("GET: %d %v", code, served)
	}
	if code := do("POST", "/x/a"); code != 403 || len(served) != 1 {
		t.Errorf("POST: %d %v", code, served)
	}
	if code := do("DELETE", "/x/a"); code != 403 {
		t.Errorf("unlisted method: %d", code)
	}
	if code := do("GET", "/y"); code != 403 {
		t.Errorf("unlisted path: %d", code)
	}
}

type targetFunc func(http.ResponseWriter, *Call)

func (f targetFunc) Serve(w http.ResponseWriter, c *Call) { f(w, c) }

func TestCANameConstraint(t *testing.T) {
	ca, err := NewCA("t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.Leaf("gitlab.com"); err == nil {
		t.Fatal("Leaf signed gitlab.com")
	}
	// Even a certificate signed with the CA's key outside Leaf fails to verify.
	evil, err := signRaw(ca, "gitlab.com")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	if _, err := evil.Verify(x509.VerifyOptions{Roots: pool, DNSName: "gitlab.com"}); err == nil {
		t.Fatal("name constraint not enforced")
	}
	good, _ := ca.Leaf(testHost)
	leaf, _ := x509.ParseCertificate(good.Certificate[0])
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: testHost}); err != nil {
		t.Fatalf("good leaf: %v", err)
	}
}

// signRaw signs a certificate for host with the CA's key directly,
// bypassing Leaf's check, as a stolen key would.
func signRaw(ca *CA, host string) (*x509.Certificate, error) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

func TestLoadOrCreateCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	a, err := LoadOrCreateCA(dir, "s")
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateCA(dir, "s")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Cert.Equal(b.Cert) || !a.Key.Equal(b.Key) {
		t.Fatal("CA not reloaded")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "ca-key.pem")); fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", fi.Mode())
	}
	if _, err := b.Leaf("gitlab.broker.internal"); err != nil {
		t.Fatal(err)
	}
}
