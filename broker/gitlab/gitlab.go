// Package gitlab is the broker's GitLab provider, served as
// gitlab.broker.internal: git smart HTTP, LFS, REST v4, GraphQL and
// GitLab's hosted MCP server, forwarded to one GitLab (gitlab.com by
// default) with the user's token. routes.go lists every request it serves.
package gitlab

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"mcp-restrict/broker"
)

// Config configures the provider.
type Config struct {
	User     string                 // the GitLab user the agent acts for
	Token    func() (string, error) // the user's token (glab's login, or the Keychain)
	MCPToken func() (string, error) // for the MCP endpoint, if not Token (fine-grained PAT)
	Upstream *url.URL               // https://gitlab.com
	Public   *url.URL               // https://gitlab.broker.internal (sbx: http://host.docker.internal:<port>)
	Client   *http.Client           // broker.NewUpstreamClient()
	Log      *slog.Logger
}

type gitlab struct {
	Config
	git, api, mcp *broker.Upstream
}

// New returns the provider.
func New(c Config) *broker.Provider {
	g := &gitlab{Config: c}
	up := func(auth func(http.Header) error) *broker.Upstream {
		return &broker.Upstream{URL: c.Upstream, Auth: auth, Public: c.Public, Client: c.Client,
			NoCredential: "no GitLab credential: run investigator login gitlab"}
	}
	// git and LFS: HTTP Basic, user and token, as git sends it.
	g.git = up(func(h http.Header) error {
		tok, err := c.Token()
		if err != nil {
			return err
		}
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.User+":"+tok)))
		return nil
	})
	// REST and GraphQL: the token as a Bearer token, which GitLab takes for
	// PATs and OAuth tokens alike (PRIVATE-TOKEN takes only PATs, so glab's
	// OAuth token got 401 there).
	g.api = up(func(h http.Header) error { return bearer(h, c.Token) })
	// The MCP server: its own token if there is one.
	mcpTok := c.Token
	if c.MCPToken != nil {
		mcpTok = c.MCPToken
	}
	g.mcp = up(func(h http.Header) error { return bearer(h, mcpTok) })
	return &broker.Provider{Name: "gitlab", Routes: g.routes(), Log: c.Log}
}

func bearer(h http.Header, tok func() (string, error)) error {
	t, err := tok()
	if err != nil {
		return err
	}
	h.Set("Authorization", "Bearer "+t)
	return nil
}

// --- lookups: what the provider says, never what the request claims ---

// get calls the REST API with the user's token and decodes a 200 into v.
func (g *gitlab) get(c *broker.Call, path, rawPath string, v any) bool {
	u := *g.Upstream
	u.Path, u.RawPath = path, rawPath
	q, err := http.NewRequestWithContext(c.R.Context(), http.MethodGet, u.String(), nil)
	if err != nil || g.api.Auth(q.Header) != nil {
		return false
	}
	resp, err := g.Client.Do(q)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, broker.MaxBody)).Decode(v) == nil
}

// mrSource fetches a merge request's source branch. A fork's comes back as
// "fork:<branch>", which never passes the policy. nil if the lookup failed.
func (g *gitlab) mrSource(c *broker.Call, pid, iid string) *string {
	var mr struct {
		SourceBranch    string `json:"source_branch"`
		SourceProjectID int64  `json:"source_project_id"`
		TargetProjectID int64  `json:"target_project_id"`
	}
	if !g.get(c, "/api/v4/projects/"+pid+"/merge_requests/"+iid,
		"/api/v4/projects/"+url.PathEscape(pid)+"/merge_requests/"+url.PathEscape(iid), &mr) {
		return nil
	}
	if mr.SourceProjectID != mr.TargetProjectID {
		return broker.Ptr("fork:" + mr.SourceBranch)
	}
	return broker.Ptr(mr.SourceBranch)
}

// sameProject reports whether target, a numeric project ID, names the
// project pid (a path or an ID). glab sends target_project_id as a number
// while the URL names the project by path, so the path's ID is looked up.
// Anything that can't be resolved counts as another project.
func (g *gitlab) sameProject(c *broker.Call, pid, target string) bool {
	if target == pid {
		return true
	}
	if _, err := strconv.ParseInt(target, 10, 64); err != nil {
		return false
	}
	if _, err := strconv.ParseInt(pid, 10, 64); err == nil {
		return false // two different numbers
	}
	var p struct {
		ID int64 `json:"id"`
	}
	if !g.get(c, "/api/v4/projects/"+pid, "/api/v4/projects/"+url.PathEscape(pid), &p) || p.ID == 0 {
		return false
	}
	return strconv.FormatInt(p.ID, 10) == target
}

// --- request bodies ---

// params merges query parameters with a JSON or form body into flat
// strings, the shape GitLab's REST API takes.
func params(c *broker.Call) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range c.R.URL.Query() {
		if len(v) > 0 {
			out[k] = v[len(v)-1]
		}
	}
	body, err := c.ReadBody()
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return out, nil
	}
	ct := strings.ToLower(c.R.Header.Get("Content-Type"))
	switch {
	case strings.HasPrefix(ct, "application/json"):
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, fmt.Errorf("body isn't a JSON object")
		}
		for k, v := range m {
			switch x := v.(type) {
			case string:
				out[k] = x
			case nil:
			default:
				b, _ := json.Marshal(x)
				out[k] = string(b)
			}
		}
	case strings.HasPrefix(ct, "application/x-www-form-urlencoded"):
		vs, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, fmt.Errorf("bad form body")
		}
		for k, v := range vs {
			out[k] = v[len(v)-1]
		}
	default:
		return nil, fmt.Errorf("unsupported Content-Type %q for an API write", ct)
	}
	return out, nil
}

func opt(p map[string]string, k string) *string {
	if v, ok := p[k]; ok {
		return &v
	}
	return nil
}

// --- responses ---

// rewriteLFSBatch points every LFS action on the upstream host back
// through the broker and removes the actions' headers, which carry
// GitLab's short-lived LFS token.
func (g *gitlab) rewriteLFSBatch(resp *http.Response) (io.Reader, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return bytes.NewReader(raw), nil // an error body; nothing to rewrite
	}
	objs, _ := doc["objects"].([]any)
	for _, o := range objs {
		om, _ := o.(map[string]any)
		acts, _ := om["actions"].(map[string]any)
		for _, a := range acts {
			am, _ := a.(map[string]any)
			if am == nil {
				continue
			}
			delete(am, "header")
			if href, _ := am["href"].(string); href != "" {
				if u, err := url.Parse(href); err == nil && strings.EqualFold(u.Host, g.Upstream.Host) {
					u.Scheme, u.Host = g.Public.Scheme, g.Public.Host
					am["href"] = u.String()
				}
			}
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(out), nil
}
