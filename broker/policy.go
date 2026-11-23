package broker

import (
	"fmt"
	"strings"
)

// The rules every code-host provider shares. The agent acts for one user,
// and only inside that user's agent namespace:
//
//   - it pushes, and deletes branches, only under refs/heads/agent/<user>/;
//   - it opens merge or pull requests only from those branches, with
//     titles starting "[agent/<user>]" (after an optional "Draft:");
//   - it changes or comments on only the ones whose source branch is
//     already one of those (looked up from the provider, never trusted
//     from the request; a fork's branch never counts);
//   - it never merges or approves: a human does.
//
// Reads are allowed. Everything a provider's route table doesn't list is
// denied.

func Allow() Decision                  { return Decision{Allow: true, Reason: "allowed"} }
func Deny(f string, a ...any) Decision { return Decision{Reason: fmt.Sprintf(f, a...)} }

// Read allows a request that only reads.
func Read(*Call) Decision { return Allow() }

// Any allows every request on a route, for providers whose job is
// custody of the credential rather than finer rules (Anthropic, AWS).
func Any(*Call) Decision { return Allow() }

// Never is a check that always denies, for requests listed only so their
// denial says why.
func Never(reason string) Check { return func(*Call) Decision { return Deny("%s", reason) } }

// Namespace is the branch prefix the agent may write under.
func Namespace(user string) string { return "agent/" + user + "/" }

// TitleTag starts every title the agent sets.
func TitleTag(user string) string { return "[agent/" + user + "]" }

func validUser(user string) bool {
	return user != "" && !strings.ContainsAny(user, "/ \t\n")
}

// AgentRef allows a full ref name only under refs/heads/agent/<user>/.
func AgentRef(user, ref string) Decision {
	if !validUser(user) {
		return Deny("broker misconfigured: no valid user")
	}
	if !ValidRefName(ref) {
		return Deny("invalid ref name %q", ref)
	}
	ns := "refs/heads/" + Namespace(user)
	if !strings.HasPrefix(ref, ns) || len(ref) == len(ns) {
		return Deny("pushes are allowed only under %s", Namespace(user))
	}
	return Allow()
}

// AgentBranch allows a branch name only under agent/<user>/.
func AgentBranch(user, branch string) Decision {
	if branch == "" {
		return Deny("no branch given")
	}
	return AgentRef(user, "refs/heads/"+branch)
}

// ExistingFromAgentBranch allows changing a merge or pull request only if
// its source branch, as the provider reported it, is an agent branch.
// source is nil if the lookup failed; a fork's branch comes as "fork:…",
// which never passes.
func ExistingFromAgentBranch(user string, source *string) Decision {
	if source == nil {
		return Deny("couldn't look up the existing merge request's source branch")
	}
	if !AgentBranch(user, *source).Allow {
		return Deny("only merge requests from %s branches can be changed (this one is from %q)", Namespace(user), *source)
	}
	return Allow()
}

// TaggedTitle requires the tag at the start of a title, after an optional
// draft marker: "[agent/u] Fix x" or "Draft: [agent/u] Fix x". A nil
// title (the request doesn't set one) passes unless required.
func TaggedTitle(user string, title *string, required bool) Decision {
	if title == nil {
		if required {
			return Deny("merge request without a title")
		}
		return Allow()
	}
	t := strings.TrimSpace(*title)
	lower := strings.ToLower(t)
	for _, p := range []string{"draft:", "[draft]", "(draft)"} {
		if strings.HasPrefix(lower, p) {
			t = strings.TrimSpace(t[len(p):])
			break
		}
	}
	if !strings.HasPrefix(t, TitleTag(user)) {
		return Deny("titles must start with %s (after an optional \"Draft:\")", TitleTag(user))
	}
	return Allow()
}

// All returns the first denial among ds, or Allow.
func All(ds ...Decision) Decision {
	for _, d := range ds {
		if !d.Allow {
			return d
		}
	}
	return Allow()
}

// ValidRefName applies git's check-ref-format rules to a full ref name.
func ValidRefName(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || ref == "@" || strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".") || strings.Contains(ref, "..") ||
		strings.Contains(ref, "@{") || strings.Contains(ref, "//") {
		return false
	}
	for _, c := range ref {
		if c < 0x20 || c == 0x7f || strings.ContainsRune(" ~^:?*[\\", c) {
			return false
		}
	}
	for _, comp := range strings.Split(ref, "/") {
		if comp == "" || strings.HasPrefix(comp, ".") || strings.HasSuffix(comp, ".lock") {
			return false
		}
	}
	return true
}

// Ptr returns a pointer to s.
func Ptr(s string) *string { return &s }
