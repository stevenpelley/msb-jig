package jig

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
// name (ProviderCLI), the image mono-jig build made, and the extra
// CAs' files. No secrets: tokens stay in the Keychain or in the tools' own
// logins on the Mac. The JIG_* variables override them
// (ProviderCLI.broker, extraCAFiles), for the checks.
type Settings struct {
	// Image is the image last built for new sandboxes (image.go).
	Image string
	// ImageFile and ImageContext are the user's own Dockerfile and its
	// build context, absolute (build -f), which every new sandbox's
	// build puts on the base image; empty: the base image alone.
	ImageFile, ImageContext string
	// CAs are the extra CA certificates' files (ca.go).
	CAs []string
	// providers are each provider's own settings, by login name.
	providers map[string]json.RawMessage
}

func (s Settings) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	for k, v := range s.providers {
		m[k] = v
	}
	if s.Image != "" {
		m["image"] = s.Image
	}
	if s.ImageFile != "" {
		m["image_file"], m["image_context"] = s.ImageFile, s.ImageContext
	}
	if len(s.CAs) > 0 {
		m["cas"] = s.CAs
	}
	return json.Marshal(m)
}

func (s *Settings) UnmarshalJSON(b []byte) error {
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
	for k, p := range map[string]*string{"image_file": &s.ImageFile, "image_context": &s.ImageContext} {
		if v, ok := m[k]; ok {
			if err := json.Unmarshal(v, p); err != nil {
				return err
			}
			delete(m, k)
		}
	}
	if v, ok := m["cas"]; ok {
		if err := json.Unmarshal(v, &s.CAs); err != nil {
			return err
		}
		delete(m, "cas")
	}
	s.providers = m
	return nil
}

// ProviderSettings are provider name's Settings, or nil if it has none.
func ProviderSettings[T any](s Settings, name string) *T {
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

// SetProvider records provider name's Settings; nil forgets them.
func (s *Settings) SetProvider(name string, v any) {
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

func LoadSettings() Settings {
	var s Settings
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

func SaveSettings(s Settings) {
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
