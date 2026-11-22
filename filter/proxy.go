package filter

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Proxy is the explicit HTTP proxy the sandbox's HTTPS_PROXY points at. It
// takes CONNECT (any TLS, never intercepted) and absolute-form plain HTTP,
// to hosts on the ACL and ports 80 and 443 only.
//
// It dials one of two ways:
//   - direct: it resolves the name itself, drops every address that isn't
//     public (the Mac's own services listen on those), and dials the address
//     it checked, so a DNS answer can't change between check and dial;
//   - through Upstream, a proxy of the Mac's (a corporate proxy, or sbx's
//     in development): it sends CONNECT for the name and lets the upstream
//     resolve, since the Mac may not resolve external names at all.
type Proxy struct {
	ACL *ACL
	// Upstream, if set, is an http:// proxy URL (with optional user:password)
	// every connection goes through.
	Upstream *url.URL
	// Resolve, AddrOK and Dial default to the system resolver, PublicAddr
	// and a net.Dialer. Tests replace them.
	Resolve     func(ctx context.Context, host string) ([]netip.Addr, error)
	AddrOK      func(netip.Addr) bool
	Dial        func(ctx context.Context, network, addr string) (net.Conn, error)
	DialTimeout time.Duration
	Log         *slog.Logger
	// OnDeny, if set, receives every denial (for `investigator denied`).
	OnDeny func(Denial)

	once      sync.Once
	transport *http.Transport
}

// Denial is one refused request.
type Denial struct {
	Time   time.Time `json:"time"`
	Client string    `json:"client"`
	Method string    `json:"method"`
	Host   string    `json:"host"`
	Port   int       `json:"port"`
	Reason string    `json:"reason"`
}

var errNotPublic = errors.New("resolves only to non-public addresses")

func (p *Proxy) init() {
	p.once.Do(func() {
		if p.Log == nil {
			p.Log = slog.Default()
		}
		if p.DialTimeout == 0 {
			p.DialTimeout = 15 * time.Second
		}
		if p.AddrOK == nil {
			p.AddrOK = PublicAddr
		}
		if p.Resolve == nil {
			p.Resolve = func(ctx context.Context, h string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
			}
		}
		if p.Dial == nil {
			d := &net.Dialer{KeepAlive: 30 * time.Second}
			p.Dial = d.DialContext
		}
		t := &http.Transport{
			// Pass the client's encoding through untouched.
			DisableCompression:    true,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       60 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		}
		if p.Upstream != nil {
			t.Proxy = http.ProxyURL(p.Upstream)
			t.DialContext = p.Dial
		} else {
			t.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
				h, ps, _ := net.SplitHostPort(addr)
				port, _ := strconv.Atoi(ps)
				return p.dialDirect(ctx, h, port)
			}
		}
		p.transport = t
	})
}

// hopHeaders aren't forwarded on plain HTTP requests.
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.init()
	if p.ACL == nil {
		http.Error(w, "investigator filter: no ACL configured", http.StatusInternalServerError)
		return
	}
	var host string
	var port int
	switch {
	case r.Method == http.MethodConnect:
		h, ps, err := net.SplitHostPort(r.Host)
		if err != nil {
			p.deny(w, r, r.Host, 0, http.StatusBadRequest, "CONNECT needs host:port")
			return
		}
		host = h
		port, _ = strconv.Atoi(ps)
	case r.URL.IsAbs():
		if r.URL.Scheme != "http" {
			p.deny(w, r, r.URL.Host, 0, http.StatusBadRequest, "only http:// URLs can be proxied without CONNECT")
			return
		}
		host, port = r.URL.Hostname(), 80
		if ps := r.URL.Port(); ps != "" {
			port, _ = strconv.Atoi(ps)
		}
	default:
		http.Error(w, "investigator filter: this is a proxy; set HTTPS_PROXY and HTTP_PROXY to it", http.StatusBadRequest)
		return
	}
	host = NormalizeHost(host)
	if r.Header.Get("X-Upstream-Https-Proxy") != "" {
		p.deny(w, r, host, port, http.StatusForbidden, "a client-chosen upstream proxy isn't allowed")
		return
	}
	if !AllowedPorts[port] {
		p.deny(w, r, host, port, http.StatusForbidden, "only ports 80 and 443 are allowed")
		return
	}
	if !ValidHostname(host) {
		p.deny(w, r, host, port, http.StatusForbidden, "only host names are allowed, not addresses")
		return
	}
	e, ok := p.ACL.Allows(host, port)
	if !ok {
		p.deny(w, r, host, port, http.StatusForbidden,
			fmt.Sprintf("%s:%d is not on the allowlist (investigator allow %s)", host, port, host))
		return
	}
	p.Log.Info("filter allowed", "client", r.RemoteAddr, "method", r.Method, "host", host, "port", port, "rule", e.String(), "layer", e.Layer, "group", e.Group)
	if r.Method == http.MethodConnect {
		p.connect(w, r, host, port)
	} else {
		p.forward(w, r)
	}
}

func (p *Proxy) deny(w http.ResponseWriter, r *http.Request, host string, port, code int, reason string) {
	d := Denial{Time: time.Now().UTC(), Client: r.RemoteAddr, Method: r.Method, Host: host, Port: port, Reason: reason}
	p.Log.Warn("filter denied", "client", d.Client, "method", d.Method, "host", host, "port", port, "reason", reason)
	if p.OnDeny != nil {
		p.OnDeny(d)
	}
	http.Error(w, "investigator filter: "+reason, code)
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request, host string, port int) {
	ctx, cancel := context.WithTimeout(r.Context(), p.DialTimeout)
	up, err := p.dial(ctx, host, port)
	cancel()
	if err != nil {
		p.dialFailed(w, r, host, port, err)
		return
	}
	defer up.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "investigator filter: can't hijack", http.StatusInternalServerError)
		return
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer c.Close()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	splice(c, brw.Reader, up)
}

// splice copies both ways until both sides are done, passing half-closes on.
func splice(client net.Conn, clientR io.Reader, up net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst net.Conn, src io.Reader) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go cp(up, clientR)
	go cp(client, up)
	<-done
	<-done
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request) {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	if c := r.Header.Get("Connection"); c != "" {
		for _, h := range strings.Split(c, ",") {
			out.Header.Del(strings.TrimSpace(h))
		}
	}
	if r.ContentLength == 0 {
		out.Body = nil
	}
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		h, ps, _ := net.SplitHostPort(canonicalAddr(r.URL))
		port, _ := strconv.Atoi(ps)
		p.dialFailed(w, r, h, port, err)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	for _, h := range hopHeaders {
		w.Header().Del(h)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func canonicalAddr(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	return net.JoinHostPort(u.Hostname(), "80")
}

func (p *Proxy) dialFailed(w http.ResponseWriter, r *http.Request, host string, port int, err error) {
	var ue *upstreamError
	switch {
	case errors.Is(err, errNotPublic):
		p.deny(w, r, host, port, http.StatusForbidden, fmt.Sprintf("%s %v", host, errNotPublic))
	case errors.As(err, &ue) && ue.status == http.StatusForbidden:
		p.deny(w, r, host, port, http.StatusForbidden, "the upstream proxy refused "+host)
	default:
		p.Log.Warn("filter dial failed", "host", host, "port", port, "err", err)
		http.Error(w, "investigator filter: can't reach "+host+": "+err.Error(), http.StatusBadGateway)
	}
}

func (p *Proxy) dial(ctx context.Context, host string, port int) (net.Conn, error) {
	if p.Upstream != nil {
		return p.dialUpstream(ctx, host, port)
	}
	return p.dialDirect(ctx, host, port)
}

func (p *Proxy) dialDirect(ctx context.Context, host string, port int) (net.Conn, error) {
	addrs, err := p.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var ok []netip.Addr
	for _, a := range addrs {
		if p.AddrOK(a.Unmap()) {
			ok = append(ok, a.Unmap())
		}
	}
	if len(ok) == 0 {
		return nil, errNotPublic
	}
	var last error
	for _, a := range ok {
		c, err := p.Dial(ctx, "tcp", netip.AddrPortFrom(a, uint16(port)).String())
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

type upstreamError struct{ status int }

func (e *upstreamError) Error() string { return fmt.Sprintf("upstream proxy answered %d", e.status) }

func (p *Proxy) dialUpstream(ctx context.Context, host string, port int) (net.Conn, error) {
	c, err := p.Dial(ctx, "tcp", canonicalAddr(p.Upstream))
	if err != nil {
		return nil, fmt.Errorf("upstream proxy: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	target := net.JoinHostPort(host, strconv.Itoa(port))
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if u := p.Upstream.User; u != nil {
		pw, _ := u.Password()
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+pw)) + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("upstream proxy: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		c.Close()
		return nil, &upstreamError{resp.StatusCode}
	}
	_ = c.SetDeadline(time.Time{})
	return &bufConn{Conn: c, r: br}, nil
}

// bufConn reads what the upstream's reader already buffered first.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *bufConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return b.Conn.Close()
}

// nonPublic are the ranges the filter never dials: the Mac's own services,
// its LAN and VPNs, and everything that isn't a unicast internet address.
var nonPublic = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "64:ff9b:1::/48", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// PublicAddr reports whether a is a public unicast address. A NAT64
// address is judged by the IPv4 address inside it.
func PublicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.Zone() != "" {
		return false
	}
	if nat64.Contains(a) {
		b := a.As16()
		return PublicAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
