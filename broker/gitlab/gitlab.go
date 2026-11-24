// Package gitlab is the broker's GitLab provider, served as
// gitlab.broker.internal: git smart HTTP, LFS, REST v4, GraphQL and
// GitLab's hosted MCP server, forwarded to one GitLab (gitlab.com by
// default) with the user's token. Its routes are in routes.go;
// here are their checks.
package gitlab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/stevenpelley/msb-jig/broker"
)

// Checks are the GitLab provider's checks, which the routes (routes.go)
// run. API is the REST upstream: lookups go through it, with
// the user's credential, and LFS links are rewritten from its URL to its
// Public one.
type Checks struct {
	User string // the GitLab user the agent acts for
	API  *broker.Upstream
}

// --- lookups: what the provider says, never what the request claims ---

// get calls the REST API with the user's token and decodes a 200 into v.
func (g *Checks) get(c *broker.Call, path, rawPath string, v any) bool {
	u := *g.API.URL
	u.Path, u.RawPath = path, rawPath
	q, err := http.NewRequestWithContext(c.R.Context(), http.MethodGet, u.String(), nil)
	if err != nil || g.API.Auth(q.Header) != nil {
		return false
	}
	resp, err := g.API.Client.Do(q)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, broker.MaxBody)).Decode(v) == nil
}

// mrSource fetches a merge request's source branch. A fork's comes back as
// "fork:<branch>", which never passes the policy. nil if the lookup failed.
func (g *Checks) mrSource(c *broker.Call, pid, iid string) *string {
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
func (g *Checks) sameProject(c *broker.Call, pid, target string) bool {
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
// strings, the shape GitLab's REST API takes, as Rails does: the body's
// value wins, and the last of a repeated key. A JSON key twice (ignoring
// case) is refused (broker.DecodeJSON), and so is _method, which Rack
// reads as the request's real method.
func params(c *broker.Call) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range c.R.URL.Query() {
		if len(v) > 0 {
			out[k] = v[len(v)-1]
		}
	}
	if _, ok := out["_method"]; ok {
		return nil, fmt.Errorf("_method isn't accepted: it would change the request's method")
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
		if err := broker.DecodeJSON(body, &m); err != nil {
			return nil, fmt.Errorf("body isn't a JSON object with unique keys: %v", err)
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
	if _, ok := out["_method"]; ok {
		return nil, fmt.Errorf("_method isn't accepted: it would change the request's method")
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

// RewriteLFSBatch points every LFS action on the upstream host back
// through the broker and removes the actions' headers, which carry
// GitLab's short-lived LFS token. In proxy mode (no Public) the links
// already name the host the sandbox reaches.
func (g *Checks) RewriteLFSBatch(resp *http.Response) (io.Reader, error) {
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
				if u, err := url.Parse(href); err == nil && g.API.Public != nil && strings.EqualFold(u.Host, g.API.URL.Host) {
					u.Scheme, u.Host = g.API.Public.Scheme, g.API.Public.Host
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
