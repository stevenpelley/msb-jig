// Package brokertest serves providers for tests: over TLS under a fresh
// CA, with a git-http-backend upstream for git end to end.
package brokertest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
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

	"mcp-restrict/broker"
)

const (
	User  = "alice"
	Token = "real-token-for-test"
)

// TokenFunc returns Token.
func TokenFunc() (string, error) { return Token, nil }

// Public is the provider's public URL for host.
func Public(host string) *url.URL { return &url.URL{Scheme: "https", Host: host} }

// Serve serves p as host over TLS with a fresh CA, as the supervisor
// would. It returns the address and the CA file.
func Serve(t *testing.T, host string, p http.Handler) (addr, caFile string) {
	t.Helper()
	ca, err := broker.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(&broker.Server{Providers: map[string]http.Handler{host: p}})
	srv.TLS = ca.TLSConfig([]string{host})
	srv.StartTLS()
	t.Cleanup(srv.Close)
	caFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, ca.CertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv.Listener.Addr().String(), caFile
}

// Client trusts caFile and sends every request to addr.
func Client(addr, caFile string) *http.Client {
	pem, _ := os.ReadFile(caFile)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return &http.Client{Transport: tr}
}

// GitEnv is a bare repository behind a git-http-backend upstream that
// wants Basic User:Token, and a broker in front of it.
type GitEnv struct {
	t       *testing.T
	Host    string // the broker's name for the provider
	Repo    string // the repository's path on it
	Dir     string
	Addr    string
	CAFile  string
	bare    string
	mu      sync.Mutex
	authErr string
}

// NewGitEnv serves a bare repository at repo, and the broker serve
// returns for its upstream, as host.
func NewGitEnv(t *testing.T, host, repo string, serve func(upstream *url.URL) (addr, caFile string)) *GitEnv {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	execPath, _ := exec.Command("git", "--exec-path").Output()
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not found")
	}
	e := &GitEnv{t: t, Dir: t.TempDir(), Host: host, Repo: repo}
	root := filepath.Join(e.Dir, "upstream")
	e.bare = filepath.Join(root, filepath.FromSlash(repo)+".git")
	e.run("", "git", "init", "-q", "--bare", "-b", "main", e.bare)
	e.run(e.bare, "git", "config", "http.receivepack", "true")
	e.run(e.bare, "git", "config", "receive.advertisePushOptions", "true")
	seed := filepath.Join(e.Dir, "seed")
	e.run("", "git", "init", "-q", "-b", "main", seed)
	e.run(seed, "git", "commit", "-q", "--allow-empty", "-m", "init")
	e.run(seed, "git", "push", "-q", e.bare, "main")

	cgiH := &cgi.Handler{Path: backend, Root: "/", Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(User+":"+Token))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != want {
			e.mu.Lock()
			e.authErr = "upstream got Authorization " + got
			e.mu.Unlock()
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Go's CGI handler refuses chunked bodies; the providers take them.
		if r.ContentLength < 0 {
			b, _ := io.ReadAll(r.Body)
			r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
			r.TransferEncoding = nil
		}
		cgiH.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	e.Addr, e.CAFile = serve(u)
	return e
}

func (e *GitEnv) env() []string {
	_, port, _ := net.SplitHostPort(e.Addr)
	var env []string
	for _, kv := range os.Environ() {
		k := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
		if strings.HasSuffix(k, "_PROXY") || strings.HasPrefix(k, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "HOME="+e.Dir,
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=http.sslCAInfo", "GIT_CONFIG_VALUE_0="+e.CAFile,
		"GIT_CONFIG_KEY_1=http.curloptResolve", "GIT_CONFIG_VALUE_1="+e.Host+":"+port+":127.0.0.1",
		"GIT_CONFIG_KEY_2=user.email", "GIT_CONFIG_VALUE_2=a@example.invalid",
		"GIT_AUTHOR_NAME=a", "GIT_COMMITTER_NAME=a",
	)
}

// URL is the repository's URL through the broker.
func (e *GitEnv) URL() string {
	_, port, _ := net.SplitHostPort(e.Addr)
	return "https://" + e.Host + ":" + port + "/" + e.Repo + ".git"
}

// Git runs git in dir against the broker.
func (e *GitEnv) Git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = e.env()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *GitEnv) run(dir string, args ...string) {
	e.t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=a", "GIT_COMMITTER_NAME=a", "GIT_AUTHOR_EMAIL=a@x", "GIT_COMMITTER_EMAIL=a@x")
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

// UpstreamHas reports whether the bare repository has ref.
func (e *GitEnv) UpstreamHas(ref string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "-q", ref)
	cmd.Dir = e.bare
	return cmd.Run() == nil
}

// AuthErr is the first wrong credential the upstream saw, if any.
func (e *GitEnv) AuthErr() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.authErr
}

// PushRules runs the push rules every code-host provider shares: pushes
// only under agent/alice/, force pushes and deletes there, nothing else
// lands, push options refused.
func (e *GitEnv) PushRules(t *testing.T, cloneURL string) {
	t.Helper()
	work := filepath.Join(e.Dir, "work")
	if out, err := e.Git(e.Dir, "clone", "-q", cloneURL, work); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	if out, err := e.Git(work, "commit", "-q", "--allow-empty", "-m", "change"); err != nil {
		t.Fatal(out)
	}
	if out, err := e.Git(work, "push", "-q", "origin", "HEAD:refs/heads/agent/alice/fix"); err != nil {
		t.Fatalf("push to namespace: %v\n%s", err, out)
	}
	if !e.UpstreamHas("refs/heads/agent/alice/fix") {
		t.Fatal("upstream lacks the pushed branch")
	}
	out, err := e.Git(work, "push", "origin", "HEAD:main")
	if err == nil || !strings.Contains(out, "pushes are allowed only under agent/alice/") || !strings.Contains(out, "[remote rejected]") {
		t.Fatalf("push to main: err=%v\n%s", err, out)
	}
	if out, err := e.Git(work, "push", "origin", "HEAD:refs/tags/v1"); err == nil {
		t.Fatalf("tag push allowed:\n%s", out)
	}
	if out, err := e.Git(work, "push", "origin", "HEAD:refs/heads/agent/alice/two", "HEAD:refs/heads/other"); err == nil {
		t.Fatalf("mixed push allowed:\n%s", out)
	}
	if e.UpstreamHas("refs/heads/agent/alice/two") || e.UpstreamHas("refs/heads/other") {
		t.Fatal("part of a denied push landed")
	}
	if out, err := e.Git(work, "commit", "-q", "--amend", "--allow-empty", "-m", "amended"); err != nil {
		t.Fatal(out)
	}
	if out, err := e.Git(work, "push", "-q", "--force", "origin", "HEAD:refs/heads/agent/alice/fix"); err != nil {
		t.Fatalf("force push: %v\n%s", err, out)
	}
	if out, err := e.Git(work, "push", "-q", "origin", ":refs/heads/agent/alice/fix"); err != nil {
		t.Fatalf("delete: %v\n%s", err, out)
	}
	if e.UpstreamHas("refs/heads/agent/alice/fix") {
		t.Fatal("delete didn't land")
	}
	out, err = e.Git(work, "push", "-o", "merge_request.create", "origin", "HEAD:refs/heads/agent/alice/opt")
	if err == nil || !strings.Contains(out, "push options") {
		t.Fatalf("push -o: err=%v\n%s", err, out)
	}
	if e.UpstreamHas("refs/heads/agent/alice/opt") {
		t.Fatal("push with options landed")
	}
	if a := e.AuthErr(); a != "" {
		t.Fatal(a)
	}
}
