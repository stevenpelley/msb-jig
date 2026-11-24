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

	"github.com/stevenpelley/msb-jig/broker"
	"github.com/stevenpelley/msb-jig/broker/gitproto"
)

// The checks on the GitLab routes (routes.go).

// --- REST checks ---

// MRCreate: from an agent branch, into the same project, with a tagged
// title, without arranging a merge or a quick action.
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
		noQuickActions(ps),
		broker.AgentBranch(g.User, source),
	)
}

// MRUpdate: only a merge request from an agent branch; no merge
// arranged, no quick action.
func (g *Checks) MRUpdate(c *broker.Call) broker.Decision {
	ps, err := params(c)
	if err != nil {
		return broker.Deny("%v", err)
	}
	if d := broker.All(noAutoMerge(ps), noQuickActions(ps)); !d.Allow {
		return d
	}
	return g.existing(c)
}

// MRNote: notes, discussions and resolving them, only on a merge request
// from an agent branch, and without a quick action.
func (g *Checks) MRNote(c *broker.Call) broker.Decision {
	ps, err := params(c)
	if err != nil {
		return broker.Deny("%v", err)
	}
	if d := noQuickActions(ps); !d.Allow {
		return d
	}
	return g.existing(c)
}

func (g *Checks) existing(c *broker.Call) broker.Decision {
	pid := c.Var("project")
	c.Note("repo", pid)
	return broker.ExistingFromAgentBranch(g.User, g.mrSource(c, pid, c.Var("iid")))
}

// noAutoMerge refuses a request that arranges a merge. GitLab reads
// booleans loosely ("1", "yes", "t", "on"…), so anything but a false
// value counts as asking.
func noAutoMerge(ps map[string]string) broker.Decision {
	for _, k := range []string{"merge_when_pipeline_succeeds", "auto_merge"} {
		if v, ok := ps[k]; ok && !falseValue(v) {
			return broker.Deny("%s (%s)", broker.NeverMerge, k)
		}
	}
	return broker.Allow()
}

func falseValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "f", "false", "n", "no", "off", "null":
		return true
	}
	return false
}

// quickActionRE is a line GitLab would run as a quick action: "/merge",
// "/approve", "/title x" (a slash and a word at the start of a line).
// A path such as "/usr/bin/env" isn't one.
var quickActionRE = regexp.MustCompile(`(?m)^[ \t]*/[A-Za-z_]+([ \t].*)?\r?$`)

// noQuickActions refuses text GitLab would run as quick actions. GitLab
// runs them in notes and in merge request descriptions, with the user's
// rights: "/merge" or "/approve" in a comment would do what the routes
// refuse. Every value is checked, including the strings inside JSON
// values, since which fields GitLab scans isn't ours to track.
func noQuickActions(ps map[string]string) broker.Decision {
	for k, v := range ps {
		if a := quickAction(v); a != "" {
			return broker.Deny("%s holds the quick action %q: GitLab would run it (quick actions aren't allowed through the broker)", k, a)
		}
	}
	return broker.Allow()
}

// quickAction returns the first quick action in s, or in the strings of
// s parsed as JSON.
func quickAction(s string) string {
	if m := quickActionRE.FindString(s); m != "" {
		return strings.TrimSpace(m)
	}
	var v any
	if strings.ContainsAny(s, "[{") && json.Unmarshal([]byte(s), &v) == nil {
		return quickActionIn(v)
	}
	return ""
}

func quickActionIn(v any) string {
	switch x := v.(type) {
	case string:
		return quickAction(x)
	case map[string]any:
		for _, e := range x {
			if a := quickActionIn(e); a != "" {
				return a
			}
		}
	case []any:
		for _, e := range x {
			if a := quickActionIn(e); a != "" {
				return a
			}
		}
	}
	return ""
}

func (g *Checks) BranchDelete(c *broker.Call) broker.Decision {
	c.Note("repo", c.Var("project"))
	return broker.AgentBranch(g.User, c.Var("branch"))
}

// --- GraphQL ---

// GraphQLQueriesOnly parses the documents rather than trusting operation
// names; only documents whose every operation is a query pass. No
// mutation is on the list yet (glab uses REST for writes).
//
// GitLab reads the query from its merged parameters, so the broker reads
// it the way GitLab would: on GET, each parameter at most once (Rails
// takes the last, url.Values.Get the first); on POST, nothing in the URL
// (a body Rails doesn't parse, say text/plain, would leave the URL's
// query to run) and no key twice (broker.ReadJSONBody).
func GraphQLQueriesOnly(c *broker.Call) broker.Decision {
	var docs []string
	switch c.R.Method {
	case http.MethodGet:
		q := c.R.URL.Query()
		for k, v := range q {
			if len(v) > 1 {
				return broker.Deny("the GraphQL parameter %s appears more than once", k)
			}
		}
		docs = []string{q.Get("query")}
	default:
		body, err := c.ReadJSONBody()
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
