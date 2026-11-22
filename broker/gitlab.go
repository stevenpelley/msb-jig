package broker

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// GitLab is the GitLab adapter: git smart HTTP, LFS, REST v4 and GraphQL,
// served as gitlab.broker.internal.
type GitLab struct {
	User       string                 // the GitLab user the agent acts for
	Token      func() (string, error) // the user's token (Keychain on the Mac)
	Upstream   *url.URL               // https://gitlab.com
	PublicHost string                 // gitlab.broker.internal (sbx mode: host.docker.internal:<port>)
	// PublicScheme is how the sandbox reaches the broker: https (the
	// default), or http in sbx mode.
	PublicScheme string
	Client     *http.Client           // NewUpstreamClient()
	Log        *slog.Logger
	// MCPToken, if set, is used for the MCP endpoint instead of Token
	// (GitLab's MCP server takes fine-grained PATs or OAuth, not classic PATs).
	MCPToken func() (string, error)
}

const maxAPIBody = 1 << 20

var repoPathRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)+$`)

func (g *GitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req := Request{Provider: "gitlab", User: g.User, Method: r.Method, Path: r.URL.EscapedPath()}
	esc := r.URL.EscapedPath()
	switch {
	case esc == GitLabMCPPath:
		g.mcp(w, r, &req)
	case strings.HasPrefix(esc, "/api/v4/"):
		g.api(w, r, &req)
	case esc == "/api/graphql":
		g.graphql(w, r, &req)
	default:
		g.git(w, r, &req)
	}
}

func (g *GitLab) scheme() string {
	if g.PublicScheme != "" {
		return g.PublicScheme
	}
	return "https"
}

func (g *GitLab) decide(req *Request) Decision {
	d := Decide(*req)
	if g.Log != nil {
		g.Log.Info("decision", "provider", req.Provider, "user", req.User, "op", req.Op, "repo", req.Repo,
			"method", req.Method, "path", req.Path, "refs", req.Refs, "allow", d.Allow, "reason", d.Reason)
	}
	return d
}

func (g *GitLab) denyHTTP(w http.ResponseWriter, d Decision) {
	httpError(w, http.StatusForbidden, d.Reason)
}

// --- git and LFS ---

func (g *GitLab) git(w http.ResponseWriter, r *http.Request, req *Request) {
	repo, rest, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), ".git/")
	if !ok || r.URL.RawPath != "" || !repoPathRE.MatchString(repo) || strings.Contains(repo, "..") {
		req.Op, req.Detail = OpUnknown, "not a git, API or GraphQL path"
		g.denyHTTP(w, g.decide(req))
		return
	}
	req.Repo = repo
	switch {
	case rest == "info/refs" && r.Method == http.MethodGet:
		svc := r.URL.Query().Get("service")
		if svc != "git-upload-pack" && svc != "git-receive-pack" {
			req.Op, req.Detail = OpUnknown, "info/refs service "+svc
			g.denyHTTP(w, g.decide(req))
			return
		}
		req.Op = OpGitRead
		if d := g.decide(req); !d.Allow {
			g.denyHTTP(w, d)
			return
		}
		if svc == "git-receive-pack" {
			g.forward(w, r, http.NoBody, g.gitAuth, stripPushCaps)
		} else {
			g.forward(w, r, http.NoBody, g.gitAuth, nil)
		}
	case rest == "git-upload-pack" && r.Method == http.MethodPost:
		req.Op = OpGitRead
		if d := g.decide(req); !d.Allow {
			g.denyHTTP(w, d)
			return
		}
		g.forward(w, r, r.Body, g.gitAuth, nil) // compressed or not, forwarded as is
	case rest == "git-receive-pack" && r.Method == http.MethodPost:
		g.receivePack(w, r, req)
	case rest == "info/lfs/objects/batch" && r.Method == http.MethodPost:
		req.Op = OpLFSBatch
		if d := g.decide(req); !d.Allow {
			g.denyHTTP(w, d)
			return
		}
		g.forward(w, r, r.Body, g.gitAuth, g.rewriteLFSBatch)
	case strings.HasPrefix(rest, "info/lfs/locks"):
		req.Op = OpLFSLocks
		if d := g.decide(req); !d.Allow {
			g.denyHTTP(w, d)
			return
		}
		g.forward(w, r, r.Body, g.gitAuth, nil)
	case strings.HasPrefix(rest, "gitlab-lfs/objects/") && (r.Method == http.MethodGet || r.Method == http.MethodPut):
		req.Op = OpLFSObject
		if d := g.decide(req); !d.Allow {
			g.denyHTTP(w, d)
			return
		}
		g.forward(w, r, r.Body, g.gitAuth, nil)
	default:
		req.Op, req.Detail = OpUnknown, r.Method+" "+rest
		g.denyHTTP(w, g.decide(req))
	}
}

func (g *GitLab) receivePack(w http.ResponseWriter, r *http.Request, req *Request) {
	req.Op = OpGitPush
	var body io.Reader = r.Body
	switch enc := strings.ToLower(r.Header.Get("Content-Encoding")); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			httpError(w, http.StatusBadRequest, "bad gzip body")
			return
		}
		body = zr
	default:
		httpError(w, http.StatusUnsupportedMediaType, "unsupported Content-Encoding "+enc)
		return
	}
	br := bufio.NewReaderSize(body, 64<<10)
	p, err := parsePushRequest(br)
	if err != nil {
		httpError(w, http.StatusBadRequest, "can't parse push: "+err.Error())
		return
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
	if g.Log != nil {
		g.Log.Info("decision", "provider", req.Provider, "user", req.User, "op", req.Op, "repo", req.Repo,
			"method", req.Method, "path", req.Path, "refs", req.Refs, "allow", d.Allow, "reason", d.Reason)
	}
	if !d.Allow {
		// Read the rest, so git sees our status instead of a broken pipe.
		_, _ = io.Copy(io.Discard, io.LimitReader(br, 4<<30))
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rejectPush(p, d.Reason))
		return
	}
	r.Header.Del("Content-Encoding") // the body is forwarded decompressed
	g.forward(w, r, io.MultiReader(bytes.NewReader(p.Prefix), br), g.gitAuth, nil)
}

// stripPushCaps removes push-options and push-cert from the advertisement,
// so git refuses `git push -o` before sending anything.
func stripPushCaps(resp *http.Response) (io.Reader, error) {
	br := bufio.NewReader(resp.Body)
	head, err := stripAdvertisedCaps(br, "push-options", "push-cert")
	if err != nil {
		return nil, err
	}
	return io.MultiReader(bytes.NewReader(head), br), nil
}

// rewriteLFSBatch points every LFS action on the upstream host back through
// the broker and removes the actions' headers, which carry GitLab's
// short-lived LFS token.
func (g *GitLab) rewriteLFSBatch(resp *http.Response) (io.Reader, error) {
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
					u.Scheme, u.Host = g.scheme(), g.PublicHost
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

// --- REST ---

func (g *GitLab) api(w http.ResponseWriter, r *http.Request, req *Request) {
	var segs []string
	for _, s := range strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4/"), "/") {
		u, err := url.PathUnescape(s)
		if err != nil {
			httpError(w, http.StatusBadRequest, "bad path")
			return
		}
		segs = append(segs, u)
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		req.Op = OpAPIRead
		if len(segs) >= 2 && segs[0] == "projects" {
			req.Repo = segs[1]
		}
		if d := g.decide(req); !d.Allow {
			g.denyHTTP(w, d)
			return
		}
		g.forward(w, r, http.NoBody, g.apiAuth, nil)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxAPIBody+1))
	if err != nil || len(body) > maxAPIBody {
		httpError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	params, err := apiParams(r, body)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	g.classifyWrite(r, segs, params, req)
	if d := g.decide(req); !d.Allow {
		g.denyHTTP(w, d)
		return
	}
	g.forward(w, r, bytes.NewReader(body), g.apiAuth, nil)
}

func (g *GitLab) classifyWrite(r *http.Request, segs []string, params map[string]string, req *Request) {
	m := r.Method
	req.Op, req.Detail = OpWrite, m+" /"+strings.Join(segs, "/")
	n := len(segs)
	at := func(i int) string {
		if i < n {
			return segs[i]
		}
		return ""
	}
	switch at(0) {
	case "personal_access_tokens", "user", "users", "keys", "deploy_keys", "deploy_tokens", "hooks", "applications", "runners":
		req.Op = OpTokenAdmin
		return
	case "projects":
	default:
		return
	}
	if n < 3 {
		return
	}
	pid := segs[1]
	req.Repo = pid
	switch at(2) {
	case "hooks", "deploy_keys", "deploy_tokens", "access_tokens", "variables", "members", "protected_branches", "push_rule":
		req.Op = OpTokenAdmin
		return
	case "merge_requests":
		switch {
		case n == 3 && m == http.MethodPost:
			req.Op = OpMRCreate
			if v, ok := params["title"]; ok {
				req.Title = strp(v)
			}
			if v, ok := params["source_branch"]; ok {
				req.SourceBranch = strp(v)
			}
			if t, ok := params["target_project_id"]; ok && !g.sameProject(r, pid, t) {
				req.Op, req.Detail = OpWrite, "merge request into another project"
			}
		case n == 4 && m == http.MethodPut:
			req.Op = OpMRUpdate
			if v, ok := params["title"]; ok {
				req.Title = strp(v)
			}
			req.ExistingSourceBranch = g.existingSourceBranch(r, pid, segs[3])
		case n == 5 && (at(4) == "merge" || at(4) == "merge_when_pipeline_succeeds" || at(4) == "cancel_merge_when_pipeline_succeeds"):
			req.Op = OpMerge
		case n == 5 && (at(4) == "approve" || at(4) == "unapprove" || at(4) == "reset_approvals"):
			req.Op = OpApprove
		case n >= 5 && (at(4) == "notes" || at(4) == "discussions") && (m == http.MethodPost || m == http.MethodPut):
			req.Op = OpMRNote
			req.ExistingSourceBranch = g.existingSourceBranch(r, pid, segs[3])
		}
	case "repository":
		if at(3) == "branches" && n == 5 && m == http.MethodDelete {
			req.Op = OpBranchDelete
			req.Branch = strp(segs[4])
		}
		// commits, files, branches (create), tags, cherry_pick, revert: unlisted.
	}
	if params["merge_when_pipeline_succeeds"] == "true" || params["auto_merge"] == "true" {
		req.Op, req.Detail = OpMerge, "auto-merge"
	}
}

// existingSourceBranch fetches the merge request, so an update is checked
// against the branch it already has. A fork's branch never counts.
// sameProject reports whether target, a numeric project ID, names the
// project pid (a path or an ID). glab always sends target_project_id as a
// number while the URL names the project by path, so resolve the path's ID
// upstream. Anything that can't be resolved counts as another project.
func (g *GitLab) sameProject(r *http.Request, pid, target string) bool {
	if target == pid {
		return true
	}
	if _, err := strconv.ParseInt(target, 10, 64); err != nil {
		return false
	}
	if _, err := strconv.ParseInt(pid, 10, 64); err == nil {
		return false // two different numbers
	}
	tok, err := g.Token()
	if err != nil {
		return false
	}
	u := *g.Upstream
	u.Path = "/api/v4/projects/" + pid
	u.RawPath = "/api/v4/projects/" + url.PathEscape(pid)
	q, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	q.Header.Set("Private-Token", tok)
	resp, err := g.Client.Do(q)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var p struct {
		ID int64 `json:"id"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxAPIBody)).Decode(&p) != nil || p.ID == 0 {
		return false
	}
	return strconv.FormatInt(p.ID, 10) == target
}

func (g *GitLab) existingSourceBranch(r *http.Request, pid, iid string) *string {
	tok, err := g.Token()
	if err != nil {
		return nil
	}
	u := *g.Upstream
	u.Path = "/api/v4/projects/" + pid + "/merge_requests/" + iid
	u.RawPath = "/api/v4/projects/" + url.PathEscape(pid) + "/merge_requests/" + url.PathEscape(iid)
	q, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		return nil
	}
	q.Header.Set("Private-Token", tok)
	resp, err := g.Client.Do(q)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var mr struct {
		SourceBranch    string `json:"source_branch"`
		SourceProjectID int64  `json:"source_project_id"`
		TargetProjectID int64  `json:"target_project_id"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxAPIBody)).Decode(&mr) != nil {
		return nil
	}
	if mr.SourceProjectID != mr.TargetProjectID {
		return strp("fork:" + mr.SourceBranch)
	}
	return strp(mr.SourceBranch)
}

// apiParams merges query parameters with a JSON or form body into flat
// strings, the shape GitLab's REST API takes.
func apiParams(r *http.Request, body []byte) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			out[k] = v[len(v)-1]
		}
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return out, nil
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
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

// --- GraphQL ---

func (g *GitLab) graphql(w http.ResponseWriter, r *http.Request, req *Request) {
	var body []byte
	var docs []string
	switch r.Method {
	case http.MethodGet:
		docs = []string{r.URL.Query().Get("query")}
	case http.MethodPost:
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, maxAPIBody+1))
		if err != nil || len(body) > maxAPIBody {
			httpError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		docs, err = graphqlDocuments(body)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
	default:
		httpError(w, http.StatusMethodNotAllowed, "GraphQL takes GET or POST")
		return
	}
	req.Op = OpGraphQLQuery
	for _, d := range docs {
		if op, detail := classifyGraphQL(d); op != OpGraphQLQuery {
			req.Op, req.Detail = op, detail
			break
		}
	}
	if d := g.decide(req); !d.Allow {
		g.denyHTTP(w, d)
		return
	}
	var fwd io.Reader = http.NoBody
	if body != nil {
		fwd = bytes.NewReader(body)
	}
	g.forward(w, r, fwd, g.apiAuth, nil)
}

// graphqlDocuments returns the query text of a single request or a batch.
func graphqlDocuments(body []byte) ([]string, error) {
	type one struct {
		Query string `json:"query"`
	}
	b := bytes.TrimSpace(body)
	if len(b) > 0 && b[0] == '[' {
		var batch []one
		if err := json.Unmarshal(b, &batch); err != nil {
			return nil, fmt.Errorf("bad GraphQL batch")
		}
		var out []string
		for _, x := range batch {
			out = append(out, x.Query)
		}
		return out, nil
	}
	var x one
	if err := json.Unmarshal(b, &x); err != nil {
		return nil, fmt.Errorf("bad GraphQL request")
	}
	return []string{x.Query}, nil
}

// classifyGraphQL parses the document rather than trusting operation names.
// Only documents whose every operation is a query pass; no mutation is on
// the list yet.
func classifyGraphQL(doc string) (Op, string) {
	q, err := parser.ParseQuery(&ast.Source{Input: doc})
	if err != nil {
		return OpUnknown, "unparseable GraphQL"
	}
	if len(q.Operations) == 0 {
		return OpUnknown, "GraphQL document without operations"
	}
	for _, op := range q.Operations {
		if op.Operation == ast.Query {
			continue
		}
		var fields []string
		for _, s := range op.SelectionSet {
			if f, ok := s.(*ast.Field); ok {
				fields = append(fields, f.Name)
			}
		}
		name := strings.Join(fields, ",")
		if strings.Contains(strings.ToLower(name), "merge") && op.Operation == ast.Mutation {
			for _, f := range fields {
				if f == "mergeRequestAccept" || f == "mergeRequestSetAutoMerge" {
					return OpMerge, "GraphQL " + f
				}
			}
		}
		return OpWrite, fmt.Sprintf("GraphQL %s %s", op.Operation, name)
	}
	return OpGraphQLQuery, ""
}

// --- credentials and forwarding ---

func (g *GitLab) gitAuth(h http.Header) error {
	tok, err := g.Token()
	if err != nil {
		return err
	}
	h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(g.User+":"+tok)))
	return nil
}

func (g *GitLab) apiAuth(h http.Header) error {
	tok, err := g.Token()
	if err != nil {
		return err
	}
	h.Set("Private-Token", tok)
	return nil
}

func (g *GitLab) forward(w http.ResponseWriter, r *http.Request, body io.Reader, auth func(http.Header) error, rewrite func(*http.Response) (io.Reader, error)) {
	out, err := outbound(r, g.Upstream, body)
	if err != nil {
		httpError(w, http.StatusBadGateway, "bad upstream request")
		return
	}
	if err := auth(out.Header); err != nil {
		httpError(w, http.StatusServiceUnavailable, "no credential: run investigator login")
		return
	}
	resp, err := g.Client.Do(out)
	if err != nil {
		httpError(w, http.StatusBadGateway, "upstream: "+err.Error())
		return
	}
	defer resp.Body.Close()
	var rb io.Reader
	if rewrite != nil && resp.StatusCode == http.StatusOK {
		if rb, err = rewrite(resp); err != nil {
			httpError(w, http.StatusBadGateway, "upstream response: "+err.Error())
			return
		}
	}
	copyResponse(w, resp, g.Upstream, g.scheme(), g.PublicHost, rb)
}
