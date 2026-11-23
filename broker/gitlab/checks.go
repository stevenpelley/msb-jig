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

// The checks on the GitLab routes (routes.go).

// --- REST checks ---

// MRCreate: from an agent branch, into the same project, with a tagged
// title, without arranging a merge.
func (g *Checks) MRCreate(c *broker.Call) broker.Decision {
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

// MRUpdate: only a merge request from an agent branch; a new title must
// be tagged; no merge arranged.
func (g *Checks) MRUpdate(c *broker.Call) broker.Decision {
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

// MRNote: notes, discussions and resolving them, only on a merge request
// from an agent branch.
func (g *Checks) MRNote(c *broker.Call) broker.Decision {
	if _, err := params(c); err != nil { // reads the body, so it's forwarded whole
		return broker.Deny("%v", err)
	}
	return g.existing(c)
}

func (g *Checks) existing(c *broker.Call) broker.Decision {
	pid := c.Var("project")
	c.Note("repo", pid)
	return broker.ExistingFromAgentBranch(g.User, g.mrSource(c, pid, c.Var("iid")))
}

func noAutoMerge(ps map[string]string) broker.Decision {
	if ps["merge_when_pipeline_succeeds"] == "true" || ps["auto_merge"] == "true" {
		return broker.Deny("%s (auto-merge)", broker.NeverMerge)
	}
	return broker.Allow()
}

func (g *Checks) BranchDelete(c *broker.Call) broker.Decision {
	c.Note("repo", c.Var("project"))
	return broker.AgentBranch(g.User, c.Var("branch"))
}

// --- GraphQL ---

// GraphQLQueriesOnly parses the documents rather than trusting operation
// names; only documents whose every operation is a query pass. No
// mutation is on the list yet (glab uses REST for writes).
func GraphQLQueriesOnly(c *broker.Call) broker.Decision {
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
						return broker.Deny("%s (GraphQL %s)", broker.NeverMerge, f.Name)
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

// GitRepo checks the repository a git or LFS path names, and rewrites the
// path to its .git form, so upstream sees one spelling.
func (g *Checks) GitRepo(c *broker.Call) broker.Decision {
	raw := c.Var("repo")
	repo := strings.TrimSuffix(raw, ".git")
	if c.R.URL.RawPath != "" || !repoPathRE.MatchString(repo) || strings.Contains(repo, "..") || strings.HasPrefix(repo, "api/") {
		return broker.Deny("not a repository path: %q", raw)
	}
	c.Note("repo", repo)
	c.R.URL.Path = "/" + repo + ".git" + strings.TrimPrefix(c.R.URL.Path, "/"+raw)
	return broker.Allow()
}

// GitRefs is the ref advertisement for a fetch or a push (the push itself
// is checked; push options are stripped from the advertisement).
func (g *Checks) GitRefs(c *broker.Call) broker.Decision {
	if svc := c.R.URL.Query().Get("service"); svc != "git-upload-pack" && svc != "git-receive-pack" {
		return broker.Deny("info/refs service %q", svc)
	}
	return g.GitRepo(c)
}

func (g *Checks) Push(c *broker.Call) broker.Decision {
	if d := g.GitRepo(c); !d.Allow {
		return d
	}
	return gitproto.Push(g.User)(c)
}
