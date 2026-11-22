// Package netstack is the sandbox VM's only network: a gvisor stack on
// gvisor-tap-vsock's tap switch, served over Lima's user-v2 sockets, that
// forwards nothing. Only the services the caller listens for on the
// gateway address exist (the filter on 3128, the broker on 443), plus DHCP
// and a DNS server that knows only our own names. A connection to any
// other address or port gets a reset. See investigator-design.md, "Why the
// broker and the filter listen on the netstack".
//
// Unlike gvisor-tap-vsock's virtualnetwork.New, this installs no TCP, UDP
// or ICMP forwarders and no NAT table: nothing in the process dials out on
// the guest's behalf, except the filter and the broker, which the caller
// serves on the gateway.
package netstack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"

	"github.com/containers/gvisor-tap-vsock/pkg/services/dhcp"
	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// GatewayMAC is Lima's gateway MAC for user-v2 networks.
const GatewayMAC = "5a:94:ef:e4:0c:dd"

const nicID tcpip.NICID = 1

// Config describes one sandbox's network.
type Config struct {
	// Subnet is the network's CIDR, as in Lima's networks.yaml
	// (e.g. 192.168.104.0/24). The gateway is its second address, as in Lima.
	Subnet string
	MTU    int // default 1500
	Log    *slog.Logger
}

// Network is a running stack. Serve the gateway's services with ListenTCP
// and ServeDNS, and attach the VM with AcceptQEMU or AcceptBess (or Serve*
// for Lima's sockets).
type Network struct {
	cfg     Config
	subnet  netip.Prefix
	gateway netip.Addr
	stack   *stack.Stack
	sw      *tap.Switch
	pool    *tap.IPPool
	dhcp    *dhcp.Server
	log     *slog.Logger

	fwMu     sync.Mutex
	forwards map[string]*forward // Lima's SSH fallback, by local address
}

// GatewayIP returns the subnet's second address, which Lima expects.
func GatewayIP(subnet netip.Prefix) netip.Addr {
	return subnet.Masked().Addr().Next().Next()
}

// New builds the stack and starts DHCP.
func New(cfg Config) (*Network, error) {
	if cfg.MTU == 0 {
		cfg.MTU = 1500
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	subnet, err := netip.ParsePrefix(cfg.Subnet)
	if err != nil || !subnet.Addr().Is4() {
		return nil, fmt.Errorf("subnet %q: want an IPv4 CIDR", cfg.Subnet)
	}
	subnet = subnet.Masked()
	gw := GatewayIP(subnet)
	n := &Network{cfg: cfg, subnet: subnet, gateway: gw, log: cfg.Log}

	if n.pool, err = tap.NewIPPool(subnet.String()); err != nil {
		return nil, err
	}
	if err := n.pool.Reserve(gw.String(), GatewayMAC); err != nil {
		return nil, err
	}
	ep, err := tap.NewLinkEndpoint(false, uint32(cfg.MTU), GatewayMAC, gw.String(), []string{gw.String()})
	if err != nil {
		return nil, err
	}
	n.sw = tap.NewSwitch(false)
	ep.Connect(n.sw)
	n.sw.Connect(ep)

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	if e := s.CreateNIC(nicID, ep); e != nil {
		return nil, errors.New(e.String())
	}
	if e := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4(gw.As4()).WithPrefix(),
	}, stack.AddressProperties{}); e != nil {
		return nil, errors.New(e.String())
	}
	// Promiscuous and spoofing: packets to any address reach the stack,
	// which has no endpoint for them, so a TCP SYN gets a reset (sent from
	// the address the guest asked for, hence spoofing) and fails at once
	// instead of timing out. Both only shape what the stack sends back to
	// the guest. What opens gvisor-tap-vsock's network is its forwarders,
	// which dial out from the Mac, and its NAT table; neither exists here.
	s.SetSpoofing(nicID, true)
	if e := s.SetPromiscuousMode(nicID, true); e != nil {
		return nil, errors.New(e.String())
	}
	sn, e := tcpip.NewSubnet(tcpip.AddrFrom4(subnet.Addr().As4()), tcpip.MaskFromBytes(net.CIDRMask(subnet.Bits(), 32)))
	if e != nil {
		return nil, e
	}
	s.SetRouteTable([]tcpip.Route{{Destination: sn, NIC: nicID}})
	n.stack = s

	conf := &types.Configuration{
		MTU:               cfg.MTU,
		Subnet:            subnet.String(),
		GatewayIP:         gw.String(),
		GatewayMacAddress: GatewayMAC,
	}
	if n.dhcp, err = dhcp.New(conf, s, n.pool); err != nil {
		return nil, fmt.Errorf("dhcp: %w", err)
	}
	go func() {
		if err := n.dhcp.Serve(); err != nil {
			n.log.Error("dhcp stopped", "err", err)
		}
	}()
	return n, nil
}

// Gateway is the address the VM's services listen on.
func (n *Network) Gateway() netip.Addr { return n.gateway }

// Subnet is the network's prefix.
func (n *Network) Subnet() netip.Prefix { return n.subnet }

// Leases maps leased IPs to MAC addresses, the gateway's included, as
// gvisor-tap-vsock reports them.
func (n *Network) Leases() map[string]string { return n.pool.Leases() }

// ListenTCP listens on the gateway's port.
func (n *Network) ListenTCP(port uint16) (net.Listener, error) {
	return gonet.ListenTCP(n.stack, tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(n.gateway.As4()), Port: port}, ipv4.ProtocolNumber)
}

// ListenUDP opens the gateway's UDP port.
func (n *Network) ListenUDP(port uint16) (net.PacketConn, error) {
	return gonet.DialUDP(n.stack, &tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(n.gateway.As4()), Port: port}, nil, ipv4.ProtocolNumber)
}

// DialGuestTCP connects from the gateway to a guest address on the subnet
// (Lima's wait_port and its SSH fallback). It refuses any other address.
func (n *Network) DialGuestTCP(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	if !n.subnet.Contains(addr.Addr()) || addr.Addr() == n.gateway {
		return nil, fmt.Errorf("%s is not a guest address on %s", addr.Addr(), n.subnet)
	}
	return gonet.DialContextTCP(ctx, n.stack, tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(addr.Addr().As4()), Port: addr.Port()}, ipv4.ProtocolNumber)
}

// AcceptQEMU runs one VM connection that frames packets QEMU's way (a
// 4-byte length before each frame), until it closes.
func (n *Network) AcceptQEMU(ctx context.Context, c net.Conn) error {
	return n.sw.Accept(ctx, c, types.QemuProtocol)
}

// AcceptBess runs one datagram connection, a packet per datagram (vz's
// file-handle network attachment, passed through Lima's fd socket).
func (n *Network) AcceptBess(ctx context.Context, c net.Conn) error {
	return n.sw.Accept(ctx, c, types.BessProtocol)
}

// Close stops the stack.
func (n *Network) Close() {
	n.stack.Close()
}
