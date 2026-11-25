package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is a registered public client: the authorization server knows
// it by ID, with one redirect URI on the Mac's loopback.
type Client struct {
	ID          string `json:"clientId"`
	RedirectURI string `json:"redirectUri"` // http://127.0.0.1:<port>/callback
}

// Port is the redirect URI's port.
func (cl Client) Port() int {
	u, err := url.Parse(cl.RedirectURI)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(u.Port())
	return p
}

// RedirectURI is the loopback redirect URI for port.
func RedirectURI(port int) string { return "http://127.0.0.1:" + strconv.Itoa(port) + "/callback" }

// Register registers a public client (no secret: token_endpoint_auth_method
// none) for the authorization code and refresh token grants, with
// redirectURI. A server that answers with another auth method or a
// secret is refused: the client keeps no secret.
func Register(ctx context.Context, c *http.Client, md Metadata, name, redirectURI string, scopes []string) (Client, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                name,
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      strings.Join(scopes, " "),
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", md.RegistrationEndpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return Client{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxMetadata))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return Client{}, fmt.Errorf("registration: %s: %s", resp.Status, oauthError(b))
	}
	var r struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		AuthMethod   string `json:"token_endpoint_auth_method"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.ClientID == "" {
		return Client{}, fmt.Errorf("registration: no client_id in the answer")
	}
	if r.ClientSecret != "" || (r.AuthMethod != "" && r.AuthMethod != "none") {
		return Client{}, fmt.Errorf("registration: the server made a confidential client (%s); only public clients are supported", r.AuthMethod)
	}
	return Client{ID: r.ClientID, RedirectURI: redirectURI}, nil
}

// Tokens are a token endpoint's answer.
type Tokens struct {
	Access  string
	Refresh string // "" if the server didn't send one (it keeps the old)
	Expires time.Time
	Scope   string
}

// Listen opens the loopback listener for a redirect: on port, or any
// free port if 0.
func Listen(port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// Authorize signs the user in: it builds the authorization URL (PKCE
// S256, a random state, the scopes and the resource), passes it to open
// (the browser), waits on l for the redirect to the client's URI, and
// exchanges the code. l must listen on the redirect URI's port. It
// stops at ctx's end.
func Authorize(ctx context.Context, c *http.Client, md Metadata, cl Client, scopes []string, l net.Listener, open func(authURL string)) (Tokens, error) {
	verifier := random(48)
	sum := sha256.Sum256([]byte(verifier))
	state := random(24)
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {cl.ID},
		"redirect_uri":          {cl.RedirectURI},
		"scope":                 {strings.Join(scopes, " ")},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"resource":              {md.Resource},
	}
	au, err := url.Parse(md.AuthorizationEndpoint)
	if err != nil {
		return Tokens{}, err
	}
	aq := au.Query()
	for k, v := range q {
		aq[k] = v
	}
	au.RawQuery = aq.Encode()

	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	ru, _ := url.Parse(cl.RedirectURI)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ru.Path {
			http.NotFound(w, r)
			return
		}
		p := r.URL.Query()
		var res result
		switch {
		case subtle.ConstantTimeCompare([]byte(p.Get("state")), []byte(state)) != 1:
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return // not ours: keep waiting
		case p.Get("error") != "":
			res.err = fmt.Errorf("the authorization server said %s: %s", p.Get("error"), p.Get("error_description"))
		case p.Get("code") == "":
			res.err = errors.New("the redirect had no code")
		default:
			res.code = p.Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		msg := "Signed in. You can close this tab."
		if res.err != nil {
			msg = "Sign-in failed: " + res.err.Error()
		}
		fmt.Fprintf(w, "<!doctype html><title>mono-jig</title><p>%s</p>", html.EscapeString(msg))
		select {
		case done <- res:
		default:
		}
	})}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()

	open(au.String())
	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		return Tokens{}, fmt.Errorf("no sign-in: %w", ctx.Err())
	}
	if res.err != nil {
		return Tokens{}, res.err
	}
	return tokenRequest(ctx, c, md.TokenEndpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {res.code},
		"redirect_uri":  {cl.RedirectURI},
		"client_id":     {cl.ID},
		"code_verifier": {verifier},
		"resource":      {md.Resource},
	})
}

// Refresh trades a refresh token for new tokens.
func Refresh(ctx context.Context, c *http.Client, tokenEndpoint, clientID, resource, refresh string) (Tokens, error) {
	return tokenRequest(ctx, c, tokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {clientID},
		"resource":      {resource},
	})
}

// ErrInvalidGrant is a token endpoint's invalid_grant: the refresh token
// (or code) is spent, revoked or expired; the user signs in again.
var ErrInvalidGrant = errors.New("invalid_grant")

func tokenRequest(ctx context.Context, c *http.Client, endpoint string, form url.Values) (Tokens, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return Tokens{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxMetadata))
	var r struct {
		AccessToken  string      `json:"access_token"`
		TokenType    string      `json:"token_type"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"`
		Scope        string      `json:"scope"`
		Error        string      `json:"error"`
		Description  string      `json:"error_description"`
	}
	_ = json.Unmarshal(b, &r)
	if resp.StatusCode != http.StatusOK || r.AccessToken == "" {
		if r.Error == "invalid_grant" {
			return Tokens{}, fmt.Errorf("token endpoint: %w: %s", ErrInvalidGrant, r.Description)
		}
		return Tokens{}, fmt.Errorf("token endpoint: %s: %s", resp.Status, oauthError(b))
	}
	if !strings.EqualFold(r.TokenType, "bearer") {
		return Tokens{}, fmt.Errorf("token endpoint: token type %q, want Bearer", r.TokenType)
	}
	t := Tokens{Access: r.AccessToken, Refresh: r.RefreshToken, Scope: r.Scope, Expires: time.Now().Add(time.Hour)}
	if n, err := r.ExpiresIn.Int64(); err == nil && n > 0 {
		t.Expires = time.Now().Add(time.Duration(n) * time.Second)
	}
	return t, nil
}

// oauthError is an error answer's error and description, or a short
// prefix of the body.
func oauthError(b []byte) string {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return strings.TrimSpace(e.Error + " " + e.Description)
	}
	s := string(b)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
