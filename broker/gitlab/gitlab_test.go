package gitlab_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"mono-jig/broker/brokertest"
	"mono-jig/broker/providers"
)

const host = "gitlab.broker.internal"

func serve(t *testing.T, upstream *url.URL) (addr, caFile string) {
	ps, err := providers.Build(context.Background(), providers.Config{GitLabUser: brokertest.User,
		GitLabURL: upstream.String(), GitLabTokenCmd: brokertest.TokenCmd}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return brokertest.Serve(t, host, ps[host])
}

// --- git, end to end ---

func gitEnv(t *testing.T) *brokertest.GitEnv {
	return brokertest.NewGitEnv(t, host, "grp/sub/proj", func(up *url.URL) (string, string) { return serve(t, up) })
}

func TestGitPushRules(t *testing.T) {
	e := gitEnv(t)
	e.PushRules(t, e.URL())
}

// A remote URL without .git works the same, under the same rules.
func TestGitWithoutDotGit(t *testing.T) {
	e := gitEnv(t)
	e.PushRules(t, strings.TrimSuffix(e.URL(), ".git"))
}

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// A client that sends push options anyway, ignoring the advertisement.
func TestPushOptionsInBodyRejected(t *testing.T) {
	e := gitEnv(t)
	c := brokertest.Client(e.Addr, e.CAFile)
	body := pkt(strings.Repeat("0", 40)+" "+strings.Repeat("a", 40)+" refs/heads/agent/alice/x\x00report-status push-options\n") +
		"0000" + pkt("merge_request.create\n") + "0000"
	resp, err := c.Post("https://"+host+"/grp/sub/proj.git/git-receive-pack", "application/x-git-receive-pack-request", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "ng refs/heads/agent/alice/x push options") {
		t.Fatalf("response %q", b)
	}
}

// --- REST, GraphQL, LFS and MCP against a fake GitLab ---

type fakeGitLab struct {
	mu    sync.Mutex
	seen  []string // "METHOD path" of forwarded writes
	tok   []string
	sse   bool
	calls []string // MCP tool calls that reached it
}

var allTools = []string{"get_merge_request", "list_pipelines", "save_merge_request", "add_commit",
	"accept_merge_request", "apply_suggestion", "save_project", "fork_repository", "save_pipeline", "brand_new_tool"}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tok = append(f.tok, r.Header.Get("Private-Token")+"|"+r.Header.Get("Authorization"))
	if r.Method != http.MethodGet {
		f.seen = append(f.seen, r.Method+" "+r.URL.EscapedPath())
	}
	f.mu.Unlock()
	p := r.URL.EscapedPath()
	switch {
	case p == "/api/v4/mcp":
		f.mcp(w, r)
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
	case r.Method == http.MethodGet && p == "/api/v4/projects/next":
		w.Header().Set("Link", `<http://`+r.Host+`/api/v4/projects?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`[]`))
	case r.Method == http.MethodGet:
		w.Header().Set("Set-Cookie", "session=x")
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

func (f *fakeGitLab) mcp(w http.ResponseWriter, r *http.Request) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&m)
	var result any
	switch m.Method {
	case "tools/list":
		var tools []map[string]any
		for _, n := range allTools {
			tools = append(tools, map[string]any{"name": n, "inputSchema": map[string]any{"type": "object"}})
		}
		result = map[string]any{"tools": tools}
	case "tools/call":
		f.mu.Lock()
		f.calls = append(f.calls, m.Params.Name)
		f.mu.Unlock()
		result = map[string]any{"content": []map[string]string{{"type": "text", "text": "called " + m.Params.Name}}}
	default:
		result = map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	w.Header().Set("Mcp-Session-Id", "sess-1")
	if f.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func fakeEnv(t *testing.T, f *fakeGitLab) (*http.Client, string) {
	t.Helper()
	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	addr, caFile := serve(t, u)
	return brokertest.Client(addr, caFile), u.Host
}

func TestAPI(t *testing.T) {
	f := &fakeGitLab{}
	c, _ := fakeEnv(t, f)
	do := func(method, path, body string) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest(method, "https://"+host+path, strings.NewReader(body))
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
		return resp.StatusCode, string(b), resp.Header
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
		{"PUT", "/api/v4/projects/grp%2Fproj/merge_requests/1", `{"auto_merge":true}`, false},
		{"PUT", "/api/v4/projects/grp%2Fproj/merge_requests/2", `{"title":"[agent/alice] taking over"}`, false},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests/1/notes", `{"body":"done"}`, true},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests/1/discussions", `{"body":"done"}`, true},
		{"PUT", "/api/v4/projects/grp%2Fproj/merge_requests/1/discussions/abc", `{"resolved":true}`, true},
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
		// Quick actions: GitLab would run them with the user's rights.
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests/1/notes", `{"body":"/merge"}`, false},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests/1/notes", `{"body":"looks good\n  /approve"}`, false},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests/1/discussions", `{"body":"/title main"}`, false},
		{"PUT", "/api/v4/projects/grp%2Fproj/merge_requests/1", `{"description":"more\n/merge"}`, false},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests", `{"title":"[agent/alice] Fix","source_branch":"agent/alice/x","target_branch":"main","description":"/approve"}`, false},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests/1/notes", `{"body":"done","extra":{"nested":["/merge"]}}`, false},
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests/1/notes", `{"body":"run /usr/bin/env, or a/merge, or /merge."}`, true},
		// GitLab reads booleans loosely.
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests", `{"title":"[agent/alice] Fix","source_branch":"agent/alice/x","target_branch":"main","merge_when_pipeline_succeeds":"1"}`, false},
		{"PUT", "/api/v4/projects/grp%2Fproj/merge_requests/1", `{"auto_merge":"yes"}`, false},
		{"PUT", "/api/v4/projects/grp%2Fproj/merge_requests/1", `{"auto_merge":false}`, true},
		// A key twice, ignoring case: Go would read the second, GitLab the first.
		{"PUT", "/api/v4/projects/grp%2Fproj/merge_requests/1", `{"title":"untagged","Title":"[agent/alice] ok"}`, false},
		// _method would change the request's method in Rack.
		{"POST", "/api/v4/projects/grp%2Fproj/merge_requests/1/notes?_method=PUT", `{"body":"x"}`, false},
		// GraphQL: no query in the URL beside the body, no key twice, no
		// repeated GET parameter.
		{"POST", "/api/graphql?query=mutation+%7B+mergeRequestAccept(input:%7B%7D)+%7B+errors+%7D+%7D", `{"query":"query { a }"}`, false},
		{"POST", "/api/graphql", `{"query":"mutation { mergeRequestAccept(input:{}) { errors } }","Query":"query { a }"}`, false},
		{"GET", "/api/graphql?query=query+%7B+a+%7D&query=mutation+%7B+m+%7D", "", false},
		{"GET", "/api/graphql?query=query+%7B+a+%7D", "", true},
		{"POST", "/elsewhere", `{}`, false},
		{"POST", "/api/v4/x/git-receive-pack", ``, false},
	}
	for _, cs := range cases {
		f.mu.Lock()
		before := len(f.seen)
		f.mu.Unlock()
		code, body, _ := do(cs.method, cs.path, cs.body)
		f.mu.Lock()
		forwarded := len(f.seen) > before || (cs.method == "GET" && code < 300)
		f.mu.Unlock()
		if cs.allow && (code >= 300 || !forwarded) {
			t.Errorf("%s %s %s: %d %s, want allowed", cs.method, cs.path, cs.body, code, body)
		}
		if !cs.allow && (code != http.StatusForbidden || forwarded) {
			t.Errorf("%s %s %s: %d %s (forwarded=%v), want 403 and not forwarded", cs.method, cs.path, cs.body, code, body, forwarded)
		}
	}
	// Pagination links come back through the broker.
	if _, _, h := do("GET", "/api/v4/projects/next", ""); h.Get("Link") != `<https://`+host+`/api/v4/projects?page=2>; rel="next"` {
		t.Errorf("Link = %q", h.Get("Link"))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tk := range f.tok {
		if tk != "|Bearer "+brokertest.Token {
			t.Errorf("upstream saw credentials %q", tk)
		}
	}
}

func TestLFSBatchRewritten(t *testing.T) {
	c, _ := fakeEnv(t, &fakeGitLab{})
	resp, err := c.Post("https://"+host+"/grp/proj.git/info/lfs/objects/batch", "application/vnd.git-lfs+json", strings.NewReader(`{"operation":"download","objects":[{"oid":"abc","size":3}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	s := string(b)
	if strings.Contains(s, "Authorization") || strings.Contains(s, "bGZz") {
		t.Fatalf("LFS token leaked: %s", s)
	}
	if !strings.Contains(s, "https://"+host+"/grp/proj.git/gitlab-lfs/objects/abc") {
		t.Fatalf("href not rewritten: %s", s)
	}
}

// --- MCP ---

func rpc(t *testing.T, c *http.Client, body string) (map[string]any, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("POST", "https://"+host+"/api/v4/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer placeholder")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var data []string
		sc := bufio.NewScanner(bytes.NewReader(raw))
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				data = append(data, d)
			}
		}
		raw = []byte(strings.Join(data, "\n"))
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %q", err, raw)
	}
	return m, resp.Header
}

func toolCall(name, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, args)
}

func TestMCPToolsList(t *testing.T) {
	for _, sse := range []bool{false, true} {
		f := &fakeGitLab{sse: sse}
		c, _ := fakeEnv(t, f)
		m, h := rpc(t, c, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		var names []string
		for _, tl := range m["result"].(map[string]any)["tools"].([]any) {
			names = append(names, tl.(map[string]any)["name"].(string))
		}
		if got := strings.Join(names, ","); got != "get_merge_request,list_pipelines,save_merge_request,add_commit" {
			t.Errorf("sse=%v: tools/list = %s", sse, got)
		}
		if h.Get("Mcp-Session-Id") != "sess-1" {
			t.Errorf("session id not passed back: %v", h)
		}
		if f.tok[0] != "|Bearer "+brokertest.Token {
			t.Errorf("upstream saw %q", f.tok[0])
		}
	}
}

func TestMCPToolCalls(t *testing.T) {
	f := &fakeGitLab{}
	c, upHost := fakeEnv(t, f)
	mrURL := func(iid int) string { return fmt.Sprintf("http://%s/grp/proj/-/merge_requests/%d", upHost, iid) }
	cases := []struct {
		name, tool, args string
		allow            bool
	}{
		{"read", "get_merge_request", `{"project_id":"grp/proj","merge_request_iid":2}`, true},
		{"create, tagged", "save_merge_request", `{"project_id":"grp/proj","source_branch":"agent/alice/x","target_branch":"main","title":"[agent/alice] Fix"}`, true},
		{"create, untagged", "save_merge_request", `{"project_id":"grp/proj","source_branch":"agent/alice/x","target_branch":"main","title":"Fix"}`, false},
		{"create from main", "save_merge_request", `{"project_id":"grp/proj","source_branch":"main","target_branch":"x","title":"[agent/alice] Fix"}`, false},
		{"create cross-project", "save_merge_request", `{"project_id":"grp/proj","source_branch":"agent/alice/x","target_branch":"main","title":"[agent/alice] Fix","target_project_id":9}`, false},
		{"update own", "save_merge_request", `{"project_id":"grp/proj","merge_request_iid":1,"description":"more"}`, true},
		{"update own, iid as string", "save_merge_request", `{"project_id":"grp/proj","merge_request_iid":"1","state_event":"close"}`, true},
		{"update own, drops tag", "save_merge_request", `{"project_id":"grp/proj","merge_request_iid":1,"title":"untagged"}`, false},
		{"update a human's", "save_merge_request", `{"project_id":"grp/proj","merge_request_iid":2,"title":"[agent/alice] mine"}`, false},
		{"note on own", "save_note", `{"project_id":"grp/proj","merge_request_iid":1,"body":"done"}`, true},
		{"note on own by url", "save_note", `{"url":"` + mrURL(1) + `","body":"done"}`, true},
		{"note on a human's", "save_note", `{"project_id":"grp/proj","merge_request_iid":2,"body":"hi"}`, false},
		{"note on a human's by url", "save_note", `{"url":"` + mrURL(2) + `","body":"hi"}`, false},
		{"note on an issue", "save_note", `{"url":"http://` + upHost + `/grp/proj/-/issues/3","body":"hi"}`, false},
		{"note on a work item", "save_note", `{"project_id":"grp/proj","work_item_iid":3,"body":"hi"}`, false},
		{"note url on another host", "save_note", `{"url":"https://evil.example/grp/proj/-/merge_requests/1","body":"hi"}`, false},
		{"review note on own", "save_merge_request_review", `{"method":"create_diff_note","project_id":"grp/proj","merge_request_iid":1,"body":"x"}`, true},
		{"approve", "save_merge_request_review", `{"method":"approve","project_id":"grp/proj","merge_request_iid":1}`, false},
		{"duo review", "save_merge_request_review", `{"method":"post_duo_review","project_id":"grp/proj","merge_request_iid":1}`, false},
		{"commit to namespace", "add_commit", `{"project_id":"grp/proj","branch":"agent/alice/x","commit_message":"m","actions":[]}`, true},
		{"commit to main", "add_commit", `{"project_id":"grp/proj","branch":"main","commit_message":"m","actions":[]}`, false},
		{"commit from another project", "add_commit", `{"project_id":"grp/proj","branch":"agent/alice/x","start_project":"evil/proj","commit_message":"m","actions":[]}`, false},
		{"branch in namespace", "add_branch", `{"project_id":"grp/proj","branch":"agent/alice/y","ref":"main"}`, true},
		{"branch outside", "add_branch", `{"project_id":"grp/proj","branch":"release","ref":"main"}`, false},
		{"branch, no name", "add_branch", `{"project_id":"grp/proj","ref":"main"}`, false},
		{"note with a quick action", "save_note", `{"project_id":"grp/proj","merge_request_iid":1,"body":"ok\n/merge"}`, false},
		{"review with a quick action", "save_merge_request_review", `{"method":"create_diff_note","project_id":"grp/proj","merge_request_iid":1,"body":"/approve"}`, false},
		{"description with a quick action", "save_merge_request", `{"project_id":"grp/proj","merge_request_iid":1,"description":"/merge"}`, false},
		{"update with auto-merge", "save_merge_request", `{"project_id":"grp/proj","merge_request_iid":1,"merge_when_pipeline_succeeds":true}`, false},
		{"update by url", "save_merge_request", `{"url":"` + mrURL(2) + `","source_branch":"agent/alice/x","title":"[agent/alice] mine"}`, false},
		{"merge", "accept_merge_request", `{"project_id":"grp/proj","merge_request_iid":1}`, false},
		{"apply suggestion", "apply_suggestion", `{"id":1}`, false},
		{"save project", "save_project", `{}`, false},
		{"fork", "fork_repository", `{}`, false},
		{"pipeline", "save_pipeline", `{}`, false},
		{"unknown tool", "brand_new_tool", `{}`, false},
		{"bad arguments", "add_branch", `"x"`, false},
	}
	for _, cs := range cases {
		n := len(f.calls)
		m, _ := rpc(t, c, toolCall(cs.tool, cs.args))
		res, _ := m["result"].(map[string]any)
		reached := len(f.calls) > n
		denied := res != nil && res["isError"] == true
		if reached != cs.allow || denied == cs.allow {
			t.Errorf("%s: reached=%v denied=%v, want allow=%v (%v)", cs.name, reached, denied, cs.allow, m)
		}
		if denied {
			text := res["content"].([]any)[0].(map[string]any)["text"].(string)
			if !strings.Contains(text, "mono-jig broker denied "+cs.tool) {
				t.Errorf("%s: denial text %q", cs.name, text)
			}
		}
	}
	for _, a := range f.tok {
		if a != "|Bearer "+brokertest.Token {
			t.Errorf("upstream saw %q", a)
		}
	}
}

func TestMCPProtocolChecks(t *testing.T) {
	f := &fakeGitLab{}
	c, _ := fakeEnv(t, f)
	for _, cs := range []struct{ name, body, want string }{
		{"batch", "[" + toolCall("get_merge_request", `{}`) + "]", "batches"},
		{"batch with whitespace", "  \n[" + toolCall("get_merge_request", `{}`) + "]", "batches"},
		{"not json-rpc", `{"method":"tools/call"}`, "JSON-RPC 2.0"},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{}}`, "not allowed"},
		{"sampling response", `{"jsonrpc":"2.0","id":1,"result":{}}`, "responses to server requests"},
		// Keys twice, ignoring case: Go would read the second, GitLab the first.
		{"tool name twice", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"accept_merge_request","Name":"get_project","arguments":{}}}`, "twice"},
		{"argument twice", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_commit","arguments":{"branch":"main","Branch":"agent/alice/x"}}}`, "twice"},
		{"method twice", `{"jsonrpc":"2.0","id":1,"method":"tools/call","Method":"ping","params":{}}`, "twice"},
	} {
		m, _ := rpc(t, c, cs.body)
		e, _ := m["error"].(map[string]any)
		if e == nil || !strings.Contains(e["message"].(string), cs.want) {
			t.Errorf("%s: %v", cs.name, m)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("calls reached the server: %v", f.calls)
	}
	m, _ := rpc(t, c, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if m["result"] == nil {
		t.Errorf("initialize: %v", m)
	}
}
