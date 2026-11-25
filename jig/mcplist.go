package jig

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// mcpTool is one tool a server lists, with the hint it gives.
type mcpTool struct {
	Name        string `json:"name"`
	Annotations struct {
		ReadOnly *bool `json:"readOnlyHint"`
	} `json:"annotations"`
}

// listMCPTools lists a hosted MCP server's tools with token, on the Mac
// (login shows them, to choose the broker's list): initialize, then
// tools/list page by page, then the session's end.
func listMCPTools(ctx context.Context, c *http.Client, url, token string) ([]mcpTool, error) {
	session := ""
	call := func(id int, method string, params any) (json.RawMessage, error) {
		msg := map[string]any{"jsonrpc": "2.0", "method": method}
		if params != nil {
			msg["params"] = params
		}
		if id > 0 {
			msg["id"] = id
		}
		b, _ := json.Marshal(msg)
		req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
			session = s
		}
		if id == 0 {
			return nil, nil // a notification
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", method, resp.Status)
		}
		return rpcResult(resp, id)
	}
	init := map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "mono-jig", "version": "1"}}
	if _, err := call(1, "initialize", init); err != nil {
		return nil, err
	}
	_, _ = call(0, "notifications/initialized", nil)
	var tools []mcpTool
	cursor := ""
	for page := 0; page < 50; page++ {
		var params any
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}
		res, err := call(2+page, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var r struct {
			Tools      []mcpTool `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &r); err != nil {
			return nil, fmt.Errorf("tools/list: %v", err)
		}
		tools = append(tools, r.Tools...)
		if cursor = r.NextCursor; cursor == "" {
			break
		}
	}
	if session != "" {
		req, _ := http.NewRequestWithContext(ctx, "DELETE", url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Mcp-Session-Id", session)
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	return tools, nil
}

// rpcResult reads the response with id from a JSON body or an SSE
// stream, and returns its result.
func rpcResult(resp *http.Response, id int) (json.RawMessage, error) {
	one := func(b []byte) (json.RawMessage, bool, error) {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &m) != nil || string(m.ID) != fmt.Sprint(id) {
			return nil, false, nil
		}
		if m.Error != nil {
			return nil, true, fmt.Errorf("%s", m.Error.Message)
		}
		return m.Result, true, nil
	}
	body := io.LimitReader(resp.Body, 16<<20)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, err := io.ReadAll(body)
		if err != nil {
			return nil, err
		}
		if r, ok, err := one(b); ok {
			return r, err
		}
		return nil, fmt.Errorf("no response with id %d", id)
	}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var data []string
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(l, "data:"), " "))
			continue
		}
		if l == "" && len(data) > 0 {
			if r, ok, err := one([]byte(strings.Join(data, "\n"))); ok {
				return r, err
			}
			data = nil
		}
	}
	if len(data) > 0 {
		if r, ok, err := one([]byte(strings.Join(data, "\n"))); ok {
			return r, err
		}
	}
	return nil, fmt.Errorf("no response with id %d in the stream", id)
}
