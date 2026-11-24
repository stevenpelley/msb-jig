package github

import (
	"encoding/json"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"

	"msb-jig/broker"
)

// mutationCheck decides one mutation from its input argument.
type mutationCheck func(g *Checks, c *broker.Call, in map[string]any) broker.Decision

// mutations is every GraphQL mutation the broker passes, with its check.
// A request may hold queries and at most one mutation; any mutation not
// here is denied.
var mutations = map[string]mutationCheck{
	"createPullRequest":               (*Checks).createPRMutation,
	"updatePullRequest":               (*Checks).updatePRMutation,
	"closePullRequest":                onPR("pullRequestId"),
	"reopenPullRequest":               onPR("pullRequestId"),
	"markPullRequestReadyForReview":   onPR("pullRequestId"),
	"convertPullRequestToDraft":       onPR("pullRequestId"),
	"requestReviews":                  onPR("pullRequestId"),
	"addComment":                      onPR("subjectId"), // only on a pull request: an issue's lookup fails
	"addPullRequestReview":            (*Checks).reviewMutation,
	"submitPullRequestReview":         (*Checks).reviewMutation,
	"addPullRequestReviewThread":      onPR("pullRequestId"),
	"addPullRequestReviewThreadReply": onPR("pullRequestReviewThreadId"),
	"resolveReviewThread":             onPR("threadId"),
	"unresolveReviewThread":           onPR("threadId"),
}

// merges are the mutations that merge or arrange a merge, named so their
// denial says why.
var merges = map[string]bool{"mergePullRequest": true, "enablePullRequestAutoMerge": true,
	"disablePullRequestAutoMerge": true, "enqueuePullRequest": true, "dequeuePullRequest": true, "mergeBranch": true}

// onPR allows a mutation on a pull request from an agent branch: the node
// its input names (field) is the pull request, or one of its review
// threads or reviews, looked up from GitHub.
func onPR(field string) mutationCheck {
	return func(g *Checks, c *broker.Call, in map[string]any) broker.Decision {
		return broker.ExistingFromAgentBranch(g.User, g.nodePRHead(c, val(str(in, field))))
	}
}

// createPRMutation: from an agent branch of the same repository.
func (g *Checks) createPRMutation(_ *broker.Call, in map[string]any) broker.Decision {
	head := val(str(in, "headRefName"))
	if strings.Contains(head, ":") || (in["headRepositoryId"] != nil && in["headRepositoryId"] != in["repositoryId"]) {
		return broker.Deny("pull requests from another repository aren't allowed")
	}
	return broker.AgentBranch(g.User, head)
}

func (g *Checks) updatePRMutation(c *broker.Call, in map[string]any) broker.Decision {
	return onPR("pullRequestId")(g, c, in)
}

// reviewMutation: a review comment, never an approval.
func (g *Checks) reviewMutation(c *broker.Call, in map[string]any) broker.Decision {
	if ev := str(in, "event"); ev != nil && strings.EqualFold(*ev, "APPROVE") {
		return broker.Deny(broker.NeverApprove)
	}
	if str(in, "pullRequestId") != nil {
		return onPR("pullRequestId")(g, c, in)
	}
	return onPR("pullRequestReviewId")(g, c, in)
}

func val(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// GraphQL parses the document rather than trusting operation names.
// Queries pass; a mutation is decided by its entry in mutations. The
// body is read with broker.ReadJSONBody: no "QUERY" beside "query", no
// query in the URL.
func (g *Checks) GraphQL(c *broker.Call) broker.Decision {
	b, err := c.ReadJSONBody()
	if err != nil {
		return broker.Deny("%v", err)
	}
	var gq struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(b, &gq); err != nil {
		return broker.Deny("bad GraphQL request (GitHub takes no batches)")
	}
	q, err := parser.ParseQuery(&ast.Source{Input: gq.Query})
	if err != nil || len(q.Operations) == 0 {
		return broker.Deny("unparseable GraphQL")
	}
	var muts []*ast.Field
	for _, op := range q.Operations {
		switch op.Operation {
		case ast.Query:
			continue
		case ast.Mutation:
		default:
			return broker.Deny("GraphQL %s isn't on the broker's list", op.Operation)
		}
		for _, s := range op.SelectionSet {
			f, ok := s.(*ast.Field)
			if !ok {
				return broker.Deny("GraphQL mutations with fragments aren't on the broker's list")
			}
			muts = append(muts, f)
		}
	}
	if len(muts) == 0 {
		return broker.Allow()
	}
	for _, f := range muts {
		if merges[f.Name] {
			return broker.Deny("%s (GraphQL %s)", broker.NeverMerge, f.Name)
		}
	}
	if len(muts) > 1 {
		return broker.Deny("only one GraphQL mutation per request")
	}
	f := muts[0]
	c.Note("mutation", f.Name)
	check := mutations[f.Name]
	if check == nil {
		return broker.Deny("the GraphQL mutation %s isn't on the broker's list", f.Name)
	}
	var in map[string]any
	if a := f.Arguments.ForName("input"); a != nil {
		v, err := a.Value.Value(gq.Variables)
		if err != nil {
			return broker.Deny("GraphQL mutation %s: bad input", f.Name)
		}
		in, _ = v.(map[string]any)
	}
	if in == nil {
		return broker.Deny("GraphQL mutation %s without input", f.Name)
	}
	return check(g, c, in)
}
