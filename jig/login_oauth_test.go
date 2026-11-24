package jig

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevenpelley/msb-jig/broker/providers"
)

var exampleOAuth = OAuthMCP{ID: "ex-ample", Title: "Example", URL: "https://mcp.example.org/", Scopes: []string{"read:all", "offline_access"}, Tools: []string{"get_item"}}

func TestOAuthMCPBroker(t *testing.T) {
	t.Setenv("JIG_EX_AMPLE_TOKEN_COMMAND", "echo tok")
	p, ok := exampleOAuth.CLI().Broker(Settings{}).(providers.MCPServer)
	if !ok || p.TokenCmd != "echo tok" || p.Name() != "ex-ample" || p.Tools[0] != "get_item" || p.Token != nil {
		t.Fatalf("env: %+v", p)
	}
	t.Setenv("JIG_EX_AMPLE_TOKEN_COMMAND", "")
	if p := exampleOAuth.CLI().Broker(Settings{}); p != nil {
		t.Fatalf("env empty: %+v", p)
	}
	if got := exampleOAuth.CLI().Usage; !strings.HasPrefix(got, "login ex-ample          sign in to Example's") {
		t.Errorf("usage %q", got)
	}
}

func TestLockFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := lockFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan bool)
	go func() {
		u, err := lockFile(p)
		if err == nil {
			u()
		}
		got <- err == nil
	}()
	select {
	case <-got:
		t.Fatal("second lock taken while the first is held")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	if !<-got {
		t.Fatal("second lock failed")
	}
}

// listMCPTools: a session, pages, JSON and SSE answers.
func TestListMCPTools(t *testing.T) {
	for _, sse := range []bool{false, true} {
		var deleted bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Method == "DELETE" {
				deleted = r.Header.Get("Mcp-Session-Id") == "s1"
				return
			}
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					Cursor string `json:"cursor"`
				} `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&m)
			if m.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "s1" {
				http.Error(w, "no session", http.StatusBadRequest)
				return
			}
			w.Header().Set("Mcp-Session-Id", "s1")
			var result any = map[string]any{}
			switch {
			case m.Method == "notifications/initialized":
				w.WriteHeader(http.StatusAccepted)
				return
			case m.Method == "tools/list" && m.Params.Cursor == "":
				result = map[string]any{"tools": []any{map[string]any{"name": "a", "annotations": map[string]bool{"readOnlyHint": true}}}, "nextCursor": "p2"}
			case m.Method == "tools/list":
				result = map[string]any{"tools": []any{map[string]any{"name": "b"}}}
			}
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
			if sse {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\ndata: %s\n\n", b)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		}))
		tools, err := listMCPTools(context.Background(), srv.Client(), srv.URL, "tok")
		srv.Close()
		if err != nil {
			t.Fatalf("sse=%v: %v", sse, err)
		}
		if len(tools) != 2 || tools[0].Name != "a" || tools[0].Annotations.ReadOnly == nil || !*tools[0].Annotations.ReadOnly || tools[1].Name != "b" || !deleted {
			t.Errorf("sse=%v: %+v, deleted %v", sse, tools, deleted)
		}
	}
}
