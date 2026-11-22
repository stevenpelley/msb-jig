// Package filter is the domain filter: the layered allowlist, the ACL it
// compiles to, and the explicit HTTP proxy that enforces it on Lima. See
// investigator-design.md, "Egress filtering".
package filter

import (
	"embed"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed lists/*.yaml
var lists embed.FS

// Profiles are the built-in profile layers.
var Profiles = []string{"investigator", "dev"}

// Group is a set of hosts for one tool or service.
type Group struct {
	Description string   `yaml:"description"`
	Hosts       []string `yaml:"hosts"`
}

// Layer turns on groups and adds hosts. Org and user layers may also
// define groups of their own.
type Layer struct {
	Name        string           `yaml:"-"`
	Description string           `yaml:"description"`
	Groups      []string         `yaml:"groups"`
	Hosts       []string         `yaml:"hosts"`
	Define      map[string]Group `yaml:"define"`
}

func decodeStrict(b []byte, v any) error {
	d := yaml.NewDecoder(strings.NewReader(string(b)))
	d.KnownFields(true)
	return d.Decode(v)
}

// Catalog returns the built-in groups.
func Catalog() (map[string]Group, error) {
	b, err := lists.ReadFile("lists/groups.yaml")
	if err != nil {
		return nil, err
	}
	var c struct {
		Groups map[string]Group `yaml:"groups"`
	}
	if err := decodeStrict(b, &c); err != nil {
		return nil, fmt.Errorf("groups.yaml: %w", err)
	}
	return c.Groups, nil
}

// BuiltinLayer returns "base" or one of Profiles.
func BuiltinLayer(name string) (Layer, error) {
	b, err := lists.ReadFile(path.Join("lists", name+".yaml"))
	if err != nil {
		return Layer{}, fmt.Errorf("no built-in layer %q", name)
	}
	return parseLayer(name, b)
}

// LoadLayer reads an org or user layer from a file.
func LoadLayer(file string) (Layer, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return Layer{}, err
	}
	return parseLayer(file, b)
}

func parseLayer(name string, b []byte) (Layer, error) {
	var l Layer
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := decodeStrict(b, &l); err != nil {
			return Layer{}, fmt.Errorf("layer %s: %w", name, err)
		}
	}
	l.Name = name
	return l, nil
}

// Layers returns base, the profile, then the extra layers (org, user) in order.
func Layers(profile string, extra ...string) ([]Layer, error) {
	ok := false
	for _, p := range Profiles {
		ok = ok || p == profile
	}
	if !ok {
		return nil, fmt.Errorf("unknown profile %q (want one of %s)", profile, strings.Join(Profiles, ", "))
	}
	var out []Layer
	for _, n := range []string{"base", profile} {
		l, err := BuiltinLayer(n)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	for _, f := range extra {
		l, err := LoadLayer(f)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// Compile turns layers into ACL entries, deduplicated, each labeled with
// the first layer and group that allowed it.
func Compile(layers []Layer) ([]Entry, error) {
	cat, err := Catalog()
	if err != nil {
		return nil, err
	}
	groups := map[string]Group{}
	for k, g := range cat {
		groups[k] = g
	}
	for _, l := range layers {
		for k, g := range l.Define {
			if _, dup := groups[k]; dup {
				return nil, fmt.Errorf("layer %s redefines group %q", l.Name, k)
			}
			groups[k] = g
		}
	}
	seen := map[string]bool{}
	var out []Entry
	add := func(spec, layer, group string) error {
		e, err := ParseEntry(spec)
		if err != nil {
			return fmt.Errorf("layer %s: %w", layer, err)
		}
		e.Layer, e.Group = layer, group
		if !seen[e.String()] {
			seen[e.String()] = true
			out = append(out, e)
		}
		return nil
	}
	for _, l := range layers {
		for _, gn := range l.Groups {
			g, ok := groups[gn]
			if !ok {
				return nil, fmt.Errorf("layer %s: unknown group %q", l.Name, gn)
			}
			for _, h := range g.Hosts {
				if err := add(h, l.Name, gn); err != nil {
					return nil, err
				}
			}
		}
		for _, h := range l.Hosts {
			if err := add(h, l.Name, ""); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// FormatACL writes entries one per line, with where each came from.
func FormatACL(entries []Entry) string {
	var b strings.Builder
	b.WriteString("# investigator filter ACL, generated from the allowlist layers\n")
	for _, e := range entries {
		src := e.Layer
		if e.Group != "" {
			src += "/" + e.Group
		}
		fmt.Fprintf(&b, "%-45s # %s\n", e.String(), src)
	}
	return b.String()
}

// FormatSbx writes the entries as sbx policy commands, one per port.
func FormatSbx(entries []Entry) string {
	byPort := map[int][]string{}
	for _, e := range entries {
		byPort[e.Port] = append(byPort[e.Port], e.Pattern+":"+fmt.Sprint(e.Port))
	}
	ports := make([]int, 0, len(byPort))
	for p := range byPort {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	var b strings.Builder
	for _, p := range ports {
		fmt.Fprintf(&b, "sbx policy allow network %q\n", strings.Join(byPort[p], ","))
	}
	return b.String()
}
