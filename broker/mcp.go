package broker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// The hosted-MCP filter (investigator-design.md, "Hosted MCP servers:
// filter tool calls"). GitLab's MCP server calls GitLab's API from
// GitLab's side, where the HTTP policy can't see it, so the broker proxies
// the MCP endpoint and decides each tool call from its name and arguments.
// Unknown tools and methods are denied, tools/list is filtered to match,
// and JSON-RPC batches are refused.

// GitLabMCPPath is GitLab's MCP endpoint, served on the same host.
const GitLabMCPPath = "/api/v4/mcp"

// gitlabReadTools are allowed by name. Every tool here only reads.
var gitlabReadTools = map[string]bool{
	"get_artifact_file": true, "get_commit": true, "get_group": true, "get_job": true,
	"get_mcp_server_version": true, "get_merge_request": true, "get_pages_content": true,
	"get_pipeline": true, "get_project": true, "get_repository_file": true,
	"get_saved_view_work_items": true, "get_user": true, "get_work_item": true,
	"get_work_item_types": true, "list_branches": true, "list_commits": true, "list_groups": true,
	"list_merge_requests": true, "list_milestones": true, "list_pipeline_schedules": true,
	"list_pipelines": true, "list_project_members": true, "list_projects": true,
	"list_releases": true, "list_repository_tree": true, "list_tags": true, "list_todos": true,
	"list_user_events": true, "list_work_item_saved_views": true, "list_work_items": true,
	"search": true, "search_labels": true, "semantic_search": true,
}

// gitlabCheckedTools write, and are classified with their arguments.
var gitlabCheckedTools = map[string]bool{
	"save_merge_request": true, "save_note": true, "save_merge_request_review": true,
	"add_commit": true, "add_branch": true, "accept_merge_request": true,
}

// gitlabToolListed reports whether tools/list should offer the tool.
// accept_merge_request is classified only so its denial says why.
func gitlabToolListed(name string) bool {
	return gitlabReadTools[name] || (gitlabCheckedTools[name] && name != "accept_merge_request")
}

// mcpMethods are the JSON-RPC methods passed on. Anything else is refused.
var mcpMethods = map[string]bool{
	"initialize": true, "ping": true, "tools/list": true, "tools/call": true,
	"notifications/initialized": true, "notifications/cancelled": true,
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (g *GitLab) mcp(w http.ResponseWriter, r *http.Request, req *Request) {
	switch r.Method {
	case http.MethodGet, http.MethodDelete:
		// The server-to-client stream, and the end of a session.
		g.forward(w, r, http.NoBody, g.mcpAuth, nil)
		return
	case http.MethodPost:
	default:
		httpError(w, http.StatusMethodNotAllowed, "MCP takes GET, POST and DELETE")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAPIBody+1))
	if err != nil || len(body) > maxAPIBody {
		httpError(w, http.StatusRequestEntityTooLarge, "MCP request too large")
		return
	}
	if t := bytes.TrimLeft(body, " \t\r\n"); len(t) > 0 && t[0] == '[' {
		rpcError(w, nil, -32600, "investigator broker: JSON-RPC batches are not accepted")
		return
	}
	var m rpcMessage
	if err := json.Unmarshal(body, &m); err != nil || m.JSONRPC != "2.0" {
		rpcError(w, nil, -32700, "investigator broker: not a JSON-RPC 2.0 message")
		return
	}
	if m.Method == "" {
		// A response to a server request (sampling, elicitation): none are expected.
		rpcError(w, m.ID, -32600, "investigator broker: responses to server requests are not accepted")
		return
	}
	if !mcpMethods[m.Method] {
		rpcError(w, m.ID, -32601, "investigator broker: MCP method "+m.Method+" is not allowed")
		return
	}
	switch m.Method {
	case "tools/list":
		g.forward(w, r, bytes.NewReader(body), g.mcpAuth, filterToolsList)
		return
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			rpcError(w, m.ID, -32602, "investigator broker: bad tools/call params")
			return
		}
		req.Path = GitLabMCPPath + "#" + p.Name
		g.classifyTool(r, p.Name, p.Arguments, req)
		if d := g.decide(req); !d.Allow {
			toolDenied(w, m.ID, p.Name, d.Reason)
			return
		}
	}
	r.ContentLength = int64(len(body))
	g.forward(w, r, bytes.NewReader(body), g.mcpAuth, nil)
}

// mcpArgs is the union of the arguments the checked tools take.
type mcpArgs struct {
	ProjectID       json.RawMessage `json:"project_id"`
	URL             string          `json:"url"`
	MergeRequestIID json.RawMessage `json:"merge_request_iid"`
	WorkItemIID     json.RawMessage `json:"work_item_iid"`
	GroupID         string          `json:"group_id"`
	Title           *string         `json:"title"`
	SourceBranch    *string         `json:"source_branch"`
	TargetProjectID json.RawMessage `json:"target_project_id"`
	Branch          *string         `json:"branch"`
	StartProject    string          `json:"start_project"`
	Method          string          `json:"method"`
}

var mrURLRE = regexp.MustCompile(`^/(.+?)/-/merge_requests/([0-9]+)/?$`)

// rawString accepts a JSON string or number (project and MR ids come as either).
func rawString(r json.RawMessage) string {
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(r, &n) == nil {
		return n.String()
	}
	return ""
}

// mrTarget finds the merge request a note or review names: url, or
// project_id with merge_request_iid. ok is false if it names anything else.
func (g *GitLab) mrTarget(a *mcpArgs) (pid, iid string, ok bool) {
	if a.URL != "" {
		u, err := url.Parse(a.URL)
		if err != nil || !strings.EqualFold(u.Host, g.Upstream.Host) {
			return "", "", false
		}
		m := mrURLRE.FindStringSubmatch(u.Path)
		if m == nil {
			return "", "", false
		}
		return m[1], m[2], true
	}
	pid, iid = rawString(a.ProjectID), rawString(a.MergeRequestIID)
	if pid == "" || iid == "" || len(a.WorkItemIID) > 0 || a.GroupID != "" {
		return "", "", false
	}
	if _, err := strconv.ParseUint(iid, 10, 64); err != nil {
		return "", "", false
	}
	return pid, iid, true
}

func (g *GitLab) classifyTool(r *http.Request, name string, raw json.RawMessage, req *Request) {
	req.Detail = "mcp " + name
	if gitlabReadTools[name] {
		req.Op = OpAPIRead
		return
	}
	if !gitlabCheckedTools[name] {
		switch name {
		case "apply_suggestion", "save_project", "fork_repository", "save_pipeline", "save_pipeline_job",
			"save_work_item", "link_work_items":
			req.Op = OpWrite
		default:
			req.Op = OpUnknown
			req.Detail = "unknown MCP tool " + name
		}
		return
	}
	var a mcpArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			req.Op, req.Detail = OpUnknown, "mcp "+name+": bad arguments"
			return
		}
	}
	req.Repo = rawString(a.ProjectID)
	switch name {
	case "accept_merge_request":
		req.Op = OpMerge

	case "save_merge_request":
		iid := rawString(a.MergeRequestIID)
		if iid == "" {
			if len(a.TargetProjectID) > 0 && string(a.TargetProjectID) != "null" {
				req.Op, req.Detail = OpWrite, "mcp save_merge_request: cross-project merge requests"
				return
			}
			req.Op, req.SourceBranch, req.Title = OpMRCreate, a.SourceBranch, a.Title
			return
		}
		req.Op, req.Title = OpMRUpdate, a.Title
		if req.Repo != "" {
			req.ExistingSourceBranch = g.existingSourceBranch(r, req.Repo, iid)
		}

	case "save_note", "save_merge_request_review":
		switch a.Method {
		case "approve", "unapprove":
			req.Op = OpApprove
			return
		case "post_duo_review":
			req.Op, req.Detail = OpWrite, "mcp "+name+": post_duo_review"
			return
		}
		pid, iid, ok := g.mrTarget(&a)
		if !ok {
			req.Op, req.Detail = OpWrite, "mcp "+name+": notes on anything but a merge request"
			return
		}
		req.Op, req.Repo = OpMRNote, pid
		req.ExistingSourceBranch = g.existingSourceBranch(r, pid, iid)

	case "add_commit", "add_branch":
		if a.StartProject != "" {
			req.Op, req.Detail = OpWrite, "mcp add_commit: start_project"
			return
		}
		req.Op = OpGitPush
		if a.Branch != nil {
			req.Refs = []RefUpdate{{Old: zeroOID, New: zeroOID, Ref: "refs/heads/" + *a.Branch}}
		}
	}
}

func (g *GitLab) mcpAuth(h http.Header) error {
	tok := g.Token
	if g.MCPToken != nil {
		tok = g.MCPToken
	}
	t, err := tok()
	if err != nil {
		return err
	}
	h.Set("Authorization", "Bearer "+t)
	return nil
}

func rpcError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg}})
}

// toolDenied answers like a failed tool, so the agent reads the reason.
func toolDenied(w http.ResponseWriter, id json.RawMessage, tool, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
		"isError": true,
		"content": []map[string]string{{"type": "text", "text": fmt.Sprintf("investigator broker denied %s: %s", tool, reason)}},
	}})
}

// filterToolsList drops the tools the broker would deny from a tools/list
// result, in a JSON body or in a server-sent event stream.
func filterToolsList(resp *http.Response) (io.Reader, error) {
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(filterSSE(resp.Body, pw)) }()
		return pr, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8*maxAPIBody))
	if err != nil {
		return nil, err
	}
	out, err := filterToolsJSON(b)
	return bytes.NewReader(out), err
}

func filterToolsJSON(b []byte) ([]byte, error) {
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
		if json.Unmarshal(t, &tn) == nil && gitlabToolListed(tn.Name) {
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
func filterSSE(in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 8*maxAPIBody)
	var data []string
	var other []string
	flush := func() error {
		var b strings.Builder
		for _, l := range other {
			b.WriteString(l + "\n")
		}
		if len(data) > 0 {
			payload := strings.Join(data, "\n")
			if f, err := filterToolsJSON([]byte(payload)); err == nil {
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
