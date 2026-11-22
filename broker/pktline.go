package broker

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Git's pkt-line framing: a 4-hex-digit length (including itself), then the
// payload. "0000" is a flush packet.

const maxPkt = 65520

var errFlush = errors.New("flush")

// readPkt reads one pkt-line. It returns errFlush for a flush packet. raw is
// every byte consumed, so a caller can forward the stream unchanged.
func readPkt(r *bufio.Reader) (payload, raw []byte, err error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, nil, err
	}
	n, err := strconv.ParseUint(string(hdr), 16, 16)
	if err != nil {
		return nil, hdr, fmt.Errorf("bad pkt-line length %q", hdr)
	}
	switch {
	case n == 0:
		return nil, hdr, errFlush
	case n < 4 || n > maxPkt:
		return nil, hdr, fmt.Errorf("bad pkt-line length %d", n)
	}
	buf := make([]byte, n)
	copy(buf, hdr)
	if _, err := io.ReadFull(r, buf[4:]); err != nil {
		return nil, buf, err
	}
	return buf[4:], buf, nil
}

func pkt(s string) []byte { return []byte(fmt.Sprintf("%04x%s", len(s)+4, s)) }

const flushPkt = "0000"

// --- receive-pack request ---

// pushRequest is the command section at the start of a receive-pack body.
type pushRequest struct {
	Updates     []RefUpdate
	Caps        []string
	PushOptions bool
	PushCert    bool
	Prefix      []byte // the raw bytes read, to forward before the rest
}

func (p *pushRequest) has(c string) bool {
	for _, x := range p.Caps {
		if x == c {
			return true
		}
	}
	return false
}

// parsePushRequest reads the commands up to their flush packet.
func parsePushRequest(r *bufio.Reader) (*pushRequest, error) {
	p := &pushRequest{}
	first := true
	for {
		payload, raw, err := readPkt(r)
		p.Prefix = append(p.Prefix, raw...)
		if err == errFlush {
			break
		}
		if err != nil {
			return nil, err
		}
		line := strings.TrimSuffix(string(payload), "\n")
		if strings.HasPrefix(line, "shallow ") {
			continue
		}
		if strings.HasPrefix(line, "push-cert") {
			p.PushCert = true
			return p, nil
		}
		if first {
			if i := strings.IndexByte(line, 0); i >= 0 {
				p.Caps = strings.Fields(line[i+1:])
				line = line[:i]
			}
			first = false
		}
		f := strings.Split(line, " ")
		if len(f) != 3 || len(f[0]) < 40 || len(f[1]) < 40 {
			return nil, fmt.Errorf("bad push command %q", line)
		}
		p.Updates = append(p.Updates, RefUpdate{Old: f[0], New: f[1], Ref: f[2]})
	}
	p.PushOptions = p.has("push-options")
	return p, nil
}

// rejectPush builds a receive-pack response that rejects every ref with
// reason, so git prints "! [remote rejected] <ref> (<reason>)".
func rejectPush(p *pushRequest, reason string) []byte {
	reason = strings.ReplaceAll(reason, "\n", " ")
	var status bytes.Buffer
	status.Write(pkt("unpack ok\n"))
	for _, u := range p.Updates {
		status.Write(pkt("ng " + u.Ref + " " + reason + "\n"))
	}
	status.WriteString(flushPkt)
	if !p.has("side-band-64k") && !p.has("side-band") {
		return status.Bytes()
	}
	var out bytes.Buffer
	out.Write(pkt("\x02investigator broker: " + reason + "\n"))
	b := status.Bytes()
	for len(b) > 0 {
		n := min(len(b), 900)
		out.Write(pkt("\x01" + string(b[:n])))
		b = b[n:]
	}
	out.WriteString(flushPkt)
	return out.Bytes()
}

// --- receive-pack advertisement ---

// stripAdvertisedCaps rewrites the ref advertisement so the client never
// offers push options or push certificates. It reads up to the end of the
// first ref line and returns the rewritten bytes to send before the rest.
func stripAdvertisedCaps(r *bufio.Reader, drop ...string) ([]byte, error) {
	var out bytes.Buffer
	for {
		payload, raw, err := readPkt(r)
		if err == errFlush {
			out.Write(raw)
			continue // the flush after "# service=..."; keep going to the first ref
		}
		if err != nil {
			return nil, err
		}
		line := string(payload)
		if strings.HasPrefix(line, "# service=") || strings.HasPrefix(line, "version ") {
			out.Write(raw)
			continue
		}
		i := strings.IndexByte(line, 0)
		if i < 0 {
			out.Write(raw)
			return out.Bytes(), nil
		}
		caps := strings.Fields(strings.TrimSuffix(line[i+1:], "\n"))
		kept := caps[:0]
	next:
		for _, c := range caps {
			for _, d := range drop {
				if c == d || strings.HasPrefix(c, d+"=") {
					continue next
				}
			}
			kept = append(kept, c)
		}
		out.Write(pkt(line[:i] + "\x00" + strings.Join(kept, " ") + "\n"))
		return out.Bytes(), nil
	}
}
