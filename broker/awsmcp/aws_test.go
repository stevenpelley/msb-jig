package awsmcp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"log/slog"

	"github.com/stevenpelley/msb-jig/broker"
	"github.com/stevenpelley/msb-jig/broker/awsmcp"
	"github.com/stevenpelley/msb-jig/broker/providers"
)

// awsProtocolVersion is what the broker asks the proxy for (bridge.go).
const awsProtocolVersion = "2025-06-18"

// The fake AWS MCP proxy: this test binary, run with FAKE_AWS_MCP=1. It
// answers initialize and tools/list; tools/call "echo" returns its
// arguments (after a ping and a notification, which the broker must
// handle), "slow" waits first, and "die" exits the process.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_AWS_MCP") == "1" {
		fakeAWSMCP()
		return
	}
	os.Exit(m.Run())
}

func fakeAWSMCP() {
	var mu sync.Mutex
	send := func(v any) {
		b, _ := json.Marshal(v)
		mu.Lock()
		defer mu.Unlock()
		os.Stdout.Write(append(b, '\n'))
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || len(m.ID) == 0 || m.Method == "" {
			continue // notifications, and the broker's answers to our ping
		}
		reply := func(result any) { send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result}) }
		switch m.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": awsProtocolVersion, "serverInfo": map[string]string{"name": "fake"},
				"capabilities": map[string]any{"tools": map[string]any{}}, "pid": os.Getpid()})
		case "tools/list":
			reply(map[string]any{"tools": []map[string]string{{"name": "aws___call_aws"}}})
		case "tools/call":
			switch m.Params.Name {
			case "die":
				os.Exit(3)
			case "slow":
				go func(id json.RawMessage) {
					time.Sleep(300 * time.Millisecond)
					send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"slow": true}})
				}(m.ID)
				continue
			}
			send(map[string]any{"jsonrpc": "2.0", "id": "srv-1", "method": "ping"})
			send(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{}})
			reply(map[string]any{"echo": m.Params.Arguments})
		default:
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "no"}})
		}
	}
}

func awsBroker(t *testing.T) *httptest.Server { return awsBrokerMode(t, false) }

// awsBrokerMode is the broker with the AWS provider alone, in server or
// proxy mode, the fake MCP server behind it.
func awsBrokerMode(t *testing.T, proxy bool) *httptest.Server {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ps, err := providers.Build(ctx, providers.Config{Proxy: proxy, Providers: []providers.Provider{providers.AWS{Command: "env FAKE_AWS_MCP=1 " + exe}}}, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&broker.Server{Providers: ps})
	t.Cleanup(func() { srv.Close(); cancel() })
	return srv
}

func awsPost(t *testing.T, srv *httptest.Server, body string) (map[string]any, *http.Response) {
	return awsPostHost(t, srv, "aws.broker.internal", body)
}

func awsPostHost(t *testing.T, srv *httptest.Server, host, body string) (map[string]any, *http.Response) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+awsmcp.Path, strings.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
	}
	return out, resp
}

func TestAWSMCPStatelessCall(t *testing.T) {
	srv := awsBroker(t)
	// No initialize, no session: curl's case.
	out, _ := awsPost(t, srv, `{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"echo","arguments":{"aws_profile":"p","x":1}}}`)
	if out["id"] != "a" {
		t.Fatalf("id not mapped back: %v", out)
	}
	echo, _ := json.Marshal(out["result"])
	if !strings.Contains(string(echo), `"x":1`) {
		t.Fatalf("result: %s", echo)
	}
}

// In proxy mode the broker answers the AWS MCP Server's own host, through
// the proxy on the Mac, and no longer serves the internal name.
func TestAWSMCPProxyMode(t *testing.T) {
	srv := awsBrokerMode(t, true)
	out, _ := awsPostHost(t, srv, providers.AWSAPIHost, `{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"echo","arguments":{"x":1}}}`)
	if out["id"] != "a" {
		t.Fatalf("id not mapped back: %v", out)
	}
	if _, resp := awsPostHost(t, srv, "aws.broker.internal", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); resp.StatusCode == http.StatusOK {
		t.Error("the internal name is served in proxy mode")
	}
}

func TestAWSMCPInitializeAndSession(t *testing.T) {
	srv := awsBroker(t)
	out, resp := awsPost(t, srv, `{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	if resp.Header.Get("Mcp-Session-Id") == "" || out["id"] != float64(7) {
		t.Fatalf("initialize: %v %v", out, resp.Header)
	}
	if r, _ := out["result"].(map[string]any); r["protocolVersion"] != awsProtocolVersion {
		t.Fatalf("result: %v", out)
	}
	_, resp = awsPost(t, srv, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification: %d", resp.StatusCode)
	}
	out, _ = awsPost(t, srv, `{"jsonrpc":"2.0","id":8,"method":"tools/list"}`)
	if !strings.Contains(fmt.Sprint(out["result"]), "aws___call_aws") {
		t.Fatalf("tools/list: %v", out)
	}
}

func TestAWSMCPConcurrentIDs(t *testing.T) {
	srv := awsBroker(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := "echo"
			if i%2 == 0 {
				name = "slow"
			}
			// Every client uses id 1: the broker's own ids keep them apart.
			out, _ := awsPost(t, srv, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{"n":%d}}}`, name, i))
			b, _ := json.Marshal(out["result"])
			if name == "echo" && !strings.Contains(string(b), fmt.Sprintf(`"n":%d`, i)) {
				t.Errorf("call %d got %s", i, b)
			}
			if name == "slow" && !strings.Contains(string(b), `"slow":true`) {
				t.Errorf("call %d got %s", i, b)
			}
		}()
	}
	wg.Wait()
}

func TestAWSMCPRestartsTheProxy(t *testing.T) {
	srv := awsBroker(t)
	out, _ := awsPost(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"die"}}`)
	if out["error"] == nil {
		t.Fatalf("a call the proxy died on: %v", out)
	}
	out, _ = awsPost(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	if out["result"] == nil {
		t.Fatalf("after restart: %v", out)
	}
}

func TestAWSMCPRefusals(t *testing.T) {
	srv := awsBroker(t)
	for _, body := range []string{
		`[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`,
		`{"jsonrpc":"2.0","id":1,"method":"resources/read"}`,
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`not json`,
	} {
		out, _ := awsPost(t, srv, body)
		if out["error"] == nil {
			t.Errorf("%s: not refused: %v", body, out)
		}
	}
	req, _ := http.NewRequest("GET", srv.URL+awsmcp.Path, nil)
	req.Host = "aws.broker.internal"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET: %d", resp.StatusCode)
	}
}
