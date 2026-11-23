package broker

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// pushBody reads the ref commands at the start of a git-receive-pack
// request and decides them. Allowed, it returns the body to forward
// (decompressed, Content-Encoding removed). Denied, it answers with git's
// own rejection, so git prints the reason, and returns false.
func pushBody(w http.ResponseWriter, r *http.Request, req *Request, log *slog.Logger) (io.Reader, bool) {
	req.Op = OpGitPush
	var body io.Reader = r.Body
	switch enc := strings.ToLower(r.Header.Get("Content-Encoding")); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			httpError(w, http.StatusBadRequest, "bad gzip body")
			return nil, false
		}
		body = zr
	default:
		httpError(w, http.StatusUnsupportedMediaType, "unsupported Content-Encoding "+enc)
		return nil, false
	}
	br := bufio.NewReaderSize(body, 64<<10)
	p, err := parsePushRequest(br)
	if err != nil {
		httpError(w, http.StatusBadRequest, "can't parse push: "+err.Error())
		return nil, false
	}
	req.Refs = p.Updates
	var d Decision
	switch {
	case p.PushCert:
		d = deny("signed pushes aren't supported")
	case p.PushOptions:
		d = deny("push options (git push -o) aren't allowed")
	default:
		d = Decide(*req)
	}
	logDecision(log, req, d)
	if !d.Allow {
		// Read the rest, so git sees our status instead of a broken pipe.
		_, _ = io.Copy(io.Discard, io.LimitReader(br, 4<<30))
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rejectPush(p, d.Reason))
		return nil, false
	}
	r.Header.Del("Content-Encoding") // the body is forwarded decompressed
	return io.MultiReader(bytes.NewReader(p.Prefix), br), true
}

func logDecision(log *slog.Logger, req *Request, d Decision) {
	if log != nil {
		log.Info("decision", "provider", req.Provider, "user", req.User, "op", req.Op, "repo", req.Repo,
			"method", req.Method, "path", req.Path, "refs", req.Refs, "allow", d.Allow, "reason", d.Reason)
	}
}
