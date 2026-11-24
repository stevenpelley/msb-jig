package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"mono-jig/broker/providers"
)

// GitLab's command-line side (cliProvider).

type gitlabSettings struct {
	User string `json:"user"`
	URL  string `json:"url"`
	// Token is where the broker reads the user's token: "glab" (the
	// user's own glab login on the Mac) or "keychain" (keychainGitLab).
	Token string `json:"token"`
}

const keychainGitLab = "mono-jig-gitlab"

var gitlabCLI = cliProvider{
	name: "gitlab", keychain: keychainGitLab, login: loginGitLab,
	status: func(s settings) string {
		g := providerSettings[gitlabSettings](s, "gitlab")
		if g == nil {
			return ""
		}
		return fmt.Sprintf("%s on %s, token from %s", g.User, g.URL, tokenSource(*g))
	},
	forgotten: func(s settings) string {
		if g := providerSettings[gitlabSettings](s, "gitlab"); g != nil && g.Token == "glab" {
			return "forgotten; your own glab login stays (glab auth logout to end it)"
		}
		return "forgotten"
	},
	broker: gitlabBroker,
}

// gitlabBroker: on once login recorded a user. JIG_GITLAB_USER (empty:
// off), JIG_GITLAB_URL and JIG_GITLAB_TOKEN_COMMAND override.
func gitlabBroker(s settings) providers.Provider {
	gl := gitlabSettings{URL: "https://gitlab.com", Token: "glab"}
	if g := providerSettings[gitlabSettings](s, "gitlab"); g != nil {
		gl = *g
	}
	user := env("JIG_GITLAB_USER", gl.User)
	if user == "" {
		return nil
	}
	if !gitlabUserRE.MatchString(user) {
		fail("the GitLab user %q isn't a GitLab username", user)
	}
	p := providers.GitLab{User: user, URL: env("JIG_GITLAB_URL", gl.URL)}
	u, err := url.Parse(p.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		fail("the GitLab URL %q: want https://host", p.URL)
	}
	p.TokenCmd = env("JIG_GITLAB_TOKEN_COMMAND", gitlabTokenCommand(gl, u.Host))
	return p
}

// gitlabTokenCommand prints the GitLab token for the broker.
func gitlabTokenCommand(g gitlabSettings, host string) string {
	if g.Token == "keychain" {
		return "security find-generic-password -s " + keychainGitLab + " -w"
	}
	return "glab api --hostname " + host + " user >/dev/null && glab config get token --host " + host
}

func tokenSource(g gitlabSettings) string {
	if g.Token == "keychain" {
		return "the Keychain (" + keychainGitLab + ")"
	}
	return "your glab login"
}

// loginGitLab records the user the agent acts for, and where the broker
// gets their token: glab's login on the Mac (logging in if needed), or a
// token pasted into the Keychain. With no user given, the user is the
// token's; given one, it checks the token belongs to them.
func loginGitLab(args []string) {
	var g gitlabSettings
	g.URL, g.Token = "https://gitlab.com", "glab"
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--keychain":
			g.Token = "keychain"
		case a == "--url" && i+1 < len(args):
			i++
			g.URL = strings.TrimRight(args[i], "/")
		case strings.HasPrefix(a, "-"):
			usage()
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) > 1 || (len(rest) == 1 && !gitlabUserRE.MatchString(rest[0])) {
		usage()
	}
	if len(rest) == 1 {
		g.User = rest[0]
	}
	u, err := url.Parse(g.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" {
		fail("--url: want https://host")
	}
	if g.Token == "keychain" {
		tok := readSecret(fmt.Sprintf("a GitLab token on %s (api scope; input hidden): ", u.Host))
		keychainStore(keychainGitLab, u.Host, tok)
	} else {
		if _, err := exec.LookPath("glab"); err != nil {
			fail("glab isn't installed: brew install glab, or use --keychain")
		}
		if exec.Command("glab", "auth", "status", "--hostname", u.Host).Run() != nil {
			fmt.Println("glab isn't logged in to", u.Host+": glab auth login")
			c := exec.Command("glab", "auth", "login", "--hostname", u.Host)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				fail("glab auth login: %v", err)
			}
		}
	}
	out, err := exec.Command("sh", "-c", gitlabTokenCommand(g, u.Host)).Output()
	if err != nil {
		fail("reading the token back: %v", err)
	}
	who, err := gitlabUser(g.URL, strings.TrimSpace(string(out)))
	if err != nil {
		fail("checking the token: %v", err)
	}
	if g.User == "" {
		g.User = who
	} else if !strings.EqualFold(who, g.User) {
		fail("the token belongs to %s, not %s", who, g.User)
	}
	s := loadSettings()
	s.setProvider("gitlab", g)
	saveSettings(s)
	fmt.Printf("gitlab: the agent acts for %s on %s, with the token from %s\n", g.User, u.Host, tokenSource(g))
}

// gitlabUser is the username a token belongs to.
func gitlabUser(base, token string) (string, error) {
	req, err := http.NewRequest("GET", base+"/api/v4/user", nil)
	if err != nil {
		return "", err
	}
	// Bearer, as the broker sends it: glab's login is OAuth, which PRIVATE-TOKEN refuses.
	req.Header.Set("Authorization", "Bearer "+token)
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s/api/v4/user: %s", base, resp.Status)
	}
	var u struct{ Username string }
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return "", err
	}
	return u.Username, nil
}
