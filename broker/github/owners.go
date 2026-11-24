package github

import (
	"net/http"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/stevenpelley/msb-jig/broker"
)

// Owners (Config.Owners): when set, the user's credential goes only with
// requests about those owners' repositories (users or organizations).
// Reads about anyone else's go upstream without it, as an anonymous
// visitor's would; writes to them are refused.
//
// What counts as "about an owner":
//
//   - REST and git: the owner in the path (/repos/{owner}/…, /orgs/{org}/…,
//     /users/{user}/…, /{owner}/{repo}.git), and /user itself.
//   - REST search (/search/…): a query whose every org:, user:, owner: and
//     repo: qualifier names an owner, with at least one, none negated,
//     and no OR, NOT or parentheses.
//   - GraphQL, which GitHub serves only with a credential: each top-level
//     field of a query must be repository(owner:), organization(login:),
//     repositoryOwner(login:) or user(login:) for an owner, search with a
//     query as above, viewer with only its scalar fields, rateLimit, or
//     introspection; anything else is refused. A mutation's target is
//     looked up and must belong to an owner.
//
// The GraphQL rule is on the top-level fields only: a query may still
// follow links from an owner's repository into others the credential can
// read (a fork's parent, a cross-reference).
type owners []string

func (o owners) on() bool { return len(o) > 0 }

// has: no owners set, or name is one (GitHub's names ignore case).
func (o owners) has(name string) bool {
	return !o.on() || (name != "" && slices.ContainsFunc(o, func(x string) bool { return strings.EqualFold(x, name) }))
}

func (o owners) list() string { return strings.Join(o, ", ") }

// byOwner serves a request about {owner}'s repository with the
// credential (mine) or without it (other).
type byOwner struct {
	o           owners
	mine, other broker.Target
}

func (b byOwner) Serve(w http.ResponseWriter, c *broker.Call) {
	if b.o.has(c.Var("owner")) {
		b.mine.Serve(w, c)
		return
	}
	b.other.Serve(w, c)
}

// mine wraps a write's check: only on an owner's repository.
func (o owners) mine(check broker.Check) broker.Check {
	if !o.on() {
		return check
	}
	return func(c *broker.Call) broker.Decision {
		if !o.has(c.Var("owner")) {
			return broker.Deny("the agent writes only to repositories of %s", o.list())
		}
		return check(c)
	}
}

// scopedRead serves the REST reads that match no more specific route
// (path is the part after the REST prefix): with the credential if
// they're about an owner, else without.
type scopedRead struct {
	o           owners
	mine, other broker.Target
}

func (s scopedRead) Serve(w http.ResponseWriter, c *broker.Call) {
	if s.o.restAbout(c.Var("path"), c.R.URL.Query().Get("q")) {
		s.mine.Serve(w, c)
		return
	}
	s.other.Serve(w, c)
}

// restAbout: a REST read of path (no leading slash) is about an owner.
func (o owners) restAbout(path, q string) bool {
	if !o.on() {
		return true
	}
	seg := strings.Split(path, "/")
	switch {
	case path == "user":
		return true
	case len(seg) >= 2 && (seg[0] == "repos" || seg[0] == "orgs" || seg[0] == "users"):
		return o.has(seg[1])
	case seg[0] == "search":
		return o.searchAbout(q)
	}
	return false
}

// searchAbout: a search query (GitHub's q) is limited to owners.
func (o owners) searchAbout(q string) bool {
	n := 0
	for _, t := range searchTerms(q) {
		if t == "OR" || t == "NOT" || strings.ContainsAny(t, "()") {
			return false
		}
		neg := strings.HasPrefix(t, "-")
		k, v, ok := strings.Cut(strings.TrimPrefix(t, "-"), ":")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		switch strings.ToLower(k) {
		case "org", "user", "owner":
		case "repo":
			v, _, _ = strings.Cut(v, "/")
		default:
			continue
		}
		if neg || !o.has(v) {
			return false
		}
		n++
	}
	return n > 0
}

// searchTerms splits a search query on spaces outside double quotes.
func searchTerms(q string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range q {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case (r == ' ' || r == '\t' || r == '\n') && !quoted:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// viewerFields are the viewer's fields a scoped query may select: the
// user's own, not their repositories or organizations.
var viewerFields = map[string]bool{"__typename": true, "login": true, "id": true, "databaseId": true,
	"name": true, "email": true, "avatarUrl": true, "url": true}

// queryAbout checks a query operation's top-level fields (see owners).
func (o owners) queryAbout(op *ast.OperationDefinition, vars map[string]any) broker.Decision {
	if !o.on() {
		return broker.Allow()
	}
	deny := func(what string) broker.Decision {
		return broker.Deny("GraphQL %s: with your credential, GraphQL reaches only repositories of %s (other owners' public repositories: REST, gh api repos/…)", what, o.list())
	}
	for _, s := range op.SelectionSet {
		f, ok := s.(*ast.Field)
		if !ok {
			return deny("fragments at the top level")
		}
		arg := func(name string) string {
			a := f.Arguments.ForName(name)
			if a == nil {
				return ""
			}
			v, err := a.Value.Value(vars)
			if err != nil {
				return ""
			}
			s, _ := v.(string)
			return s
		}
		switch f.Name {
		case "__typename", "__schema", "__type", "rateLimit":
		case "repository":
			if !o.has(arg("owner")) {
				return deny("repository " + arg("owner") + "/" + arg("name"))
			}
		case "organization", "repositoryOwner", "user":
			if !o.has(arg("login")) {
				return deny(f.Name + " " + arg("login"))
			}
		case "search":
			if !o.searchAbout(arg("query")) {
				return deny("search " + arg("query"))
			}
		case "viewer":
			for _, vs := range f.SelectionSet {
				vf, ok := vs.(*ast.Field)
				if !ok || !viewerFields[vf.Name] {
					return deny("viewer's fields beyond its own (login, id, name, …)")
				}
			}
		default:
			return deny(f.Name)
		}
	}
	return broker.Allow()
}

// mutationTarget names the input field holding the node a mutation acts
// on, whose owner is looked up.
var mutationTarget = map[string][]string{
	"createPullRequest":               {"repositoryId"},
	"addPullRequestReview":            {"pullRequestId"},
	"submitPullRequestReview":         {"pullRequestId", "pullRequestReviewId"},
	"addComment":                      {"subjectId"},
	"addPullRequestReviewThreadReply": {"pullRequestReviewThreadId"},
	"resolveReviewThread":             {"threadId"},
	"unresolveReviewThread":           {"threadId"},
}

// nodeOwnerQuery finds the owner of a repository, or of the repository a
// pull request, review or review thread is in.
const nodeOwnerQuery = `query($id: ID!) { node(id: $id) {
  ... on Repository { owner { login } }
  ... on PullRequest { repository { owner { login } } }
  ... on PullRequestReview { repository { owner { login } } }
  ... on PullRequestReviewThread { repository { owner { login } } }
} }`

// mutationAbout: a mutation's target belongs to an owner.
func (g *Checks) mutationAbout(c *broker.Call, name string, in map[string]any) broker.Decision {
	if !g.Owners.on() {
		return broker.Allow()
	}
	fields, ok := mutationTarget[name]
	if !ok {
		fields = []string{"pullRequestId"}
	}
	id := ""
	for _, f := range fields {
		if id = val(str(in, f)); id != "" {
			break
		}
	}
	type owner struct {
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	var resp struct {
		Data struct {
			Node *struct {
				owner
				Repository *owner `json:"repository"`
			} `json:"node"`
		} `json:"data"`
	}
	if id == "" || !g.graphql(c, nodeOwnerQuery, map[string]any{"id": id}, &resp) || resp.Data.Node == nil {
		return broker.Deny("GraphQL %s: couldn't look up whose repository it's on", name)
	}
	login := resp.Data.Node.Owner.Login
	if r := resp.Data.Node.Repository; r != nil {
		login = r.Owner.Login
	}
	if !g.Owners.has(login) {
		return broker.Deny("the agent writes only to repositories of %s", g.Owners.list())
	}
	return broker.Allow()
}
