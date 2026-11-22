// Package broker is the credential broker: it serves each managed provider
// on its own hostname, turns every request into a normalized decision
// request, decides it against the policy, injects the user's credential and
// forwards it. See investigator-design.md, "The piece to build first" and
// "Broker hardening".
package broker

import (
	"fmt"
	"strings"
)

// Op is what a request does, as the adapter classified it. The policy is a
// table over these, so an adapter can't allow anything the table doesn't.
type Op string

const (
	OpGitRead      Op = "git.read"       // ref advertisement and upload-pack (clone, fetch)
	OpGitPush      Op = "git.push"       // receive-pack; every ref is checked
	OpLFSBatch     Op = "lfs.batch"      // LFS batch API; the response is rewritten
	OpLFSObject    Op = "lfs.object"     // LFS object transfer through the broker
	OpLFSLocks     Op = "lfs.locks"      // LFS locks API
	OpAPIRead      Op = "api.read"       // REST GET and HEAD
	OpGraphQLQuery Op = "graphql.query"  // GraphQL documents with only query operations
	OpMRCreate     Op = "mr.create"      // open a merge or pull request
	OpMRUpdate     Op = "mr.update"      // edit one (title, description, state, target)
	OpMRNote       Op = "mr.note"        // comment on or resolve threads in one
	OpBranchDelete Op = "branch.delete"  // delete a branch through the API
	OpMerge        Op = "mr.merge"       // never allowed; named so the denial says why
	OpApprove      Op = "mr.approve"     // never allowed
	OpTokenAdmin   Op = "token.admin"    // tokens, keys, hooks, account settings: never allowed
	OpWrite        Op = "write.unlisted" // any other write: denied by default
	OpUnknown      Op = "unknown"        // a request the adapter doesn't recognize
)

// RefUpdate is one ref command from a push, or a ref an API call writes.
type RefUpdate struct {
	Old, New, Ref string
}

const zeroOID = "0000000000000000000000000000000000000000"

// Request is the normalized decision request an adapter produces.
type Request struct {
	Provider string
	User     string // the user the agent acts for; sets the namespace
	Op       Op
	Repo     string // owner/repo or group/sub/project, as the adapter normalized it
	Method   string
	Path     string

	Refs                 []RefUpdate // git.push, and API writes that touch refs
	Branch               *string     // branch.delete
	Title                *string     // set when the request sets a title
	SourceBranch         *string     // mr.create
	ExistingSourceBranch *string     // mr.update and mr.note: fetched from the provider
	Detail               string      // adapter's note for the log (e.g. why it's unlisted)
}

// Decision is the policy's answer, logged with the request.
type Decision struct {
	Allow  bool
	Reason string
}

func allow() Decision                  { return Decision{Allow: true, Reason: "allowed"} }
func deny(f string, a ...any) Decision { return Decision{Reason: fmt.Sprintf(f, a...)} }
func namespace(user string) string     { return "agent/" + user + "/" }
func titleTag(user string) string      { return "[agent/" + user + "]" }
func refNamespace(user string) string  { return "refs/heads/" + namespace(user) }
func strp(s string) *string            { return &s }
func val(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Decide is the whole policy. Reads pass; writes pass only if listed here,
// with their predicates; everything else is denied.
func Decide(r Request) Decision {
	if r.User == "" || strings.ContainsAny(r.User, "/ \t\n") {
		return deny("broker misconfigured: no valid user")
	}
	switch r.Op {
	case OpGitRead, OpLFSBatch, OpLFSObject, OpLFSLocks, OpAPIRead, OpGraphQLQuery:
		return allow()

	case OpGitPush:
		if len(r.Refs) == 0 {
			return deny("push with no ref updates")
		}
		for _, u := range r.Refs {
			if d := checkRef(r.User, u.Ref); !d.Allow {
				return d
			}
		}
		return allow() // creates, force updates and deletes inside the namespace

	case OpBranchDelete:
		return checkBranch(r.User, val(r.Branch))

	case OpMRCreate:
		if r.SourceBranch == nil {
			return deny("merge request without a source branch")
		}
		if d := checkBranch(r.User, *r.SourceBranch); !d.Allow {
			return d
		}
		if r.Title == nil {
			return deny("merge request without a title")
		}
		return checkTitle(r.User, *r.Title)

	case OpMRUpdate, OpMRNote:
		if r.ExistingSourceBranch == nil {
			return deny("couldn't check the existing merge request's source branch")
		}
		if d := checkBranch(r.User, *r.ExistingSourceBranch); !d.Allow {
			return deny("only merge requests from %s branches can be changed (this one is from %q)", namespace(r.User), *r.ExistingSourceBranch)
		}
		if r.Title != nil {
			return checkTitle(r.User, *r.Title)
		}
		return allow()

	case OpMerge:
		return deny("merging is never allowed through the broker; a human merges")
	case OpApprove:
		return deny("approving is never allowed through the broker")
	case OpTokenAdmin:
		return deny("token, key and account endpoints are never allowed")
	case OpWrite:
		return deny("this write isn't on the broker's list (%s)", r.Detail)
	default:
		return deny("request not recognized by the broker (%s)", r.Detail)
	}
}

func checkRef(user, ref string) Decision {
	if !ValidRefName(ref) {
		return deny("invalid ref name %q", ref)
	}
	if !strings.HasPrefix(ref, refNamespace(user)) || len(ref) == len(refNamespace(user)) {
		return deny("pushes are allowed only under %s", namespace(user))
	}
	return allow()
}

func checkBranch(user, branch string) Decision {
	if branch == "" {
		return deny("no branch given")
	}
	return checkRef(user, "refs/heads/"+branch)
}

// checkTitle requires the tag at the start of the title, after an optional
// draft marker: "[agent/u] Fix x" or "Draft: [agent/u] Fix x".
func checkTitle(user, title string) Decision {
	t := strings.TrimSpace(title)
	lower := strings.ToLower(t)
	for _, p := range []string{"draft:", "[draft]", "(draft)"} {
		if strings.HasPrefix(lower, p) {
			t = strings.TrimSpace(t[len(p):])
			break
		}
	}
	if !strings.HasPrefix(t, titleTag(user)) {
		return deny("titles must start with %s (after an optional \"Draft:\")", titleTag(user))
	}
	return allow()
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
