package filter

import (
	"bufio"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// Entry allows one host, or the subdomains of one ("*.example.com"; not
// example.com itself), on one port.
type Entry struct {
	Pattern string
	Port    int
	Layer   string // where it came from, for the log
	Group   string
}

func (e Entry) String() string { return e.Pattern + ":" + strconv.Itoa(e.Port) }

// AllowedPorts are the only ports the filter dials.
var AllowedPorts = map[int]bool{80: true, 443: true}

// ParseEntry parses "host", "host:port", "*.suffix" or "*.suffix:port".
func ParseEntry(spec string) (Entry, error) {
	s := strings.ToLower(strings.TrimSpace(spec))
	e := Entry{Port: 443}
	if h, p, ok := strings.Cut(s, ":"); ok {
		n, err := strconv.Atoi(p)
		if err != nil || !AllowedPorts[n] {
			return Entry{}, fmt.Errorf("entry %q: port must be 80 or 443", spec)
		}
		s, e.Port = h, n
	}
	name := strings.TrimPrefix(s, "*.")
	if !ValidHostname(name) || strings.Contains(name, "*") {
		return Entry{}, fmt.Errorf("entry %q: not a hostname or *.suffix", spec)
	}
	if s != name && !strings.Contains(name, ".") {
		return Entry{}, fmt.Errorf("entry %q: a wildcard needs at least two labels after it", spec)
	}
	e.Pattern = s
	return e, nil
}

// ValidHostname accepts DNS names only: no IP literals, no numeric last
// label (so no "0x7f.1" style addresses), letters, digits and hyphens.
func ValidHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return false
	}
	labels := strings.Split(h, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	last := labels[len(labels)-1]
	if strings.Trim(last, "0123456789") == "" || strings.HasPrefix(last, "0x") {
		return false
	}
	return true
}

// NormalizeHost lowercases a hostname and drops one trailing dot.
func NormalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// ACL answers whether a host and port are allowed.
type ACL struct {
	exact map[string]Entry // "host:port"
	wild  map[string]Entry // "suffix:port", for "*.suffix"
	all   []Entry
}

// NewACL builds an ACL. It refuses an empty one: a filter with no ACL
// must not start (smokescreen's nil ACL allowed everything).
func NewACL(entries []Entry) (*ACL, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("empty ACL")
	}
	a := &ACL{exact: map[string]Entry{}, wild: map[string]Entry{}, all: entries}
	for _, e := range entries {
		if suf, ok := strings.CutPrefix(e.Pattern, "*."); ok {
			a.wild[suf+":"+strconv.Itoa(e.Port)] = e
		} else {
			a.exact[e.String()] = e
		}
	}
	return a, nil
}

// Entries returns what the ACL was built from.
func (a *ACL) Entries() []Entry { return a.all }

// Allows matches host (normalized, a valid hostname) and port.
func (a *ACL) Allows(host string, port int) (Entry, bool) {
	p := ":" + strconv.Itoa(port)
	if e, ok := a.exact[host+p]; ok {
		return e, true
	}
	for h := host; ; {
		_, rest, ok := strings.Cut(h, ".")
		if !ok {
			return Entry{}, false
		}
		if e, ok := a.wild[rest+p]; ok {
			return e, true
		}
		h = rest
	}
}

// ParseACL reads the format FormatACL writes: one entry per line, "#" comments.
func ParseACL(r io.Reader) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		if strings.TrimSpace(line) == "" {
			continue
		}
		e, err := ParseEntry(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}
