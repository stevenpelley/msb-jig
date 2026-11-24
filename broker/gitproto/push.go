// Package gitproto reads and writes git's smart HTTP protocol where the
// broker needs to: the ref commands at the start of a push, git's own
// rejection format, and the receive-pack advertisement.
package gitproto

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/stevenpelley/msb-jig/broker"
)

// RefUpdate is one ref command from a push.
type RefUpdate struct {
	Old, New, Ref string
}

// Push is the check for git-receive-pack: every ref the push updates,
// creates or deletes must be under refs/heads/agent/<user>/. Signed
// pushes and push options are refused. A denial is written in git's own
// format, so git prints the reason. Allowed, the body is forwarded
// decompressed.
func Push(user string) broker.Check {
	return func(c *broker.Call) broker.Decision {
		var body io.Reader = c.Body
		switch enc := strings.ToLower(c.R.Header.Get("Content-Encoding")); enc {
		case "", "identity":
		case "gzip":
			zr, err := gzip.NewReader(c.Body)
			if err != nil {
				return broker.Deny("bad gzip body")
			}
			body = zr
		default:
			return broker.Deny("unsupported Content-Encoding %s", enc)
		}
		br := bufio.NewReaderSize(body, 64<<10)
		p, err := parsePushRequest(br)
		if err != nil {
			return broker.Deny("can't parse push: %v", err)
		}
		c.Note("refs", p.Updates)
		d := broker.Allow()
		switch {
		case p.PushCert:
			d = broker.Deny("signed pushes aren't supported")
		case p.PushOptions:
			d = broker.Deny("push options (git push -o) aren't allowed")
		case len(p.Updates) == 0:
			d = broker.Deny("push with no ref updates")
		default:
			for _, u := range p.Updates {
				if d = broker.AgentRef(user, u.Ref); !d.Allow {
					break
				}
			}
		}
		if !d.Allow {
			c.DenyWith(func(w http.ResponseWriter, reason string) {
				// Read the rest, so git sees our status instead of a broken pipe.
				_, _ = io.Copy(io.Discard, io.LimitReader(br, 4<<30))
				w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
				w.Header().Set("Cache-Control", "no-cache")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(rejectPush(p, reason))
			})
			return d
		}
		c.R.Header.Del("Content-Encoding") // forwarded decompressed
		c.Body = io.MultiReader(bytes.NewReader(p.Prefix), br)
		return d
	}
}

// StripPushCaps removes push-options and push-cert from a ref
// advertisement, so git refuses `git push -o` before sending anything.
func StripPushCaps(resp *http.Response) (io.Reader, error) {
	br := bufio.NewReader(resp.Body)
	head, err := stripAdvertisedCaps(br, "push-options", "push-cert")
	if err != nil {
		return nil, err
	}
	return io.MultiReader(bytes.NewReader(head), br), nil
}
