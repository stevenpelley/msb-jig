package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAS is an MCP server at /mcp and its authorization server, on one
// host: protected-resource metadata, AS metadata, registration,
// authorization (redirecting straight back, as a signed-in browser
// would), and a token endpoint that rotates refresh tokens and refuses a
// spent one.
type fakeAS struct {
	*httptest.Server
	t          *testing.T
	quoted     bool   // WWW-Authenticate's resource_metadata quoted
	noS256     bool   // leave S256 out of the AS metadata
	issuer     string // the AS metadata's issuer, if not the server's URL
	secret     bool   // registration makes a confidential client
	resource   string // the resource metadata's resource, if not the server's
	expiresIn  int
	mu         sync.Mutex
	clients    map[string]string // client id: redirect URI
	codes      map[string]string // code: code challenge
	live       map[string]bool   // refresh tokens that work
	n          int
	refreshes  int
	lastScope  string
	lastResour string
}

func newFakeAS(t *testing.T) *fakeAS {
	f := &fakeAS{t: t, clients: map[string]string{}, codes: map[string]string{}, live: map[string]bool{}, expiresIn: 3600}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAS) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	js := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	switch r.URL.Path {
	case "/mcp":
		prm := f.URL + "/.well-known/oauth-protected-resource"
		if f.quoted {
			prm = `"` + prm + `"`
		}
		w.Header().Set("WWW-Authenticate", "Bearer resource_metadata="+prm)
		w.WriteHeader(http.StatusUnauthorized)
	case "/.well-known/oauth-protected-resource":
		res := f.URL + "/mcp"
		if f.resource != "" {
			res = f.resource
		}
		js(map[string]any{"resource": res, "authorization_servers": []string{f.URL}, "scopes_supported": []string{"read:all", "offline_access", "write:all"}})
	case "/.well-known/oauth-authorization-server":
		methods := []string{"S256"}
		if f.noS256 {
			methods = []string{"plain"}
		}
		iss := f.URL
		if f.issuer != "" {
			iss = f.issuer
		}
		js(map[string]any{"issuer": iss, "authorization_endpoint": f.URL + "/authorize", "token_endpoint": f.URL + "/token",
			"registration_endpoint": f.URL + "/register", "code_challenge_methods_supported": methods})
	case "/register":
		var req struct {
			RedirectURIs []string `json:"redirect_uris"`
			AuthMethod   string   `json:"token_endpoint_auth_method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.n++
		id := fmt.Sprintf("client-%d", f.n)
		f.clients[id] = req.RedirectURIs[0]
		w.WriteHeader(http.StatusCreated)
		if f.secret {
			js(map[string]any{"client_id": id, "client_secret": "s", "token_endpoint_auth_method": "client_secret_post"})
			return
		}
		js(map[string]any{"client_id": id, "token_endpoint_auth_method": req.AuthMethod})
	case "/authorize":
		q := r.URL.Query()
		redirect, ok := f.clients[q.Get("client_id")]
		if !ok || q.Get("redirect_uri") != redirect || q.Get("code_challenge_method") != "S256" {
			http.Error(w, "bad authorization request", http.StatusBadRequest)
			return
		}
		f.lastScope, f.lastResour = q.Get("scope"), q.Get("resource")
		f.n++
		code := fmt.Sprintf("code-%d", f.n)
		f.codes[code] = q.Get("code_challenge")
		http.Redirect(w, r, redirect+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	case "/token":
		_ = r.ParseForm()
		fail := func() {
			w.WriteHeader(http.StatusBadRequest)
			js(map[string]string{"error": "invalid_grant", "error_description": "no"})
		}
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			ch, ok := f.codes[r.PostForm.Get("code")]
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != ch || r.PostForm.Get("redirect_uri") != f.clients[r.PostForm.Get("client_id")] {
				fail()
				return
			}
			delete(f.codes, r.PostForm.Get("code"))
		case "refresh_token":
			rt := r.PostForm.Get("refresh_token")
			if !f.live[rt] {
				// Reuse of a spent token: revoke the family, as servers that
				// detect reuse do.
				f.live = map[string]bool{}
				fail()
				return
			}
			delete(f.live, rt)
			f.refreshes++
		default:
			fail()
			return
		}
		f.n++
		rt := fmt.Sprintf("refresh-%d", f.n)
		f.live[rt] = true
		js(map[string]any{"access_token": fmt.Sprintf("access-%d", f.n), "token_type": "Bearer", "refresh_token": rt, "expires_in": f.expiresIn})
	default:
		http.NotFound(w, r)
	}
}

func TestDiscover(t *testing.T) {
	for _, quoted := range []bool{false, true} {
		f := newFakeAS(t)
		f.quoted = quoted
		md, err := Discover(context.Background(), f.Client(), f.URL+"/mcp")
		if err != nil {
			t.Fatal(err)
		}
		if md.Resource != f.URL+"/mcp" || md.Issuer != f.URL || md.TokenEndpoint != f.URL+"/token" || md.RegistrationEndpoint != f.URL+"/register" {
			t.Errorf("quoted=%v: %+v", quoted, md)
		}
	}
	for name, set := range map[string]func(*fakeAS){
		"no S256":         func(f *fakeAS) { f.noS256 = true },
		"issuer mismatch": func(f *fakeAS) { f.issuer = "https://evil.example" },
		"other resource":  func(f *fakeAS) { f.resource = "https://evil.example/mcp" },
	} {
		f := newFakeAS(t)
		set(f)
		if _, err := Discover(context.Background(), f.Client(), f.URL+"/mcp"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := Discover(context.Background(), http.DefaultClient, "http://example.org/mcp"); err == nil {
		t.Error("http URL: no error")
	}
}

// signIn registers and signs in, the browser following the redirect.
func signIn(t *testing.T, f *fakeAS) (Metadata, Client, Tokens) {
	t.Helper()
	ctx := context.Background()
	md, err := Discover(ctx, f.Client(), f.URL+"/mcp")
	if err != nil {
		t.Fatal(err)
	}
	l, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := Register(ctx, f.Client(), md, "mono-jig test", RedirectURI(l.Addr().(*net.TCPAddr).Port), []string{"read:all", "offline_access"})
	if err != nil {
		t.Fatal(err)
	}
	browser := f.Client() // follows the redirect to the loopback
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tok, err := Authorize(ctx, f.Client(), md, cl, []string{"read:all", "offline_access"}, l, func(u string) {
		go func() {
			resp, err := browser.Get(u)
			if err == nil {
				resp.Body.Close()
			}
		}()
	})
	if err != nil {
		t.Fatal(err)
	}
	return md, cl, tok
}

func TestSignIn(t *testing.T) {
	f := newFakeAS(t)
	_, cl, tok := signIn(t, f)
	if !strings.HasPrefix(tok.Access, "access-") || !strings.HasPrefix(tok.Refresh, "refresh-") || time.Until(tok.Expires) < 59*time.Minute {
		t.Errorf("tokens %+v", tok)
	}
	if cl.Port() == 0 || !strings.HasPrefix(cl.RedirectURI, "http://127.0.0.1:") {
		t.Errorf("client %+v", cl)
	}
	if f.lastScope != "read:all offline_access" || f.lastResour != f.URL+"/mcp" {
		t.Errorf("authorize scope %q resource %q", f.lastScope, f.lastResour)
	}
}

func TestRegisterRefusesConfidentialClient(t *testing.T) {
	f := newFakeAS(t)
	f.secret = true
	md, _ := Discover(context.Background(), f.Client(), f.URL+"/mcp")
	if _, err := Register(context.Background(), f.Client(), md, "x", RedirectURI(1234), nil); err == nil {
		t.Error("no error")
	}
}

// A wrong state on the redirect is ignored, not taken.
func TestAuthorizeChecksState(t *testing.T) {
	f := newFakeAS(t)
	md, _ := Discover(context.Background(), f.Client(), f.URL+"/mcp")
	l, _ := Listen(0)
	cl := Client{ID: "c", RedirectURI: RedirectURI(l.Addr().(*net.TCPAddr).Port)}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := Authorize(ctx, f.Client(), md, cl, nil, l, func(string) {
		go func() {
			resp, err := http.Get(cl.RedirectURI + "?code=x&state=forged")
			if err == nil {
				resp.Body.Close()
			}
		}()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err %v", err)
	}
}

type memStore struct {
	mu    sync.Mutex
	token string
	saves int
}

func (m *memStore) Load() (string, error) { m.mu.Lock(); defer m.mu.Unlock(); return m.token, nil }
func (m *memStore) Save(s string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.token = s
	m.saves++
	return nil
}

func TestSource(t *testing.T) {
	f := newFakeAS(t)
	md, cl, tok := signIn(t, f)
	store := &memStore{token: tok.Refresh}
	var lock sync.Mutex
	locker := func() (func(), error) { lock.Lock(); return lock.Unlock, nil }
	// Two brokers (two sandboxes) share the store and the lock.
	a := &Source{TokenEndpoint: md.TokenEndpoint, ClientID: cl.ID, Resource: md.Resource, Store: store, Lock: locker, Client: f.Client(), Login: "mono-jig login example"}
	b := &Source{TokenEndpoint: md.TokenEndpoint, ClientID: cl.ID, Resource: md.Resource, Store: store, Lock: locker, Client: f.Client(), Login: "mono-jig login example"}
	t1, err := a.Token()
	if err != nil {
		t.Fatal(err)
	}
	if t2, _ := a.Token(); t2 != t1 || f.refreshes != 1 {
		t.Errorf("not cached: %q %q, %d refreshes", t1, t2, f.refreshes)
	}
	if _, err := b.Token(); err != nil {
		t.Fatalf("second broker: %v", err)
	}
	if f.refreshes != 2 || store.saves != 2 {
		t.Errorf("%d refreshes, %d saves", f.refreshes, store.saves)
	}
	// Short-lived tokens are refreshed, concurrently, without spending a
	// rotated token twice.
	f.mu.Lock()
	f.expiresIn = 30
	f.mu.Unlock()
	a.expires, b.expires = time.Now(), time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 10; i++ {
		for _, s := range []*Source{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.Token(); err != nil {
					errs <- err
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// A revoked sign-in says to log in again.
	f.mu.Lock()
	f.live = map[string]bool{}
	f.mu.Unlock()
	a.access = ""
	if _, err := a.Token(); err == nil || !strings.Contains(err.Error(), "run mono-jig login example") {
		t.Errorf("revoked: %v", err)
	}
}
