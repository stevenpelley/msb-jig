package jig

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/stevenpelley/msb-jig/broker/providers"
)

// GitHub's command-line side (ProviderCLI).

type githubSettings struct {
	User string `json:"user"`
	// Host is the web host, default github.com. The API is at api.<host>
	// (github.com, and GitHub Enterprise Cloud with data residency,
	// <sub>.ghe.com; unverified for the latter).
	Host string `json:"host,omitempty"`
	// Token: "gh" (the user's own gh login on the Mac) or "keychain"
	// (keychainGitHub).
	Token string `json:"token"`
}

func (g githubSettings) host() string    { return or(g.Host, "github.com") }
func (g githubSettings) apiHost() string { return "api." + g.host() }

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

const keychainGitHub = "mono-jig-github"

var GitHub = ProviderCLI{
	Name:  "github",
	Usage: "login github [<user>] [--host <host>] [--keychain]\n                        the same on github.com (or <host>, with its API at\n                        api.<host>), with your gh login's token\n", Keychain: keychainGitHub, Login: loginGitHub,
	Status: func(s Settings) string {
		g := ProviderSettings[githubSettings](s, "github")
		if g == nil {
			return ""
		}
		return fmt.Sprintf("%s on %s, token from %s", g.User, g.host(), githubTokenSource(*g))
	},
	Forgotten: func(s Settings) string {
		if g := ProviderSettings[githubSettings](s, "github"); g != nil && g.Token == "gh" {
			return "forgotten; your own gh login stays (gh auth logout to end it)"
		}
		return "forgotten"
	},
	Broker: githubBroker,
}

// githubBroker: on once login recorded a user. JIG_GITHUB_USER (empty:
// off), JIG_GITHUB_HOST and JIG_GITHUB_TOKEN_COMMAND override.
func githubBroker(s Settings) providers.Provider {
	gh := githubSettings{Token: "gh"}
	if g := ProviderSettings[githubSettings](s, "github"); g != nil {
		gh = *g
	}
	gh.User = env("JIG_GITHUB_USER", gh.User)
	if gh.User == "" {
		return nil
	}
	if !githubUserRE.MatchString(gh.User) {
		fail("the GitHub user %q isn't a GitHub username", gh.User)
	}
	gh.Host = env("JIG_GITHUB_HOST", gh.Host)
	if !hostRE.MatchString(gh.host()) {
		fail("the GitHub host %q isn't a host name", gh.host())
	}
	return providers.GitHub{User: gh.User, WebHost: gh.host(), APIHost: gh.apiHost(),
		TokenCmd: env("JIG_GITHUB_TOKEN_COMMAND", githubTokenCommand(gh))}
}

// githubTokenCommand prints the GitHub token for the broker.
func githubTokenCommand(g githubSettings) string {
	if g.Token == "keychain" {
		return KeychainCommand(keychainGitHub)
	}
	return "gh auth token --hostname " + g.host()
}

func githubTokenSource(g githubSettings) string {
	if g.Token == "keychain" {
		return "the Keychain (" + keychainGitHub + ")"
	}
	return "your gh login"
}

// loginGitHub records the GitHub user the agent acts for (on github.com,
// or --host's), and where
// the broker gets their token: gh's login on the Mac (logging in if
// needed), or a token pasted into the Keychain. With no user given, the
// user is the token's; given one, it checks the token belongs to them.
func loginGitHub(args []string) {
	g := githubSettings{Token: "gh"}
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--keychain":
			g.Token = "keychain"
		case a == "--host" && i+1 < len(args):
			i++
			if g.Host = strings.ToLower(args[i]); g.Host == "github.com" {
				g.Host = ""
			}
			if !hostRE.MatchString(g.host()) {
				fail("--host: want a host name, such as github.com")
			}
		case strings.HasPrefix(a, "-"):
			usage()
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) > 1 || (len(rest) == 1 && !githubUserRE.MatchString(rest[0])) {
		usage()
	}
	if len(rest) == 1 {
		g.User = rest[0]
	}
	if g.Token == "keychain" {
		tok := ReadSecret("a GitHub token (repo scope, or a fine-grained token with contents, pull requests and actions; input hidden): ")
		KeychainStore(keychainGitHub, g.host(), tok)
	} else {
		if _, err := exec.LookPath("gh"); err != nil {
			fail("gh isn't installed: brew install gh, or use --keychain")
		}
		if exec.Command("gh", "auth", "token", "--hostname", g.host()).Run() != nil {
			fmt.Println("gh isn't logged in to", g.host()+": gh auth login")
			c := exec.Command("gh", "auth", "login", "--hostname", g.host(), "--web")
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				fail("gh auth login: %v", err)
			}
		}
	}
	out, err := exec.Command("sh", "-c", githubTokenCommand(g)).Output()
	if err != nil {
		fail("reading the token back: %v", err)
	}
	who, err := githubUser(g.apiHost(), strings.TrimSpace(string(out)))
	if err != nil {
		fail("checking the token: %v", err)
	}
	if g.User == "" {
		g.User = who
	} else if !strings.EqualFold(who, g.User) {
		fail("the token belongs to %s, not %s", who, g.User)
	}
	s := LoadSettings()
	s.SetProvider("github", g)
	SaveSettings(s)
	fmt.Printf("github: the agent acts for %s on %s, with the token from %s\n", g.User, g.host(), githubTokenSource(g))
}

// githubUser is the login a token belongs to.
func githubUser(apiHost, token string) (string, error) {
	req, err := http.NewRequest("GET", "https://"+apiHost+"/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s/user: %s", apiHost, resp.Status)
	}
	var u struct{ Login string }
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return "", err
	}
	return u.Login, nil
}
