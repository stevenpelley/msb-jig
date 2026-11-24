// Package github is the broker's GitHub provider: git smart HTTP (to
// github.com) and the API, REST and GraphQL (to api.github.com). LFS isn't
// served. In server mode the sandbox calls it as github.broker.internal,
// the way gh calls a GitHub Enterprise Server; in proxy mode it calls
// github.com and api.github.com. Its route tables, one set per mode, are
// in routes.go; here are their checks, and graphql.go has every mutation.
package github

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/stevenpelley/msb-jig/broker"
)

// Checks are the GitHub provider's checks, which the routes (routes.go)
// run. API is the REST upstream (api.github.com): lookups go
// through it, with the user's credential.
type Checks struct {
	User string // the GitHub user the agent acts for
	API  *broker.Upstream
}

// --- lookups: what GitHub says, never what the request claims ---

// prHead fetches a pull request's head branch by number. A fork's comes
// back as "fork:<branch>", which never passes the policy. nil if the
// lookup failed or the number isn't a pull request.
func (g *Checks) prHead(c *broker.Call) *string {
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
func (g *Checks) nodePRHead(c *broker.Call, id string) *string {
	if id == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]any{"query": nodePRQuery, "variables": map[string]any{"id": id}})
	u := *g.API.URL
	u.Path, u.RawPath = "/graphql", ""
	q, err := http.NewRequestWithContext(c.R.Context(), http.MethodPost, u.String(), bytes.NewReader(b))
	if err != nil || g.API.Auth(q.Header) != nil {
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
func (g *Checks) get(c *broker.Call, escPath string, v any) bool {
	u := *g.API.URL
	p, err := url.PathUnescape(escPath)
	if err != nil {
		return false
	}
	u.Path, u.RawPath = p, escPath
	q, err := http.NewRequestWithContext(c.R.Context(), http.MethodGet, u.String(), nil)
	if err != nil || g.API.Auth(q.Header) != nil {
		return false
	}
	return g.do(q, v)
}

func (g *Checks) do(q *http.Request, v any) bool {
	q.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.API.Client.Do(q)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, broker.MaxBody)).Decode(v) == nil
}

// --- the log endpoints ---

// Logs serves a run or job log. GitHub answers with a redirect to signed
// blob storage; the broker follows it itself, without the user's token,
// so the sandbox needs no route to that storage (which would be a route
// to anyone's).
type Logs struct{ API *broker.Upstream }

func (l Logs) Serve(w http.ResponseWriter, c *broker.Call) {
	resp, err := l.API.Do(c)
	if err != nil {
		broker.HTTPError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 3 {
		l.API.CopyResponse(w, resp, resp.Body)
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
	lr, err := l.API.Client.Do(q)
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
