package broker

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The broker's second front end, for proxy mode (docs/transparent-broker.md):
// microsandbox's outbound proxy. microsandbox sends every TCP connection the
// guest opens, after its own policy, as CONNECT <ip>:<port> with the
// sandbox's password. For each one the front end reads the guest's first
// bytes and:
//
//   - TLS whose SNI is a brokered name: terminates it with the broker's CA
//     and serves the requests to the broker (Server), like ServeTLS does;
//   - TLS with another SNI: copies bytes both ways to <ip>:<port>;
//   - TLS with no SNI (or a ClientHello it can't read): refuses it, since
//     microsandbox lets one through to any IP the guest resolved an
//     allowed name to, so a bare https://<github's IP> would otherwise
//     reach a brokered host without the broker;
//   - plain HTTP whose first request names a brokered host: 403, since the
//     broker never sends a credential in cleartext; else copied;
//   - anything else (SSH, a server-first protocol): copied.

// ProxyUser is the user name microsandbox sends with the password.
const ProxyUser = "jig"

// Proxy is the CONNECT front end. Create it with NewProxy.
type Proxy struct {
	password string
	brokered map[string]bool
	tls      *tls.Config
	log      *slog.Logger
	conns    *connListener
	srv      *http.Server
	// Dial opens a spliced connection (tests replace it).
	Dial func(network, addr string) (net.Conn, error)
	// FirstWait is how long the guest has to send its first bytes before
	// the connection counts as server-first and is spliced.
	FirstWait time.Duration
}

// NewProxy serves handler (a Server) for the brokered names over TLS
// under ca, and splices everything else. password is what microsandbox
// sends as ProxyUser's.
func NewProxy(handler http.Handler, ca *CA, brokered []string, password string, log *slog.Logger) *Proxy {
	p := &Proxy{password: password, brokered: map[string]bool{}, log: log, conns: newConnListener(), FirstWait: 3 * time.Second,
		Dial: func(network, addr string) (net.Conn, error) { return net.DialTimeout(network, addr, 10*time.Second) }}
	for _, h := range brokered {
		p.brokered[strings.ToLower(h)] = true
	}
	p.tls = ca.TLSConfig(brokered)
	// gh negotiates h2; http.Server answers it on a *tls.Conn whose
	// config offers it.
	p.tls.NextProtos = []string{"h2", "http/1.1"}
	// No WriteTimeout: model responses stream for minutes.
	p.srv = &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = p.srv.Serve(p.conns) }()
	return p
}

// Serve accepts CONNECTs on l until it's closed.
func (p *Proxy) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go p.handle(c)
	}
}

// Close stops the terminated connections' server.
func (p *Proxy) Close() error { p.conns.Close(); return p.srv.Close() }

func (p *Proxy) handle(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	// Room for a whole TLS record (16 KiB and its header) or HTTP headers.
	br := bufio.NewReaderSize(c, 64<<10)
	req, err := http.ReadRequest(br)
	if err != nil {
		c.Close()
		return
	}
	target := req.Host
	if req.Method != http.MethodConnect {
		p.answer(c, "405 Method Not Allowed", "", "proxy", "method", req.Method)
		return
	}
	if !p.authorized(req.Header.Get("Proxy-Authorization")) {
		p.answer(c, "407 Proxy Authentication Required", "Proxy-Authenticate: Basic realm=\"mono-jig\"\r\n", "proxy", "target", target)
		return
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		p.answer(c, "400 Bad Request", "", "proxy", "target", target)
		return
	}
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		c.Close()
		return
	}
	// The guest's first bytes. A server-first protocol sends none.
	_ = c.SetReadDeadline(time.Now().Add(p.FirstWait))
	first, _ := br.Peek(1)
	_ = c.SetReadDeadline(time.Time{})
	in := &prefixConn{Conn: c, r: br}
	switch {
	case len(first) == 1 && first[0] == 0x16: // a TLS handshake record
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		sni := peekSNI(br)
		_ = c.SetReadDeadline(time.Time{})
		switch {
		case sni == "":
			p.refuse(c, target, "tls", "", "no server name")
			return
		case p.brokered[strings.ToLower(sni)]:
			p.logf("terminate", target, "tls", sni)
			if !p.conns.put(tls.Server(in, p.tls)) {
				c.Close()
			}
			return
		}
		p.splice(in, target, "tls", sni)
	case len(first) == 1 && isTokenByte(first[0]):
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		host := peekHost(br)
		_ = c.SetReadDeadline(time.Time{})
		if host == "" {
			p.refuse(c, target, "http", "", "no Host header in the first request")
			return
		}
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if p.brokered[strings.ToLower(host)] {
			p.logf("refuse", target, "http", host, "reason", "a brokered host over plain HTTP")
			_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\nmono-jig broker: "+host+" is brokered; use https\n")
			c.Close()
			return
		}
		p.splice(in, target, "http", host)
	default:
		p.splice(in, target, "other", "")
	}
}

// authorized checks the Basic credential microsandbox sends.
func (p *Proxy) authorized(h string) bool {
	b, ok := strings.CutPrefix(h, "Basic ")
	if !ok {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(b)
	if err != nil {
		return false
	}
	want := ProxyUser + ":" + p.password
	return p.password != "" && subtle.ConstantTimeCompare(raw, []byte(want)) == 1
}

func (p *Proxy) answer(c net.Conn, status, headers string, kind string, kv ...any) {
	_, _ = io.WriteString(c, "HTTP/1.1 "+status+"\r\n"+headers+"Content-Length: 0\r\nConnection: close\r\n\r\n")
	c.Close()
	if p.log != nil {
		p.log.Warn("connect refused", append([]any{"status", status, "kind", kind}, kv...)...)
	}
}

func (p *Proxy) refuse(c net.Conn, target, kind, name, why string) {
	p.logf("refuse", target, kind, name, "reason", why)
	c.Close()
}

func (p *Proxy) logf(action, target, kind, name string, kv ...any) {
	if p.log != nil {
		p.log.Info("connect", append([]any{"action", action, "target", target, "kind", kind, "name", name}, kv...)...)
	}
}

// splice copies bytes both ways between the guest and target, an IP and
// port microsandbox's policy already allowed.
func (p *Proxy) splice(in net.Conn, target, kind, name string) {
	defer in.Close()
	p.logf("splice", target, kind, name)
	up, err := p.Dial("tcp", target)
	if err != nil {
		p.logf("dial failed", target, kind, name, "err", err.Error())
		return
	}
	defer up.Close()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(up, in)
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		close(done)
	}()
	_, _ = io.Copy(in, up)
	if cw, ok := in.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	<-done
}

// peekSNI reads the server name from the ClientHello at the front of br,
// without consuming it: a TLS server handshake runs on a copy of the
// first record and stops once it has the hello. "" if there's none, or
// the hello isn't in the first record.
func peekSNI(br *bufio.Reader) string {
	hdr, err := br.Peek(5)
	if err != nil || hdr[0] != 0x16 {
		return ""
	}
	n := 5 + (int(hdr[3])<<8 | int(hdr[4]))
	if n > br.Size() {
		return ""
	}
	rec, err := br.Peek(n)
	if err != nil {
		return ""
	}
	var sni string
	errDone := errors.New("done")
	_ = tls.Server(readOnlyConn{r: bytes.NewReader(rec)}, &tls.Config{
		GetConfigForClient: func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
			sni = hi.ServerName
			return nil, errDone
		},
	}).Handshake()
	return sni
}

// peekHost reads the Host of the plain HTTP request at the front of br,
// without consuming it. "" if its headers don't fit in br's buffer.
func peekHost(br *bufio.Reader) string {
	for {
		b, _ := br.Peek(br.Buffered())
		if i := bytes.Index(b, []byte("\r\n\r\n")); i >= 0 {
			req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(b[:i+4])))
			if err != nil {
				return ""
			}
			return req.Host
		}
		if len(b) >= br.Size() {
			return ""
		}
		// Wait for more.
		if _, err := br.Peek(len(b) + 1); err != nil {
			return ""
		}
	}
}

// isTokenByte reports whether b can start an HTTP method.
func isTokenByte(b byte) bool { return b >= 'A' && b <= 'Z' }

// prefixConn reads what a bufio.Reader holds, then the connection.
type prefixConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *prefixConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// readOnlyConn is a net.Conn that reads r and discards writes.
type readOnlyConn struct {
	net.Conn
	r io.Reader
}

func (c readOnlyConn) Read(p []byte) (int, error)       { return c.r.Read(p) }
func (c readOnlyConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c readOnlyConn) Close() error                     { return nil }
func (c readOnlyConn) SetDeadline(time.Time) error      { return nil }
func (c readOnlyConn) SetReadDeadline(time.Time) error  { return nil }
func (c readOnlyConn) SetWriteDeadline(time.Time) error { return nil }
func (c readOnlyConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c readOnlyConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }

// connListener hands terminated connections to one http.Server.
type connListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newConnListener() *connListener {
	return &connListener{ch: make(chan net.Conn), done: make(chan struct{})}
}

func (l *connListener) put(c net.Conn) bool {
	select {
	case l.ch <- c:
		return true
	case <-l.done:
		return false
	}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error { l.once.Do(func() { close(l.done) }); return nil }

func (l *connListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
