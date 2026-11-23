package allowlist

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Entry allows one host, or the subdomains of one ("*.example.com"; not
// example.com itself), on one port.
type Entry struct {
	Pattern string
	Port    int
	Layer   string // where it came from, for the listing
	Group   string
}

func (e Entry) String() string { return e.Pattern + ":" + strconv.Itoa(e.Port) }

// AllowedPorts are the only ports an entry may name.
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

// Matches reports whether the entry allows host (lowercase) on port.
func (e Entry) Matches(host string, port int) bool {
	if port != e.Port {
		return false
	}
	if suf, ok := strings.CutPrefix(e.Pattern, "*."); ok {
		return strings.HasSuffix(host, "."+suf)
	}
	return host == e.Pattern
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
