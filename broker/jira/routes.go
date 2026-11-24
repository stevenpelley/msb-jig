// Package jira is the broker's Jira provider: Jira Cloud's REST API,
// read-only, with the user's scoped API token. Scoped tokens work only
// through Atlassian's API gateway
// (https://api.atlassian.com/ex/jira/<cloudId>), with HTTP Basic
// email:token; the sandbox calls the provider as if it were the site
// (https://<site>/rest/api/3/…) and the broker sends each call to the
// gateway. The token's scopes are the first limit (read scopes only);
// these routes are the second: reads, and the few POSTs that only read.
package jira

import (
	"github.com/stevenpelley/msb-jig/broker"
)

// Config is one Jira Cloud site.
type Config struct {
	// URL is the site's API: https://api.atlassian.com/ex/jira/<cloudId>.
	URL   string
	Email string       // the token's Atlassian account
	Token broker.Token // the user's scoped API token
}

// readPOSTs are the REST API's POSTs that only read, under /rest/api/2/
// and /rest/api/3/: searches, bulk fetches, and JQL and expression
// checks.
var readPOSTs = []string{
	"search/jql", "search/approximate-count",
	"issue/bulkfetch", "comment/list", "changelog/bulkfetch", "issue/{key}/changelog/list",
	"jql/parse", "jql/match", "jql/autocompletedata", "jql/autocompletedata/suggestions",
	"worklog/list", "issue/watching", "permissions/project", "permissions/check",
	"expression/evaluate", "expression/eval", "expression/analyse",
}

const readOnly = "Jira is read-only through the broker: no creates, edits, transitions, comments or deletes"

// Routes is every request the provider serves: GET and HEAD under the
// REST APIs (/rest/api/2/, /rest/api/3/, Jira Software's
// /rest/agile/1.0/), the read POSTs above, and nothing else.
func Routes(up broker.Upstreams, c Config) []broker.Route {
	api := up.To(c.URL, broker.Basic(c.Email, c.Token))
	var routes []broker.Route
	for _, v := range []string{"2", "3"} {
		for _, p := range readPOSTs {
			routes = append(routes, broker.Route{Method: "POST", Path: "/rest/api/" + v + "/" + p, Op: "jira.search", Check: broker.Any, To: api})
		}
	}
	return append(routes,
		broker.Route{Method: "GET HEAD", Path: "/rest/api/{v}/{path...}", Op: "jira.read", Check: readAPI, To: api},
		broker.Route{Method: "GET HEAD", Path: "/rest/agile/1.0/{path...}", Op: "jira.read", Check: read, To: api},
		broker.Route{Method: "*", Path: "/rest/{path...}", Op: "jira.write", Check: broker.Never(readOnly)},
	)
}

// readAPI is read, for versions 2 and 3 only.
func readAPI(c *broker.Call) broker.Decision {
	if v := c.Var("v"); v != "2" && v != "3" {
		return broker.Deny("only the REST API's versions 2 and 3 are served")
	}
	return read(c)
}

// read passes a GET, without updateHistory: the one read parameter that
// writes (it adds the issue to the user's recently viewed list).
func read(c *broker.Call) broker.Decision {
	q := c.R.URL.Query()
	if q.Has("updateHistory") {
		q.Del("updateHistory")
		c.R.URL.RawQuery = q.Encode()
	}
	return broker.Allow()
}
