package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"msb-jig/broker"
	"msb-jig/broker/providers"
)

// sbx mode: plain HTTP, Host host.docker.internal:<port>, a session
// token, redirects pointed back at the sandbox's address.
func TestSbxMode(t *testing.T) {
	const tok = "real-token"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok || r.Header.Get("Private-Token") != "" {
			http.Error(w, "bad token", 401)
			return
		}
		w.Header().Set("Location", "http://"+r.Host+"/api/v4/projects/1")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(up.Close)
	pub := &url.URL{Scheme: "http", Host: "host.docker.internal:18443"}
	p := build(providers.GitLab{User: "alice", URL: up.URL, TokenCmd: "echo " + tok}, pub, nil)[providers.GitLabHost]
	srv := httptest.NewServer(sessionCheck("s3ssion", &broker.Server{Providers: map[string]http.Handler{pub.Hostname(): p}}))
	t.Cleanup(srv.Close)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(mod func(*http.Request)) *http.Response {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v4/projects/1", nil)
		req.Host = pub.Host
		mod(req)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := get(func(*http.Request) {}); r.StatusCode != 401 {
		t.Errorf("no session token: %d", r.StatusCode)
	}
	if r := get(func(r *http.Request) { r.Header.Set("Private-Token", "wrong") }); r.StatusCode != 401 {
		t.Errorf("wrong session token: %d", r.StatusCode)
	}
	if r := get(func(r *http.Request) { r.SetBasicAuth("x", "s3ssion") }); r.StatusCode != http.StatusFound {
		t.Errorf("session token via Basic: %d", r.StatusCode)
	}
	r := get(func(r *http.Request) { r.Header.Set("Private-Token", "s3ssion") })
	if r.StatusCode != http.StatusFound || r.Header.Get("Location") != "http://"+pub.Host+"/api/v4/projects/1" {
		t.Errorf("with session token: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
}
