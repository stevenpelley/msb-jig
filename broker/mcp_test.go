package broker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeMCP is GitLab's MCP endpoint in front of fakeGitLab (which answers
// the broker's merge request lookups). It records the tool calls that
// reached it and the credential they carried.
type fakeMCP struct {
	gl    *fakeGitLab
	sse   bool
	mu    sync.Mutex
	calls []string
	auth  []string
}

var allGitLabTools = []string{"get_merge_request", "list_pipelines", "save_merge_request", "add_commit",
	"accept_merge_request", "apply_suggestion", "save_project", "fork_repository", "save_pipeline", "brand_new_tool"}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != GitLabMCPPath {
		f.gl.ServeHTTP(w, r)
		return
	}
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()
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
		for _, n := range allGitLabTools {
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
	w.Write(b)
}

func mcpEnv(t *testing.T, sse bool) (*http.Client, string, *fakeMCP, string) {
	t.Helper()
	f := &fakeMCP{gl: &fakeGitLab{}, sse: sse}
	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	addr, caFile, _ := brokerFor(t, up.URL, "")
	upHost, _ := url.Parse(up.URL)
	return brokerClient(addr, caFile), "https://" + testHost + GitLabMCPPath, f, upHost.Host
}

func rpc(t *testing.T, c *http.Client, u, body string) (map[string]any, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("POST", u, strings.NewReader(body))
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
		sc := bufio.NewScanner(strings.NewReader(string(raw)))
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

func call(name, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, args)
}

func toolNames(m map[string]any) []string {
	var out []string
	for _, t := range m["result"].(map[string]any)["tools"].([]any) {
		out = append(out, t.(map[string]any)["name"].(string))
	}
	return out
}

func TestMCPToolsList(t *testing.T) {
	for _, sse := range []bool{false, true} {
		c, u, f, _ := mcpEnv(t, sse)
		m, h := rpc(t, c, u, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		got := strings.Join(toolNames(m), ",")
		if got != "get_merge_request,list_pipelines,save_merge_request,add_commit" {
			t.Errorf("sse=%v: tools/list = %s", sse, got)
		}
		if h.Get("Mcp-Session-Id") != "sess-1" {
			t.Errorf("session id not passed back: %v", h)
		}
		if f.auth[0] != "Bearer "+testToken {
			t.Errorf("upstream saw %q", f.auth[0])
		}
	}
}

func TestMCPToolCalls(t *testing.T) {
	c, u, f, upHost := mcpEnv(t, false)
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
		m, _ := rpc(t, c, u, call(cs.tool, cs.args))
		res, _ := m["result"].(map[string]any)
		reached := len(f.calls) > n
		denied := res != nil && res["isError"] == true
		if reached != cs.allow || denied == cs.allow {
			t.Errorf("%s: reached=%v denied=%v, want allow=%v (%v)", cs.name, reached, denied, cs.allow, m)
		}
		if denied {
			text := res["content"].([]any)[0].(map[string]any)["text"].(string)
			if !strings.Contains(text, "investigator broker denied "+cs.tool) {
				t.Errorf("%s: denial text %q", cs.name, text)
			}
		}
	}
	for _, a := range f.auth {
		if a != "Bearer "+testToken {
			t.Errorf("upstream saw %q", a)
		}
	}
}

func TestMCPProtocolChecks(t *testing.T) {
	c, u, f, _ := mcpEnv(t, false)
	for _, cs := range []struct{ name, body, want string }{
		{"batch", "[" + call("get_merge_request", `{}`) + "]", "batches"},
		{"batch with whitespace", "  \n[" + call("get_merge_request", `{}`) + "]", "batches"},
		{"not json-rpc", `{"method":"tools/call"}`, "JSON-RPC 2.0"},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{}}`, "not allowed"},
		{"sampling response", `{"jsonrpc":"2.0","id":1,"result":{}}`, "responses to server requests"},
	} {
		m, _ := rpc(t, c, u, cs.body)
		e, _ := m["error"].(map[string]any)
		if e == nil || !strings.Contains(e["message"].(string), cs.want) {
			t.Errorf("%s: %v", cs.name, m)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("calls reached the server: %v", f.calls)
	}
	m, _ := rpc(t, c, u, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if m["result"] == nil {
		t.Errorf("initialize: %v", m)
	}
}
