package github

import (
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

func TestOwnersREST(t *testing.T) {
	o := owners{"example-org", "Example-Two"}
	cases := []struct {
		path, q string
		want    bool
	}{
		{"user", "", true},
		{"user/repos", "", false},
		{"user/orgs", "", false},
		{"repos/example-org/app", "", true},
		{"repos/EXAMPLE-ORG/app/pulls", "", true},
		{"repos/example-two/app", "", true},
		{"repos/other/app", "", false},
		{"orgs/example-org/repos", "", true},
		{"orgs/other/repos", "", false},
		{"users/example-org/repos", "", true},
		{"repositories/123", "", false},
		{"notifications", "", false},
		{"rate_limit", "", false},
		{"search/code", "foo org:example-org", true},
		{"search/issues", "is:pr repo:example-org/app is:open", true},
		{"search/issues", "is:pr repo:other/app", false},
		{"search/issues", "is:pr org:example-org org:other", false},
		{"search/issues", "is:pr", false},
		{"search/issues", "foo -org:example-org", false},
		{"search/issues", "org:example-org OR is:public", false},
		{"search/issues", "(org:example-org) label:bug", false},
		{"search/issues", `"org:other" org:example-org`, true}, // a quoted phrase, not a qualifier
		{"search/issues", `org:"example-org"`, true},
		{"search/repositories", "user:example-two", true},
	}
	for _, c := range cases {
		if got := o.restAbout(c.path, c.q); got != c.want {
			t.Errorf("restAbout(%q, %q) = %v, want %v", c.path, c.q, got, c.want)
		}
	}
	if !(owners{}).restAbout("repos/anyone/x", "") {
		t.Error("no owners: everything is about them")
	}
}

func TestOwnersGraphQL(t *testing.T) {
	o := owners{"example-org"}
	cases := []struct {
		q    string
		vars map[string]any
		want bool
	}{
		{`query { viewer { login } }`, nil, true},
		{`query { viewer { login repositories(first: 5) { nodes { name } } } }`, nil, false},
		{`query { viewer { ... on User { login } } }`, nil, false},
		{`query { repository(owner: "example-org", name: "app") { pullRequests(first: 5) { nodes { title } } } }`, nil, true},
		{`query($o: String!, $n: String!) { repository(owner: $o, name: $n) { id } }`, map[string]any{"o": "Example-Org", "n": "app"}, true},
		{`query($o: String!) { repository(owner: $o, name: "app") { id } }`, map[string]any{"o": "other"}, false},
		{`query { a: repository(owner: "example-org", name: "x") { id } b: repository(owner: "other", name: "y") { id } }`, nil, false},
		{`query { organization(login: "example-org") { repositories(first: 5) { nodes { name } } } }`, nil, true},
		{`query { organization(login: "other") { id } }`, nil, false},
		{`query { repositoryOwner(login: "example-org") { id } }`, nil, true},
		{`query { user(login: "someone") { id } }`, nil, false},
		{`query { search(query: "org:example-org is:pr", type: ISSUE, first: 5) { issueCount } }`, nil, true},
		{`query { search(query: "is:pr", type: ISSUE, first: 5) { issueCount } }`, nil, false},
		{`query { node(id: "R_x") { id } }`, nil, false},
		{`query { nodes(ids: ["R_x"]) { id } }`, nil, false},
		{`query { rateLimit { remaining } __typename }`, nil, true},
		{`query { ...F } fragment F on Query { viewer { login } }`, nil, false},
		{`query { ... on Query { viewer { login } } }`, nil, false},
	}
	for _, c := range cases {
		doc, err := parser.ParseQuery(&ast.Source{Input: c.q})
		if err != nil {
			t.Fatalf("%s: %v", c.q, err)
		}
		if got := o.queryAbout(doc.Operations[0], c.vars).Allow; got != c.want {
			t.Errorf("queryAbout(%s) = %v, want %v", c.q, got, c.want)
		}
	}
}
