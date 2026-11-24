package main

import (
	"fmt"
	"slices"
	"strings"

	"mono-jig/broker/providers"
)

// cliProvider is a provider's command-line side: mono-jig login and
// logout, its line in mono-jig login's status, its settings
// (settings.json, under name) and the broker provider they turn on. Each
// is in login_<name>.go; cliProviders lists them, in the order login all
// asks.
type cliProvider struct {
	name     string // mono-jig login <name>; its key in settings.json
	keychain string // its Keychain item, if any: logout and uninstall delete it
	login    func(args []string)
	// status says how it's logged in, or "" if it isn't.
	status func(s settings) string
	// forgotten is what logout says, before the settings and the Keychain
	// item go.
	forgotten func(s settings) string
	// broker is the provider the settings turn on, under the JIG_*
	// variables; nil if it's off.
	broker func(s settings) providers.Provider
}

func cliProviders() []cliProvider {
	return []cliProvider{gitlabCLI, githubCLI, claudeCLI, awsCLI}
}

func cliProviderNamed(name string) (cliProvider, bool) {
	ps := cliProviders()
	i := slices.IndexFunc(ps, func(p cliProvider) bool { return p.name == name })
	if i < 0 {
		return cliProvider{}, false
	}
	return ps[i], true
}

func cliProviderNames() string {
	var names []string
	for _, p := range cliProviders() {
		names = append(names, p.name)
	}
	return strings.Join(names, ", ")
}

// brokerConfig is the providers mono-jig login recorded, under the JIG_*
// variables.
func brokerConfig() providers.Config {
	var b providers.Config
	s := loadSettings()
	for _, p := range cliProviders() {
		if bp := p.broker(s); bp != nil {
			b.Providers = append(b.Providers, bp)
		}
	}
	return b
}

// login logs in to one provider, or all, or shows the status.
func login(args []string) {
	if len(args) == 0 {
		loginStatus()
		return
	}
	switch args[0] {
	case "all", "--all":
		if len(args) != 1 {
			usage()
		}
		loginAll()
	default:
		p, ok := cliProviderNamed(args[0])
		if !ok {
			usage()
		}
		p.login(args[1:])
	}
	fmt.Println("sandboxes pick this up when they next start (mono-jig stop, then open)")
}

// loginAll asks about each provider in turn and logs in to those you
// choose, with each login's defaults.
func loginAll() {
	for _, p := range cliProviders() {
		if strings.EqualFold(ask("log in to "+p.name+"? [y/N] "), "y") {
			p.login(nil)
		}
	}
}

func loginStatus() {
	s := loadSettings()
	for _, p := range cliProviders() {
		st := p.status(s)
		if st == "" {
			st = "not logged in (mono-jig login " + p.name + ")"
		}
		fmt.Printf("%-7s %s\n", p.name, st)
	}
}

// logout forgets a provider: the settings, and its Keychain item. It
// leaves the user's own logins alone (glab's, AWS's SSO session).
func logout(args []string) {
	ps := cliProviders()
	if len(args) > 0 {
		ps = nil
		for _, a := range args {
			p, ok := cliProviderNamed(a)
			if !ok {
				usage()
			}
			ps = append(ps, p)
		}
	}
	s := loadSettings()
	for _, p := range ps {
		fmt.Printf("%s: %s\n", p.name, p.forgotten(s))
		s.setProvider(p.name, nil)
		if p.keychain != "" {
			keychainDelete(p.keychain)
		}
	}
	saveSettings(s)
	fmt.Println("running sandboxes keep the old setting until they stop")
}

// keychainItems are the providers' Keychain items.
func keychainItems() []string {
	var out []string
	for _, p := range cliProviders() {
		if p.keychain != "" {
			out = append(out, p.keychain)
		}
	}
	return out
}
