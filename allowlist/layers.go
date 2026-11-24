// Package allowlist is the sandbox's egress allowlist: groups of hosts,
// and layers that turn groups on and add hosts. The built-in base layer
// (lists/base.yaml) is sbx's balanced defaults plus our own groups; org
// and user layers come from files. Compile turns the layers into entries,
// which the supervisor gives microsandbox as its egress rules. See
// docs/design.md, "Egress filtering".
package allowlist

import (
	"embed"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed lists/*.yaml
var lists embed.FS

// groupFiles hold the built-in groups.
var groupFiles = []string{"lists/sbx-balanced.yaml", "lists/groups.yaml"}

// Group is a set of hosts for one tool or service.
type Group struct {
	Description string `yaml:"description"`
	// Provider, if set, names the broker provider that serves these hosts:
	// the group is left out when the broker serves it.
	Provider string   `yaml:"provider"`
	Hosts    []string `yaml:"hosts"`
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
	out := map[string]Group{}
	for _, f := range groupFiles {
		b, err := lists.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var c struct {
			Groups map[string]Group `yaml:"groups"`
		}
		if err := decodeStrict(b, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for k, g := range c.Groups {
			if _, dup := out[k]; dup {
				return nil, fmt.Errorf("%s redefines group %q", f, k)
			}
			out[k] = g
		}
	}
	return out, nil
}

// Base returns the built-in base layer: sbx's balanced groups, our own,
// and the personal track's providers' and agent's groups.
func Base() Layer {
	b, err := lists.ReadFile("lists/base.yaml")
	if err == nil {
		var l Layer
		if l, err = ParseLayer("base", b); err == nil {
			return l
		}
	}
	panic(err)
}

// MustParseLayer is ParseLayer for a layer built into a track (go:embed),
// panicking on an error.
func MustParseLayer(name string, yaml []byte) Layer {
	l, err := ParseLayer(name, yaml)
	if err != nil {
		panic(err)
	}
	return l
}

// LoadLayer reads an org or user layer from a file.
func LoadLayer(file string) (Layer, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return Layer{}, err
	}
	return ParseLayer(file, b)
}

// ParseLayer reads a layer (YAML) named name.
func ParseLayer(name string, b []byte) (Layer, error) {
	var l Layer
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := decodeStrict(b, &l); err != nil {
			return Layer{}, fmt.Errorf("layer %s: %w", name, err)
		}
	}
	l.Name = name
	return l, nil
}

// Layers returns the built-in layers (a track's: Base, an org's), then
// the layer files (an org's, the user's) in order.
func Layers(builtin []Layer, files ...string) ([]Layer, error) {
	out := append([]Layer{}, builtin...)
	for _, f := range files {
		l, err := LoadLayer(f)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// Compile turns layers into entries, deduplicated, each labeled with the
// first layer and group that allowed it. Groups whose provider is in
// brokered are left out: the broker serves those hosts. Their hosts leave
// wherever else they're listed too (another group, a layer's hosts), so a
// provider can broker a host a broader group also allows; a wildcard
// elsewhere that covers one stays.
func Compile(layers []Layer, brokered ...string) ([]Entry, error) {
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
	skip := map[string]bool{}
	for _, p := range brokered {
		skip[p] = true
	}
	seen := map[string]bool{}
	for _, g := range groups {
		if g.Provider != "" && skip[g.Provider] {
			for _, h := range g.Hosts {
				if e, err := ParseEntry(h); err == nil {
					seen[e.String()] = true // brokered: never added
				}
			}
		}
	}
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
			if g.Provider != "" && skip[g.Provider] {
				continue
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

// Format writes entries one per line, with where each came from.
func Format(entries []Entry) string {
	var b strings.Builder
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
