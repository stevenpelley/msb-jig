package jig

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/stevenpelley/msb-jig/broker/providers"
)

// Jira's command-line side (ProviderCLI): one Jira Cloud site, read-only,
// with a scoped API token the user makes at id.atlassian.com (no admin
// needed) and pastes into the Keychain. The site, its cloud ID and the
// token's account email go in settings.json.

const keychainJira = "mono-jig-jira"

type jiraSettings struct {
	Site    string `json:"site"`    // example.atlassian.net
	CloudID string `json:"cloudId"` // from https://<site>/_edge/tenant_info
	Email   string `json:"email"`
}

var Jira = ProviderCLI{
	Name:     "jira",
	Usage:    "login jira [<site>]     store a read-only scoped Jira API token for a site (example.atlassian.net)\n",
	Keychain: keychainJira, Login: loginJira,
	Status: func(s Settings) string {
		j := ProviderSettings[jiraSettings](s, "jira")
		if j == nil {
			return ""
		}
		st := j.Site + " as " + j.Email + ", read-only through the broker"
		if JiraTokenCommand() == "" {
			st += "; no token in the Keychain (" + keychainJira + "): run login jira again"
		}
		return st
	},
	Forgotten: func(Settings) string {
		return "token removed from the Keychain; revoke it at id.atlassian.com, Security, API tokens, if you're done with it"
	},
	Broker: jiraBroker,
}

// JiraTokenCommand is the Jira token's command: the Keychain item's, or
// JIG_JIRA_TOKEN_COMMAND (empty: off); "" if there's none.
func JiraTokenCommand() string {
	cmd, set := os.LookupEnv("JIG_JIRA_TOKEN_COMMAND")
	if !set && KeychainHas(keychainJira) {
		cmd = KeychainCommand(keychainJira)
	}
	return cmd
}

// jiraBroker: on with the settings and a token.
func jiraBroker(s Settings) providers.Provider {
	j := ProviderSettings[jiraSettings](s, "jira")
	cmd := JiraTokenCommand()
	if j == nil || cmd == "" {
		return nil
	}
	return providers.Jira{Site: j.Site, CloudID: j.CloudID, Email: j.Email, TokenCmd: cmd}
}

var (
	jiraSiteRE  = regexp.MustCompile(`^[a-z0-9-]+\.atlassian\.net$`)
	jiraEmailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	jiraCloudRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

func loginJira(args []string) {
	if len(args) > 1 {
		usage()
	}
	site := ""
	if len(args) == 1 {
		site = args[0]
	}
	if site == "" {
		site = Ask("Jira site (example.atlassian.net): ")
	}
	site = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(site, "https://"), "/"))
	if !jiraSiteRE.MatchString(site) {
		fail("the site %q: want <name>.atlassian.net", site)
	}
	cloudID, err := jiraCloudID(site)
	if err != nil {
		fail("finding %s's cloud ID: %v", site, err)
	}
	email := Ask("your Atlassian account's email: ")
	if !jiraEmailRE.MatchString(email) {
		fail("that isn't an email address")
	}
	fmt.Println("At https://id.atlassian.com/manage-profile/security/api-tokens, Create API token with scopes:")
	fmt.Println("app Jira, and read scopes only (read:jira-work, read:jira-user; for boards and sprints,")
	fmt.Println("read:board-scope:jira-software and read:sprint:jira-software), with the longest expiry allowed.")
	tok := ReadSecret("paste the token (input hidden): ")
	if err := jiraCheck(cloudID, email, tok); err != nil {
		fail("the token doesn't work: %v", err)
	}
	KeychainStore(keychainJira, "mono-jig", tok)
	s := LoadSettings()
	s.SetProvider("jira", jiraSettings{Site: site, CloudID: cloudID, Email: email})
	SaveSettings(s)
	fmt.Println("jira:", site, "as", email, "; token stored in the Keychain as", keychainJira)
}

var jiraClient = &http.Client{Timeout: 30 * time.Second}

// jiraCloudID reads the site's cloud ID from its public tenant_info.
func jiraCloudID(site string) (string, error) {
	resp, err := jiraClient.Get("https://" + site + "/_edge/tenant_info")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var t struct {
		CloudID string `json:"cloudId"`
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&t); err != nil || !jiraCloudRE.MatchString(t.CloudID) {
		return "", fmt.Errorf("no cloud ID in the answer")
	}
	return t.CloudID, nil
}

// jiraCheck reads the token's own user (GET /rest/api/3/myself) through
// Atlassian's gateway, as the broker will.
func jiraCheck(cloudID, email, tok string) error {
	req, _ := http.NewRequest("GET", providers.JiraAPI(url.PathEscape(cloudID))+"/rest/api/3/myself", nil)
	req.SetBasicAuth(email, tok)
	req.Header.Set("Accept", "application/json")
	resp, err := jiraClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /rest/api/3/myself: %s (check the email and the token's scopes)", resp.Status)
	}
	return nil
}
