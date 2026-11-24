package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
)

// settings are what mono-jig login records, in <root>/settings.json:
// which providers the broker serves, and as whom, each under its login
// name (cliProvider), and the image mono-jig image build made. No
// secrets: tokens stay in the Keychain or in the tools' own logins on the
// Mac. The JIG_* variables override them (cliProvider.broker), for the
// checks.
type settings struct {
	// Image is the image mono-jig image build made (image.go).
	Image string
	// providers are each provider's own settings, by login name.
	providers map[string]json.RawMessage
}

func (s settings) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	for k, v := range s.providers {
		m[k] = v
	}
	if s.Image != "" {
		m["image"] = s.Image
	}
	return json.Marshal(m)
}

func (s *settings) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if v, ok := m["image"]; ok {
		if err := json.Unmarshal(v, &s.Image); err != nil {
			return err
		}
		delete(m, "image")
	}
	s.providers = m
	return nil
}

// providerSettings are provider name's settings, or nil if it has none.
func providerSettings[T any](s settings, name string) *T {
	raw, ok := s.providers[name]
	if !ok || string(raw) == "null" {
		return nil
	}
	v := new(T)
	if err := json.Unmarshal(raw, v); err != nil {
		fail("%s: %s: %v", settingsPath(), name, err)
	}
	return v
}

// setProvider records provider name's settings; nil forgets them.
func (s *settings) setProvider(name string, v any) {
	s.providers = maps.Clone(s.providers)
	if s.providers == nil {
		s.providers = map[string]json.RawMessage{}
	}
	if v == nil {
		delete(s.providers, name)
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		fail("%v", err)
	}
	s.providers[name] = b
}

func stateRoot() string {
	home, _ := os.UserHomeDir()
	return env("JIG_STATE", filepath.Join(home, ".mono-jig"))
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
