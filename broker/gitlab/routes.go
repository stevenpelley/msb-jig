package gitlab

import (
	"mcp-restrict/broker"
	"mcp-restrict/broker/gitproto"
)

// Config is what the provider needs from investigator login.
type Config struct {
	User     string       // the GitLab user the agent acts for
	URL      string       // https://gitlab.com
	Token    broker.Token // the user's token (glab's login, or the Keychain)
	MCPToken broker.Token // a fine-grained PAT for GitLab's MCP server; nil: Token
}

// Routes is every request the GitLab provider serves: its upstreams, each
// with its credential, then its routes. The first route that matches
// serves a request; anything else is denied. The checks are in checks.go
// and mcp.go.
func Routes(up broker.Upstreams, c Config) []broker.Route {
	git := up.To(c.URL, broker.Basic(c.User, c.Token)) // git and LFS: HTTP Basic, as git sends it
	api := up.To(c.URL, broker.Bearer(c.Token))        // REST and GraphQL (Bearer takes PATs and OAuth tokens)
	mcp := up.To(c.URL, broker.Bearer(c.mcpToken()))   // GitLab's MCP server
	gl := &Checks{User: c.User, API: api}

	const p = "/api/v4/projects/{project}"
	const mr = p + "/merge_requests/{iid}"
	return []broker.Route{
		// GitLab's MCP server: each tool call is checked (mcp.go).
		{Method: "POST", Path: "/api/v4/mcp", Op: "mcp", Check: gl.MCPCall, To: mcp},
		{Method: "GET DELETE", Path: "/api/v4/mcp", Op: "mcp.session", Check: broker.Read, To: mcp},

		// REST: never merge or approve. Listed so the denial says why.
		{Method: "PUT POST", Path: mr + "/merge", Op: "mr.merge", Check: broker.Never(broker.NeverMerge)},
		{Method: "POST", Path: mr + "/merge_when_pipeline_succeeds", Op: "mr.merge", Check: broker.Never(broker.NeverMerge)},
		{Method: "POST", Path: mr + "/approve", Op: "mr.approve", Check: broker.Never(broker.NeverApprove)},
		{Method: "POST", Path: mr + "/unapprove", Op: "mr.approve", Check: broker.Never(broker.NeverApprove)},

		// REST: reads.
		{Method: "GET HEAD", Path: "/api/v4/{path...}", Op: "api.read", Check: broker.Read, To: api},

		// REST: merge requests from agent branches.
		{Method: "POST", Path: p + "/merge_requests", Op: "mr.create", Check: gl.MRCreate, To: api},
		{Method: "PUT", Path: mr, Op: "mr.update", Check: gl.MRUpdate, To: api},
		{Method: "POST", Path: mr + "/notes", Op: "mr.note", Check: gl.MRNote, To: api},
		{Method: "PUT", Path: mr + "/notes/{note}", Op: "mr.note", Check: gl.MRNote, To: api},
		{Method: "POST", Path: mr + "/discussions", Op: "mr.note", Check: gl.MRNote, To: api},
		{Method: "PUT", Path: mr + "/discussions/{discussion}", Op: "mr.note", Check: gl.MRNote, To: api},
		{Method: "POST", Path: mr + "/discussions/{discussion}/notes", Op: "mr.note", Check: gl.MRNote, To: api},
		{Method: "PUT", Path: mr + "/discussions/{discussion}/notes/{note}", Op: "mr.note", Check: gl.MRNote, To: api},
		{Method: "DELETE", Path: p + "/repository/branches/{branch}", Op: "branch.delete", Check: gl.BranchDelete, To: api},

		// GraphQL: queries only.
		{Method: "GET POST", Path: "/api/graphql", Op: "graphql", Check: GraphQLQueriesOnly, To: api},

		// git and LFS, at <repo>.git/… or <repo>/….
		{Method: "GET", Path: "/{repo...}/info/refs", Op: "git.refs", Check: gl.GitRefs, To: git, Rewrite: gitproto.StripPushCaps},
		{Method: "POST", Path: "/{repo...}/git-upload-pack", Op: "git.fetch", Check: gl.GitRepo, To: git},
		{Method: "POST", Path: "/{repo...}/git-receive-pack", Op: "git.push", Check: gl.Push, To: git},
		{Method: "POST", Path: "/{repo...}/info/lfs/objects/batch", Op: "lfs.batch", Check: gl.GitRepo, To: git, Rewrite: gl.RewriteLFSBatch},
		{Method: "GET PUT", Path: "/{repo...}/gitlab-lfs/objects/{oid}", Op: "lfs.object", Check: gl.GitRepo, To: git},
		{Method: "GET PUT", Path: "/{repo...}/gitlab-lfs/objects/{oid}/{size}", Op: "lfs.object", Check: gl.GitRepo, To: git},
		{Method: "GET POST", Path: "/{repo...}/info/lfs/locks", Op: "lfs.locks", Check: gl.GitRepo, To: git},
		{Method: "POST", Path: "/{repo...}/info/lfs/locks/verify", Op: "lfs.locks", Check: gl.GitRepo, To: git},
		{Method: "POST", Path: "/{repo...}/info/lfs/locks/{id}/unlock", Op: "lfs.locks", Check: gl.GitRepo, To: git},
	}
}

func (c Config) mcpToken() broker.Token {
	if c.MCPToken != nil {
		return c.MCPToken
	}
	return c.Token
}
