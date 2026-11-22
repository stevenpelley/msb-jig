package netstack

import (
	"net"
	"strings"

	"github.com/miekg/dns"
)

// Names is what our DNS answers: exact names and "*.suffix" patterns,
// all resolving to the gateway. Every other name gets NXDOMAIN, so the
// guest can't reach anything by name except through HTTPS_PROXY (and DNS
// queries carry nothing out).
type Names []string

// DefaultNames are the filter's and the broker's.
var DefaultNames = Names{"proxy.internal", "*.broker.internal"}

func (ns Names) match(q string) bool {
	q = strings.TrimSuffix(strings.ToLower(q), ".")
	for _, n := range ns {
		if suf, ok := strings.CutPrefix(n, "*."); ok {
			if strings.HasSuffix(q, "."+suf) {
				return true
			}
		} else if q == n {
			return true
		}
	}
	return false
}

// ServeDNS answers DNS on the gateway's UDP and TCP port 53 until the
// network closes.
func (n *Network) ServeDNS(names Names) error {
	pc, err := n.ListenUDP(53)
	if err != nil {
		return err
	}
	ln, err := n.ListenTCP(53)
	if err != nil {
		pc.Close()
		return err
	}
	h := n.dnsHandler(names)
	go func() { _ = (&dns.Server{PacketConn: pc, Handler: h}).ActivateAndServe() }()
	go func() { _ = (&dns.Server{Listener: ln, Handler: h}).ActivateAndServe() }()
	return nil
}

func (n *Network) dnsHandler(names Names) dns.HandlerFunc {
	gw := net.IP(n.gateway.AsSlice())
	return func(w dns.ResponseWriter, req *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(req)
		m.Authoritative = true
		m.RecursionAvailable = false
		if len(req.Question) != 1 || req.Opcode != dns.OpcodeQuery {
			m.Rcode = dns.RcodeRefused
			_ = w.WriteMsg(m)
			return
		}
		q := req.Question[0]
		switch {
		case !names.match(q.Name):
			m.Rcode = dns.RcodeNameError
			n.log.Info("dns refused", "name", q.Name, "type", dns.TypeToString[q.Qtype])
		case q.Qtype == dns.TypeA || q.Qtype == dns.TypeANY:
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   gw,
			})
		default:
			// The name exists but has no records of this type (AAAA, ...).
		}
		_ = w.WriteMsg(m)
	}
}
