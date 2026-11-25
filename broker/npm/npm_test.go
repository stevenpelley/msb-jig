package npm_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/stevenpelley/msb-jig/broker/brokertest"
	"github.com/stevenpelley/msb-jig/broker/npm"
	"github.com/stevenpelley/msb-jig/broker/providers"
)

// fakeRegistry records what it was sent, and serves a package document
// whose tarball URL is its own.
type fakeRegistry struct {
	mu   sync.Mutex
	seen []string // "METHOD path Authorization"
	self string   // its URL
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.EscapedPath()+" "+r.Header.Get("Authorization"))
	f.mu.Unlock()
	if r.Method == http.MethodGet && r.URL.EscapedPath() == "/@example%2fpkg" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"@example/pkg","versions":{"1.0.0":{"dist":{"tarball":"`+f.self+`/@example/pkg/-/pkg-1.0.0.tgz"}}}}`)
		return
	}
	_, _ = io.WriteString(w, "{}")
}

func (f *fakeRegistry) reset() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.seen
	f.seen = nil
	return s
}

func serve(t *testing.T, proxy bool, regs ...providers.NPMRegistry) (http.Handler, providers.NPM) {
	t.Helper()
	n := providers.NPM{Registries: regs}
	hs, err := providers.Build(context.Background(), providers.Config{Providers: []providers.Provider{n}, Proxy: proxy}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return hs[n.Hosts(proxy)[0]], n
}

func TestRoutes(t *testing.T) {
	f := &fakeRegistry{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.self = srv.URL
	const host = "registry-example-org.npm.broker.internal"
	h, n := serve(t, false, providers.NPMRegistry{Host: "registry.example.org", TokenCmd: brokertest.TokenCmd, URL: srv.URL})
	if got := n.Hosts(false); len(got) != 1 || got[0] != host {
		t.Fatalf("hosts %v", got)
	}
	addr, caFile := brokertest.Serve(t, host, h)
	c := brokertest.Client(addr, caFile)
	cases := []struct {
		method, path string
		allow        bool
	}{
		{"GET", "/left-pad", true},
		{"GET", "/@example%2fpkg", true},
		{"HEAD", "/left-pad", true},
		{"GET", "/left-pad/-/left-pad-1.3.0.tgz", true},
		{"GET", "/@example/pkg/-/pkg-1.0.0.tgz", true},
		{"GET", "/download/@example/pkg/1.0.0/abc", true},
		{"GET", "/-/package/left-pad/dist-tags", true},
		{"GET", "/-/v1/search", true},
		{"GET", "/-/whoami", true},
		{"GET", "/-/ping", true},
		{"GET", "/-/npm/v1/keys", true},
		{"POST", "/-/npm/v1/security/advisories/bulk", true},
		{"PUT", "/left-pad", false},
		{"PUT", "/@example%2fpkg", false},
		{"PUT", "/left-pad/-rev/3-abc", false},
		{"DELETE", "/left-pad/-rev/3-abc", false},
		{"DELETE", "/left-pad/-/left-pad-1.3.0.tgz/-rev/3-abc", false},
		{"POST", "/left-pad", false},
		{"PUT", "/-/package/left-pad/dist-tags/latest", false},
		{"DELETE", "/-/package/left-pad/dist-tags/latest", false},
		{"GET", "/-/package/left-pad/collaborators", false},
		{"POST", "/-/package/left-pad/access", false},
		{"POST", "/-/v1/login", false},
		{"PUT", "/-/user/org.couchdb.user:alice", false},
		{"GET", "/-/user/org.couchdb.user:alice", false},
		{"GET", "/-/npm/v1/tokens", false},
		{"POST", "/-/npm/v1/tokens", false},
		{"DELETE", "/-/user/token/abc", false},
		{"GET", "/-/npm/v1/user", false},
		{"POST", "/-/npm/v1/user", false},
		{"POST", "/-/org/example/user", false},
		{"POST", "/-/stage/package/left-pad", false},
		{"POST", "/-/npm/v1/oidc/token/exchange/package/left-pad", false},
		{"POST", "/-/npm/v1/security/audits/quick", false},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(tc.method, "https://"+host+tc.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer placeholder")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		seen := f.reset()
		switch {
		case tc.allow && (resp.StatusCode != 200 || len(seen) != 1):
			t.Errorf("%s %s: %d %s, want allowed", tc.method, tc.path, resp.StatusCode, b)
		case tc.allow && !strings.HasSuffix(seen[0], " Bearer "+brokertest.Token):
			t.Errorf("%s %s: upstream saw %q, want the user's token", tc.method, tc.path, seen[0])
		case !tc.allow && (resp.StatusCode != 403 || len(seen) != 0):
			t.Errorf("%s %s: %d %s (forwarded %v), want refused", tc.method, tc.path, resp.StatusCode, b, seen)
		}
	}

	// Server mode: tarball URLs come back pointing at the broker.
	resp, err := c.Get("https://" + host + "/@example%2fpkg")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if want := `"https://` + host + `/@example/pkg/-/pkg-1.0.0.tgz"`; !strings.Contains(string(b), want) {
		t.Errorf("package document %s, want the tarball at %s", b, want)
	}
}

// No token: nothing is sent upstream.
func TestNoToken(t *testing.T) {
	f := &fakeRegistry{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	h, n := serve(t, true, providers.NPMRegistry{Host: "registry.example.org", URL: srv.URL})
	host := n.Hosts(true)[0]
	if host != "registry.example.org" {
		t.Fatalf("proxy mode host %s", host)
	}
	req := httptest.NewRequest("GET", "https://"+host+"/left-pad", nil)
	req.Header.Set("Authorization", "Bearer from-the-sandbox")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("status %d", w.Code)
	}
	if seen := f.reset(); len(seen) != 1 || !strings.HasSuffix(seen[0], "/left-pad ") {
		t.Errorf("upstream saw %v, want no credential", seen)
	}
}

func TestGuest(t *testing.T) {
	n := providers.NPM{Registries: []providers.NPMRegistry{{Host: providers.NPMJSHost}, {Host: "npm.example.org", Scopes: []string{"@example"}}}}
	g := n.Guest(false)
	if g.Env["NPM_CONFIG_REGISTRY"] != "https://registry-npmjs-org.npm.broker.internal/" || g.Env["NPM_CONFIG_GLOBALCONFIG"] != providers.NPMRCFile {
		t.Errorf("env %v", g.Env)
	}
	if len(g.Files) != 1 || !strings.Contains(g.Files[0].Content, "@example:registry=https://npm-example-org.npm.broker.internal/\n") {
		t.Errorf("files %+v", g.Files)
	}
	if err := g.Validate(); err != nil {
		t.Error(err)
	}
	if p := n.Guest(true); len(p.Env) != 0 || len(p.Files) != 0 || len(p.Instructions) != 1 {
		t.Errorf("proxy mode guest %+v", p)
	}
}

// The rewrite finds matches split across reads.
func TestReplacingAcrossReads(t *testing.T) {
	in := strings.Repeat(`{"tarball":"https://up.example/a.tgz"},`, 5000)
	want := strings.ReplaceAll(in, `"https://up.example/`, `"https://down.example/`)
	for _, r := range []io.Reader{strings.NewReader(in), iotest.OneByteReader(strings.NewReader(in)), iotest.HalfReader(strings.NewReader(in))} {
		got, err := io.ReadAll(npm.Replacing(r, `"https://up.example/`, `"https://down.example/`))
		if err != nil || string(got) != want {
			t.Errorf("replacing: err %v, %d bytes, want %d", err, len(got), len(want))
		}
	}
}
