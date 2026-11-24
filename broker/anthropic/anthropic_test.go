package anthropic_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevenpelley/msb-jig/broker"
	"github.com/stevenpelley/msb-jig/broker/providers"
)

const anthropicHost = "anthropic.broker.internal"

func anthropicBroker(t *testing.T, upstream http.Handler, tokenCmd string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		upstream.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)
	ps, err := providers.Build(context.Background(), providers.Config{Providers: []providers.Provider{providers.Anthropic{TokenCmd: tokenCmd, APIURL: up.URL}}}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&broker.Server{Providers: ps})
	t.Cleanup(srv.Close)
	return srv, &calls
}

func anthropicReq(t *testing.T, srv *httptest.Server, method, path, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Host = anthropicHost
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestAnthropicCredential(t *testing.T) {
	type seen struct {
		auth, key, body string
		beta            []string
	}
	cases := []struct {
		name, tok string
		hdr       map[string]string
		want      seen
	}{
		{"oauth token, Claude Code's placeholder", "sk-ant-oat01-real",
			map[string]string{"Authorization": "Bearer placeholder", "Anthropic-Beta": "oauth-2025-04-20,claude-code-20250219"},
			seen{auth: "Bearer sk-ant-oat01-real", beta: []string{"oauth-2025-04-20,claude-code-20250219"}}},
		{"oauth token, no beta flag sent", "sk-ant-oat01-real",
			map[string]string{"X-Api-Key": "placeholder"},
			seen{auth: "Bearer sk-ant-oat01-real", beta: []string{"oauth-2025-04-20"}}},
		{"api key", "sk-ant-api03-real",
			map[string]string{"Authorization": "Bearer placeholder", "X-Api-Key": "placeholder"},
			seen{key: "sk-ant-api03-real"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got seen
			srv, _ := anthropicBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				got = seen{auth: r.Header.Get("Authorization"), key: r.Header.Get("X-Api-Key"), beta: r.Header.Values("Anthropic-Beta"), body: string(b)}
				_, _ = io.WriteString(w, `{"ok":true}`)
			}), "echo "+c.tok)
			resp := anthropicReq(t, srv, "POST", "/v1/messages", `{"model":"m"}`, c.hdr)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			c.want.body = `{"model":"m"}`
			if got.auth != c.want.auth || got.key != c.want.key || got.body != c.want.body || strings.Join(got.beta, "|") != strings.Join(c.want.beta, "|") {
				t.Fatalf("upstream saw %+v, want %+v", got, c.want)
			}
		})
	}
}

// A streamed response reaches the client event by event.
func TestAnthropicStreams(t *testing.T) {
	release := make(chan struct{})
	srv, _ := anthropicBroker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "event: message_stop\ndata: {}\n\n")
	}), "echo sk-ant-oat01-real")
	defer close(release)
	resp := anthropicReq(t, srv, "POST", "/v1/messages", `{"stream":true}`, nil)
	first := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(resp.Body).ReadString('\n')
		first <- l
	}()
	select {
	case l := <-first:
		if l != "event: message_start\n" {
			t.Fatalf("first line %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first event didn't arrive before the stream ended: the broker buffers")
	}
}

// Reads of the account pass.
func TestAnthropicAccountReads(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	srv, calls := anthropicBroker(t, ok, "echo sk-ant-oat01-real")
	if resp := anthropicReq(t, srv, "GET", "/api/oauth/profile", "", nil); resp.StatusCode != http.StatusOK || calls.Load() != 1 {
		t.Errorf("GET /api/oauth/profile: %d, %d calls", resp.StatusCode, calls.Load())
	}
}

func TestAnthropicRefusals(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	srv, calls := anthropicBroker(t, ok, "echo sk-ant-oat01-real")
	if resp := anthropicReq(t, srv, "GET", "/admin", "", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("path outside /v1/ and /api/: %d", resp.StatusCode)
	}
	// Writes to the account's OAuth endpoints could mint a credential.
	if resp := anthropicReq(t, srv, "POST", "/api/oauth/claude_cli/create_api_key", "{}", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("create_api_key: %d", resp.StatusCode)
	}
	srv2, calls2 := anthropicBroker(t, ok, "false")
	if resp := anthropicReq(t, srv2, "POST", "/v1/messages", "{}", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no credential: %d", resp.StatusCode)
	}
	if calls.Load()+calls2.Load() != 0 {
		t.Errorf("refused requests reached upstream")
	}
}
