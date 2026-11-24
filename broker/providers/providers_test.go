package providers

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"msb-jig/broker"
)

// upstream records what reached it, and answers with a Location on its
// own host, which proxy mode must leave alone.
type upstream struct {
	*httptest.Server
	mu  sync.Mutex
	got []string // "METHOD path auth"
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization") + r.Header.Get("X-Api-Key")
		u.mu.Lock()
		u.got = append(u.got, r.Method+" "+r.URL.Path+" "+auth)
		u.mu.Unlock()
		w.Header().Set("Location", u.URL+"/next")
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) last() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.got) == 0 {
		return ""
	}
	g := u.got[len(u.got)-1]
	u.got = nil
	return g
}

func TestProxyMode(t *testing.T) {
	gh, gl, an := newUpstream(t), newUpstream(t), newUpstream(t)
	c := Config{Proxy: true, Providers: []Provider{
		GitLab{User: "alice", TokenCmd: "echo gltok", URL: gl.URL},
		GitHub{User: "alice", TokenCmd: "echo ghtok", GitURL: gh.URL, APIURL: gh.URL},
		Anthropic{TokenCmd: "echo sk-ant-api-key", APIURL: an.URL},
	}}
	glHost := strings.TrimPrefix(strings.Split(gl.URL, ":")[1], "//")
	if got, want := c.Hosts(), []string{glHost, "github.com", "api.github.com", "api.anthropic.com"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Hosts = %q, want %q", got, want)
	}
	ps, err := Build(context.Background(), c, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for k := range ps {
		keys = append(keys, k)
	}
	if len(ps) != 4 {
		t.Errorf("hosts served: %q", keys)
	}
	srv := &broker.Server{Providers: ps}
	do := func(method, host, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://"+host+path, strings.NewReader(body))
		r.RequestURI = path
		r.Header.Set("Authorization", "token placeholder")
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}
	cases := []struct {
		method, host, path, body string
		code                     int
		up                       *upstream
		want                     string // what the upstream got
	}{
		// api.github.com, mapped to the routes' /api/v3 and /api/graphql.
		{"GET", "api.github.com", "/repos/o/r/pulls/1", "", 200, gh, "GET /repos/o/r/pulls/1 Bearer ghtok"},
		{"POST", "api.github.com", "/graphql", `{"query":"query { viewer { login } }"}`, 200, gh, "POST /graphql Bearer ghtok"},
		{"PUT", "api.github.com", "/repos/o/r/pulls/1/merge", "", 403, gh, ""},
		// github.com: git with the credential; other reads without it.
		{"POST", "github.com", "/o/r/git-upload-pack", "", 200, gh, "POST /o/r.git/git-upload-pack Basic YWxpY2U6Z2h0b2s="},
		{"GET", "github.com", "/o/r/archive/refs/heads/main.tar.gz", "", 200, gh, "GET /o/r/archive/refs/heads/main.tar.gz "},
		{"POST", "github.com", "/login/oauth/access_token", "", 403, gh, ""},
		// Each host has its own table: github.com doesn't serve the API.
		{"GET", "github.com", "/api/v3/user", "", 200, gh, "GET /api/v3/user "},
		{"POST", "github.com", "/api/graphql", `{"query":"query { viewer { login } }"}`, 403, gh, ""},
		{"POST", "api.github.com", "/o/r/git-receive-pack", "", 403, gh, ""},
		// GitLab: the API with the credential, other reads without it.
		{"GET", glHost, "/api/v4/projects/1", "", 200, gl, "GET /api/v4/projects/1 Bearer gltok"},
		{"GET", glHost, "/g/p/-/raw/main/README.md", "", 200, gl, "GET /g/p/-/raw/main/README.md "},
		{"POST", glHost, "/users/sign_in", "", 403, gl, ""},
		// Anthropic, unchanged.
		{"POST", "api.anthropic.com", "/v1/messages", `{}`, 200, an, "POST /v1/messages sk-ant-api-key"},
		// Server mode's names aren't served.
		{"GET", "github.broker.internal", "/api/v3/user", "", 404, gh, ""},
	}
	for _, tc := range cases {
		w := do(tc.method, tc.host, tc.path, tc.body)
		if w.Code != tc.code {
			t.Errorf("%s %s%s: %d %s", tc.method, tc.host, tc.path, w.Code, w.Body)
		}
		if got := tc.up.last(); got != tc.want {
			t.Errorf("%s %s%s: upstream got %q, want %q", tc.method, tc.host, tc.path, got, tc.want)
		}
		if tc.code == 200 && w.Header().Get("Location") != tc.up.URL+"/next" {
			t.Errorf("%s %s%s: Location rewritten to %q", tc.method, tc.host, tc.path, w.Header().Get("Location"))
		}
	}
}

func TestServerModeDeniesUnlistedReads(t *testing.T) {
	gh := newUpstream(t)
	c := Config{Providers: []Provider{GitHub{User: "alice", TokenCmd: "echo ghtok", GitURL: gh.URL, APIURL: gh.URL}}}
	ps, err := Build(context.Background(), c, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "https://"+GitHubHost+"/o/r/archive/main.tar.gz", nil)
	r.RequestURI = r.URL.Path
	w := httptest.NewRecorder()
	(&broker.Server{Providers: ps}).ServeHTTP(w, r)
	if w.Code != 403 || gh.last() != "" {
		t.Errorf("server mode forwarded an unlisted read: %d", w.Code)
	}
	r = httptest.NewRequest("GET", "https://"+GitHubHost+"/api/v3/user", nil)
	r.RequestURI = r.URL.Path
	w = httptest.NewRecorder()
	(&broker.Server{Providers: ps}).ServeHTTP(w, r)
	if loc := w.Header().Get("Location"); loc != "https://"+GitHubHost+"/api/v3/next" {
		t.Errorf("server mode: Location %q", loc)
	}
}
