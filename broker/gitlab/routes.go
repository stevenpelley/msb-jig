package gitlab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"

	"mcp-restrict/broker"
	"mcp-restrict/broker/gitproto"
)

// routes is every request the GitLab provider serves, first match wins.
// Anything else is denied. The upstreams (gitlab.go):
//
//	g.git  HTTP Basic <user>:<token>  -> GitLab
//	g.api  Bearer <token>             -> GitLab
//	g.mcp  Bearer <MCP token>         -> GitLab's MCP server
func (g *gitlab) routes() []broker.Route {
	const p = "/api/v4/projects/{project}"
	const mr = p + "/merge_requests/{iid}"
	return []broker.Route{
		// GitLab's MCP server: each tool call is checked (mcp.go).
		{Method: "POST", Path: "/api/v4/mcp", Op: "mcp", Check: g.mcpCall, To: g.mcp},
		{Method: "GET DELETE", Path: "/api/v4/mcp", Op: "mcp.session", Check: broker.Read, To: g.mcp},

		// REST: never merge or approve. Listed so the denial says why.
		{Method: "PUT POST", Path: mr + "/merge", Op: "mr.merge", Check: broker.Never(neverMerge)},
		{Method: "POST", Path: mr + "/merge_when_pipeline_succeeds", Op: "mr.merge", Check: broker.Never(neverMerge)},
		{Method: "POST", Path: mr + "/approve", Op: "mr.approve", Check: broker.Never(neverApprove)},
		{Method: "POST", Path: mr + "/unapprove", Op: "mr.approve", Check: broker.Never(neverApprove)},

		// REST: reads.
		{Method: "GET HEAD", Path: "/api/v4/{path...}", Op: "api.read", Check: broker.Read, To: g.api},

		// REST: merge requests from agent branches.
		{Method: "POST", Path: p + "/merge_requests", Op: "mr.create", Check: g.mrCreate, To: g.api},
		{Method: "PUT", Path: mr, Op: "mr.update", Check: g.mrUpdate, To: g.api},
		{Method: "POST", Path: mr + "/notes", Op: "mr.note", Check: g.mrNote, To: g.api},
		{Method: "PUT", Path: mr + "/notes/{note}", Op: "mr.note", Check: g.mrNote, To: g.api},
		{Method: "POST", Path: mr + "/discussions", Op: "mr.note", Check: g.mrNote, To: g.api},
		{Method: "PUT", Path: mr + "/discussions/{discussion}", Op: "mr.note", Check: g.mrNote, To: g.api},
		{Method: "POST", Path: mr + "/discussions/{discussion}/notes", Op: "mr.note", Check: g.mrNote, To: g.api},
		{Method: "PUT", Path: mr + "/discussions/{discussion}/notes/{note}", Op: "mr.note", Check: g.mrNote, To: g.api},
		{Method: "DELETE", Path: p + "/repository/branches/{branch}", Op: "branch.delete", Check: g.branchDelete, To: g.api},

		// GraphQL: queries only.
		{Method: "GET POST", Path: "/api/graphql", Op: "graphql", Check: graphQLQueriesOnly, To: g.api},

		// git and LFS, at <repo>.git/… or <repo>/….
		{Method: "GET", Path: "/{repo...}/info/refs", Op: "git.refs", Check: g.gitRefs, To: g.git, Rewrite: gitproto.StripPushCaps},
		{Method: "POST", Path: "/{repo...}/git-upload-pack", Op: "git.fetch", Check: g.gitRepo, To: g.git},
		{Method: "POST", Path: "/{repo...}/git-receive-pack", Op: "git.push", Check: g.push, To: g.git},
		{Method: "POST", Path: "/{repo...}/info/lfs/objects/batch", Op: "lfs.batch", Check: g.gitRepo, To: g.git, Rewrite: g.rewriteLFSBatch},
		{Method: "GET PUT", Path: "/{repo...}/gitlab-lfs/objects/{oid}", Op: "lfs.object", Check: g.gitRepo, To: g.git},
		{Method: "GET PUT", Path: "/{repo...}/gitlab-lfs/objects/{oid}/{size}", Op: "lfs.object", Check: g.gitRepo, To: g.git},
		{Method: "GET POST", Path: "/{repo...}/info/lfs/locks", Op: "lfs.locks", Check: g.gitRepo, To: g.git},
		{Method: "POST", Path: "/{repo...}/info/lfs/locks/verify", Op: "lfs.locks", Check: g.gitRepo, To: g.git},
		{Method: "POST", Path: "/{repo...}/info/lfs/locks/{id}/unlock", Op: "lfs.locks", Check: g.gitRepo, To: g.git},
	}
}

const (
	neverMerge   = "merging is never allowed through the broker; a human merges"
	neverApprove = "approving is never allowed through the broker"
)

// --- REST checks ---

// mrCreate: from an agent branch, into the same project, with a tagged
// title, without arranging a merge.
func (g *gitlab) mrCreate(c *broker.Call) broker.Decision {
	ps, err := params(c)
	if err != nil {
		return broker.Deny("%v", err)
	}
	pid := c.Var("project")
	c.Note("repo", pid)
	if t, ok := ps["target_project_id"]; ok && !g.sameProject(c, pid, t) {
		return broker.Deny("merge requests into another project aren't allowed")
	}
	source := ps["source_branch"]
	return broker.All(
		noAutoMerge(ps),
		broker.AgentBranch(g.User, source),
		broker.TaggedTitle(g.User, opt(ps, "title"), true),
	)
}

// mrUpdate: only a merge request from an agent branch; a new title must
// be tagged; no merge arranged.
func (g *gitlab) mrUpdate(c *broker.Call) broker.Decision {
	ps, err := params(c)
	if err != nil {
		return broker.Deny("%v", err)
	}
	if d := noAutoMerge(ps); !d.Allow {
		return d
	}
	return broker.All(
		g.existing(c),
		broker.TaggedTitle(g.User, opt(ps, "title"), false),
	)
}

// mrNote: notes, discussions and resolving them, only on a merge request
// from an agent branch.
func (g *gitlab) mrNote(c *broker.Call) broker.Decision {
	if _, err := params(c); err != nil { // reads the body, so it's forwarded whole
		return broker.Deny("%v", err)
	}
	return g.existing(c)
}

func (g *gitlab) existing(c *broker.Call) broker.Decision {
	pid := c.Var("project")
	c.Note("repo", pid)
	return broker.ExistingFromAgentBranch(g.User, g.mrSource(c, pid, c.Var("iid")))
}

func noAutoMerge(ps map[string]string) broker.Decision {
	if ps["merge_when_pipeline_succeeds"] == "true" || ps["auto_merge"] == "true" {
		return broker.Deny("%s (auto-merge)", neverMerge)
	}
	return broker.Allow()
}

func (g *gitlab) branchDelete(c *broker.Call) broker.Decision {
	c.Note("repo", c.Var("project"))
	return broker.AgentBranch(g.User, c.Var("branch"))
}

// --- GraphQL ---

// graphQLQueriesOnly parses the documents rather than trusting operation
// names; only documents whose every operation is a query pass. No
// mutation is on the list yet (glab uses REST for writes).
func graphQLQueriesOnly(c *broker.Call) broker.Decision {
	var docs []string
	switch c.R.Method {
	case http.MethodGet:
		docs = []string{c.R.URL.Query().Get("query")}
	default:
		body, err := c.ReadBody()
		if err != nil {
			return broker.Deny("%v", err)
		}
		if docs, err = graphqlDocuments(body); err != nil {
			return broker.Deny("%v", err)
		}
	}
	for _, d := range docs {
		q, err := parser.ParseQuery(&ast.Source{Input: d})
		if err != nil || len(q.Operations) == 0 {
			return broker.Deny("unparseable GraphQL")
		}
		for _, op := range q.Operations {
			if op.Operation == ast.Query {
				continue
			}
			var fields []string
			for _, s := range op.SelectionSet {
				if f, ok := s.(*ast.Field); ok {
					fields = append(fields, f.Name)
					if f.Name == "mergeRequestAccept" || f.Name == "mergeRequestSetAutoMerge" {
						return broker.Deny("%s (GraphQL %s)", neverMerge, f.Name)
					}
				}
			}
			return broker.Deny("GraphQL %s %s isn't on the broker's list", op.Operation, strings.Join(fields, ","))
		}
	}
	return broker.Allow()
}

// graphqlDocuments returns the query text of a single request or a batch.
func graphqlDocuments(body []byte) ([]string, error) {
	type one struct {
		Query string `json:"query"`
	}
	b := bytes.TrimSpace(body)
	if len(b) > 0 && b[0] == '[' {
		var batch []one
		if err := json.Unmarshal(b, &batch); err != nil {
			return nil, fmt.Errorf("bad GraphQL batch")
		}
		var out []string
		for _, x := range batch {
			out = append(out, x.Query)
		}
		return out, nil
	}
	var x one
	if err := json.Unmarshal(b, &x); err != nil {
		return nil, fmt.Errorf("bad GraphQL request")
	}
	return []string{x.Query}, nil
}

// --- git ---

var repoPathRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)+$`)

// gitRepo checks the repository a git or LFS path names, and rewrites the
// path to its .git form, so upstream sees one spelling.
func (g *gitlab) gitRepo(c *broker.Call) broker.Decision {
	raw := c.Var("repo")
	repo := strings.TrimSuffix(raw, ".git")
	if c.R.URL.RawPath != "" || !repoPathRE.MatchString(repo) || strings.Contains(repo, "..") || strings.HasPrefix(repo, "api/") {
		return broker.Deny("not a repository path: %q", raw)
	}
	c.Note("repo", repo)
	c.R.URL.Path = "/" + repo + ".git" + strings.TrimPrefix(c.R.URL.Path, "/"+raw)
	return broker.Allow()
}

// gitRefs is the ref advertisement for a fetch or a push (the push itself
// is checked; push options are stripped from the advertisement).
func (g *gitlab) gitRefs(c *broker.Call) broker.Decision {
	if svc := c.R.URL.Query().Get("service"); svc != "git-upload-pack" && svc != "git-receive-pack" {
		return broker.Deny("info/refs service %q", svc)
	}
	return g.gitRepo(c)
}

func (g *gitlab) push(c *broker.Call) broker.Decision {
	if d := g.gitRepo(c); !d.Allow {
		return d
	}
	return gitproto.Push(g.User)(c)
}
