package providers

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/stevenpelley/msb-jig/broker/jira"
	"github.com/stevenpelley/msb-jig/guest"
)

// JiraHost is the Jira provider's name in server mode.
const JiraHost = "jira" + InternalSuffix

// Jira: one Jira Cloud site's REST API, read-only, with the user's scoped
// API token (broker/jira). The sandbox calls it as the site: in server
// mode at JiraHost, in proxy mode at the site's own host. The broker
// sends each call to Atlassian's API gateway for the site's cloud ID.
type Jira struct {
	Site     string // example.atlassian.net
	CloudID  string // the site's cloud ID (https://<site>/_edge/tenant_info)
	Email    string // the token's Atlassian account
	TokenCmd string // prints the user's scoped API token
	APIURL   string // tests: the site's API, if not https://api.atlassian.com/ex/jira/<CloudID>
}

// JiraAPI is Atlassian's API gateway for a site's Jira, where scoped API
// tokens work.
func JiraAPI(cloudID string) string { return "https://api.atlassian.com/ex/jira/" + cloudID }

var (
	jiraSiteRE  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
	jiraCloudRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

func (Jira) Name() string { return "jira" }

func (j Jira) Hosts(proxy bool) []string {
	if proxy {
		return []string{strings.ToLower(j.Site)}
	}
	return []string{JiraHost}
}

func (j Jira) Handlers(_ context.Context, b *Builder) (map[string]http.Handler, error) {
	if !jiraSiteRE.MatchString(strings.ToLower(j.Site)) {
		return nil, fmt.Errorf("the Jira site %q: want a host name (example.atlassian.net)", j.Site)
	}
	if !jiraCloudRE.MatchString(j.CloudID) {
		return nil, fmt.Errorf("the Jira cloud ID %q: want a UUID", j.CloudID)
	}
	if j.Email == "" {
		return nil, fmt.Errorf("no Jira account email")
	}
	p := b.Provider("jira", jira.Routes(b.Upstreams(JiraHost, "jira"), jira.Config{
		URL: or(j.APIURL, JiraAPI(j.CloudID)), Email: j.Email, Token: Token(j.TokenCmd)}))
	return map[string]http.Handler{j.Hosts(b.Proxy)[0]: p}, nil
}

// Guest: the site as the sandbox reaches it in $JIRA_URL, and the
// instructions. No token or client settings: the broker adds the
// credential.
func (j Jira) Guest(proxy bool) guest.Config {
	u := "https://" + j.Hosts(proxy)[0]
	return guest.Config{
		Env:          map[string]string{"JIRA_URL": u},
		Instructions: []string{guest.Instructions("jira", map[string]string{"URL": u, "Site": j.Site, "Email": j.Email})},
	}
}
