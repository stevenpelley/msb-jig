package github_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stevenpelley/msb-jig/broker/brokertest"
	"github.com/stevenpelley/msb-jig/broker/providers"
)

// ownersUpstream is github.com and api.github.com in one: it records the
// credential each forwarded request came with, and answers the broker's
// lookups (a pull request from an agent branch, its node and owner).
type ownersUpstream struct {
	mu   sync.Mutex
	seen map[string]string // "METHOD path" -> Authorization
}

func (u *ownersUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.EscapedPath()
	if r.Method == http.MethodPost && p == "/graphql" {
		b, _ := io.ReadAll(r.Body)
		var q struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(b, &q)
		if strings.Contains(q.Query, "node(id: $id)") {
			owner := map[string]string{"PR_mine": "example-org", "PR_other": "other", "R_mine": "Example-Org", "R_other": "other"}[q.Variables["id"].(string)]
			node := map[string]any{"headRefName": "agent/alice/x", "isCrossRepository": false,
				"repository": map[string]any{"owner": map[string]any{"login": owner}}}
			if strings.HasPrefix(q.Variables["id"].(string), "R_") {
				node = map[string]any{"owner": map[string]any{"login": owner}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": node}})
			return
		}
	}
	if r.Method == http.MethodGet && strings.HasSuffix(p, "/pulls/1") {
		repo := strings.TrimSuffix(strings.TrimPrefix(p, "/repos/"), "/pulls/1")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"head": map[string]any{"ref": "agent/alice/x", "repo": map[string]any{"full_name": repo}},
			"base": map[string]any{"repo": map[string]any{"full_name": repo}}})
		return
	}
	u.mu.Lock()
	u.seen[r.Method+" "+p] = r.Header.Get("Authorization")
	u.mu.Unlock()
	_, _ = w.Write([]byte(`{}`))
}

func TestOwners(t *testing.T) {
	up := &ownersUpstream{seen: map[string]string{}}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	ps, err := providers.Build(context.Background(), providers.Config{Providers: []providers.Provider{providers.GitHub{
		User: brokertest.User, TokenCmd: brokertest.TokenCmd, Owners: []string{"example-org"},
		GitURL: srv.URL, APIURL: srv.URL}}}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	addr, caFile := brokertest.Serve(t, ghHost, ps[ghHost])
	c := brokertest.Client(addr, caFile)
	gql := func(q, vars string) string {
		b, _ := json.Marshal(q)
		return `{"query":` + string(b) + `,"variables":` + vars + `}`
	}
	const bearer, basic, none = "Bearer " + brokertest.Token, "Basic", ""
	cases := []struct {
		method, path, body string
		// want is the credential the upstream sees ("Basic": any Basic), or
		// "denied" if the broker refuses.
		want string
	}{
		{"GET", "/api/v3/repos/example-org/app", "", bearer},
		{"GET", "/api/v3/repos/Example-Org/app/issues", "", bearer},
		{"GET", "/api/v3/repos/other/app", "", none},
		{"GET", "/api/v3/user", "", bearer},
		{"GET", "/api/v3/user/repos", "", none},
		{"GET", "/api/v3/orgs/example-org/repos", "", bearer},
		{"GET", "/api/v3/search/issues?q=org:example-org+is:pr", "", bearer},
		{"GET", "/api/v3/search/issues?q=is:pr", "", none},
		{"GET", "/example-org/app.git/info/refs?service=git-upload-pack", "", basic},
		{"GET", "/other/app.git/info/refs?service=git-upload-pack", "", none},
		{"POST", "/other/app.git/git-upload-pack", "", none},
		{"POST", "/other/app.git/git-receive-pack", "", "denied"},
		{"POST", "/api/v3/repos/example-org/app/pulls", `{"head":"agent/alice/x","base":"main","title":"x"}`, bearer},
		{"POST", "/api/v3/repos/other/app/pulls", `{"head":"agent/alice/x","base":"main","title":"x"}`, "denied"},
		{"PATCH", "/api/v3/repos/other/app/pulls/1", `{"body":"x"}`, "denied"},
		{"POST", "/api/v3/repos/other/app/issues/1/comments", `{"body":"x"}`, "denied"},
		{"DELETE", "/api/v3/repos/other/app/git/refs/heads/agent/alice/x", "", "denied"},
		{"POST", "/api/graphql", gql(`query { repository(owner: "example-org", name: "app") { id } }`, `{}`), bearer},
		{"POST", "/api/graphql", gql(`query { repository(owner: "other", name: "app") { id } }`, `{}`), "denied"},
		{"POST", "/api/graphql", gql(`mutation($i: CreatePullRequestInput!) { createPullRequest(input: $i) { clientMutationId } }`,
			`{"i":{"repositoryId":"R_mine","baseRefName":"main","headRefName":"agent/alice/x","title":"x"}}`), bearer},
		{"POST", "/api/graphql", gql(`mutation($i: CreatePullRequestInput!) { createPullRequest(input: $i) { clientMutationId } }`,
			`{"i":{"repositoryId":"R_other","baseRefName":"main","headRefName":"agent/alice/x","title":"x"}}`), "denied"},
		{"POST", "/api/graphql", gql(`mutation { addComment(input:{subjectId:"PR_mine", body:"x"}) { clientMutationId } }`, `{}`), bearer},
		{"POST", "/api/graphql", gql(`mutation { addComment(input:{subjectId:"PR_other", body:"x"}) { clientMutationId } }`, `{}`), "denied"},
	}
	for _, tc := range cases {
		up.mu.Lock()
		clear(up.seen)
		up.mu.Unlock()
		req, _ := http.NewRequest(tc.method, "https://"+ghHost+tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "token placeholder")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		up.mu.Lock()
		var got string
		forwarded := len(up.seen) > 0
		for _, a := range up.seen {
			got = a
		}
		up.mu.Unlock()
		switch {
		case tc.want == "denied":
			if resp.StatusCode < 400 || forwarded {
				t.Errorf("%s %s: %d %s (forwarded=%v), want refused", tc.method, tc.path, resp.StatusCode, b, forwarded)
			}
		case !forwarded:
			t.Errorf("%s %s: %d %s, not forwarded", tc.method, tc.path, resp.StatusCode, b)
		case tc.want == basic && !strings.HasPrefix(got, "Basic "), tc.want != basic && got != tc.want:
			t.Errorf("%s %s: upstream saw Authorization %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}
