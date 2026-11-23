package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// settings are what investigator login records, in <root>/settings.json:
// which providers the broker serves, and as whom. No secrets: tokens stay
// in the Keychain (keychainGitLab, keychainClaude) or in glab's own login.
// The INV_* variables override them (loadConfig), for the checks.
type settings struct {
	GitLab *gitlabSettings `json:"gitlab,omitempty"`
	AWS    *awsSettings    `json:"aws,omitempty"`
}

type gitlabSettings struct {
	User string `json:"user"`
	URL  string `json:"url"`
	// Token is where the broker reads the user's token: "glab" (the
	// user's own glab login on the Mac) or "keychain" (keychainGitLab).
	Token string `json:"token"`
}

type awsSettings struct {
	// Profiles for the AWS MCP proxy, the first the default.
	Profiles []string `json:"profiles"`
}

// The Keychain items login writes and logout and uninstall delete.
const (
	keychainGitLab = "inv-gitlab"
	keychainClaude = "inv-claude"
)

func stateRoot() string {
	home, _ := os.UserHomeDir()
	return env("INV_STATE", filepath.Join(home, ".investigator"))
}

func settingsPath() string { return filepath.Join(stateRoot(), "settings.json") }

func loadSettings() settings {
	var s settings
	b, err := os.ReadFile(settingsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return s
	}
	if err != nil {
		fail("%v", err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		fail("%s: %v", settingsPath(), err)
	}
	return s
}

func saveSettings(s settings) {
	if err := os.MkdirAll(stateRoot(), 0o700); err != nil {
		fail("%v", err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		fail("%v", err)
	}
	if err := os.WriteFile(settingsPath(), append(b, '\n'), 0o600); err != nil {
		fail("%v", err)
	}
}

// gitlabTokenCommand prints the GitLab token for the broker.
func gitlabTokenCommand(g gitlabSettings, host string) string {
	if g.Token == "keychain" {
		return "security find-generic-password -s " + keychainGitLab + " -w"
	}
	return "glab api --hostname " + host + " user >/dev/null && glab config get token --host " + host
}
