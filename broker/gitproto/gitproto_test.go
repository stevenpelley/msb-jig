package gitproto

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

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
