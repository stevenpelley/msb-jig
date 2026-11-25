package jira_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stevenpelley/msb-jig/broker/brokertest"
	"github.com/stevenpelley/msb-jig/broker/providers"
)

const (
	site    = "example.atlassian.net"
	cloudID = "01234567-89ab-cdef-0123-456789abcdef"
	email   = "alice@example.org"
)

// fakeGateway is Atlassian's API gateway: it records what reached it.
type fakeGateway struct {
	mu   sync.Mutex
	seen []string // "METHOD path?query auth"
}

func (f *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "{}")
}

func (f *fakeGateway) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.seen
	f.seen = nil
	return s
}

func env(t *testing.T, proxy bool) (*http.Client, string, *fakeGateway) {
	t.Helper()
	f := &fakeGateway{}
	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	p := providers.Jira{Site: site, CloudID: cloudID, Email: email, TokenCmd: brokertest.TokenCmd, APIURL: up.URL + "/ex/jira/" + cloudID}
	ps, err := providers.Build(context.Background(), providers.Config{Proxy: proxy, Providers: []providers.Provider{p}}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := providers.JiraHost
	if proxy {
		h = site
	}
	addr, caFile := brokertest.Serve(t, h, ps[h])
	return brokertest.Client(addr, caFile), "https://" + h, f
}

func TestRoutes(t *testing.T) {
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+brokertest.Token))
	for _, proxy := range []bool{false, true} {
		c, base, f := env(t, proxy)
		for _, cs := range []struct {
			method, path string
			allow        bool
			upstream     string // the path and query that reach the gateway, if not the same
		}{
			{"GET", "/rest/api/3/issue/KEY-1?fields=summary", true, ""},
			{"GET", "/rest/api/3/issue/KEY-1?updateHistory=true&fields=summary", true, "/rest/api/3/issue/KEY-1?fields=summary"},
			{"GET", "/rest/api/2/myself", true, ""},
			{"GET", "/rest/agile/1.0/board/1/sprint", true, ""},
			{"HEAD", "/rest/api/3/project", true, ""},
			{"POST", "/rest/api/3/search/jql", true, ""},
			{"POST", "/rest/api/3/issue/KEY-1/changelog/list", true, ""},
			{"POST", "/rest/api/2/expression/evaluate", true, ""},
			{"GET", "/rest/api/1/issue/KEY-1", false, ""},
			{"POST", "/rest/api/3/issue", false, ""},
			{"PUT", "/rest/api/3/issue/KEY-1", false, ""},
			{"DELETE", "/rest/api/3/issue/KEY-1", false, ""},
			{"POST", "/rest/api/3/issue/KEY-1/comment", false, ""},
			{"POST", "/rest/api/3/issue/KEY-1/transitions", false, ""},
			{"POST", "/rest/api/3/search/jql;x", false, ""},
			{"POST", "/rest/agile/1.0/sprint/1/issue", false, ""},
			{"GET", "/rest/../rest/api/3/myself", false, ""},
			{"GET", "/secure/Dashboard.jspa", false, ""},
			{"POST", "/rest/api/3/issue/KEY-1/changelog/list/x", false, ""},
		} {
			req, _ := http.NewRequest(cs.method, base+cs.path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer placeholder")
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			got := f.take()
			if !cs.allow {
				if resp.StatusCode < 400 || len(got) != 0 {
					t.Errorf("proxy=%v %s %s: %d, reached %v", proxy, cs.method, cs.path, resp.StatusCode, got)
				}
				continue
			}
			want := cs.upstream
			if want == "" {
				want = cs.path
			}
			if len(got) != 1 || got[0] != cs.method+" /ex/jira/"+cloudID+want+" "+basic {
				t.Errorf("proxy=%v %s %s: %d, upstream saw %q", proxy, cs.method, cs.path, resp.StatusCode, got)
			}
		}
	}
}

func TestConfigChecked(t *testing.T) {
	for _, p := range []providers.Jira{
		{Site: "", CloudID: cloudID, Email: email},
		{Site: site, CloudID: "../x", Email: email},
		{Site: site, CloudID: cloudID},
	} {
		if _, err := providers.Build(context.Background(), providers.Config{Providers: []providers.Provider{p}}, "", nil); err == nil {
			t.Errorf("%+v: no error", p)
		}
	}
}

func TestGuest(t *testing.T) {
	p := providers.Jira{Site: site, CloudID: cloudID, Email: email}
	for proxy, want := range map[bool]string{false: "https://" + providers.JiraHost, true: "https://" + site} {
		g := p.Guest(proxy)
		if g.Env["JIRA_URL"] != want || !strings.Contains(g.Instructions[0], want) || !strings.Contains(g.Instructions[0], email) {
			t.Errorf("proxy=%v: %v\n%s", proxy, g.Env, g.Instructions[0])
		}
		if err := g.Validate(); err != nil {
			t.Error(err)
		}
	}
}
