package guest

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	for name, c := range map[string]Config{
		"space in a value":   {Env: map[string]string{"A": "b c"}},
		"relative path":      {Files: []File{{Path: "etc/x"}}},
		"twice":              {Files: []File{{Path: "/x"}, {Path: "/x"}}},
		"owner":              {Files: []File{{Path: "/x", Owner: "bob"}}},
		"marker not first":   {Files: []File{{Path: "/x", Marker: "# m", Content: "x\n# m\n"}}},
		"link and content":   {Files: []File{{Path: "/x", Link: "/y", Content: "z"}}},
		"newline in git key": {Git: map[string][]string{"a\nb": {"c"}}},
	} {
		if c.Validate() == nil {
			t.Errorf("%s: valid", name)
		}
	}
	ok := Config{Env: map[string]string{"A": "https://x/y"},
		Files: []File{{Path: "~/x", Owner: "user", Marker: "# m", Content: "# m\nx\n"}, {Path: "/y", Link: "/z"},
			{Path: "~/j", Merge: map[string]any{"a": true}}}}
	if err := ok.Validate(); err != nil {
		t.Error(err)
	}
}

// provision.sh's apply reads only the JSON fields Config has (besides
// to_entries' .key and .value, and file suffixes).
func TestApplyReadsConfigFields(t *testing.T) {
	fields := []string{"env", "git", "gitClear", "files", "packages", "path", "owner", "mode", "marker", "content", "link", "merge"}
	apply := Provision()["components/90-apply.sh"]
	i := strings.Index(apply, "if want apply; then")
	if i < 0 {
		t.Fatal("no apply component")
	}
	for _, m := range regexp.MustCompile(`(?:\(|'|\s)\.([a-zA-Z]+)\b|has\("([a-z]+)"\)`).FindAllStringSubmatch(apply[i:], -1) {
		f := m[1] + m[2]
		if !slices.Contains([]string{"tmp", "sh", "key", "value"}, f) && !slices.Contains(fields, f) {
			t.Errorf("apply reads .%s, which Config doesn't have", f)
		}
	}
}

func TestInstructions(t *testing.T) {
	one := Instructions("aws", map[string]string{"URL": "https://u", "Profile": "p", "Others": ""})
	two := Instructions("aws", map[string]string{"URL": "https://u", "Profile": "p", "Others": "q r"})
	if strings.Contains(one, "Other profiles") || !strings.HasSuffix(one, "another way in.\n") {
		t.Errorf("one profile:\n%s", one)
	}
	if !strings.HasSuffix(two, "another way in.\n- Other profiles: q r. Pick one per call with the tools'\n  `aws_profile` argument (`mcpcli py --profile <p>`).\n") {
		t.Errorf("two profiles:\n%s", two)
	}
}

// provision.sh sources lib.sh and components/ in name order; a changed
// component changes the hash.
func TestProvision(t *testing.T) {
	p := Provision()
	for _, f := range []string{"provision.sh", "lib.sh", "components/10-ca.sh", "components/20-base.sh", "components/90-apply.sh"} {
		if p[f] == "" {
			t.Errorf("no %s", f)
		}
	}
	h := ProvisionHash(p)
	if h != ProvisionHash(Provision()) {
		t.Error("hash not stable")
	}
	p["components/20-base.sh"] = "# another base\n"
	if ProvisionHash(p) == h || ProvisionHash(Provision()) != h {
		t.Error("hash didn't follow the change, or Provision shares its map")
	}
	for f, body := range Provision() {
		if strings.HasPrefix(f, "components/") && !strings.HasSuffix(f, ".sh") || strings.Contains(body, "\r") {
			t.Errorf("%s: not a component script", f)
		}
	}
}
