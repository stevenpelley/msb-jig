package broker

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// Anthropic forwards Claude Code's API calls to the Anthropic API with the
// user's credential, which never enters the sandbox: there, Claude Code
// holds a placeholder and ANTHROPIC_BASE_URL points here. Unlike the GitLab
// adapter it enforces no per-operation policy; its job is custody of the
// credential, so that api.anthropic.com can leave the filter's allowlist.
type Anthropic struct {
	// Token returns the user's credential: an OAuth token from
	// `claude setup-token` (sk-ant-oat…), sent as a Bearer token, or an API
	// key, sent as x-api-key.
	Token      func() (string, error)
	Upstream   *url.URL // https://api.anthropic.com
	PublicHost string   // anthropic.broker.internal
	Client     *http.Client
	Log        *slog.Logger
}

// oauthBeta is the beta flag the API wants with OAuth tokens (as Claude Code
// sends it; unverified that the API still requires it).
const oauthBeta = "oauth-2025-04-20"

func (a *Anthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/v1/") && !strings.HasPrefix(p, "/api/") {
		a.log(r, false, "path outside /v1/ and /api/")
		httpError(w, http.StatusNotFound, "not an Anthropic API path: "+p)
		return
	}
	tok, err := a.Token()
	if err != nil || tok == "" {
		a.log(r, false, "no credential")
		httpError(w, http.StatusServiceUnavailable, "no Claude credential on the Mac: run `claude setup-token` there and store it (see lima/sandbox.sh)")
		return
	}
	out, err := outbound(r, a.Upstream, r.Body)
	if err != nil {
		httpError(w, http.StatusBadGateway, "bad upstream request")
		return
	}
	setAnthropicAuth(out.Header, tok)
	a.log(r, true, "forwarded")
	resp, err := a.Client.Do(out)
	if err != nil {
		httpError(w, http.StatusBadGateway, "upstream: "+err.Error())
		return
	}
	defer resp.Body.Close()
	copyResponse(w, resp, a.Upstream, "https", a.PublicHost, nil)
}

// setAnthropicAuth replaces whatever the sandbox sent with the real
// credential, in the form its kind needs.
func setAnthropicAuth(h http.Header, tok string) {
	h.Del("Authorization")
	h.Del("X-Api-Key")
	if !strings.HasPrefix(tok, "sk-ant-oat") {
		h.Set("X-Api-Key", tok)
		return
	}
	h.Set("Authorization", "Bearer "+tok)
	for _, v := range h.Values("Anthropic-Beta") {
		for _, f := range strings.Split(v, ",") {
			if strings.TrimSpace(f) == oauthBeta {
				return
			}
		}
	}
	h.Add("Anthropic-Beta", oauthBeta)
}

func (a *Anthropic) log(r *http.Request, allow bool, reason string) {
	if a.Log == nil {
		return
	}
	a.Log.Info("decision", "provider", "anthropic", "op", "anthropic.call", "method", r.Method, "path", r.URL.Path, "allow", allow, "reason", reason)
}
