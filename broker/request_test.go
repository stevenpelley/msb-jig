package broker

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBadPath(t *testing.T) {
	for _, p := range []string{"/", "/api/v4/projects/grp%2Fproj/merge_requests/1", "/a/.b/c.", "/a/...", "/v1/messages/"} {
		if why := badPath(p); why != "" {
			t.Errorf("badPath(%q) = %q, want ok", p, why)
		}
	}
	for _, p := range []string{
		"/a/../b", "/a/./b", "/a/..", "/a/%2e%2e/b", "/a/%2E./b", "/a/.%2e",
		"/api/v4/projects/grp%2F..%2Fother/merge_requests", // inside an encoded segment
		"/a/%5c..%5cb", // backslashes, which some servers read as slashes
		"/a/%zz",
	} {
		if badPath(p) == "" {
			t.Errorf("badPath(%q) passed", p)
		}
	}
}

// The server refuses dot segments before any provider sees them.
func TestServerRefusesDotSegments(t *testing.T) {
	reached := false
	s := &Server{Providers: map[string]http.Handler{"x.broker.internal": http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })}}
	for _, p := range []string{"/api/v4/projects/1/merge_requests/7/notes/..", "/a/%2e%2e/merge"} {
		r := httptest.NewRequest("PUT", p, nil)
		r.Host = "x.broker.internal"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || reached || !strings.Contains(w.Body.String(), "segments") {
			t.Errorf("%s: %d %s, reached=%v", p, w.Code, w.Body, reached)
		}
	}
}

// Method overrides are dropped before a provider sees the request.
func TestServerDropsMethodOverride(t *testing.T) {
	var got http.Header
	s := &Server{Providers: map[string]http.Handler{"x.broker.internal": http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Header })}}
	r := httptest.NewRequest("POST", "/a", nil)
	r.Host = "x.broker.internal"
	r.Header.Set("X-HTTP-Method-Override", "PUT")
	r.Header.Set("X-Method-Override", "PUT")
	s.ServeHTTP(httptest.NewRecorder(), r)
	if got == nil || got.Get("X-HTTP-Method-Override") != "" || got.Get("X-Method-Override") != "" {
		t.Errorf("headers reached the provider: %v", got)
	}
}

func TestDecodeJSON(t *testing.T) {
	var v struct {
		Name string `json:"name"`
	}
	for _, b := range []string{
		`{"name":"a","Name":"b"}`,
		`{"name":"a","name":"b"}`,
		`{"params":{"name":"a","NAME":"b"}}`, // at any depth
		`[{"x":1,"X":2}]`,
		"{\"kind\":1,\"Kind\":2}", // the Kelvin sign folds to k
	} {
		if err := DecodeJSON([]byte(b), &v); err == nil {
			t.Errorf("DecodeJSON(%s) passed", b)
		}
	}
	if err := DecodeJSON([]byte(`{"name":"a","names":["b"],"x":{"name":"c"}}`), &v); err != nil || v.Name != "a" {
		t.Errorf("DecodeJSON: %v, %q", err, v.Name)
	}
}

func TestReadJSONBody(t *testing.T) {
	call := func(target, body string) *Call {
		r := httptest.NewRequest("POST", target, strings.NewReader(body))
		return &Call{R: r, Body: r.Body}
	}
	if _, err := call("/api/graphql?query=mutation", `{"query":"query{a}"}`).ReadJSONBody(); err == nil {
		t.Error("URL parameters passed")
	}
	if _, err := call("/api/graphql", `{"query":"mutation{m}","QUERY":"query{a}"}`).ReadJSONBody(); err == nil {
		t.Error("a case-variant key passed")
	}
	c := call("/api/graphql", `{"query":"query{a}"}`)
	b, err := c.ReadJSONBody()
	if err != nil || string(b) != `{"query":"query{a}"}` {
		t.Errorf("ReadJSONBody = %s, %v", b, err)
	}
	if rest, _ := io.ReadAll(c.Body); string(rest) != string(b) {
		t.Errorf("body not left for the forward: %q", rest)
	}
	if b, err := call("/x", " ").ReadJSONBody(); b != nil || err != nil {
		t.Errorf("empty body: %q, %v", b, err)
	}
}
