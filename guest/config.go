package guest

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/template"
)

// Config is what the guest needs from the providers and the agent at
// every start: the environment, git's settings, files, and the agent's
// instructions. The supervisor builds it and writes it to the guest as
// JSON (ConfigFile); provision.sh's apply component applies it. Nothing
// in it is secret: the tokens are placeholders.
type Config struct {
	// Env is every session's environment: /etc/environment and
	// /etc/profile.d, and the sandbox's own (msb exec and ssh read
	// neither file). Values can't hold spaces, quotes or newlines.
	Env map[string]string `json:"env"`
	// Git is system git settings: each key's values replace the guest's.
	Git map[string][]string `json:"git,omitempty"`
	// GitClear are keys removed first: those a provider no longer served
	// would have set.
	GitClear []string `json:"gitClear,omitempty"`
	// Files are written when absent or still ours (File.Marker); those the
	// last apply wrote and this one doesn't are removed, if still ours.
	Files []File `json:"files,omitempty"`
	// Packages are installed if missing (apt).
	Packages []string `json:"packages,omitempty"`
	// Instructions are the providers' sections of the agent's
	// instructions, in Markdown; the agent puts them in its own file.
	Instructions []string `json:"-"`
}

// File is a file in the guest: Content, a symbolic link to Link, or JSON
// merged into what's there (Merge; never removed).
type File struct {
	Path  string `json:"path"`            // absolute, or ~/… in the user's home
	Owner string `json:"owner,omitempty"` // "user" (the sandbox's) or root, the default
	Mode  string `json:"mode,omitempty"`  // octal; default 0644
	// Marker, when set, is the file's first line: an existing file whose
	// first line differs is someone else's and left alone.
	Marker  string         `json:"marker,omitempty"`
	Content string         `json:"content,omitempty"`
	Link    string         `json:"link,omitempty"`
	Merge   map[string]any `json:"merge,omitempty"`
}

// ConfigFile is where the supervisor writes the Config for provision.sh.
const ConfigFile = "/root/guest.json"

// Add merges o into c: o's environment and git keys win, and its files,
// packages and instructions follow c's.
func (c *Config) Add(o Config) {
	if c.Env == nil {
		c.Env = map[string]string{}
	}
	maps.Copy(c.Env, o.Env)
	if len(o.Git) > 0 && c.Git == nil {
		c.Git = map[string][]string{}
	}
	maps.Copy(c.Git, o.Git)
	c.GitClear = append(c.GitClear, o.GitClear...)
	c.Files = append(c.Files, o.Files...)
	c.Packages = append(c.Packages, o.Packages...)
	c.Instructions = append(c.Instructions, o.Instructions...)
}

// Validate checks what provision.sh can't quote or place.
func (c Config) Validate() error {
	for k, v := range c.Env {
		if k == "" || strings.ContainsAny(k, "= \t\n\"'") {
			return fmt.Errorf("environment name %q", k)
		}
		if strings.ContainsAny(v, " \t\n\"'\\$`") {
			return fmt.Errorf("%s: the value %q has a space, quote or special character", k, v)
		}
	}
	for k, vs := range c.Git {
		for _, v := range vs {
			if strings.ContainsAny(k+v, "\t\n") {
				return fmt.Errorf("git setting %q: tab or newline", k)
			}
		}
	}
	seen := map[string]bool{}
	for _, f := range c.Files {
		if !strings.HasPrefix(f.Path, "/") && !strings.HasPrefix(f.Path, "~/") || strings.ContainsAny(f.Path, " \t\n") {
			return fmt.Errorf("file %q: want an absolute or ~/ path without spaces", f.Path)
		}
		if seen[f.Path] {
			return fmt.Errorf("file %q: twice", f.Path)
		}
		seen[f.Path] = true
		if f.Owner != "" && f.Owner != "user" && f.Owner != "root" {
			return fmt.Errorf("file %q: owner %q", f.Path, f.Owner)
		}
		if strings.ContainsAny(f.Marker, "\t\n") {
			return fmt.Errorf("file %q: marker with a tab or newline", f.Path)
		}
		if f.Marker != "" && f.Link == "" && f.Merge == nil && !strings.HasPrefix(f.Content, f.Marker+"\n") {
			return fmt.Errorf("file %q: the content doesn't start with its marker", f.Path)
		}
		kinds := 0
		for _, set := range []bool{f.Link != "", f.Merge != nil} {
			if set {
				kinds++
			}
		}
		if kinds > 1 || kinds == 1 && f.Content != "" {
			return fmt.Errorf("file %q: one of content, link and merge", f.Path)
		}
	}
	return nil
}

// JSON is the Config as provision.sh reads it, packages sorted and
// without duplicates.
func (c Config) JSON() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	c.Packages = slices.Compact(slices.Sorted(slices.Values(c.Packages)))
	c.GitClear = slices.Compact(slices.Sorted(slices.Values(c.GitClear)))
	return json.MarshalIndent(c, "", "  ")
}

// GitKeys are the Config's git keys, sorted.
func (c Config) GitKeys() []string { return slices.Sorted(maps.Keys(c.Git)) }

//go:embed instructions/*.md
var instructions embed.FS

// Instructions renders the section instructions/<name>.md with data
// (text/template).
func Instructions(name string, data any) string {
	t := template.Must(template.New(name+".md").Option("missingkey=error").ParseFS(instructions, "instructions/"+name+".md"))
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		panic(fmt.Sprintf("instructions %s: %v", name, err))
	}
	return b.String()
}
