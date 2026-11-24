package gitlab

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"mono-jig/broker"
	"mono-jig/broker/mcp"
)

// GitLab's MCP server calls GitLab's API from GitLab's side, where the
// REST routes (routes.go) can't see it, so the broker decides each tool call
// from its name and arguments (docs/design.md, "Hosted MCP servers: filter
// tool calls"). tools/list is filtered to the tools below; any other tool
// is denied.

// readTools only read; they pass by name.
var readTools = []string{
	"get_artifact_file", "get_commit", "get_group", "get_job", "get_mcp_server_version",
	"get_merge_request", "get_pages_content", "get_pipeline", "get_project", "get_repository_file",
	"get_saved_view_work_items", "get_user", "get_work_item", "get_work_item_types",
	"list_branches", "list_commits", "list_groups", "list_merge_requests", "list_milestones",
	"list_pipeline_schedules", "list_pipelines", "list_project_members", "list_projects",
	"list_releases", "list_repository_tree", "list_tags", "list_todos", "list_user_events",
	"list_work_item_saved_views", "list_work_items", "search", "search_labels", "semantic_search",
}

// toolCheck decides one call of a writing tool from its arguments.
type toolCheck func(g *Checks, c *broker.Call, a *toolArgs) broker.Decision

// writeTools are checked with their arguments.
var writeTools = map[string]toolCheck{
	"save_merge_request":        (*Checks).saveMergeRequest,
	"save_note":                 (*Checks).saveNote,
	"save_merge_request_review": (*Checks).saveNote,
	"add_commit":                (*Checks).addToBranch,
	"add_branch":                (*Checks).addToBranch,
}

func (g *Checks) toolListed(name string) bool {
	return writeTools[name] != nil || isReadTool(name)
}

func isReadTool(name string) bool {
	for _, t := range readTools {
		if t == name {
			return true
		}
	}
	return false
}

// MCPCall checks one MCP request.
func (g *Checks) MCPCall(c *broker.Call) broker.Decision {
	m, d := mcp.Read(c)
	if !d.Allow {
		return d
	}
	switch m.Method {
	case "tools/list":
		c.Rewrite(mcp.FilterToolsList(g.toolListed))
		return broker.Allow()
	case "tools/call":
	default:
		return broker.Allow() // initialize, ping, notifications
	}
	var call mcp.ToolCall
	if err := json.Unmarshal(m.Params, &call); err != nil {
		return broker.Deny("bad tools/call params")
	}
	mcp.DenyAsTool(c, m.ID, call.Name)
	c.Note("tool", call.Name)
	switch {
	case isReadTool(call.Name):
		return broker.Allow()
	case call.Name == "accept_merge_request":
		return broker.Deny(broker.NeverMerge)
	case writeTools[call.Name] == nil:
		return broker.Deny("the GitLab MCP tool %s isn't on the broker's list", call.Name)
	}
	var a toolArgs
	if len(call.Arguments) > 0 {
		var all map[string]any
		if json.Unmarshal(call.Arguments, &a) != nil || json.Unmarshal(call.Arguments, &all) != nil {
			return broker.Deny("bad arguments")
		}
		a.flat = flatten(all)
	}
	c.Note("repo", rawString(a.ProjectID))
	return writeTools[call.Name](g, c, &a)
}

// toolArgs is the union of the arguments the write tools take.
type toolArgs struct {
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

	flat map[string]string // every argument, as params gives REST's
}

// flatten makes arguments the shape params gives: strings as they are,
// other values as JSON.
func flatten(all map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range all {
		switch x := v.(type) {
		case string:
			out[k] = x
		case nil:
		default:
			b, _ := json.Marshal(x)
			out[k] = string(b)
		}
	}
	return out
}

// saveMergeRequest creates (no merge_request_iid) or updates one, named
// by project_id and merge_request_iid only: one named by url could be
// taken for a create. No merge arranged, no quick action (as on REST).
func (g *Checks) saveMergeRequest(c *broker.Call, a *toolArgs) broker.Decision {
	if a.URL != "" {
		return broker.Deny("save_merge_request with url isn't on the broker's list: give project_id and merge_request_iid")
	}
	if d := broker.All(noAutoMerge(a.flat), noQuickActions(a.flat)); !d.Allow {
		return d
	}
	iid := rawString(a.MergeRequestIID)
	if iid == "" {
		if len(a.TargetProjectID) > 0 && string(a.TargetProjectID) != "null" {
			return broker.Deny("merge requests into another project aren't allowed")
		}
		var src string
		if a.SourceBranch != nil {
			src = *a.SourceBranch
		}
		return broker.AgentBranch(g.User, src)
	}
	pid := rawString(a.ProjectID)
	var source *string
	if pid != "" {
		source = g.mrSource(c, pid, iid)
	}
	return broker.ExistingFromAgentBranch(g.User, source)
}

// saveNote comments on or reviews a merge request from an agent branch;
// never approves, and runs no quick action.
func (g *Checks) saveNote(c *broker.Call, a *toolArgs) broker.Decision {
	if d := noQuickActions(a.flat); !d.Allow {
		return d
	}
	switch a.Method {
	case "approve", "unapprove":
		return broker.Deny(broker.NeverApprove)
	case "post_duo_review":
		return broker.Deny("post_duo_review isn't on the broker's list")
	}
	pid, iid, ok := g.mrTarget(a)
	if !ok {
		return broker.Deny("notes on anything but a merge request aren't on the broker's list")
	}
	c.Note("repo", pid)
	return broker.ExistingFromAgentBranch(g.User, g.mrSource(c, pid, iid))
}

// addToBranch commits to or creates a branch: an agent branch, in the
// same project.
func (g *Checks) addToBranch(_ *broker.Call, a *toolArgs) broker.Decision {
	if a.StartProject != "" {
		return broker.Deny("start_project isn't allowed")
	}
	var b string
	if a.Branch != nil {
		b = *a.Branch
	}
	return broker.AgentBranch(g.User, b)
}

var mrURLRE = regexp.MustCompile(`^/(.+?)/-/merge_requests/([0-9]+)/?$`)

// mrTarget finds the merge request a note or review names: url, or
// project_id with merge_request_iid. ok is false if it names anything else.
func (g *Checks) mrTarget(a *toolArgs) (pid, iid string, ok bool) {
	if a.URL != "" {
		u, err := url.Parse(a.URL)
		if err != nil || !strings.EqualFold(u.Host, g.API.URL.Host) {
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
