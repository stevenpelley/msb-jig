// Package mcp holds what the broker's MCP endpoints share: reading one
// JSON-RPC message from a request, answering in JSON-RPC, and filtering a
// tools/list result.
package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"mono-jig/broker"
)

// Message is one JSON-RPC message.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Methods are the MCP methods passed on. Anything else is refused.
var Methods = map[string]bool{
	"initialize": true, "ping": true, "tools/list": true, "tools/call": true,
	"notifications/initialized": true, "notifications/cancelled": true,
}

// Read reads the call's body as one JSON-RPC request with an allowed
// method. Not one, it returns a denial, written as a JSON-RPC error:
// batches, responses to server requests (sampling, elicitation; none are
// expected) and other methods are refused.
func Read(c *broker.Call) (*Message, broker.Decision) {
	var id json.RawMessage
	code := -32600
	c.DenyWith(func(w http.ResponseWriter, reason string) { RPCError(w, id, code, "mono-jig broker: "+reason) })
	body, err := c.ReadBody()
	if err != nil {
		return nil, broker.Deny("%v", err)
	}
	if t := bytes.TrimLeft(body, " \t\r\n"); len(t) > 0 && t[0] == '[' {
		return nil, broker.Deny("JSON-RPC batches are not accepted")
	}
	var m Message
	if err := json.Unmarshal(body, &m); err != nil || m.JSONRPC != "2.0" {
		code = -32700
		return nil, broker.Deny("not a JSON-RPC 2.0 message")
	}
	id = m.ID
	if m.Method == "" {
		return nil, broker.Deny("responses to server requests are not accepted")
	}
	c.Note("rpc", m.Method)
	if !Methods[m.Method] {
		code = -32601
		return nil, broker.Deny("MCP method %s is not allowed", m.Method)
	}
	return &m, broker.Allow()
}

// ToolCall is a tools/call's params.
type ToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// DenyAsTool writes later denials of the call as a failed tool result, so
// the agent reads the reason.
func DenyAsTool(c *broker.Call, id json.RawMessage, tool string) {
	c.DenyWith(func(w http.ResponseWriter, reason string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
			"isError": true,
			"content": []map[string]string{{"type": "text", "text": fmt.Sprintf("mono-jig broker denied %s: %s", tool, reason)}},
		}})
	})
}

// RPCError answers with a JSON-RPC error.
func RPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg}})
}

// WriteResult answers with a JSON-RPC result.
func WriteResult(w http.ResponseWriter, id, result json.RawMessage, session string) {
	w.Header().Set("Content-Type", "application/json")
	if session != "" {
		w.Header().Set("Mcp-Session-Id", session)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// FilterToolsList returns a response rewrite that keeps only the tools
// keep accepts in a tools/list result, in a JSON body or an SSE stream.
func FilterToolsList(keep func(name string) bool) func(*http.Response) (io.Reader, error) {
	return func(resp *http.Response) (io.Reader, error) {
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			pr, pw := io.Pipe()
			go func() { pw.CloseWithError(filterSSE(resp.Body, pw, keep)) }()
			return pr, nil
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 8*broker.MaxBody))
		if err != nil {
			return nil, err
		}
		out, err := filterToolsJSON(b, keep)
		return bytes.NewReader(out), err
	}
}

func filterToolsJSON(b []byte, keep func(string) bool) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	res, ok := m["result"]
	if !ok {
		return b, nil // an error response
	}
	var r map[string]json.RawMessage
	if err := json.Unmarshal(res, &r); err != nil {
		return nil, err
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(r["tools"], &tools); err != nil {
		return nil, fmt.Errorf("tools/list result without tools")
	}
	kept := []json.RawMessage{}
	for _, t := range tools {
		var tn struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(t, &tn) == nil && keep(tn.Name) {
			kept = append(kept, t)
		}
	}
	if r["tools"], _ = json.Marshal(kept); r["tools"] == nil {
		return nil, fmt.Errorf("marshal")
	}
	m["result"], _ = json.Marshal(r)
	return json.Marshal(m)
}

// filterSSE rewrites each event's data as filterToolsJSON does. Events
// whose data isn't a tools/list result pass through unchanged.
func filterSSE(in io.Reader, out io.Writer, keep func(string) bool) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 8*broker.MaxBody)
	var data, other []string
	flush := func() error {
		var b strings.Builder
		for _, l := range other {
			b.WriteString(l + "\n")
		}
		if len(data) > 0 {
			payload := strings.Join(data, "\n")
			if f, err := filterToolsJSON([]byte(payload), keep); err == nil {
				payload = string(f)
			}
			for _, l := range strings.Split(payload, "\n") {
				b.WriteString("data: " + l + "\n")
			}
		}
		b.WriteString("\n")
		data, other = nil, nil
		_, err := io.WriteString(out, b.String())
		return err
	}
	for sc.Scan() {
		l := sc.Text()
		switch {
		case l == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(l, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(l, "data:"), " "))
		default:
			other = append(other, l)
		}
	}
	if len(data) > 0 || len(other) > 0 {
		if err := flush(); err != nil {
			return err
		}
	}
	return sc.Err()
}
