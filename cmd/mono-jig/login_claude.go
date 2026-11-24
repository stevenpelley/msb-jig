package main

import (
	"fmt"
	"os"
	"strings"

	"mono-jig/broker/providers"
)

// Claude's command-line side (cliProvider): the Anthropic provider, for
// Claude Code. Its token is in the Keychain only.

const keychainClaude = "mono-jig-claude"

var claudeCLI = cliProvider{
	name: "claude", keychain: keychainClaude, login: loginClaude,
	status: func(settings) string {
		if keychainHas(keychainClaude) {
			return "token in the Keychain (" + keychainClaude + ")"
		}
		return "not logged in (mono-jig login claude): the agent signs in inside the sandbox"
	},
	forgotten: func(settings) string {
		return "token removed from the Keychain; revoke it at claude.ai, Settings, Claude Code, if you're done with it"
	},
	broker: claudeBroker,
}

// claudeBroker: on while the Keychain item exists.
// JIG_CLAUDE_TOKEN_COMMAND overrides (empty: off).
func claudeBroker(settings) providers.Provider {
	cmd, set := os.LookupEnv("JIG_CLAUDE_TOKEN_COMMAND")
	if !set && keychainHas(keychainClaude) {
		cmd = "security find-generic-password -s " + keychainClaude + " -w"
	}
	if cmd == "" {
		return nil
	}
	return providers.Anthropic{TokenCmd: cmd}
}

// loginClaude stores a long-lived Claude token, from claude setup-token
// run on the Mac, in the Keychain. The broker sends it for the sandbox.
func loginClaude(args []string) {
	if len(args) != 0 {
		usage()
	}
	fmt.Println("Run `claude setup-token` in another terminal on this Mac and sign in; it prints a token.")
	tok := readSecret("paste the token (input hidden): ")
	if !strings.HasPrefix(tok, "sk-ant-") {
		fail("that isn't a Claude token (sk-ant-…)")
	}
	keychainStore(keychainClaude, "mono-jig", tok)
	fmt.Println("claude: token stored in the Keychain as", keychainClaude)
}
