package github

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"mcp-restrict/broker"
	"mcp-restrict/broker/gitproto"
)

// routes is every request the GitHub provider serves, first match wins.
// Anything else is denied. The upstreams (github.go):
//
//	g.git      HTTP Basic <user>:<token>  -> github.com
//	g.api      Bearer <token>             -> api.github.com (/api/v3 removed)
//	g.graphql  Bearer <token>             -> api.github.com/graphql
func (g *github) routes() []broker.Route {
	const repo = "/api/v3/repos/{owner}/{repo}"
	const pr = repo + "/pulls/{number}"
	return []broker.Route{
		// Never merge. Listed so the denial says why; approving is refused
		// in reviewPR.
		{Method: "PUT", Path: pr + "/merge", Op: "pr.merge", Check: broker.Never(neverMerge)},
		{Method: "POST", Path: repo + "/merges", Op: "pr.merge", Check: broker.Never(neverMerge)},

		// REST: reads. Logs are fetched by the broker (github.go, logs).
		{Method: "GET", Path: repo + "/actions/runs/{run}/logs", Op: "api.read", Check: broker.Read, To: logs{g}},
		{Method: "GET", Path: repo + "/actions/runs/{run}/attempts/{attempt}/logs", Op: "api.read", Check: broker.Read, To: logs{g}},
		{Method: "GET", Path: repo + "/actions/jobs/{job}/logs", Op: "api.read", Check: broker.Read, To: logs{g}},
		{Method: "GET HEAD", Path: "/api/v3/{path...}", Op: "api.read", Check: broker.Read, To: g.api},

		// REST: pull requests from agent branches.
		{Method: "POST", Path: repo + "/pulls", Op: "pr.create", Check: g.createPR, To: g.api},
		{Method: "PATCH", Path: pr, Op: "pr.update", Check: g.updatePR, To: g.api},
		{Method: "POST DELETE", Path: pr + "/requested_reviewers", Op: "pr.update", Check: g.existingPR, To: g.api},
		{Method: "POST", Path: pr + "/reviews", Op: "pr.review", Check: g.reviewPR, To: g.api},
		{Method: "POST", Path: pr + "/reviews/{review}/events", Op: "pr.review", Check: g.reviewPR, To: g.api},
		{Method: "POST", Path: pr + "/comments", Op: "pr.note", Check: g.existingPR, To: g.api},
		{Method: "POST", Path: pr + "/comments/{comment}/replies", Op: "pr.note", Check: g.existingPR, To: g.api},
		// A pull request's conversation comments are issue comments; the
		// lookup finds only pull requests, so comments on issues are denied.
		{Method: "POST", Path: repo + "/issues/{number}/comments", Op: "pr.note", Check: g.existingPR, To: g.api},
		{Method: "DELETE", Path: repo + "/git/refs/heads/{branch...}", Op: "branch.delete", Check: g.deleteBranch, To: g.api},

		// GraphQL: queries, and the mutations in graphql.go.
		{Method: "POST", Path: "/api/graphql", Op: "graphql", Check: g.graphQL, To: g.graphql},

		// git, at <owner>/<repo>(.git)/….
		{Method: "GET", Path: "/{owner}/{repo}/info/refs", Op: "git.refs", Check: g.gitRefs, To: g.git, Rewrite: gitproto.StripPushCaps},
		{Method: "POST", Path: "/{owner}/{repo}/git-upload-pack", Op: "git.fetch", Check: g.gitRepo, To: g.git},
		{Method: "POST", Path: "/{owner}/{repo}/git-receive-pack", Op: "git.push", Check: g.push, To: g.git},
	}
}

const (
	neverMerge   = "merging is never allowed through the broker; a human merges"
	neverApprove = "approving is never allowed through the broker"
)

// --- REST checks ---

// body reads a REST write's JSON object (empty is allowed).
func body(c *broker.Call) (map[string]any, broker.Decision) {
	b, err := c.ReadBody()
	if err != nil {
		return nil, broker.Deny("%v", err)
	}
	var m map[string]any
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, broker.Deny("an API write's body must be a JSON object")
		}
	}
	return m, broker.Allow()
}

func str(m map[string]any, k string) *string {
	if s, ok := m[k].(string); ok {
		return &s
	}
	return nil
}

func (g *github) note(c *broker.Call) { c.Note("repo", c.Var("owner")+"/"+c.Var("repo")) }

// createPR: from an agent branch of the same repository, with a tagged
// title.
func (g *github) createPR(c *broker.Call) broker.Decision {
	g.note(c)
	p, d := body(c)
	if !d.Allow {
		return d
	}
	if p["head_repo"] != nil || p["issue"] != nil {
		return broker.Deny("pull requests from another repository or from an issue aren't allowed")
	}
	head := ""
	if h := str(p, "head"); h != nil {
		head = *h
		if owner, branch, cross := strings.Cut(head, ":"); cross {
			if !strings.EqualFold(owner, c.Var("owner")) {
				return broker.Deny("pull requests from another repository aren't allowed")
			}
			head = branch
		}
	}
	return broker.All(broker.AgentBranch(g.User, head), broker.TaggedTitle(g.User, str(p, "title"), true))
}

// updatePR: only a pull request from an agent branch; a new title must be
// tagged.
func (g *github) updatePR(c *broker.Call) broker.Decision {
	g.note(c)
	p, d := body(c)
	if !d.Allow {
		return d
	}
	return broker.All(broker.ExistingFromAgentBranch(g.User, g.prHead(c)), broker.TaggedTitle(g.User, str(p, "title"), false))
}

// existingPR: reviewers and comments, only on a pull request from an
// agent branch.
func (g *github) existingPR(c *broker.Call) broker.Decision {
	g.note(c)
	if _, d := body(c); !d.Allow {
		return d
	}
	return broker.ExistingFromAgentBranch(g.User, g.prHead(c))
}

// reviewPR: a review comment, never an approval.
func (g *github) reviewPR(c *broker.Call) broker.Decision {
	g.note(c)
	p, d := body(c)
	if !d.Allow {
		return d
	}
	if ev := str(p, "event"); ev != nil && strings.EqualFold(*ev, "APPROVE") {
		return broker.Deny(neverApprove)
	}
	return broker.ExistingFromAgentBranch(g.User, g.prHead(c))
}

func (g *github) deleteBranch(c *broker.Call) broker.Decision {
	g.note(c)
	return broker.AgentBranch(g.User, c.Var("branch"))
}

// --- git ---

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// gitRepo checks the repository a git path names, and rewrites the path
// to its .git form, so upstream sees one spelling.
func (g *github) gitRepo(c *broker.Call) broker.Decision {
	owner, repo := c.Var("owner"), strings.TrimSuffix(c.Var("repo"), ".git")
	if c.R.URL.RawPath != "" || owner == "api" || !nameRE.MatchString(owner) || !nameRE.MatchString(repo) ||
		strings.Contains(owner+"/"+repo, "..") {
		return broker.Deny("not a repository path")
	}
	c.Note("repo", owner+"/"+repo)
	rest := strings.TrimPrefix(c.R.URL.Path, "/"+owner+"/"+c.Var("repo")) // "/info/refs", "/git-upload-pack", …
	c.R.URL.Path = "/" + owner + "/" + repo + ".git" + rest
	return broker.Allow()
}

// gitRefs is the ref advertisement for a fetch or a push (the push itself
// is checked; push options are stripped from the advertisement).
func (g *github) gitRefs(c *broker.Call) broker.Decision {
	if svc := c.R.URL.Query().Get("service"); svc != "git-upload-pack" && svc != "git-receive-pack" {
		return broker.Deny("info/refs service %q", svc)
	}
	return g.gitRepo(c)
}

func (g *github) push(c *broker.Call) broker.Decision {
	if d := g.gitRepo(c); !d.Allow {
		return d
	}
	return gitproto.Push(g.User)(c)
}
