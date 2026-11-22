package broker

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testUser  = "alice"
	testToken = "glpat-real-token-for-test"
	testHost  = "gitlab.broker.internal"
)

// brokerFor serves a GitLab adapter for upstream over TLS with a fresh CA,
// as investigator-net would. It returns the broker's address and CA file.
func brokerFor(t *testing.T, upstream string, session string) (addr string, caFile string, ca *CA) {
	t.Helper()
	ca, err := NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	up, _ := url.Parse(upstream)
	gl := &GitLab{User: testUser, Token: func() (string, error) { return testToken, nil }, Upstream: up,
		PublicHost: testHost, Client: NewUpstreamClient()}
	srv := httptest.NewUnstartedServer(&Server{Providers: map[string]http.Handler{testHost: gl}, SessionToken: session})
	srv.TLS = ca.TLSConfig([]string{testHost})
	srv.StartTLS()
	t.Cleanup(srv.Close)
	caFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, ca.CertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv.Listener.Addr().String(), caFile, ca
}

// --- git, end to end ---

type gitEnv struct {
	t       *testing.T
	dir     string
	addr    string
	caFile  string
	bare    string
	mu      sync.Mutex
	authErr string
}

func newGitEnv(t *testing.T) *gitEnv {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	backend := "/usr/lib/git-core/git-http-backend"
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not found")
	}
	e := &gitEnv{t: t, dir: t.TempDir()}
	root := filepath.Join(e.dir, "upstream")
	e.bare = filepath.Join(root, "grp", "sub", "proj.git")
	e.run("", "git", "init", "-q", "--bare", "-b", "main", e.bare)
	e.run(e.bare, "git", "config", "http.receivepack", "true")
	e.run(e.bare, "git", "config", "receive.advertisePushOptions", "true")
	seed := filepath.Join(e.dir, "seed")
	e.run("", "git", "init", "-q", "-b", "main", seed)
	e.run(seed, "git", "commit", "-q", "--allow-empty", "-m", "init")
	e.run(seed, "git", "push", "-q", e.bare, "main")

	cgiH := &cgi.Handler{Path: backend, Root: "/", Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(testUser+":"+testToken))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != want {
			e.mu.Lock()
			e.authErr = "upstream got Authorization " + got
			e.mu.Unlock()
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Go's CGI handler refuses chunked bodies; GitLab takes them.
		if r.ContentLength < 0 {
			b, _ := io.ReadAll(r.Body)
			r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
			r.TransferEncoding = nil
		}
		cgiH.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)
	e.addr, e.caFile, _ = brokerFor(t, up.URL, "")
	return e
}

func (e *gitEnv) env() []string {
	_, port, _ := net.SplitHostPort(e.addr)
	var env []string
	for _, kv := range os.Environ() {
		k := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
		if strings.HasSuffix(k, "_PROXY") || strings.HasPrefix(k, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "HOME="+e.dir,
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=http.sslCAInfo", "GIT_CONFIG_VALUE_0="+e.caFile,
		"GIT_CONFIG_KEY_1=http.curloptResolve", "GIT_CONFIG_VALUE_1="+testHost+":"+port+":127.0.0.1",
		"GIT_CONFIG_KEY_2=user.email", "GIT_CONFIG_VALUE_2=a@example.invalid",
		"GIT_AUTHOR_NAME=a", "GIT_COMMITTER_NAME=a",
	)
}

func (e *gitEnv) url() string {
	_, port, _ := net.SplitHostPort(e.addr)
	return "https://" + testHost + ":" + port + "/grp/sub/proj.git"
}

func (e *gitEnv) git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = e.env()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *gitEnv) run(dir string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=a", "GIT_COMMITTER_NAME=a", "GIT_AUTHOR_EMAIL=a@x", "GIT_COMMITTER_EMAIL=a@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (e *gitEnv) upstreamHas(ref string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "-q", ref)
	cmd.Dir = e.bare
	return cmd.Run() == nil
}

func TestGitThroughBroker(t *testing.T) {
	e := newGitEnv(t)
	work := filepath.Join(e.dir, "work")
	if out, err := e.git(e.dir, "clone", "-q", e.url(), work); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	if out, err := e.git(work, "commit", "-q", "--allow-empty", "-m", "change"); err != nil {
		t.Fatal(out)
	}

	// Allowed: a new branch in the namespace.
	if out, err := e.git(work, "push", "-q", "origin", "HEAD:refs/heads/agent/alice/fix"); err != nil {
		t.Fatalf("push to namespace: %v\n%s", err, out)
	}
	if !e.upstreamHas("refs/heads/agent/alice/fix") {
		t.Fatal("upstream lacks the pushed branch")
	}

	// Denied: main, with a readable reason.
	out, err := e.git(work, "push", "origin", "HEAD:main")
	if err == nil || !strings.Contains(out, "pushes are allowed only under agent/alice/") || !strings.Contains(out, "[remote rejected]") {
		t.Fatalf("push to main: err=%v\n%s", err, out)
	}

	// Denied: a tag, and a mixed push (nothing lands).
	if out, err := e.git(work, "push", "origin", "HEAD:refs/tags/v1"); err == nil {
		t.Fatalf("tag push allowed:\n%s", out)
	}
	if out, err := e.git(work, "push", "origin", "HEAD:refs/heads/agent/alice/two", "HEAD:refs/heads/other"); err == nil {
		t.Fatalf("mixed push allowed:\n%s", out)
	}
	if e.upstreamHas("refs/heads/agent/alice/two") || e.upstreamHas("refs/heads/other") {
		t.Fatal("part of a denied push landed")
	}

	// Allowed: force push and delete inside the namespace.
	if out, err := e.git(work, "commit", "-q", "--amend", "--allow-empty", "-m", "amended"); err != nil {
		t.Fatal(out)
	}
	if out, err := e.git(work, "push", "-q", "--force", "origin", "HEAD:refs/heads/agent/alice/fix"); err != nil {
		t.Fatalf("force push: %v\n%s", err, out)
	}
	if out, err := e.git(work, "push", "-q", "origin", ":refs/heads/agent/alice/fix"); err != nil {
		t.Fatalf("delete: %v\n%s", err, out)
	}
	if e.upstreamHas("refs/heads/agent/alice/fix") {
		t.Fatal("delete didn't land")
	}

	// Push options: the broker stripped the capability, so git refuses.
	out, err = e.git(work, "push", "-o", "merge_request.create", "origin", "HEAD:refs/heads/agent/alice/opt")
	if err == nil || !strings.Contains(out, "push options") {
		t.Fatalf("push -o: err=%v\n%s", err, out)
	}
	if e.upstreamHas("refs/heads/agent/alice/opt") {
		t.Fatal("push with options landed")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.authErr != "" {
		t.Fatal(e.authErr)
	}
}

// A client that sends push options anyway, ignoring the advertisement.
func TestPushOptionsInBodyRejected(t *testing.T) {
	e := newGitEnv(t)
	c := brokerClient(e.addr, e.caFile)
	var b bytes.Buffer
	b.Write(pkt(zeroOID + " " + strings.Repeat("a", 40) + " refs/heads/agent/alice/x\x00report-status push-options\n"))
	b.WriteString(flushPkt)
	b.Write(pkt("merge_request.create\n"))
	b.WriteString(flushPkt)
	resp, err := c.Post("https://"+testHost+"/grp/sub/proj.git/git-receive-pack", "application/x-git-receive-pack-request", &b)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ng refs/heads/agent/alice/x push options") {
		t.Fatalf("response %q", body)
	}
}

// brokerClient trusts caFile and sends every request to addr.
func brokerClient(addr, caFile string) *http.Client {
	pem, _ := os.ReadFile(caFile)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return &http.Client{Transport: tr}
}

// --- REST, GraphQL and LFS against a fake GitLab ---

type fakeGitLab struct {
	mu   sync.Mutex
	seen []string // "METHOD path" of forwarded writes
	tok  []string
}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tok = append(f.tok, r.Header.Get("Private-Token")+"|"+r.Header.Get("Authorization"))
	if r.Method != http.MethodGet {
		f.seen = append(f.seen, r.Method+" "+r.URL.EscapedPath())
	}
	f.mu.Unlock()
	p := r.URL.EscapedPath()
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/merge_requests/1"):
		_ = json.NewEncoder(w).Encode(map[string]any{"iid": 1, "source_branch": "agent/alice/x", "source_project_id": 7, "target_project_id": 7})
	case r.Method == http.MethodGet && p == "/api/v4/projects/grp%2Fproj":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "path_with_namespace": "grp/proj"})
	case r.Method == http.MethodGet && p == "/api/v4/projects/grp%2Funknown":
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/merge_requests/2"):
		_ = json.NewEncoder(w).Encode(map[string]any{"iid": 2, "source_branch": "feature", "source_project_id": 7, "target_project_id": 7})
	case strings.HasSuffix(p, "/info/lfs/objects/batch"):
		_ = json.NewEncoder(w).Encode(map[string]any{"objects": []any{map[string]any{
			"oid": "abc", "size": 3,
			"actions": map[string]any{"download": map[string]any{
				"href":   "http://" + r.Host + "/grp/proj.git/gitlab-lfs/objects/abc",
				"header": map[string]any{"Authorization": "Basic bGZzOnNlY3JldA=="},
			}},
		}}})
	case r.Method == http.MethodGet:
		w.Header().Set("Set-Cookie", "session=x")
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

func TestGitLabAPI(t *testing.T) {
	f := &fakeGitLab{}
	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	addr, caFile, _ := brokerFor(t, up.URL, "")
	c := brokerClient(addr, caFile)
	base := "https://" + testHost

	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Private-Token", "placeholder")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.Header.Get("Set-Cookie") != "" {
			t.Errorf("%s %s: Set-Cookie passed through", method, path)
		}
		return resp.StatusCode, string(b)
	}
	cases := []struct {
		method, path, body string
		allow              bool
	}{
		{"GET", "/api/v4/projects/grp%2Fproj/merge_requests", "", true},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests", `{"title":"[agent/alice] Fix","source_branch":"agent/alice/x","target_branch":"main"}`, true},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests", `{"title":"Draft: [agent/alice] Fix","source_branch":"agent/alice/x","target_branch":"main"}`, true},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests", `{"title":"Fix","source_branch":"agent/alice/x","target_branch":"main"}`, false},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests", `{"title":"[agent/alice] Fix","source_branch":"main","target_branch":"x"}`, false},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests", `{"title":"[agent/alice] Fix","source_branch":"agent/alice/x","target_branch":"main","target_project_id":99}`, false},
		// glab sends the numeric ID of the URL's own project.
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests", `{"title":"[agent/alice] Fix","source_branch":"agent/alice/x","target_branch":"main","target_project_id":7}`, true},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests", `{"title":"Fix","source_branch":"agent/alice/x","target_branch":"main","target_project_id":7}`, false},
		{"POST", "/api/v4/projects/7/merge_requests", `{"title":"[agent/alice] Fix","source_branch":"agent/alice/x","target_branch":"main","target_project_id":8}`, false},
		{"POST", "/api/v4/projects/grp%2Funknown/merge_requests", `{"title":"[agent/alice] Fix","source_branch":"agent/alice/x","target_branch":"main","target_project_id":7}`, false},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests", `{"title":"[agent/alice] Fix","source_branch":"agent/alice/x","target_branch":"main","merge_when_pipeline_succeeds":true}`, false},
		{"PUT", "/api/v4/projects/grp%2Fproj/merge_requests/1", `{"description":"more"}`, true},
		{"PUT", "/api/v4/projects/grp%2Fproj/merge_requests/1", `{"title":"untagged"}`, false},
		{"PUT", "/api/v4/projects/grp%2Fproj/merge_requests/2", `{"title":"[agent/alice] taking over"}`, false},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests/1/notes", `{"body":"done"}`, true},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests/2/notes", `{"body":"hi"}`, false},
		{"PUT", "/api/v4/projects/grp%2Fproj/merge_requests/1/merge", `{}`, false},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests/1/approve", `{}`, false},
		{"POST", "/api/v4/projects/grp%2Fproj/repository/commits", `{"branch":"main","commit_message":"x","actions":[]}`, false},
		{"POST", "/api/v4/projects/grp%2Fproj/repository/files/a.txt", `{"branch":"agent/alice/x"}`, false},
		{"POST", "/api/v4/projects/grp%2Fproj/repository/tags", `{"tag_name":"v1","ref":"main"}`, false},
		{"POST", "/api/v4/personal_access_tokens/self/rotate", `{}`, false},
		{"POST", "/api/v4/user/keys", `{"key":"ssh-ed25519 AAA"}`, false},
		{"POST", "/api/v4/projects/grp%2Fproj/hooks", `{"url":"https://x"}`, false},
		{"DELETE", "/api/v4/projects/grp%2Fproj/repository/branches/agent%2Falice%2Fx", "", true},
		{"DELETE", "/api/v4/projects/grp%2Fproj/repository/branches/main", "", false},
		{"POST", "/api/graphql", `{"query":"query { currentUser { username } }"}`, true},
		{"POST", "/api/graphql", `{"query":"mutation { mergeRequestAccept(input:{projectPath:\"g/p\", iid:\"1\"}) { errors } }"}`, false},
		{"POST", "/api/graphql", `{"query":"query Merge { currentUser { username } } mutation Q { mergeRequestCreate(input:{}) { errors } }"}`, false},
		{"POST", "/api/graphql", `[{"query":"query { a }"},{"query":"mutation { createNote(input:{}) { errors } }"}]`, false},
		{"POST", "/api/graphql", `{"query":"query { "}`, false},
		{"POST", "/elsewhere", `{}`, false},
	}
	for _, c := range cases {
		f.mu.Lock()
		before := len(f.seen)
		f.mu.Unlock()
		code, body := do(c.method, c.path, c.body)
		f.mu.Lock()
		forwarded := len(f.seen) > before || (c.method == "GET" && code < 300)
		f.mu.Unlock()
		if c.allow && (code >= 300 || !forwarded) {
			t.Errorf("%s %s %s: %d %s, want allowed", c.method, c.path, c.body, code, body)
		}
		if !c.allow && (code != http.StatusForbidden || forwarded) {
			t.Errorf("%s %s %s: %d %s (forwarded=%v), want 403 and not forwarded", c.method, c.path, c.body, code, body, forwarded)
		}
	}
	f.mu.Lock()
	for _, tk := range f.tok {
		if !strings.Contains(tk, testToken) || strings.Contains(tk, "placeholder") {
			t.Errorf("upstream saw credentials %q", tk)
		}
	}
	f.mu.Unlock()
}

func TestLFSBatchRewritten(t *testing.T) {
	f := &fakeGitLab{}
	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	addr, caFile, _ := brokerFor(t, up.URL, "")
	c := brokerClient(addr, caFile)
	resp, err := c.Post("https://"+testHost+"/grp/proj.git/info/lfs/objects/batch", "application/vnd.git-lfs+json", strings.NewReader(`{"operation":"download","objects":[{"oid":"abc","size":3}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	s := string(b)
	if strings.Contains(s, "Authorization") || strings.Contains(s, "bGZz") {
		t.Fatalf("LFS token leaked: %s", s)
	}
	if !strings.Contains(s, "https://"+testHost+"/grp/proj.git/gitlab-lfs/objects/abc") {
		t.Fatalf("href not rewritten: %s", s)
	}
}

func TestServerChecks(t *testing.T) {
	f := &fakeGitLab{}
	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	addr, caFile, _ := brokerFor(t, up.URL, "s3ssion")
	c := brokerClient(addr, caFile)
	get := func(mod func(*http.Request)) int {
		req, _ := http.NewRequest("GET", "https://"+testHost+"/api/v4/projects", nil)
		mod(req)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(func(*http.Request) {}); code != http.StatusUnauthorized {
		t.Errorf("no session token: %d", code)
	}
	if code := get(func(r *http.Request) { r.Header.Set("Private-Token", "wrong") }); code != http.StatusUnauthorized {
		t.Errorf("wrong session token: %d", code)
	}
	if code := get(func(r *http.Request) { r.Header.Set("Private-Token", "s3ssion") }); code != http.StatusOK {
		t.Errorf("session token via PRIVATE-TOKEN: %d", code)
	}
	if code := get(func(r *http.Request) { r.SetBasicAuth("x", "s3ssion") }); code != http.StatusOK {
		t.Errorf("session token via Basic: %d", code)
	}
	// Host must match SNI.
	if code := get(func(r *http.Request) { r.Header.Set("Private-Token", "s3ssion"); r.Host = "github.broker.internal" }); code != http.StatusMisdirectedRequest {
		t.Errorf("Host/SNI mismatch: %d", code)
	}
	// Absolute-form request URI, sent raw.
	pem, _ := os.ReadFile(caFile)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: testHost})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET http://evil.example/ HTTP/1.1\r\nHost: "+testHost+"\r\nPrivate-Token: s3ssion\r\nConnection: close\r\n\r\n")
	line, _ := io.ReadAll(io.LimitReader(conn, 64))
	if !bytes.HasPrefix(line, []byte("HTTP/1.1 400")) {
		t.Errorf("absolute-form: %q", line)
	}
	// Unknown SNI gets no certificate.
	if _, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: "gitlab.com"}); err == nil {
		t.Error("handshake succeeded for gitlab.com")
	}
}

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

// sbx mode: plain HTTP on one port per provider, any Host, a session token.
func TestSbxMode(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Private-Token") != testToken {
			http.Error(w, "bad token", 401)
			return
		}
		w.Header().Set("Location", "http://"+r.Host+"/api/v4/projects/1")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	var srv *httptest.Server
	gl := &GitLab{User: testUser, Token: func() (string, error) { return testToken, nil }, Upstream: u,
		PublicScheme: "http", Client: NewUpstreamClient()}
	srv = httptest.NewServer(&Server{Only: gl, SessionToken: "s3ssion"})
	t.Cleanup(srv.Close)
	gl.PublicHost = "host.docker.internal:" + srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(tok string) *http.Response {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v4/projects/1", nil)
		req.Host = gl.PublicHost
		if tok != "" {
			req.Header.Set("Private-Token", tok)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := get(""); r.StatusCode != 401 {
		t.Errorf("no session token: %d", r.StatusCode)
	}
	if r := get("wrong"); r.StatusCode != 401 {
		t.Errorf("wrong session token: %d", r.StatusCode)
	}
	r := get("s3ssion")
	if r.StatusCode != http.StatusFound || r.Header.Get("Location") != "http://"+gl.PublicHost+"/api/v4/projects/1" {
		t.Errorf("with session token: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
}
