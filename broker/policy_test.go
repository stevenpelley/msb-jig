package broker

import "testing"

func TestPolicy(t *testing.T) {
	const u = "alice"
	cases := []struct {
		name  string
		d     Decision
		allow bool
	}{
		{"ref in namespace", AgentRef(u, "refs/heads/agent/alice/fix"), true},
		{"ref nested in namespace", AgentRef(u, "refs/heads/agent/alice/stack/1"), true},
		{"main", AgentRef(u, "refs/heads/main"), false},
		{"the bare namespace", AgentRef(u, "refs/heads/agent/alice/"), false},
		{"another user's namespace", AgentRef(u, "refs/heads/agent/bob/x"), false},
		{"prefix trick", AgentRef(u, "refs/heads/agent/alicex/y"), false},
		{"dot-dot escape", AgentRef(u, "refs/heads/agent/alice/../main"), false},
		{"tag", AgentRef(u, "refs/tags/agent/alice/v1"), false},
		{"lock suffix", AgentRef(u, "refs/heads/agent/alice/x.lock"), false},
		{"no user", AgentRef("", "refs/heads/agent//x"), false},
		{"user with a slash", AgentRef("a/b", "refs/heads/agent/a/b/x"), false},
		{"branch", AgentBranch(u, "agent/alice/x"), true},
		{"no branch", AgentBranch(u, ""), false},
		{"existing own", ExistingFromAgentBranch(u, Ptr("agent/alice/x")), true},
		{"existing human's", ExistingFromAgentBranch(u, Ptr("feature")), false},
		{"existing unknown", ExistingFromAgentBranch(u, nil), false},
		{"existing fork", ExistingFromAgentBranch(u, Ptr("fork:agent/alice/x")), false},
		{"all, one denies", All(Allow(), Deny("x"), Allow()), false},
	}
	for _, c := range cases {
		if c.d.Allow != c.allow {
			t.Errorf("%s: allow=%v (%s), want %v", c.name, c.d.Allow, c.d.Reason, c.allow)
		}
	}
}

func TestValidRefName(t *testing.T) {
	good := []string{"refs/heads/main", "refs/heads/agent/a/b-c_d.e"}
	bad := []string{"refs/heads/a..b", "refs/heads/.x", "refs/heads/x.", "refs/heads/x/", "refs/heads//x",
		"refs/heads/a b", "refs/heads/a~1", "refs/heads/a^", "refs/heads/a:b", "refs/heads/a@{1}", "heads/x",
		"refs/heads/x.lock", "refs/heads/a\x00b", "refs/heads/a*", "refs/heads/a[", "refs/heads/a\\b"}
	for _, r := range good {
		if !ValidRefName(r) {
			t.Errorf("%q rejected", r)
		}
	}
	for _, r := range bad {
		if ValidRefName(r) {
			t.Errorf("%q accepted", r)
		}
	}
}
