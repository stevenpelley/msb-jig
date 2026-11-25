package jig

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/stevenpelley/msb-jig/broker/providers"
	"github.com/stevenpelley/msb-jig/oauth"
)

// OAuthMCP is a hosted MCP server the user signs in to with OAuth, the
// harness being the OAuth client (package oauth): login registers a
// public client, signs in in the Mac's browser and keeps the refresh
// token in the Keychain; the broker refreshes the access token itself.
// For servers no vendor CLI holds a token for. A track makes one per
// server and adds its CLI() to Track.Providers.
type OAuthMCP struct {
	ID         string   // its provider and login name (providers.MCPServer.ID)
	Title      string   // its name in messages and the agent's instructions
	URL        string   // the MCP endpoint
	Scopes     []string // the scopes asked for: read scopes, and offline_access for a refresh token
	Tools      []string // the tools the agent may call (read-only ones)
	ClientName string   // the client's name as registered; default "mono-jig"
}

// oauthSettings are settings.json's entry for an OAuthMCP: the
// registered client and where to refresh. Nothing secret.
type oauthSettings struct {
	Issuer        string   `json:"issuer"`
	TokenEndpoint string   `json:"tokenEndpoint"`
	Resource      string   `json:"resource"`
	ClientID      string   `json:"clientId"`
	RedirectURI   string   `json:"redirectUri"`
	Scopes        []string `json:"scopes"`
}

func (o OAuthMCP) keychain() string { return "mono-jig-" + o.ID }

func (o OAuthMCP) title() string { return or(o.Title, o.ID) }

// envName is the override for tests: JIG_<ID>_TOKEN_COMMAND, a token
// command used instead of the OAuth client (empty: off).
func (o OAuthMCP) envName() string {
	return "JIG_" + strings.ToUpper(strings.ReplaceAll(o.ID, "-", "_")) + "_TOKEN_COMMAND"
}

// CLI is its ProviderCLI: login <ID> signs in.
func (o OAuthMCP) CLI() ProviderCLI {
	return ProviderCLI{
		Name:     o.ID,
		Keychain: o.keychain(),
		Usage:    fmt.Sprintf("%-24ssign in to %s's MCP server in the browser (the harness is the OAuth client)\n", "login "+o.ID, o.title()),
		Login:    o.login,
		Status: func(s Settings) string {
			st := ProviderSettings[oauthSettings](s, o.ID)
			if st == nil {
				return ""
			}
			msg := fmt.Sprintf("signed in (client %s at %s, scopes %s); %d tools", st.ClientID, st.Issuer, strings.Join(st.Scopes, " "), len(o.Tools))
			if !KeychainHas(o.keychain()) {
				msg += "; no refresh token in the Keychain: run login " + o.ID + " again"
			}
			return msg
		},
		Forgotten: func(s Settings) string {
			msg := "refresh token removed from the Keychain"
			if st := ProviderSettings[oauthSettings](s, o.ID); st != nil {
				msg += "; the client " + st.ClientID + " stays registered at " + st.Issuer
			}
			return msg
		},
		Broker: o.broker,
	}
}

func (o OAuthMCP) provider() providers.MCPServer {
	return providers.MCPServer{ID: o.ID, Title: o.title(), URL: o.URL, Tools: o.Tools}
}

// broker: on with the settings and the refresh token. Its token is an
// oauth.Source, refreshed in this process.
func (o OAuthMCP) broker(s Settings) providers.Provider {
	p := o.provider()
	if cmd, set := os.LookupEnv(o.envName()); set {
		if cmd == "" {
			return nil
		}
		p.TokenCmd = cmd
		return p
	}
	st := ProviderSettings[oauthSettings](s, o.ID)
	if st == nil || !KeychainHas(o.keychain()) {
		return nil
	}
	src := &oauth.Source{
		TokenEndpoint: st.TokenEndpoint, ClientID: st.ClientID, Resource: st.Resource,
		Store:  keychainStore(o.keychain()),
		Lock:   func() (func(), error) { return lockFile(filepath.Join(stateRoot(), o.ID+".oauth.lock")) },
		Client: oauthClient(),
		Login:  "mono-jig login " + o.ID,
	}
	p.Token = src.Token
	return p
}

// keychainStore is an oauth.Store in a Keychain item.
type keychainStore string

func (k keychainStore) Load() (string, error) { return keychainRead(string(k)) }
func (k keychainStore) Save(t string) error   { return keychainUpdate(string(k), t) }

// lockFile takes an exclusive flock on path, waiting for it.
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// oauthClient reaches the authorization server from the Mac: its proxy
// settings, no redirects followed (an endpoint that redirects is
// refused rather than followed with a token).
func oauthClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = http.ProxyFromEnvironment
	return &http.Client{Transport: t, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (o OAuthMCP) login(args []string) {
	if len(args) != 0 {
		usage()
	}
	ctx := context.Background()
	c := oauthClient()
	md, err := oauth.Discover(ctx, c, o.URL)
	if err != nil {
		fail("%s: finding the authorization server: %v", o.ID, err)
	}
	for _, sc := range o.Scopes {
		if len(md.Scopes) > 0 && !slices.Contains(md.Scopes, sc) {
			fmt.Printf("%s: note: the server doesn't list the scope %s (it lists %s)\n", o.ID, sc, strings.Join(md.Scopes, " "))
		}
	}
	s := LoadSettings()
	cl, l := o.client(ctx, c, md, ProviderSettings[oauthSettings](s, o.ID))
	fmt.Printf("%s: signing in at %s, scopes %s; a browser opens (or open the URL below)\n", o.ID, md.Issuer, strings.Join(o.Scopes, " "))
	sctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tok, err := oauth.Authorize(sctx, c, md, cl, o.Scopes, l, func(u string) {
		fmt.Println(u)
		_ = exec.Command("open", u).Start()
	})
	if err != nil {
		fail("%s: sign-in: %v", o.ID, err)
	}
	if tok.Refresh == "" {
		fail("%s: the server sent no refresh token (is offline_access among the scopes?): the broker couldn't keep the sign-in", o.ID)
	}
	KeychainDelete(o.keychain())
	if err := keychainUpdate(o.keychain(), tok.Refresh); err != nil {
		fail("%v", err)
	}
	s.SetProvider(o.ID, oauthSettings{Issuer: md.Issuer, TokenEndpoint: md.TokenEndpoint, Resource: md.Resource,
		ClientID: cl.ID, RedirectURI: cl.RedirectURI, Scopes: o.Scopes})
	SaveSettings(s)
	fmt.Printf("%s: signed in, scopes %q; the refresh token is in the Keychain as %s\n", o.ID, tok.Scope, o.keychain())
	o.showTools(ctx, tok.Access)
}

// client is the registered client, reused while its issuer and redirect
// port are; else a new registration, on a free port.
func (o OAuthMCP) client(ctx context.Context, c *http.Client, md oauth.Metadata, st *oauthSettings) (oauth.Client, net.Listener) {
	if st != nil && st.Issuer == md.Issuer && st.ClientID != "" {
		cl := oauth.Client{ID: st.ClientID, RedirectURI: st.RedirectURI}
		if l, err := oauth.Listen(cl.Port()); err == nil && cl.Port() != 0 {
			return cl, l
		}
		fmt.Printf("%s: the registered client's port %d is busy: registering a new client\n", o.ID, cl.Port())
	}
	l, err := oauth.Listen(0)
	if err != nil {
		fail("%v", err)
	}
	name := or(o.ClientName, "mono-jig")
	cl, err := oauth.Register(ctx, c, md, name, oauth.RedirectURI(l.Addr().(*net.TCPAddr).Port), o.Scopes)
	if err != nil {
		l.Close()
		fail("%s: %v", o.ID, err)
	}
	fmt.Printf("%s: registered the client %q (%s) at %s\n", o.ID, name, cl.ID, md.Issuer)
	return cl, l
}

// showTools lists the server's tools, and which the broker serves, so
// the track's list can be checked against what the server offers.
func (o OAuthMCP) showTools(ctx context.Context, access string) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	tools, err := listMCPTools(ctx, oauthClient(), o.URL, access)
	if err != nil {
		fmt.Printf("%s: listing the server's tools failed: %v\n", o.ID, err)
		return
	}
	fmt.Printf("%s: the server offers %d tools (served: the broker's list; read-only: the server's hint):\n", o.ID, len(tools))
	for _, t := range tools {
		served, hint := "", ""
		if slices.Contains(o.Tools, t.Name) {
			served = "served"
		}
		if t.Annotations.ReadOnly != nil && *t.Annotations.ReadOnly {
			hint = "read-only"
		}
		fmt.Printf("  %-50s %-7s %s\n", t.Name, served, hint)
	}
	for _, name := range o.Tools {
		if !slices.ContainsFunc(tools, func(t mcpTool) bool { return t.Name == name }) {
			fmt.Printf("  %-50s on the broker's list, not offered\n", name)
		}
	}
}
