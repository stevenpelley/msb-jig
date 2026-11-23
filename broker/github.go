package broker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// GitHub is the GitHub adapter, served as github.broker.internal: git smart
// HTTP (to github.com), and the API the way gh calls a GitHub Enterprise
// Server, REST under /api/v3/ and GraphQL at /api/graphql (to
// api.github.com). The same rules as GitLab's: push only under
// agent/<user>/, tagged PR titles, change only PRs from those branches,
// never merge or approve. LFS isn't served yet.
type GitHub struct {
	User       string                 // the GitHub user the agent acts for
	Token      func() (string, error) // the user's token (gh's login, or the Keychain)
	Git        *url.URL               // https://github.com
	API        *url.URL               // https://api.github.com
	PublicHost string                 // github.broker.internal
	Client     *http.Client           // NewUpstreamClient()
	Log        *slog.Logger
}

const githubAPIPrefix = "/api/v3"

var (
	githubRepoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	// The run and job log endpoints answer with a redirect to signed blob
	// storage. The broker follows it itself, so the sandbox needs no route
	// to that storage, which would be a route to anyone's.
	githubLogsRE = regexp.MustCompile(`^/repos/[^/]+/[^/]+/actions/(runs/\d+(/attempts/\d+)?|jobs/\d+)/logs$`)
)

func (g *GitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req := Request{Provider: "github", User: g.User, Method: r.Method, Path: r.URL.EscapedPath()}
	esc := r.URL.EscapedPath()
	switch {
	case esc == "/api/graphql":
		g.graphql(w, r, &req)
	case esc == githubAPIPrefix || strings.HasPrefix(esc, githubAPIPrefix+"/"):
		g.api(w, r, &req)
	case strings.HasPrefix(esc, "/api/"):
		req.Op, req.Detail = OpUnknown, "not a git, REST or GraphQL path"
		g.deny(w, &req)
	default:
		g.git(w, r, &req)
	}
}

func (g *GitHub) decide(req *Request) Decision {
	d := Decide(*req)
	logDecision(g.Log, req, d)
	return d
}

func (g *GitHub) deny(w http.ResponseWriter, req *Request) {
	httpError(w, http.StatusForbidden, g.decide(req).Reason)
}

// --- git ---

func (g *GitHub) git(w http.ResponseWriter, r *http.Request, req *Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	var repo, rest string
	ok := false
	for _, e := range []string{"info/refs", "git-upload-pack", "git-receive-pack"} {
		if b, found := strings.CutSuffix(p, "/"+e); found {
			repo, rest, ok = strings.TrimSuffix(b, ".git"), e, true
			break
		}
	}
	if !ok || r.URL.RawPath != "" || !githubRepoRE.MatchString(repo) || strings.Contains(repo, "..") {
		req.Op, req.Detail = OpUnknown, "not a git path (LFS isn't served for GitHub)"
		g.deny(w, req)
		return
	}
	req.Repo = repo
	r.URL.Path = "/" + repo + ".git/" + rest // one spelling upstream
	switch {
	case rest == "info/refs" && r.Method == http.MethodGet:
		svc := r.URL.Query().Get("service")
		if svc != "git-upload-pack" && svc != "git-receive-pack" {
			req.Op, req.Detail = OpUnknown, "info/refs service "+svc
			g.deny(w, req)
			return
		}
		req.Op = OpGitRead
		if d := g.decide(req); !d.Allow {
			httpError(w, http.StatusForbidden, d.Reason)
			return
		}
		var rw func(*http.Response) (io.Reader, error)
		if svc == "git-receive-pack" {
			rw = stripPushCaps
		}
		g.forward(w, r, g.Git, http.NoBody, g.gitAuth, rw)
	case rest == "git-upload-pack" && r.Method == http.MethodPost:
		req.Op = OpGitRead
		if d := g.decide(req); !d.Allow {
			httpError(w, http.StatusForbidden, d.Reason)
			return
		}
		g.forward(w, r, g.Git, r.Body, g.gitAuth, nil)
	case rest == "git-receive-pack" && r.Method == http.MethodPost:
		if body, ok := pushBody(w, r, req, g.Log); ok {
			g.forward(w, r, g.Git, body, g.gitAuth, nil)
		}
	default:
		req.Op, req.Detail = OpUnknown, r.Method+" "+rest
		g.deny(w, req)
	}
}

// --- REST ---

func (g *GitHub) api(w http.ResponseWriter, r *http.Request, req *Request) {
	// gh's Enterprise form, /api/v3/<path>, is api.github.com/<path>.
	r.URL.Path = strings.TrimPrefix(r.URL.Path, githubAPIPrefix)
	if r.URL.RawPath != "" {
		r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, githubAPIPrefix)
	}
	if r.URL.Path == "" {
		r.URL.Path = "/"
	}
	var segs []string
	for _, s := range strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/") {
		u, err := url.PathUnescape(s)
		if err != nil {
			httpError(w, http.StatusBadRequest, "bad path")
			return
		}
		segs = append(segs, u)
	}
	if len(segs) >= 3 && segs[0] == "repos" {
		req.Repo = segs[1] + "/" + segs[2]
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		req.Op = OpAPIRead
		if d := g.decide(req); !d.Allow {
			httpError(w, http.StatusForbidden, d.Reason)
			return
		}
		g.forward(w, r, g.API, http.NoBody, g.apiAuth, nil)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAPIBody+1))
	if err != nil || len(body) > maxAPIBody {
		httpError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var params map[string]any
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &params); err != nil {
			httpError(w, http.StatusBadRequest, "an API write's body must be a JSON object")
			return
		}
	}
	g.classifyWrite(r, segs, params, req)
	if d := g.decide(req); !d.Allow {
		httpError(w, http.StatusForbidden, d.Reason)
		return
	}
	g.forward(w, r, g.API, bytes.NewReader(body), g.apiAuth, nil)
}

func str(m map[string]any, k string) *string {
	if s, ok := m[k].(string); ok {
		return &s
	}
	return nil
}

// classifyWrite names a REST write. Anything not named here stays
// OpWrite, which the policy denies.
func (g *GitHub) classifyWrite(r *http.Request, segs []string, params map[string]any, req *Request) {
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
	case "user", "users", "authorizations", "applications", "orgs", "admin", "app", "installation":
		req.Op = OpTokenAdmin
		return
	case "repos":
	default:
		return
	}
	if n < 4 {
		return
	}
	owner := segs[1]
	switch at(3) {
	case "keys", "hooks", "collaborators", "invitations", "environments", "rulesets", "branches":
		req.Op = OpTokenAdmin // deploy keys, webhooks, access, branch protection
	case "codespaces":
		req.Op = OpTokenAdmin
	case "actions", "dependabot":
		if at(4) == "secrets" || at(4) == "variables" || at(4) == "permissions" {
			req.Op = OpTokenAdmin
		}
	case "merges":
		req.Op, req.Detail = OpMerge, "merging a branch"
	case "git":
		if at(4) == "refs" && at(5) == "heads" && n > 6 && m == http.MethodDelete {
			req.Op = OpBranchDelete
			req.Branch = strp(strings.Join(segs[6:], "/"))
		}
	case "pulls":
		switch {
		case n == 4 && m == http.MethodPost:
			req.Op = OpMRCreate
			req.Title = str(params, "title")
			if h := str(params, "head"); h != nil {
				if o, b, cross := strings.Cut(*h, ":"); cross {
					if !strings.EqualFold(o, owner) {
						req.Op, req.Detail = OpWrite, "a pull request from another repository"
					}
					req.SourceBranch = strp(b)
				} else {
					req.SourceBranch = h
				}
			}
			if params["head_repo"] != nil || params["issue"] != nil {
				req.Op, req.Detail = OpWrite, "a pull request from another repository or an issue"
			}
		case n == 5 && m == http.MethodPatch:
			req.Op = OpMRUpdate
			req.Title = str(params, "title")
			req.ExistingSourceBranch = g.restPRHead(r, segs[1], segs[2], segs[4])
		case n == 6 && at(5) == "merge":
			req.Op = OpMerge
		case n == 6 && at(5) == "requested_reviewers" && (m == http.MethodPost || m == http.MethodDelete):
			req.Op = OpMRUpdate
			req.ExistingSourceBranch = g.restPRHead(r, segs[1], segs[2], segs[4])
		case n >= 6 && at(5) == "reviews" && m == http.MethodPost:
			// A new review (n 6), or submitting a pending one (…/reviews/<id>/events).
			if ev := str(params, "event"); ev != nil && strings.EqualFold(*ev, "APPROVE") {
				req.Op = OpApprove
				return
			}
			if n == 6 || (n == 8 && at(7) == "events") {
				req.Op = OpMRNote
				req.ExistingSourceBranch = g.restPRHead(r, segs[1], segs[2], segs[4])
			}
		case n >= 6 && at(5) == "comments" && m == http.MethodPost:
			req.Op = OpMRNote
			req.ExistingSourceBranch = g.restPRHead(r, segs[1], segs[2], segs[4])
		}
	case "issues":
		// A pull request's conversation comments are issue comments; only
		// those on a pull request (restPRHead finds one) are listed.
		if n == 6 && at(5) == "comments" && m == http.MethodPost {
			req.Op = OpMRNote
			req.ExistingSourceBranch = g.restPRHead(r, segs[1], segs[2], segs[4])
		}
	}
}

// restPRHead fetches a pull request's head branch; a fork's never counts.
func (g *GitHub) restPRHead(r *http.Request, owner, repo, num string) *string {
	var pr struct {
		Head struct {
			Ref  string `json:"ref"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	}
	p := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/pulls/" + url.PathEscape(num)
	if !g.get(r, p, &pr) || pr.Head.Ref == "" {
		return nil
	}
	if pr.Head.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, pr.Base.Repo.FullName) {
		return strp("fork:" + pr.Head.Ref)
	}
	return strp(pr.Head.Ref)
}

// get calls the API with the user's token and decodes a 200 into v.
func (g *GitHub) get(r *http.Request, escPath string, v any) bool {
	u := *g.API
	u.RawPath = escPath
	p, err := url.PathUnescape(escPath)
	if err != nil {
		return false
	}
	u.Path = p
	q, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil || g.apiAuth(q.Header) != nil {
		return false
	}
	return g.do(q, v)
}

func (g *GitHub) do(q *http.Request, v any) bool {
	q.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.Client.Do(q)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, maxAPIBody)).Decode(v) == nil
}

// --- GraphQL ---

func (g *GitHub) graphql(w http.ResponseWriter, r *http.Request, req *Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "GraphQL takes POST")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAPIBody+1))
	if err != nil || len(body) > maxAPIBody {
		httpError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var gq struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(body, &gq); err != nil {
		httpError(w, http.StatusBadRequest, "bad GraphQL request (GitHub takes no batches)")
		return
	}
	g.classifyGraphQL(r, gq.Query, gq.Variables, req)
	if d := g.decide(req); !d.Allow {
		httpError(w, http.StatusForbidden, d.Reason)
		return
	}
	r.URL.Path, r.URL.RawPath = "/graphql", ""
	g.forward(w, r, g.API, bytes.NewReader(body), g.apiAuth, nil)
}

// githubMerges are the mutations that merge, or arrange a merge.
var githubMerges = map[string]bool{"mergePullRequest": true, "enablePullRequestAutoMerge": true,
	"disablePullRequestAutoMerge": true, "enqueuePullRequest": true, "dequeuePullRequest": true, "mergeBranch": true}

// classifyGraphQL parses the document rather than trusting operation
// names. Queries pass. A mutation is allowed only as one listed field,
// whose input names the pull request (or its thread or review), which is
// then looked up.
func (g *GitHub) classifyGraphQL(r *http.Request, doc string, vars map[string]any, req *Request) {
	q, err := parser.ParseQuery(&ast.Source{Input: doc})
	if err != nil || len(q.Operations) == 0 {
		req.Op, req.Detail = OpUnknown, "unparseable GraphQL"
		return
	}
	var muts []*ast.Field
	for _, op := range q.Operations {
		if op.Operation == ast.Query {
			continue
		}
		if op.Operation != ast.Mutation {
			req.Op, req.Detail = OpWrite, "GraphQL "+string(op.Operation)
			return
		}
		for _, s := range op.SelectionSet {
			f, ok := s.(*ast.Field)
			if !ok {
				req.Op, req.Detail = OpWrite, "GraphQL mutation with fragments"
				return
			}
			muts = append(muts, f)
		}
	}
	if len(muts) == 0 {
		req.Op = OpGraphQLQuery
		return
	}
	for _, f := range muts {
		if githubMerges[f.Name] {
			req.Op, req.Detail = OpMerge, "GraphQL "+f.Name
			return
		}
	}
	if len(muts) > 1 {
		req.Op, req.Detail = OpWrite, "several GraphQL mutations in one request"
		return
	}
	f := muts[0]
	req.Op, req.Detail = OpWrite, "GraphQL mutation "+f.Name
	var in map[string]any
	if a := f.Arguments.ForName("input"); a != nil {
		v, err := a.Value.Value(vars)
		if err != nil {
			req.Detail += ": bad input"
			return
		}
		in, _ = v.(map[string]any)
	}
	if in == nil {
		return
	}
	switch f.Name {
	case "createPullRequest":
		req.Op = OpMRCreate
		req.Title, req.SourceBranch = str(in, "title"), str(in, "headRefName")
		if h := val(req.SourceBranch); strings.Contains(h, ":") ||
			(in["headRepositoryId"] != nil && in["headRepositoryId"] != in["repositoryId"]) {
			req.Op, req.Detail = OpWrite, "a pull request from another repository"
		}
	case "updatePullRequest":
		req.Op = OpMRUpdate
		req.Title = str(in, "title")
		req.ExistingSourceBranch = g.nodePRHead(r, val(str(in, "pullRequestId")))
	case "closePullRequest", "reopenPullRequest", "markPullRequestReadyForReview",
		"convertPullRequestToDraft", "requestReviews":
		req.Op = OpMRUpdate
		req.ExistingSourceBranch = g.nodePRHead(r, val(str(in, "pullRequestId")))
	case "addComment":
		req.Op = OpMRNote
		req.ExistingSourceBranch = g.nodePRHead(r, val(str(in, "subjectId")))
	case "addPullRequestReview", "submitPullRequestReview":
		if ev := str(in, "event"); ev != nil && strings.EqualFold(*ev, "APPROVE") {
			req.Op = OpApprove
			return
		}
		id := val(str(in, "pullRequestId"))
		if id == "" {
			id = val(str(in, "pullRequestReviewId"))
		}
		req.Op = OpMRNote
		req.ExistingSourceBranch = g.nodePRHead(r, id)
	case "addPullRequestReviewThread":
		req.Op = OpMRNote
		req.ExistingSourceBranch = g.nodePRHead(r, val(str(in, "pullRequestId")))
	case "addPullRequestReviewThreadReply":
		req.Op = OpMRNote
		req.ExistingSourceBranch = g.nodePRHead(r, val(str(in, "pullRequestReviewThreadId")))
	case "resolveReviewThread", "unresolveReviewThread":
		req.Op = OpMRNote
		req.ExistingSourceBranch = g.nodePRHead(r, val(str(in, "threadId")))
	}
}

// nodePRQuery finds the pull request behind a node: the pull request
// itself, one of its review threads, or one of its reviews.
const nodePRQuery = `query($id: ID!) { node(id: $id) {
  ... on PullRequest { headRefName isCrossRepository }
  ... on PullRequestReviewThread { pullRequest { headRefName isCrossRepository } }
  ... on PullRequestReview { pullRequest { headRefName isCrossRepository } }
} }`

func (g *GitHub) nodePRHead(r *http.Request, id string) *string {
	if id == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]any{"query": nodePRQuery, "variables": map[string]any{"id": id}})
	u := *g.API
	u.Path, u.RawPath = "/graphql", ""
	q, err := http.NewRequestWithContext(r.Context(), http.MethodPost, u.String(), bytes.NewReader(b))
	if err != nil || g.apiAuth(q.Header) != nil {
		return nil
	}
	q.Header.Set("Content-Type", "application/json")
	type head struct {
		HeadRefName       string `json:"headRefName"`
		IsCrossRepository bool   `json:"isCrossRepository"`
	}
	var resp struct {
		Data struct {
			Node *struct {
				head
				PullRequest *head `json:"pullRequest"`
			} `json:"node"`
		} `json:"data"`
	}
	if !g.do(q, &resp) || resp.Data.Node == nil {
		return nil
	}
	h := resp.Data.Node.head
	if resp.Data.Node.PullRequest != nil {
		h = *resp.Data.Node.PullRequest
	}
	switch {
	case h.HeadRefName == "":
		return nil // not a pull request (an issue, say)
	case h.IsCrossRepository:
		return strp("fork:" + h.HeadRefName)
	}
	return strp(h.HeadRefName)
}

// --- credentials and forwarding ---

func (g *GitHub) gitAuth(h http.Header) error {
	tok, err := g.Token()
	if err != nil {
		return err
	}
	h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(g.User+":"+tok)))
	return nil
}

func (g *GitHub) apiAuth(h http.Header) error {
	tok, err := g.Token()
	if err != nil {
		return err
	}
	h.Set("Authorization", "Bearer "+tok)
	return nil
}

func (g *GitHub) forward(w http.ResponseWriter, r *http.Request, up *url.URL, body io.Reader, auth func(http.Header) error, rewrite func(*http.Response) (io.Reader, error)) {
	out, err := outbound(r, up, body)
	if err != nil {
		httpError(w, http.StatusBadGateway, "bad upstream request")
		return
	}
	if err := auth(out.Header); err != nil {
		httpError(w, http.StatusServiceUnavailable, "no credential: run investigator login github")
		return
	}
	resp, err := g.Client.Do(out)
	if err != nil {
		httpError(w, http.StatusBadGateway, "upstream: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if up == g.API && resp.StatusCode/100 == 3 && r.Method == http.MethodGet && githubLogsRE.MatchString(r.URL.Path) {
		g.followLogs(w, r, resp)
		return
	}
	if up == g.API {
		g.rewriteAPILinks(resp.Header)
	}
	var rb io.Reader
	if rewrite != nil && resp.StatusCode == http.StatusOK {
		if rb, err = rewrite(resp); err != nil {
			httpError(w, http.StatusBadGateway, "upstream response: "+err.Error())
			return
		}
	}
	copyResponse(w, resp, up, "https", g.PublicHost, rb)
}

// rewriteAPILinks points Location and Link (pagination) headers on
// api.github.com back at the broker's /api/v3, where gh follows them.
func (g *GitHub) rewriteAPILinks(h http.Header) {
	from := g.API.Scheme + "://" + g.API.Host
	to := "https://" + g.PublicHost + githubAPIPrefix
	if loc := h.Get("Location"); strings.HasPrefix(loc, from+"/") {
		h.Set("Location", to+strings.TrimPrefix(loc, from))
	}
	if l := h.Get("Link"); l != "" {
		h.Set("Link", strings.ReplaceAll(l, "<"+from+"/", "<"+to+"/"))
	}
}

// followLogs fetches the signed log URL a log endpoint redirected to,
// without the user's token, and streams it back.
func (g *GitHub) followLogs(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Scheme != "https" {
		httpError(w, http.StatusBadGateway, "bad log redirect")
		return
	}
	q, err := http.NewRequestWithContext(r.Context(), http.MethodGet, loc.String(), nil)
	if err != nil {
		httpError(w, http.StatusBadGateway, "bad log redirect")
		return
	}
	lr, err := g.Client.Do(q)
	if err != nil {
		httpError(w, http.StatusBadGateway, "logs: "+err.Error())
		return
	}
	defer lr.Body.Close()
	for _, k := range []string{"Content-Type", "Content-Disposition"} {
		if v := lr.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(lr.StatusCode)
	_, _ = io.Copy(flushWriter{w}, lr.Body)
}
