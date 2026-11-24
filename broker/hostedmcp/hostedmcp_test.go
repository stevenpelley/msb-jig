package hostedmcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stevenpelley/msb-jig/broker/brokertest"
	"github.com/stevenpelley/msb-jig/broker/providers"
)

const host = "example.mcp.broker.internal"

// fakeServer is a hosted MCP server at /v1/mcp that wants the user's
// token. It records the tool calls and requests that reached it.
type fakeServer struct {
	mu    sync.Mutex
	sse   bool
	calls []string // tools/call names
	seen  []string // "METHOD path?query auth"
}

var allTools = []string{"search_logs", "get_metric", "create_notebook", "delete_monitor"}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+brokertest.Token {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://x/.well-known/oauth-protected-resource"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path != "/v1/mcp" {
		http.NotFound(w, r)
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(http.StatusOK)
		return
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&m)
	var result any = map[string]any{}
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

func (f *fakeServer) callsSoFar() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// env serves f through the broker as the provider "example", in server
// mode (host) or proxy mode (mcp.example.org, the URL's host).
func env(t *testing.T, f *fakeServer, proxy bool) (c *http.Client, base string) {
	t.Helper()
	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	p := providers.MCPServer{ID: "example", URL: "https://mcp.example.org/v1/mcp", Tools: []string{"search_logs", "get_metric"},
		TokenCmd: brokertest.TokenCmd, Upstream: up.URL}
	ps, err := providers.Build(context.Background(), providers.Config{Proxy: proxy, Providers: []providers.Provider{p}}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := host
	if proxy {
		h = "mcp.example.org"
	}
	addr, caFile := brokertest.Serve(t, h, ps[h])
	return brokertest.Client(addr, caFile), "https://" + h
}

func rpc(t *testing.T, c *http.Client, url, body string) (map[string]any, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer placeholder")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	s := string(b)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		for _, l := range strings.Split(s, "\n") {
			if strings.HasPrefix(l, "data: ") {
				s = strings.TrimPrefix(l, "data: ")
			}
		}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("response %d %q: %v", resp.StatusCode, b, err)
	}
	return m, resp.Header
}

func toolCall(name string) string {
	return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`
}

func TestToolsListFiltered(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		for _, sse := range []bool{false, true} {
			f := &fakeServer{sse: sse}
			c, base := env(t, f, proxy)
			m, h := rpc(t, c, base+"/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			var names []string
			for _, tl := range m["result"].(map[string]any)["tools"].([]any) {
				names = append(names, tl.(map[string]any)["name"].(string))
			}
			if got := strings.Join(names, ","); got != "search_logs,get_metric" {
				t.Errorf("proxy=%v sse=%v: tools/list = %s", proxy, sse, got)
			}
			if h.Get("Mcp-Session-Id") != "sess-1" {
				t.Errorf("session id not passed back: %v", h)
			}
			if f.seen[0] != "POST /v1/mcp Bearer "+brokertest.Token {
				t.Errorf("upstream saw %q", f.seen[0])
			}
		}
	}
}

func TestToolCalls(t *testing.T) {
	f := &fakeServer{}
	c, base := env(t, f, false)
	for _, cs := range []struct {
		tool  string
		allow bool
	}{
		{"search_logs", true}, {"get_metric", true},
		{"create_notebook", false}, {"delete_monitor", false}, {"brand_new_tool", false}, {"", false},
	} {
		n := f.callsSoFar()
		m, _ := rpc(t, c, base+"/v1/mcp", toolCall(cs.tool))
		res, _ := m["result"].(map[string]any)
		reached := f.callsSoFar() > n
		if reached != cs.allow {
			t.Errorf("%q: reached the server = %v, want %v (%v)", cs.tool, reached, cs.allow, m)
		}
		if !cs.allow && (res == nil || res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), "isn't on the broker's list")) {
			t.Errorf("%q: denial not a tool error: %v", cs.tool, m)
		}
	}
}

func TestProtocolChecks(t *testing.T) {
	f := &fakeServer{}
	c, base := env(t, f, false)
	for _, cs := range []struct{ name, url, body, want string }{
		{"batch", "/v1/mcp", "[" + toolCall("search_logs") + "]", "batches"},
		{"unknown method", "/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{}}`, "not allowed"},
		{"tool name twice", "/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_monitor","Name":"search_logs","arguments":{}}}`, "twice"},
		{"URL parameters", "/v1/mcp?toolsets=all", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "parameters"},
	} {
		m, _ := rpc(t, c, base+cs.url, cs.body)
		e, _ := m["error"].(map[string]any)
		if e == nil || !strings.Contains(e["message"].(string), cs.want) {
			t.Errorf("%s: %v", cs.name, m)
		}
	}
	if len(f.seen) != 0 {
		t.Errorf("requests reached the server: %v", f.seen)
	}
	if m, _ := rpc(t, c, base+"/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`); m["result"] == nil {
		t.Errorf("initialize: %v", m)
	}
}

// The session's GET and DELETE pass; other paths (OAuth discovery, the
// server's other endpoints) and URL parameters don't.
func TestOtherRequests(t *testing.T) {
	f := &fakeServer{}
	c, base := env(t, f, true)
	for _, cs := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/v1/mcp", 200},
		{"DELETE", "/v1/mcp", 200},
		{"GET", "/v1/mcp?toolsets=all", 403},
		{"GET", "/.well-known/oauth-protected-resource", 403},
		{"POST", "/oauth2/register", 403},
		{"PUT", "/v1/mcp", 403},
		{"POST", "/v1/mcp/other", 403},
	} {
		req, _ := http.NewRequest(cs.method, base+cs.path, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != cs.code {
			t.Errorf("%s %s: %d, want %d", cs.method, cs.path, resp.StatusCode, cs.code)
		}
	}
	if len(f.seen) != 2 {
		t.Errorf("upstream saw %v", f.seen)
	}
}

func TestConfigChecked(t *testing.T) {
	for _, p := range []providers.MCPServer{
		{ID: "Example", URL: "https://mcp.example.org/v1/mcp", Tools: []string{"a"}},
		{ID: "example", URL: "https://mcp.example.org/v1/mcp?toolsets=all", Tools: []string{"a"}},
		{ID: "example", URL: "mcp.example.org/v1/mcp", Tools: []string{"a"}},
		{ID: "example", URL: "https://mcp.example.org/v1/mcp"},
	} {
		if _, err := providers.Build(context.Background(), providers.Config{Providers: []providers.Provider{p}}, "", nil); err == nil {
			t.Errorf("%+v: no error", p)
		}
	}
}

// The guest gets the URL as the sandbox reaches it, mcpcli once (AWS
// adds the same files), and the instructions.
func TestGuest(t *testing.T) {
	p := providers.MCPServer{ID: "ex-ample", Title: "Example", URL: "https://mcp.example.org/v1/mcp", Tools: []string{"b", "a"}}
	for _, cs := range []struct {
		proxy bool
		url   string
	}{{false, "https://ex-ample.mcp.broker.internal/v1/mcp"}, {true, "https://mcp.example.org/v1/mcp"}} {
		g := providers.Config{Proxy: cs.proxy, Providers: []providers.Provider{p, providers.AWS{Profiles: []string{"x"}}}}.Guest()
		if g.Env["EX_AMPLE_MCP_URL"] != cs.url {
			t.Errorf("proxy=%v: env %v", cs.proxy, g.Env)
		}
		if err := g.Validate(); err != nil {
			t.Errorf("proxy=%v: %v", cs.proxy, err)
		}
		if !strings.Contains(g.Instructions[0], "## Example") || !strings.Contains(g.Instructions[0], "MCP_GATEWAY_URL=$EX_AMPLE_MCP_URL mcpcli tools") ||
			!strings.Contains(g.Instructions[0], "`a`, `b`") || !strings.Contains(g.Instructions[0], cs.url) {
			t.Errorf("instructions:\n%s", g.Instructions[0])
		}
	}
}
