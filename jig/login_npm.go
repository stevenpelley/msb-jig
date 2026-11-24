package jig

import (
	"fmt"
	"os"
	"strings"

	"github.com/stevenpelley/msb-jig/broker/providers"
)

// npm's command-line side (ProviderCLI): the npm provider, read-only, for
// registry.npmjs.org. Its token is in the Keychain only: npm login's are
// session tokens that last two hours, so the user makes a granular
// read-only one. A track adds registries with its own Broker.

const keychainNPM = "mono-jig-npm"

var NPM = ProviderCLI{
	Name:     "npm",
	Usage:    "login npm               store a read-only npm token (a granular access token)\n",
	Keychain: keychainNPM, Login: loginNPM,
	Status: func(Settings) string {
		if KeychainHas(keychainNPM) {
			return "token in the Keychain (" + keychainNPM + "), read-only through the broker"
		}
		return ""
	},
	Forgotten: func(Settings) string {
		return "token removed from the Keychain; revoke it at npmjs.com, Access Tokens, if you're done with it"
	},
	Broker: func(Settings) providers.Provider {
		cmd := NPMTokenCommand()
		if cmd == "" {
			return nil
		}
		return providers.NPM{Registries: []providers.NPMRegistry{{Host: providers.NPMJSHost, TokenCmd: cmd}}}
	},
}

// NPMTokenCommand is the npm token's command: the Keychain item's, or
// JIG_NPM_TOKEN_COMMAND (empty: off); "" if not logged in.
func NPMTokenCommand() string {
	cmd, set := os.LookupEnv("JIG_NPM_TOKEN_COMMAND")
	if !set && KeychainHas(keychainNPM) {
		cmd = KeychainCommand(keychainNPM)
	}
	return cmd
}

func loginNPM(args []string) {
	if len(args) != 0 {
		usage()
	}
	fmt.Println("At npmjs.com, Access Tokens, generate a granular access token: read-only,")
	fmt.Println("for the packages or organizations you install from, with the longest expiry allowed.")
	tok := ReadSecret("paste the token (input hidden): ")
	if !strings.HasPrefix(tok, "npm_") {
		fail("that isn't an npm token (npm_…)")
	}
	KeychainStore(keychainNPM, "mono-jig", tok)
	fmt.Println("npm: token stored in the Keychain as", keychainNPM)
}
