package jig

import (
	"fmt"
	"slices"
	"strings"

	"github.com/stevenpelley/msb-jig/broker/providers"
)

// ProviderCLI is a provider's command-line side: mono-jig login and
// logout, its line in mono-jig login's status, its settings
// (settings.json, under Name; ProviderSettings, SetProvider) and the
// broker provider they turn on. The personal track's are GitLab, GitHub,
// Claude and AWS (login_<name>.go); Track.Providers lists a track's, in
// the order login all asks.
type ProviderCLI struct {
	Name     string // mono-jig login <name>; its key in settings.json
	Keychain string // its Keychain item, if any: logout and uninstall delete it
	Login    func(args []string)
	// Usage is its lines in the usage message: "login <name> [args]",
	// then what it does, the next lines indented by 24 spaces. Empty: "login
	// <name>".
	Usage string
	// Status says how it's logged in, or "" if it isn't.
	Status func(s Settings) string
	// Forgotten is what logout says, before the settings and the Keychain
	// item go.
	Forgotten func(s Settings) string
	// Broker is the provider the settings turn on, under the JIG_*
	// variables; nil if it's off.
	Broker func(s Settings) providers.Provider
}

func cliProviders() []ProviderCLI {
	return track.Providers
}

func cliProviderNamed(name string) (ProviderCLI, bool) {
	ps := cliProviders()
	i := slices.IndexFunc(ps, func(p ProviderCLI) bool { return p.Name == name })
	if i < 0 {
		return ProviderCLI{}, false
	}
	return ps[i], true
}

func cliProviderNames() string {
	var names []string
	for _, p := range cliProviders() {
		names = append(names, p.Name)
	}
	return strings.Join(names, ", ")
}

// brokerConfig is the providers mono-jig login recorded, under the JIG_*
// variables.
func brokerConfig() providers.Config {
	var b providers.Config
	s := LoadSettings()
	for _, p := range cliProviders() {
		if bp := p.Broker(s); bp != nil {
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
		p.Login(args[1:])
	}
	fmt.Println("sandboxes pick this up when they next start (mono-jig stop, then start)")
}

// loginAll asks about each provider in turn and logs in to those you
// choose, with each login's defaults.
func loginAll() {
	for _, p := range cliProviders() {
		if strings.EqualFold(Ask("log in to "+p.Name+"? [y/N] "), "y") {
			p.Login(nil)
		}
	}
}

func loginStatus() {
	s := LoadSettings()
	for _, p := range cliProviders() {
		st := p.Status(s)
		if st == "" {
			st = "not logged in (mono-jig login " + p.Name + ")"
		}
		fmt.Printf("%-7s %s\n", p.Name, st)
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
	s := LoadSettings()
	for _, p := range ps {
		fmt.Printf("%s: %s\n", p.Name, p.Forgotten(s))
		s.SetProvider(p.Name, nil)
		if p.Keychain != "" {
			KeychainDelete(p.Keychain)
		}
	}
	SaveSettings(s)
	fmt.Println("running sandboxes keep the old setting until they stop")
}

// keychainItems are the providers' Keychain items.
func keychainItems() []string {
	var out []string
	for _, p := range cliProviders() {
		if p.Keychain != "" {
			out = append(out, p.Keychain)
		}
	}
	return out
}
