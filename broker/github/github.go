// Package github is the broker's GitHub provider, served as
// github.broker.internal: git smart HTTP (to github.com), and the API the
// way gh calls a GitHub Enterprise Server, REST under /api/v3/ and GraphQL
// at /api/graphql (to api.github.com). LFS isn't served. routes.go lists
// every request it serves; graphql.go every mutation.
package github

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"mcp-restrict/broker"
)

// Config configures the provider.
type Config struct {
	User   string                 // the GitHub user the agent acts for
	Token  func() (string, error) // the user's token (gh's login, or the Keychain)
	Git    *url.URL               // https://github.com
	API    *url.URL               // https://api.github.com
	Public *url.URL               // https://github.broker.internal
	Client *http.Client           // broker.NewUpstreamClient()
	Log    *slog.Logger
}

type github struct {
	Config
	git, api, graphql *broker.Upstream
}

const noCredential = "no GitHub credential: run investigator login github"

// New returns the provider.
func New(c Config) *broker.Provider {
	g := &github{Config: c}
	// git: HTTP Basic, user and token, as git sends it.
	g.git = &broker.Upstream{URL: c.Git, Public: c.Public, Client: c.Client, NoCredential: noCredential,
		Auth: func(h http.Header) error {
			tok, err := c.Token()
			if err != nil {
				return err
			}
			h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.User+":"+tok)))
			return nil
		}}
	// REST: /api/v3/<path> is api.github.com/<path>, with a Bearer token.
	g.api = &broker.Upstream{URL: c.API, Strip: "/api/v3", Public: c.Public, Client: c.Client,
		NoCredential: noCredential, Auth: g.bearer}
	// GraphQL: /api/graphql is api.github.com/graphql.
	g.graphql = &broker.Upstream{URL: c.API, Path: "/graphql", Public: c.Public, Client: c.Client,
		NoCredential: noCredential, Auth: g.bearer}
	return &broker.Provider{Name: "github", Routes: g.routes(), Log: c.Log}
}

func (g *github) bearer(h http.Header) error {
	tok, err := g.Token()
	if err != nil {
		return err
	}
	h.Set("Authorization", "Bearer "+tok)
	return nil
}

// --- lookups: what GitHub says, never what the request claims ---

// prHead fetches a pull request's head branch by number. A fork's comes
// back as "fork:<branch>", which never passes the policy. nil if the
// lookup failed or the number isn't a pull request.
func (g *github) prHead(c *broker.Call) *string {
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
	p := "/repos/" + url.PathEscape(c.Var("owner")) + "/" + url.PathEscape(c.Var("repo")) + "/pulls/" + url.PathEscape(c.Var("number"))
	if !g.get(c, p, &pr) || pr.Head.Ref == "" {
		return nil
	}
	if pr.Head.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, pr.Base.Repo.FullName) {
		return broker.Ptr("fork:" + pr.Head.Ref)
	}
	return broker.Ptr(pr.Head.Ref)
}

// nodePRQuery finds the pull request behind a node: the pull request
// itself, one of its review threads, or one of its reviews.
const nodePRQuery = `query($id: ID!) { node(id: $id) {
  ... on PullRequest { headRefName isCrossRepository }
  ... on PullRequestReviewThread { pullRequest { headRefName isCrossRepository } }
  ... on PullRequestReview { pullRequest { headRefName isCrossRepository } }
} }`

// nodePRHead is prHead for a GraphQL node ID.
func (g *github) nodePRHead(c *broker.Call, id string) *string {
	if id == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]any{"query": nodePRQuery, "variables": map[string]any{"id": id}})
	u := *g.API
	u.Path, u.RawPath = "/graphql", ""
	q, err := http.NewRequestWithContext(c.R.Context(), http.MethodPost, u.String(), bytes.NewReader(b))
	if err != nil || g.bearer(q.Header) != nil {
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
		return broker.Ptr("fork:" + h.HeadRefName)
	}
	return broker.Ptr(h.HeadRefName)
}

// get calls the REST API with the user's token and decodes a 200 into v.
func (g *github) get(c *broker.Call, escPath string, v any) bool {
	u := *g.API
	p, err := url.PathUnescape(escPath)
	if err != nil {
		return false
	}
	u.Path, u.RawPath = p, escPath
	q, err := http.NewRequestWithContext(c.R.Context(), http.MethodGet, u.String(), nil)
	if err != nil || g.bearer(q.Header) != nil {
		return false
	}
	return g.do(q, v)
}

func (g *github) do(q *http.Request, v any) bool {
	q.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.Client.Do(q)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, broker.MaxBody)).Decode(v) == nil
}

// --- the log endpoints ---

// logs serves a run or job log. GitHub answers with a redirect to signed
// blob storage; the broker follows it itself, without the user's token,
// so the sandbox needs no route to that storage (which would be a route
// to anyone's).
type logs struct{ g *github }

func (l logs) Serve(w http.ResponseWriter, c *broker.Call) {
	resp, err := l.g.api.Do(c)
	if err != nil {
		broker.HTTPError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 3 {
		l.g.api.CopyResponse(w, resp, resp.Body)
		return
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Scheme != "https" {
		broker.HTTPError(w, http.StatusBadGateway, "bad log redirect")
		return
	}
	q, err := http.NewRequestWithContext(c.R.Context(), http.MethodGet, loc.String(), nil)
	if err != nil {
		broker.HTTPError(w, http.StatusBadGateway, "bad log redirect")
		return
	}
	lr, err := l.g.Client.Do(q)
	if err != nil {
		broker.HTTPError(w, http.StatusBadGateway, "logs: "+err.Error())
		return
	}
	defer lr.Body.Close()
	for _, k := range []string{"Content-Type", "Content-Disposition"} {
		if v := lr.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(lr.StatusCode)
	_, _ = io.Copy(broker.FlushWriter{W: w}, lr.Body)
}
