package github

import (
	"mono-jig/broker"
	"mono-jig/broker/gitproto"
)

// Config is what the provider needs from mono-jig login.
type Config struct {
	User  string       // the GitHub user the agent acts for
	Token broker.Token // the user's token (gh's login, or the Keychain)
	// Tests: the upstreams, if not https://github.com and https://api.github.com.
	GitURL, APIURL string
	// Anonymous (proxy mode, where every github.com request reaches the
	// broker): reads that match no route go to github.com without the
	// credential, for public archives, release downloads and go get.
	Anonymous bool
}

// Routes is every request the GitHub provider serves: its upstreams, each
// with its credential, then its routes. The first route that matches
// serves a request. Anything else is denied, except in proxy mode
// (Config.Anonymous), where reads (GET, HEAD) that match no route go to
// github.com without the credential: the last route. The checks are in
// checks.go and graphql.go.
//
// In proxy mode the sandbox calls github.com and api.github.com by their
// own names; providers.Build maps api.github.com's paths to /api/v3/...
// and /api/graphql (broker.Alias), so these routes serve both modes.
func Routes(up broker.Upstreams, c Config) []broker.Route {
	git := up.To(c.git(), broker.Basic(c.User, c.Token)) // git: HTTP Basic, as git sends it
	api := up.To(c.api(), broker.Bearer(c.Token))        // REST: /api/v3/<path> is api.github.com/<path>
	api.Strip = "/api/v3"
	graphql := up.To(c.api(), broker.Bearer(c.Token)) // GraphQL: /api/graphql is api.github.com/graphql
	graphql.Path = "/graphql"
	logs := Logs{API: api} // follows a log's redirect itself, so the sandbox needs no route to blob storage
	gh := &Checks{User: c.User, API: api}

	const repo = "/api/v3/repos/{owner}/{repo}"
	const pr = repo + "/pulls/{number}"
	routes := []broker.Route{
		// Never merge. Listed so the denial says why; approving is refused
		// in ReviewPR.
		{Method: "PUT", Path: pr + "/merge", Op: "pr.merge", Check: broker.Never(broker.NeverMerge)},
		{Method: "POST", Path: repo + "/merges", Op: "pr.merge", Check: broker.Never(broker.NeverMerge)},

		// REST: reads.
		{Method: "GET", Path: repo + "/actions/runs/{run}/logs", Op: "api.read", Check: broker.Read, To: logs},
		{Method: "GET", Path: repo + "/actions/runs/{run}/attempts/{attempt}/logs", Op: "api.read", Check: broker.Read, To: logs},
		{Method: "GET", Path: repo + "/actions/jobs/{job}/logs", Op: "api.read", Check: broker.Read, To: logs},
		{Method: "GET HEAD", Path: "/api/v3/{path...}", Op: "api.read", Check: broker.Read, To: api},

		// REST: pull requests from agent branches.
		{Method: "POST", Path: repo + "/pulls", Op: "pr.create", Check: gh.CreatePR, To: api},
		{Method: "PATCH", Path: pr, Op: "pr.update", Check: gh.UpdatePR, To: api},
		{Method: "POST DELETE", Path: pr + "/requested_reviewers", Op: "pr.update", Check: gh.ExistingPR, To: api},
		{Method: "POST", Path: pr + "/reviews", Op: "pr.review", Check: gh.ReviewPR, To: api},
		{Method: "POST", Path: pr + "/reviews/{review}/events", Op: "pr.review", Check: gh.ReviewPR, To: api},
		{Method: "POST", Path: pr + "/comments", Op: "pr.note", Check: gh.ExistingPR, To: api},
		{Method: "POST", Path: pr + "/comments/{comment}/replies", Op: "pr.note", Check: gh.ExistingPR, To: api},
		// A pull request's conversation comments are issue comments; the
		// lookup finds only pull requests, so comments on issues are denied.
		{Method: "POST", Path: repo + "/issues/{number}/comments", Op: "pr.note", Check: gh.ExistingPR, To: api},
		{Method: "DELETE", Path: repo + "/git/refs/heads/{branch...}", Op: "branch.delete", Check: gh.DeleteBranch, To: api},

		// GraphQL: queries, and the mutations listed in mutations
		// (graphql.go); merges refused.
		{Method: "POST", Path: "/api/graphql", Op: "graphql", Check: gh.GraphQL, To: graphql},

		// git, at <owner>/<repo>(.git)/…. No LFS.
		{Method: "GET", Path: "/{owner}/{repo}/info/refs", Op: "git.refs", Check: gh.GitRefs, To: git, Rewrite: gitproto.StripPushCaps},
		{Method: "POST", Path: "/{owner}/{repo}/git-upload-pack", Op: "git.fetch", Check: gh.GitRepo, To: git},
		{Method: "POST", Path: "/{owner}/{repo}/git-receive-pack", Op: "git.push", Check: gh.Push, To: git},
	}
	if c.Anonymous {
		routes = append(routes, broker.Route{Method: "GET HEAD", Path: "/{path...}", Op: "anonymous.read", Check: broker.Read, To: up.To(c.git(), broker.NoAuth)})
	}
	return routes
}

func (c Config) git() string { return or(c.GitURL, "https://github.com") }
func (c Config) api() string { return or(c.APIURL, "https://api.github.com") }

func or(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
