package broker

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestDecide(t *testing.T) {
	const u = "alice"
	oid := strings.Repeat("a", 40)
	push := func(refs ...string) Request {
		r := Request{User: u, Op: OpGitPush}
		for _, ref := range refs {
			r.Refs = append(r.Refs, RefUpdate{Old: zeroOID, New: oid, Ref: ref})
		}
		return r
	}
	cases := []struct {
		name  string
		req   Request
		allow bool
	}{
		{"push to namespace", push("refs/heads/agent/alice/fix"), true},
		{"push nested in namespace", push("refs/heads/agent/alice/stack/1"), true},
		{"push to main", push("refs/heads/main"), false},
		{"push to the bare namespace", push("refs/heads/agent/alice/"), false},
		{"push to another user's namespace", push("refs/heads/agent/bob/x"), false},
		{"prefix trick", push("refs/heads/agent/alicex/y"), false},
		{"dot-dot escape", push("refs/heads/agent/alice/../main"), false},
		{"tag", push("refs/tags/agent/alice/v1"), false},
		{"one bad ref denies the push", push("refs/heads/agent/alice/ok", "refs/heads/main"), false},
		{"lock suffix", push("refs/heads/agent/alice/x.lock"), false},
		{"empty push", Request{User: u, Op: OpGitPush}, false},
		{"mr create ok", Request{User: u, Op: OpMRCreate, SourceBranch: strp("agent/alice/x"), Title: strp("[agent/alice] Fix")}, true},
		{"mr create draft", Request{User: u, Op: OpMRCreate, SourceBranch: strp("agent/alice/x"), Title: strp("Draft: [agent/alice] Fix")}, true},
		{"mr create no tag", Request{User: u, Op: OpMRCreate, SourceBranch: strp("agent/alice/x"), Title: strp("Fix")}, false},
		{"mr create tag not first", Request{User: u, Op: OpMRCreate, SourceBranch: strp("agent/alice/x"), Title: strp("Fix [agent/alice]")}, false},
		{"mr create from main", Request{User: u, Op: OpMRCreate, SourceBranch: strp("main"), Title: strp("[agent/alice] Fix")}, false},
		{"mr update own", Request{User: u, Op: OpMRUpdate, ExistingSourceBranch: strp("agent/alice/x")}, true},
		{"mr update own, drops tag", Request{User: u, Op: OpMRUpdate, ExistingSourceBranch: strp("agent/alice/x"), Title: strp("Fix")}, false},
		{"mr update human's", Request{User: u, Op: OpMRUpdate, ExistingSourceBranch: strp("feature"), Title: strp("[agent/alice] mine now")}, false},
		{"mr update unknown", Request{User: u, Op: OpMRUpdate}, false},
		{"mr update fork", Request{User: u, Op: OpMRUpdate, ExistingSourceBranch: strp("fork:agent/alice/x")}, false},
		{"merge", Request{User: u, Op: OpMerge}, false},
		{"approve", Request{User: u, Op: OpApprove}, false},
		{"token", Request{User: u, Op: OpTokenAdmin}, false},
		{"unlisted", Request{User: u, Op: OpWrite}, false},
		{"unknown", Request{User: u, Op: OpUnknown}, false},
		{"read", Request{User: u, Op: OpAPIRead}, true},
		{"branch delete own", Request{User: u, Op: OpBranchDelete, Branch: strp("agent/alice/x")}, true},
		{"branch delete main", Request{User: u, Op: OpBranchDelete, Branch: strp("main")}, false},
		{"no user", Request{Op: OpAPIRead}, false},
	}
	for _, c := range cases {
		d := Decide(c.req)
		if d.Allow != c.allow {
			t.Errorf("%s: allow=%v (%s), want %v", c.name, d.Allow, d.Reason, c.allow)
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

func TestParsePushRequest(t *testing.T) {
	old, nw := strings.Repeat("0", 40), strings.Repeat("b", 40)
	var b bytes.Buffer
	b.Write(pkt(old + " " + nw + " refs/heads/agent/a/x\x00report-status side-band-64k push-options agent=git/2\n"))
	b.Write(pkt(nw + " " + old + " refs/heads/agent/a/y\n"))
	b.WriteString(flushPkt)
	b.WriteString("PACKDATA")
	br := bufio.NewReader(&b)
	p, err := parsePushRequest(br)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Updates) != 2 || p.Updates[1].Ref != "refs/heads/agent/a/y" || !p.PushOptions || !p.has("side-band-64k") {
		t.Fatalf("parsed %+v", p)
	}
	rest, _ := br.ReadString(0)
	if rest != "PACKDATA" {
		t.Fatalf("rest = %q", rest)
	}
	resp := string(rejectPush(p, "nope"))
	if !strings.Contains(resp, "ng refs/heads/agent/a/x nope") || !strings.HasSuffix(resp, "0000") {
		t.Fatalf("reject = %q", resp)
	}
}

func TestStripAdvertisedCaps(t *testing.T) {
	var b bytes.Buffer
	b.Write(pkt("# service=git-receive-pack\n"))
	b.WriteString(flushPkt)
	b.Write(pkt(strings.Repeat("c", 40) + " refs/heads/main\x00report-status push-options push-cert=123 atomic\n"))
	b.Write(pkt(strings.Repeat("d", 40) + " refs/heads/x\n"))
	b.WriteString(flushPkt)
	br := bufio.NewReader(&b)
	head, err := stripAdvertisedCaps(br, "push-options", "push-cert")
	if err != nil {
		t.Fatal(err)
	}
	s := string(head)
	if strings.Contains(s, "push-options") || strings.Contains(s, "push-cert") || !strings.Contains(s, "report-status atomic") {
		t.Fatalf("head = %q", s)
	}
}
