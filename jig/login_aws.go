package jig

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"msb-jig/broker/providers"
)

// AWS's command-line side (ProviderCLI): the profiles for the AWS MCP
// proxy. The credentials are the user's own, in ~/.aws.

type awsSettings struct {
	// Profiles for the AWS MCP proxy, the first the default.
	Profiles []string `json:"profiles"`
}

var AWS = ProviderCLI{
	Name: "aws", Login: loginAWS,
	Status: func(s Settings) string {
		a := ProviderSettings[awsSettings](s, "aws")
		if a == nil || len(a.Profiles) == 0 {
			return ""
		}
		return "profiles " + strings.Join(a.Profiles, " ") + " (the first is the default)"
	},
	Forgotten: func(Settings) string {
		return "forgotten; your SSO session stays (aws sso logout ends it for every tool)"
	},
	Broker: awsBroker,
}

// awsBroker: on once login recorded profiles. JIG_AWS_PROFILES (empty:
// off) and JIG_AWS_MCP_COMMAND override.
func awsBroker(s Settings) providers.Provider {
	var profiles []string
	if a := ProviderSettings[awsSettings](s, "aws"); a != nil {
		profiles = a.Profiles
	}
	p := providers.AWS{Profiles: strings.Fields(env("JIG_AWS_PROFILES", strings.Join(profiles, " "))),
		Command: env("JIG_AWS_MCP_COMMAND", "")}
	if len(p.Profiles) == 0 && p.Command == "" {
		return nil
	}
	return p
}

// loginAWS records the profiles for the AWS MCP proxy and runs aws sso
// login for each that uses SSO. The profiles and the SSO cache are the
// user's own, in ~/.aws (docs/prototype-plan.md step 7). With no profiles
// given, it lists yours and asks.
func loginAWS(args []string) {
	if _, err := exec.LookPath("aws"); err != nil {
		fail("the AWS CLI isn't installed: brew install awscli")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		fmt.Println("note: uv isn't installed, and runs AWS's MCP proxy: brew install uv")
	}
	out, err := exec.Command("aws", "configure", "list-profiles").Output()
	if err != nil {
		fail("aws configure list-profiles: %v", err)
	}
	profiles := strings.Fields(string(out))
	if len(args) == 0 {
		if len(profiles) == 0 {
			fail("no AWS profiles in your AWS config (aws configure sso)")
		}
		fmt.Println("your AWS profiles:\n  " + strings.Join(profiles, "\n  "))
		args = strings.Fields(ask("profiles for the agent, the default first (space-separated): "))
		if len(args) == 0 {
			fail("no profiles chosen")
		}
	}
	for _, p := range args {
		if strings.HasPrefix(p, "-") {
			usage()
		}
		if !slices.Contains(profiles, p) {
			fail("no AWS profile %s in your AWS config (aws configure list-profiles)", p)
		}
		if exec.Command("aws", "sts", "get-caller-identity", "--profile", p).Run() == nil {
			fmt.Println("aws:", p, "is signed in")
			continue
		}
		fmt.Println("aws: signing in", p)
		c := exec.Command("aws", "sso", "login", "--profile", p)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			fail("aws sso login --profile %s: %v", p, err)
		}
	}
	s := LoadSettings()
	s.SetProvider("aws", awsSettings{Profiles: args})
	SaveSettings(s)
	fmt.Printf("aws: profiles %s (the first is the default)\n", strings.Join(args, " "))
}
