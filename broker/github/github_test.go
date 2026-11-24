package github_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stevenpelley/msb-jig/broker"
	"github.com/stevenpelley/msb-jig/broker/brokertest"
	"github.com/stevenpelley/msb-jig/broker/providers"
)

const ghHost = "github.broker.internal"

// serve serves the provider with git and API upstreams.
func serve(t *testing.T, git, api *url.URL, client *http.Client) (addr, caFile string) {
	if client == nil {
		client = broker.NewUpstreamClient()
	}
	ps, err := providers.Build(context.Background(), providers.Config{Providers: []providers.Provider{providers.GitHub{User: brokertest.User,
		TokenCmd: brokertest.TokenCmd, GitURL: git.String(), APIURL: api.String()}}, Client: client}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return brokertest.Serve(t, ghHost, ps[ghHost])
}

func gitEnv(t *testing.T) *brokertest.GitEnv {
	api, _ := url.Parse("https://api.invalid")
	return brokertest.NewGitEnv(t, ghHost, "octo/dotfiles", func(up *url.URL) (string, string) { return serve(t, up, api, nil) })
}

func TestGitPushRules(t *testing.T) {
	e := gitEnv(t)
	e.PushRules(t, e.URL())
}

// Without .git, as GitHub's web URLs are copied.
func TestGitWithoutDotGit(t *testing.T) {
	e := gitEnv(t)
	e.PushRules(t, strings.TrimSuffix(e.URL(), ".git"))
}

// fakeGitHub is api.github.com: two pull requests (1 from an agent
// branch, 2 from a human's, 3 from a fork), their nodes, and log redirects.
type fakeGitHub struct {
	mu   sync.Mutex
	seen []string // "METHOD path" of forwarded requests that aren't lookups
	tok  []string
	logs string // the signed log URL
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tok = append(f.tok, r.Header.Get("Authorization"))
	f.mu.Unlock()
	p := r.URL.EscapedPath()
	pr := func(ref string, fork bool) map[string]any {
		head := "octo/dotfiles"
		if fork {
			head = "mallory/dotfiles"
		}
		return map[string]any{"head": map[string]any{"ref": ref, "repo": map[string]any{"full_name": head}},
			"base": map[string]any{"repo": map[string]any{"full_name": "octo/dotfiles"}}}
	}
	switch {
	case r.Method == http.MethodGet && p == "/repos/octo/dotfiles/pulls/1":
		_ = json.NewEncoder(w).Encode(pr("agent/alice/x", false))
		return
	case r.Method == http.MethodGet && p == "/repos/octo/dotfiles/pulls/2":
		_ = json.NewEncoder(w).Encode(pr("feature", false))
		return
	case r.Method == http.MethodGet && p == "/repos/octo/dotfiles/pulls/3":
		_ = json.NewEncoder(w).Encode(pr("agent/alice/x", true))
		return
	case r.Method == http.MethodGet && p == "/repos/octo/dotfiles/pulls/9":
		w.WriteHeader(http.StatusNotFound)
		return
	case r.Method == http.MethodGet && p == "/repos/octo/dotfiles/actions/jobs/5/logs":
		w.Header().Set("Location", f.logs)
		w.WriteHeader(http.StatusFound)
		return
	case r.Method == http.MethodGet && p == "/repos/octo/dotfiles/pulls":
		w.Header().Set("Link", `<http://`+r.Host+`/repos/octo/dotfiles/pulls?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`[]`))
		return
	case r.Method == http.MethodPost && p == "/graphql":
		b, _ := io.ReadAll(r.Body)
		var q struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(b, &q)
		if strings.Contains(q.Query, "node(id: $id)") {
			node := map[string]any{
				"PR_agent":     map[string]any{"headRefName": "agent/alice/x", "isCrossRepository": false},
				"PR_human":     map[string]any{"headRefName": "feature", "isCrossRepository": false},
				"PR_fork":      map[string]any{"headRefName": "agent/alice/x", "isCrossRepository": true},
				"THREAD_agent": map[string]any{"pullRequest": map[string]any{"headRefName": "agent/alice/x", "isCrossRepository": false}},
				"ISSUE":        map[string]any{},
			}[q.Variables["id"].(string)]
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": node}})
			return
		}
	}
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+p)
	f.mu.Unlock()
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func TestGitHubAPI(t *testing.T) {
	blob := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the log storage got the token")
		}
		_, _ = w.Write([]byte("the job's log"))
	}))
	t.Cleanup(blob.Close)
	f := &fakeGitHub{logs: blob.URL + "/signed?sig=x"}
	api := httptest.NewServer(f)
	t.Cleanup(api.Close)
	// The upstream client trusts the fake storage's certificate, and like
	// NewUpstreamClient doesn't follow redirects.
	up := blob.Client()
	up.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	gitURL, _ := url.Parse("https://git.invalid")
	apiURL, _ := url.Parse(api.URL)
	addr, caFile := serve(t, gitURL, apiURL, up)
	c := brokertest.Client(addr, caFile)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	do := func(method, path, body string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, "https://"+ghHost+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "token placeholder")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	gql := func(q string, vars string) string {
		b, _ := json.Marshal(q)
		if vars == "" {
			vars = "{}"
		}
		return `{"query":` + string(b) + `,"variables":` + vars + `}`
	}
	const create = `mutation PullRequestCreate($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { id } } }`
	cases := []struct {
		method, path, body string
		allow              bool
	}{
		{"GET", "/api/v3/repos/octo/dotfiles", "", true},
		{"GET", "/api/v3/user", "", true},
		// REST pull requests
		{"POST", "/api/v3/repos/octo/dotfiles/pulls", `{"title":"[agent/alice] Fix","head":"agent/alice/x","base":"main"}`, true},
		{"POST", "/api/v3/repos/octo/dotfiles/pulls", `{"title":"[agent/alice] Fix","head":"octo:agent/alice/x","base":"main"}`, true},
		{"POST", "/api/v3/repos/octo/dotfiles/pulls", `{"title":"[agent/alice] Fix","head":"mallory:agent/alice/x","base":"main"}`, false},
		{"POST", "/api/v3/repos/octo/dotfiles/pulls", `{"title":"Fix","head":"agent/alice/x","base":"main"}`, true}, // no title tag needed
		{"POST", "/api/v3/repos/octo/dotfiles/pulls", `{"title":"[agent/alice] Fix","head":"feature","base":"main"}`, false},
		{"PATCH", "/api/v3/repos/octo/dotfiles/pulls/1", `{"body":"more","base":"agent/alice/y"}`, true},
		{"PATCH", "/api/v3/repos/octo/dotfiles/pulls/1", `{"title":"untagged"}`, true},
		{"PATCH", "/api/v3/repos/octo/dotfiles/pulls/2", `{"title":"[agent/alice] mine now"}`, false},
		{"PATCH", "/api/v3/repos/octo/dotfiles/pulls/3", `{"state":"closed"}`, false},
		{"PUT", "/api/v3/repos/octo/dotfiles/pulls/1/merge", `{}`, false},
		{"POST", "/api/v3/repos/octo/dotfiles/pulls/1/reviews", `{"event":"APPROVE"}`, false},
		{"POST", "/api/v3/repos/octo/dotfiles/pulls/1/reviews", `{"event":"COMMENT","body":"x"}`, true},
		{"POST", "/api/v3/repos/octo/dotfiles/pulls/1/comments/7/replies", `{"body":"done"}`, true},
		{"POST", "/api/v3/repos/octo/dotfiles/pulls/2/comments", `{"body":"x"}`, false},
		{"POST", "/api/v3/repos/octo/dotfiles/issues/1/comments", `{"body":"x"}`, true},
		{"POST", "/api/v3/repos/octo/dotfiles/issues/9/comments", `{"body":"x"}`, false},
		{"POST", "/api/v3/repos/octo/dotfiles/merges", `{"base":"main","head":"agent/alice/x"}`, false},
		{"DELETE", "/api/v3/repos/octo/dotfiles/git/refs/heads/agent/alice/x", "", true},
		{"DELETE", "/api/v3/repos/octo/dotfiles/git/refs/heads/main", "", false},
		{"PATCH", "/api/v3/repos/octo/dotfiles/git/refs/heads/agent/alice/x", `{"sha":"abc"}`, false},
		{"PUT", "/api/v3/repos/octo/dotfiles/contents/a.txt", `{"message":"x","content":"eA==","branch":"agent/alice/x"}`, false},
		{"POST", "/api/v3/user/keys", `{"key":"ssh-ed25519 AAA"}`, false},
		{"POST", "/api/v3/repos/octo/dotfiles/keys", `{"key":"ssh-ed25519 AAA"}`, false},
		{"POST", "/api/v3/repos/octo/dotfiles/hooks", `{"config":{"url":"https://x"}}`, false},
		{"PUT", "/api/v3/repos/octo/dotfiles/actions/secrets/X", `{}`, false},
		{"POST", "/api/v3/repos/octo/dotfiles/pulls", `not json`, false},
		{"POST", "/api/uploads/repos/octo/dotfiles/releases/1/assets", ``, false},
		// GraphQL
		{"POST", "/api/graphql", gql(`query { viewer { login } }`, ""), true},
		{"POST", "/api/graphql", gql(create, `{"input":{"repositoryId":"R","baseRefName":"main","headRefName":"agent/alice/x","title":"[agent/alice] Fix"}}`), true},
		{"POST", "/api/graphql", gql(create, `{"input":{"repositoryId":"R","baseRefName":"main","headRefName":"agent/alice/x","title":"Fix"}}`), true},
		{"POST", "/api/graphql", gql(create, `{"input":{"repositoryId":"R","baseRefName":"main","headRefName":"feature","title":"[agent/alice] Fix"}}`), false},
		{"POST", "/api/graphql", gql(create, `{"input":{"repositoryId":"R","baseRefName":"main","headRefName":"agent/alice/x","headRepositoryId":"F","title":"[agent/alice] Fix"}}`), false},
		// A key twice, ignoring case: Go would read the second, GitHub the first.
		{"POST", "/api/graphql", `{"query":"mutation { mergePullRequest(input:{pullRequestId:\"P\"}) { clientMutationId } }","QUERY":"query { viewer { login } }"}`, false},
		{"POST", "/api/v3/repos/octo/dotfiles/pulls", `{"title":"[agent/alice] Fix","head":"agent/alice/x","head":"feature","base":"main"}`, false},
		// No parameters in the URL beside a checked body.
		{"POST", "/api/graphql?query=mutation+%7B+m+%7D", gql(`query { viewer { login } }`, ""), false},
		{"PATCH", "/api/v3/repos/octo/dotfiles/pulls/1?title=untagged", `{"body":"more"}`, false},
		// Literal arguments, not variables.
		{"POST", "/api/graphql", gql(`mutation { createPullRequest(input:{repositoryId:"R", baseRefName:"main", headRefName:"agent/alice/x", title:"[agent/alice] Fix"}) { clientMutationId } }`, ""), true},
		{"POST", "/api/graphql", gql(`mutation { createPullRequest(input:{repositoryId:"R", baseRefName:"main", headRefName:"main", title:"[agent/alice] Fix"}) { clientMutationId } }`, ""), false},
		{"POST", "/api/graphql", gql(`mutation($i: UpdatePullRequestInput!) { updatePullRequest(input: $i) { clientMutationId } }`, `{"i":{"pullRequestId":"PR_agent","body":"x"}}`), true},
		{"POST", "/api/graphql", gql(`mutation($i: UpdatePullRequestInput!) { updatePullRequest(input: $i) { clientMutationId } }`, `{"i":{"pullRequestId":"PR_human","title":"[agent/alice] x"}}`), false},
		{"POST", "/api/graphql", gql(`mutation($i: UpdatePullRequestInput!) { updatePullRequest(input: $i) { clientMutationId } }`, `{"i":{"pullRequestId":"PR_fork"}}`), false},
		{"POST", "/api/graphql", gql(`mutation { markPullRequestReadyForReview(input:{pullRequestId:"PR_agent"}) { clientMutationId } }`, ""), true},
		{"POST", "/api/graphql", gql(`mutation { closePullRequest(input:{pullRequestId:"PR_human"}) { clientMutationId } }`, ""), false},
		{"POST", "/api/graphql", gql(`mutation { addComment(input:{subjectId:"PR_agent", body:"x"}) { clientMutationId } }`, ""), true},
		{"POST", "/api/graphql", gql(`mutation { addComment(input:{subjectId:"ISSUE", body:"x"}) { clientMutationId } }`, ""), false},
		{"POST", "/api/graphql", gql(`mutation { resolveReviewThread(input:{threadId:"THREAD_agent"}) { clientMutationId } }`, ""), true},
		{"POST", "/api/graphql", gql(`mutation { addPullRequestReview(input:{pullRequestId:"PR_agent", event:APPROVE}) { clientMutationId } }`, ""), false},
		{"POST", "/api/graphql", gql(`mutation { addPullRequestReview(input:{pullRequestId:"PR_agent", event:COMMENT, body:"x"}) { clientMutationId } }`, ""), true},
		{"POST", "/api/graphql", gql(`mutation { mergePullRequest(input:{pullRequestId:"PR_agent"}) { clientMutationId } }`, ""), false},
		{"POST", "/api/graphql", gql(`mutation { enablePullRequestAutoMerge(input:{pullRequestId:"PR_agent"}) { clientMutationId } }`, ""), false},
		{"POST", "/api/graphql", gql(`mutation { createCommitOnBranch(input:{}) { clientMutationId } }`, ""), false},
		{"POST", "/api/graphql", gql(`mutation { a: addComment(input:{subjectId:"PR_agent", body:"x"}) { clientMutationId } b: mergePullRequest(input:{pullRequestId:"PR_agent"}) { clientMutationId } }`, ""), false},
		{"POST", "/api/graphql", gql(`query Merge { viewer { login } } mutation M { deleteRef(input:{refId:"x"}) { clientMutationId } }`, ""), false},
		{"POST", "/api/graphql", gql(`subscription { x }`, ""), false},
		{"POST", "/api/graphql", gql(`query {`, ""), false},
		{"POST", "/api/graphql", `[{"query":"query { viewer { login } }"}]`, false},
		{"POST", "/api/graphql", gql(create, `{"input":"oops"}`), false},
	}
	for _, c := range cases {
		f.mu.Lock()
		before := len(f.seen)
		f.mu.Unlock()
		resp, body := do(c.method, c.path, c.body)
		f.mu.Lock()
		forwarded := len(f.seen) > before
		f.mu.Unlock()
		if c.allow && (resp.StatusCode >= 300 || !forwarded) {
			t.Errorf("%s %s %s: %d %s, want allowed", c.method, c.path, c.body, resp.StatusCode, body)
		}
		if !c.allow && (resp.StatusCode < 400 || forwarded) {
			t.Errorf("%s %s %s: %d %s (forwarded=%v), want refused and not forwarded", c.method, c.path, c.body, resp.StatusCode, body, forwarded)
		}
	}

	// Pagination links come back through the broker.
	resp, _ := do("GET", "/api/v3/repos/octo/dotfiles/pulls", "")
	if l := resp.Header.Get("Link"); l != `<https://`+ghHost+`/api/v3/repos/octo/dotfiles/pulls?page=2>; rel="next"` {
		t.Errorf("Link: %q", l)
	}
	// Logs: the broker follows the redirect to storage, without the token.
	resp, body := do("GET", "/api/v3/repos/octo/dotfiles/actions/jobs/5/logs", "")
	if resp.StatusCode != 200 || body != "the job's log" {
		t.Errorf("logs: %d %q", resp.StatusCode, body)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tk := range f.tok {
		if tk != "Bearer "+brokertest.Token {
			t.Errorf("upstream saw credentials %q", tk)
		}
	}
}
