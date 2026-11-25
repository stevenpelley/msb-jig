package github

import (
	"github.com/stevenpelley/msb-jig/broker"
	"github.com/stevenpelley/msb-jig/broker/gitproto"
)

// Config is what the provider needs from mono-jig login.
type Config struct {
	User  string       // the GitHub user the agent acts for
	Token broker.Token // the user's token (gh's login, or the Keychain)
	// Owners, if set, are the users and organizations whose repositories
	// get the user's credential; anyone else's are read without it and
	// never written (owners.go). Empty: every owner's.
	Owners []string
	// Tests: the upstreams, if not https://github.com and https://api.github.com.
	GitURL, APIURL string
}

// The provider's route tables, one set per broker mode. Each table is
// every request a host serves: the first route that matches serves a
// request, and anything else is denied, except where a table ends in
// anonymous reads. The checks are in checks.go and graphql.go; Owners'
// rules in owners.go.
//
//   - Server mode (ServerRoutes): one host, github.broker.internal,
//     called the way gh calls a GitHub Enterprise Server: REST under
//     /api/v3, GraphQL at /api/graphql, git at /<owner>/<repo>.
//   - Proxy mode (ProxyRoutes): the real hosts. api.github.com takes the
//     API at GitHub's own paths (/repos/..., /graphql); github.com takes
//     git, then reads that match no route, sent without the credential
//     (public archives, release downloads, go get).
//
// The rules are the same in both; only the paths and hosts differ.

// ServerRoutes is server mode's one table.
func ServerRoutes(up broker.Upstreams, c Config) []broker.Route {
	p := c.parts(up, "/api/v3")
	return append(p.apiRoutes("/api/graphql"), p.gitRoutes()...)
}

// ProxyRoutes is proxy mode's tables: github.com's (web) and
// api.github.com's (api).
func ProxyRoutes(up broker.Upstreams, c Config) (web, api []broker.Route) {
	p := c.parts(up, "")
	web = append(p.gitRoutes(), broker.Route{Method: "GET HEAD", Path: "/{path...}", Op: "anonymous.read", Check: broker.Read, To: up.To(c.git(), broker.NoAuth)})
	return web, p.apiRoutes("/graphql")
}

// parts are the upstreams, each with its credential, and the checks the
// routes share. prefix is what the sandbox's REST paths have and
// api.github.com's don't. The anon* upstreams are the same without the
// credential, for other owners' repositories (Owners).
type parts struct {
	prefix            string
	git, api, graphql *broker.Upstream
	anonGit, anonAPI  *broker.Upstream
	logs, anonLogs    Logs
	gh                *Checks
	owners            owners
}

func (c Config) parts(up broker.Upstreams, prefix string) parts {
	api := up.To(c.api(), broker.Bearer(c.Token)) // REST: <prefix>/<path> is api.github.com/<path>
	api.Strip = prefix
	anonAPI := up.To(c.api(), broker.NoAuth)
	anonAPI.Strip = prefix
	graphql := up.To(c.api(), broker.Bearer(c.Token)) // GraphQL, at api.github.com/graphql
	graphql.Path = "/graphql"
	o := owners(c.Owners)
	return parts{
		prefix:   prefix,
		git:      up.To(c.git(), broker.Basic(c.User, c.Token)), // git: HTTP Basic, as git sends it
		api:      api,
		graphql:  graphql,
		anonGit:  up.To(c.git(), broker.NoAuth),
		anonAPI:  anonAPI,
		logs:     Logs{API: api}, // follows a log's redirect itself, so the sandbox needs no route to blob storage
		anonLogs: Logs{API: anonAPI},
		gh:       &Checks{User: c.User, API: api, Owners: o},
		owners:   o,
	}
}

// apiRoutes are REST under p.prefix and GraphQL at graphqlPath.
func (p parts) apiRoutes(graphqlPath string) []broker.Route {
	api, gh, mine := p.api, p.gh, p.owners.mine
	logs := byOwner{p.owners, p.logs, p.anonLogs}
	repo := p.prefix + "/repos/{owner}/{repo}"
	pr := repo + "/pulls/{number}"
	return []broker.Route{
		// Never merge. Listed so the denial says why; approving is refused
		// in ReviewPR.
		{Method: "PUT", Path: pr + "/merge", Op: "pr.merge", Check: broker.Never(broker.NeverMerge)},
		{Method: "POST", Path: repo + "/merges", Op: "pr.merge", Check: broker.Never(broker.NeverMerge)},

		// REST: reads; with Owners, other owners' without the credential.
		{Method: "GET", Path: repo + "/actions/runs/{run}/logs", Op: "api.read", Check: broker.Read, To: logs},
		{Method: "GET", Path: repo + "/actions/runs/{run}/attempts/{attempt}/logs", Op: "api.read", Check: broker.Read, To: logs},
		{Method: "GET", Path: repo + "/actions/jobs/{job}/logs", Op: "api.read", Check: broker.Read, To: logs},
		{Method: "GET HEAD", Path: p.prefix + "/{path...}", Op: "api.read", Check: broker.Read, To: scopedRead{p.owners, api, p.anonAPI}},

		// REST: pull requests from agent branches, on Owners' repositories.
		{Method: "POST", Path: repo + "/pulls", Op: "pr.create", Check: mine(gh.CreatePR), To: api},
		{Method: "PATCH", Path: pr, Op: "pr.update", Check: mine(gh.UpdatePR), To: api},
		{Method: "POST DELETE", Path: pr + "/requested_reviewers", Op: "pr.update", Check: mine(gh.ExistingPR), To: api},
		{Method: "POST", Path: pr + "/reviews", Op: "pr.review", Check: mine(gh.ReviewPR), To: api},
		{Method: "POST", Path: pr + "/reviews/{review}/events", Op: "pr.review", Check: mine(gh.ReviewPR), To: api},
		{Method: "POST", Path: pr + "/comments", Op: "pr.note", Check: mine(gh.ExistingPR), To: api},
		{Method: "POST", Path: pr + "/comments/{comment}/replies", Op: "pr.note", Check: mine(gh.ExistingPR), To: api},
		// A pull request's conversation comments are issue comments; the
		// lookup finds only pull requests, so comments on issues are denied.
		{Method: "POST", Path: repo + "/issues/{number}/comments", Op: "pr.note", Check: mine(gh.ExistingPR), To: api},
		{Method: "DELETE", Path: repo + "/git/refs/heads/{branch...}", Op: "branch.delete", Check: mine(gh.DeleteBranch), To: api},

		// GraphQL: queries (with Owners, about them), and the mutations
		// listed in mutations (graphql.go); merges refused.
		{Method: "POST", Path: graphqlPath, Op: "graphql", Check: gh.GraphQL, To: p.graphql},
	}
}

// gitRoutes are git, at /<owner>/<repo>(.git)/…. No LFS. With Owners,
// other owners' repositories are fetched without the credential.
func (p parts) gitRoutes() []broker.Route {
	gh, git := p.gh, byOwner{p.owners, p.git, p.anonGit}
	return []broker.Route{
		{Method: "GET", Path: "/{owner}/{repo}/info/refs", Op: "git.refs", Check: gh.GitRefs, To: git, Rewrite: gitproto.StripPushCaps},
		{Method: "POST", Path: "/{owner}/{repo}/git-upload-pack", Op: "git.fetch", Check: gh.GitRepo, To: git},
		{Method: "POST", Path: "/{owner}/{repo}/git-receive-pack", Op: "git.push", Check: p.owners.mine(gh.Push), To: git},
	}
}

func (c Config) git() string { return or(c.GitURL, "https://github.com") }
func (c Config) api() string { return or(c.APIURL, "https://api.github.com") }

func or(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
