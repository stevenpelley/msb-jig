package github

import (
	"encoding/json"
	"regexp"
	"strings"

	"mono-jig/broker"
	"mono-jig/broker/gitproto"
)

// The checks on the GitHub routes (routes.go).

// --- REST checks ---

// body reads a REST write's JSON object (empty is allowed), with no
// parameters in the URL and no key twice (broker.ReadJSONBody).
func body(c *broker.Call) (map[string]any, broker.Decision) {
	b, err := c.ReadJSONBody()
	if err != nil {
		return nil, broker.Deny("%v", err)
	}
	var m map[string]any
	if b != nil {
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

func (g *Checks) note(c *broker.Call) { c.Note("repo", c.Var("owner")+"/"+c.Var("repo")) }

// CreatePR: from an agent branch of the same repository, with a tagged
// title.
func (g *Checks) CreatePR(c *broker.Call) broker.Decision {
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

// UpdatePR: only a pull request from an agent branch; a new title must be
// tagged.
func (g *Checks) UpdatePR(c *broker.Call) broker.Decision {
	g.note(c)
	p, d := body(c)
	if !d.Allow {
		return d
	}
	return broker.All(broker.ExistingFromAgentBranch(g.User, g.prHead(c)), broker.TaggedTitle(g.User, str(p, "title"), false))
}

// ExistingPR: reviewers and comments, only on a pull request from an
// agent branch.
func (g *Checks) ExistingPR(c *broker.Call) broker.Decision {
	g.note(c)
	if _, d := body(c); !d.Allow {
		return d
	}
	return broker.ExistingFromAgentBranch(g.User, g.prHead(c))
}

// ReviewPR: a review comment, never an approval.
func (g *Checks) ReviewPR(c *broker.Call) broker.Decision {
	g.note(c)
	p, d := body(c)
	if !d.Allow {
		return d
	}
	if ev := str(p, "event"); ev != nil && strings.EqualFold(*ev, "APPROVE") {
		return broker.Deny(broker.NeverApprove)
	}
	return broker.ExistingFromAgentBranch(g.User, g.prHead(c))
}

func (g *Checks) DeleteBranch(c *broker.Call) broker.Decision {
	g.note(c)
	return broker.AgentBranch(g.User, c.Var("branch"))
}

// --- git ---

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// GitRepo checks the repository a git path names, and rewrites the path
// to its .git form, so upstream sees one spelling.
func (g *Checks) GitRepo(c *broker.Call) broker.Decision {
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
