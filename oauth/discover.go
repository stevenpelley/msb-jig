// Package oauth is the harness's OAuth client for hosted MCP servers
// (MCP's authorization spec, 2025-06-18): it finds the server's
// authorization server (RFC 9728, RFC 8414), registers itself as a public
// client (RFC 7591), signs the user in with the authorization code flow,
// PKCE S256 and a loopback redirect (RFC 8252) in the Mac's browser, and
// refreshes the access token from a stored refresh token (Source). It runs
// on the Mac only: login signs in, the broker refreshes. Nothing here
// enters the sandbox.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Metadata is what the client needs about a server and its
// authorization server.
type Metadata struct {
	Resource              string   `json:"resource"` // the MCP server's resource identifier, sent as resource
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorizationEndpoint"`
	TokenEndpoint         string   `json:"tokenEndpoint"`
	RegistrationEndpoint  string   `json:"registrationEndpoint"`
	Scopes                []string `json:"scopes,omitempty"` // the server's scopes_supported, for messages
}

const maxMetadata = 1 << 20

var resourceMetadataRE = regexp.MustCompile(`resource_metadata="?([^",\s]+)"?`)

// Discover finds an MCP server's authorization server. It calls the
// server without a token, expecting a 401 whose WWW-Authenticate names
// the protected-resource metadata (else the well-known URL at the
// server's host), reads that, then its first authorization server's
// metadata. Every URL must be https, and the authorization server must
// offer PKCE S256 and dynamic registration.
func Discover(ctx context.Context, c *http.Client, serverURL string) (Metadata, error) {
	var md Metadata
	su, err := httpsURL(serverURL)
	if err != nil {
		return md, err
	}
	prmURL := ""
	req, _ := http.NewRequestWithContext(ctx, "POST", serverURL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
		if m := resourceMetadataRE.FindStringSubmatch(resp.Header.Get("WWW-Authenticate")); resp.StatusCode == http.StatusUnauthorized && m != nil {
			prmURL = m[1]
		}
	} else {
		return md, err
	}
	if prmURL == "" {
		prmURL = su.Scheme + "://" + su.Host + "/.well-known/oauth-protected-resource" + strings.TrimSuffix(su.EscapedPath(), "/")
	}
	if _, err := httpsURL(prmURL); err != nil {
		return md, fmt.Errorf("resource metadata: %w", err)
	}
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	if err := getJSON(ctx, c, prmURL, &prm); err != nil {
		return md, fmt.Errorf("resource metadata %s: %w", prmURL, err)
	}
	if len(prm.AuthorizationServers) == 0 {
		return md, fmt.Errorf("resource metadata %s names no authorization server", prmURL)
	}
	md.Resource, md.Scopes = prm.Resource, prm.ScopesSupported
	if md.Resource == "" {
		md.Resource = serverURL
	}
	// RFC 9728 3.3: the metadata is about the server asked, or a client
	// could be steered into getting a token for another resource.
	if ru, err := httpsURL(md.Resource); err != nil || !strings.EqualFold(ru.Host, su.Host) {
		return md, fmt.Errorf("resource metadata %s: the resource %q isn't the server's", prmURL, md.Resource)
	}
	issuer := prm.AuthorizationServers[0]
	iu, err := httpsURL(issuer)
	if err != nil {
		return md, fmt.Errorf("authorization server: %w", err)
	}
	var as struct {
		Issuer                 string   `json:"issuer"`
		AuthorizationEndpoint  string   `json:"authorization_endpoint"`
		TokenEndpoint          string   `json:"token_endpoint"`
		RegistrationEndpoint   string   `json:"registration_endpoint"`
		CodeChallengeMethods   []string `json:"code_challenge_methods_supported"`
		ResponseTypesSupported []string `json:"response_types_supported"`
	}
	path := strings.TrimSuffix(iu.EscapedPath(), "/")
	var errs []error
	for _, wk := range []string{"oauth-authorization-server", "openid-configuration"} {
		u := iu.Scheme + "://" + iu.Host + "/.well-known/" + wk + path
		if err := getJSON(ctx, c, u, &as); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", u, err))
			continue
		}
		errs = nil
		break
	}
	if errs != nil {
		return md, fmt.Errorf("authorization server metadata: %w", errors.Join(errs...))
	}
	if strings.TrimSuffix(as.Issuer, "/") != strings.TrimSuffix(issuer, "/") {
		return md, fmt.Errorf("authorization server metadata: issuer %q, want %q", as.Issuer, issuer)
	}
	if !slices.Contains(as.CodeChallengeMethods, "S256") {
		return md, fmt.Errorf("the authorization server doesn't offer PKCE S256")
	}
	if as.RegistrationEndpoint == "" {
		return md, fmt.Errorf("the authorization server doesn't offer dynamic client registration")
	}
	for _, e := range []string{as.AuthorizationEndpoint, as.TokenEndpoint, as.RegistrationEndpoint} {
		if _, err := httpsURL(e); err != nil {
			return md, fmt.Errorf("authorization server metadata: %w", err)
		}
	}
	md.Issuer, md.AuthorizationEndpoint, md.TokenEndpoint, md.RegistrationEndpoint =
		as.Issuer, as.AuthorizationEndpoint, as.TokenEndpoint, as.RegistrationEndpoint
	return md, nil
}

func httpsURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("%q isn't an https URL", s)
	}
	return u, nil
}

func getJSON(ctx context.Context, c *http.Client, u string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxMetadata)).Decode(v)
}
