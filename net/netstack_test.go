package netstack

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const guestMAC = "52:55:55:12:34:56"

// guest is a fake VM: its own gvisor stack, its frames carried over the
// network's QEMU socket with QEMU's framing.
type guest struct {
	s  *stack.Stack
	ch *channel.Endpoint
}

func newGuest(t *testing.T, ctx context.Context, qemuSock string) *guest {
	t.Helper()
	mac, _ := tcpip.ParseMACAddress(guestMAC)
	ch := channel.New(512, 1500, mac)
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	if e := s.CreateNIC(1, ethernet.New(ch)); e != nil {
		t.Fatal(e)
	}
	c, err := net.Dial("unix", qemuSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	go func() { // guest -> network
		for {
			pkt := ch.ReadContext(ctx)
			if pkt == nil {
				return
			}
			frame := pkt.ToView().AsSlice()
			var hdr [4]byte
			binary.BigEndian.PutUint32(hdr[:], uint32(len(frame)))
			_, err := c.Write(append(hdr[:], frame...))
			pkt.DecRef()
			if err != nil {
				return
			}
		}
	}()
	go func() { // network -> guest
		r := bufio.NewReader(c)
		for {
			var hdr [4]byte
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				return
			}
			frame := make([]byte, binary.BigEndian.Uint32(hdr[:]))
			if _, err := io.ReadFull(r, frame); err != nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(frame)})
			ch.InjectInbound(0, pkt)
			pkt.DecRef()
		}
	}()
	return &guest{s: s, ch: ch}
}

func (g *guest) configure(t *testing.T, ip, gw netip.Addr) {
	t.Helper()
	if e := g.s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4(ip.As4()), PrefixLen: 24}}, stack.AddressProperties{}); e != nil {
		t.Fatal(e)
	}
	g.s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4(gw.As4()), NIC: 1},
	})
}

func (g *guest) dial(ctx context.Context, ap string) (net.Conn, error) {
	a := netip.MustParseAddrPort(ap)
	return gonet.DialContextTCP(ctx, g.s, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(a.Addr().As4()), Port: a.Port()}, ipv4.ProtocolNumber)
}

// dhcp runs DISCOVER/OFFER from the guest, with no address yet.
func (g *guest) dhcp(t *testing.T) *dhcpv4.DHCPv4 {
	t.Helper()
	if e := g.s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: header.IPv4Any, PrefixLen: 0}}, stack.AddressProperties{}); e != nil {
		t.Fatal(e)
	}
	g.s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	defer g.s.RemoveAddress(1, header.IPv4Any)
	var wq waiter.Queue
	uep, e := g.s.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if e != nil {
		t.Fatal(e)
	}
	uep.SocketOptions().SetBroadcast(true)
	if e := uep.Bind(tcpip.FullAddress{NIC: 1, Port: 68}); e != nil {
		t.Fatal(e)
	}
	conn := gonet.NewUDPConn(&wq, uep)
	defer conn.Close()
	mac, _ := net.ParseMAC(guestMAC)
	d, _ := dhcpv4.NewDiscovery(mac)
	if _, err := conn.WriteTo(d.ToBytes(), &net.UDPAddr{IP: net.IPv4bcast, Port: 67}); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no DHCP offer: %v", err)
	}
	offer, err := dhcpv4.FromBytes(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	return offer
}

// ping sends one ICMP echo and reports whether a reply came back.
func (g *guest) ping(t *testing.T, dst netip.Addr) bool {
	t.Helper()
	var wq waiter.Queue
	ep, e := g.s.NewEndpoint(icmp.ProtocolNumber4, ipv4.ProtocolNumber, &wq)
	if e != nil {
		t.Fatal(e)
	}
	c := gonet.NewUDPConn(&wq, ep)
	defer c.Close()
	req := header.ICMPv4(make([]byte, header.ICMPv4MinimumSize+4))
	req.SetType(header.ICMPv4Echo)
	req.SetIdent(7)
	req.SetSequence(1)
	copy(req.Payload(), "ping")
	if _, err := c.WriteTo(req, &net.UDPAddr{IP: dst.AsSlice()}); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, _, err := c.ReadFrom(buf)
	return err == nil && n >= header.ICMPv4MinimumSize && header.ICMPv4(buf[:n]).Type() == header.ICMPv4EchoReply
}

func serveText(t *testing.T, ln net.Listener, text string) {
	t.Cleanup(func() { ln.Close() })
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, text) }))
	}()
}

func TestNetstack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	n, err := New(Config{Subnet: "192.168.104.0/24", Log: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	gw := n.Gateway()
	if gw.String() != "192.168.104.2" {
		t.Fatalf("gateway %s", gw)
	}
	for port, text := range map[uint16]string{3128: "filter", 443: "broker"} {
		ln, err := n.ListenTCP(port)
		if err != nil {
			t.Fatal(err)
		}
		serveText(t, ln, text)
	}
	if err := n.ServeDNS(DefaultNames); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "net")
	if err := n.ServeLima(ctx, dir, "inv"); err != nil {
		t.Fatal(err)
	}
	_, qemuSock, epSock := SocketPaths(dir, "inv")
	g := newGuest(t, ctx, qemuSock)

	// DHCP offers an address on the subnet, the gateway as router and DNS.
	offer := g.dhcp(t)
	ip, _ := netip.AddrFromSlice(offer.YourIPAddr.To4())
	if !n.Subnet().Contains(ip) || ip == gw || offer.Router()[0].String() != gw.String() || offer.DNS()[0].String() != gw.String() {
		t.Fatalf("offer: %s", offer.Summary())
	}
	g.configure(t, ip, gw)

	// The gateway's services answer.
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, a string) (net.Conn, error) { return g.dial(ctx, a) }}}
	for port, want := range map[int]string{3128: "filter", 443: "broker"} {
		resp, err := hc.Get(fmt.Sprintf("http://%s:%d/", gw, port))
		if err != nil {
			t.Fatalf("gateway:%d: %v", port, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != want {
			t.Errorf("gateway:%d = %q", port, b)
		}
	}

	// Anything else on TCP is reset at once: other ports on the gateway,
	// and any address off the subnet.
	for _, a := range []string{gw.String() + ":22", gw.String() + ":80", "1.1.1.1:443", "10.0.0.1:443", "192.168.5.2:22"} {
		dctx, dcancel := context.WithTimeout(ctx, 3*time.Second)
		start := time.Now()
		c, err := g.dial(dctx, a)
		dcancel()
		if err == nil {
			c.Close()
			t.Errorf("%s: connected", a)
		} else if !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: %v after %s, want a reset", a, err, time.Since(start))
		}
	}
	// Other addresses on the subnet don't exist (no ARP answers), and
	// loopback never leaves a guest.
	for _, a := range []string{"192.168.104.50:22", "127.0.0.1:22"} {
		dctx, dcancel := context.WithTimeout(ctx, 2*time.Second)
		if c, err := g.dial(dctx, a); err == nil {
			c.Close()
			t.Errorf("connected to %s", a)
		}
		dcancel()
	}

	// ICMP: an echo to the gateway is answered; to anything else, nothing.
	for dst, want := range map[string]bool{gw.String(): true, "1.1.1.1": false} {
		if got := g.ping(t, netip.MustParseAddr(dst)); got != want {
			t.Errorf("ping %s: reply=%v, want %v", dst, got, want)
		}
	}

	// DNS: our names resolve to the gateway, everything else is NXDOMAIN,
	// and DNS to any other server goes nowhere.
	query := func(server, name string, qt uint16) (*dns.Msg, error) {
		a := netip.MustParseAddrPort(server)
		pc, err := gonet.DialUDP(g.s, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(a.Addr().As4()), Port: a.Port()}, ipv4.ProtocolNumber)
		if err != nil {
			return nil, err
		}
		defer pc.Close()
		c := &dns.Client{Timeout: 2 * time.Second}
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(name), qt)
		r, _, err := c.ExchangeWithConn(m, &dns.Conn{Conn: pc})
		return r, err
	}
	dnsAddr := gw.String() + ":53"
	for _, name := range []string{"proxy.internal", "gitlab.broker.internal", "GitHub.Broker.Internal"} {
		r, err := query(dnsAddr, name, dns.TypeA)
		if err != nil || r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != gw.String() {
			t.Errorf("%s: %v %v", name, r, err)
		}
	}
	if r, err := query(dnsAddr, "gitlab.broker.internal", dns.TypeAAAA); err != nil || r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
		t.Errorf("AAAA: %v %v", r, err)
	}
	for _, name := range []string{"example.com", "broker.internal", "proxy.internal.evil.com", "x.proxy.internal"} {
		if r, err := query(dnsAddr, name, dns.TypeA); err != nil || r.Rcode != dns.RcodeNameError {
			t.Errorf("%s: %v %v, want NXDOMAIN", name, r, err)
		}
	}
	if r, err := query("8.8.8.8:53", "example.com", dns.TypeA); err == nil {
		t.Errorf("DNS to 8.8.8.8 answered: %v", r)
	}

	// The endpoint socket: leases, wait_port, and SSH-only forwards.
	ep := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", epSock)
	}}}
	resp, err := ep.Get("http://lima/services/dhcp/leases")
	if err != nil {
		t.Fatal(err)
	}
	var leases map[string]string
	json.NewDecoder(resp.Body).Decode(&leases)
	resp.Body.Close()
	if leases[ip.String()] != guestMAC {
		t.Errorf("leases %v", leases)
	}
	sshLn, err := gonet.ListenTCP(g.s, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(ip.As4()), Port: 22}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer sshLn.Close()
	go func() {
		for {
			c, err := sshLn.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, "SSH-2.0-fake\r\n")
			c.Close()
		}
	}()
	resp, err = ep.Get(fmt.Sprintf("http://lima/extension/wait_port?ip=%s&port=22&timeout=5", ip))
	if err != nil || resp.StatusCode != 200 {
		t.Errorf("wait_port: %v %v", resp, err)
	}
	post := func(path string, v any) int {
		b, _ := json.Marshal(v)
		resp, err := ep.Post("http://lima"+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	local := free.Addr().String()
	free.Close()
	for _, bad := range []types.ExposeRequest{
		{Local: local, Remote: ip.String() + ":80", Protocol: types.TCP},
		{Local: local, Remote: "1.1.1.1:22", Protocol: types.TCP},
		{Local: "0.0.0.0:2222", Remote: ip.String() + ":22", Protocol: types.TCP},
		{Local: local, Remote: ip.String() + ":22", Protocol: types.UDP},
	} {
		if code := post("/services/forwarder/expose", bad); code != http.StatusForbidden {
			t.Errorf("expose %+v: %d", bad, code)
		}
	}
	if code := post("/services/forwarder/expose", types.ExposeRequest{Local: local, Remote: ip.String() + ":22", Protocol: types.TCP}); code != 200 {
		t.Fatalf("expose ssh: %d", code)
	}
	c, err := net.Dial("tcp", local)
	if err != nil {
		t.Fatal(err)
	}
	banner, _ := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if banner != "SSH-2.0-fake\r\n" {
		t.Errorf("forwarded banner %q", banner)
	}
	if code := post("/services/dns/add", types.Zone{Name: "lima.internal."}); code != 200 {
		t.Errorf("dns/add: %d", code)
	}
	if code := post("/services/forwarder/unexpose", types.UnexposeRequest{Local: local, Protocol: types.TCP}); code != 200 {
		t.Errorf("unexpose: %d", code)
	}
	if c, err := net.Dial("tcp", local); err == nil {
		c.Close()
		t.Error("forward still open after unexpose")
	}
	if !strings.Contains(logs.String(), "dns refused") {
		t.Error("refused DNS names aren't logged")
	}
}
