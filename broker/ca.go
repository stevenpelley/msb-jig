package broker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// BrokerDomain is the only DNS domain the broker's CA can sign for.
const BrokerDomain = "broker.internal"

// CA is the broker's own certificate authority. Its certificate carries a
// critical X.509 name constraint, so even a stolen key could sign only
// names under broker.internal, and in proxy mode under the real hosts the
// broker serves (github.com, gitlab.com, api.anthropic.com). The VM
// trusts it; the key stays with the supervisor, on the Mac.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey

	mu    sync.Mutex
	leafs map[string]*tls.Certificate
}

// NewCA creates a CA named for one sandbox, that can sign names under
// broker.internal and the domains given.
func NewCA(sandbox string, domains ...string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: "mono-jig broker CA (" + sandbox + ")"},
		NotBefore:                   time.Now().Add(-time.Hour),
		NotAfter:                    time.Now().AddDate(5, 0, 0),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         permitted(domains),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, leafs: map[string]*tls.Certificate{}}, nil
}

// LoadOrCreateCA keeps the sandbox's CA in dir (ca.pem, and ca-key.pem
// with mode 0600), since the VM trusts it from provisioning on. It
// creates one the first time, for broker.internal and domains; a CA kept
// from before must cover them (the VM trusts only the one it was made
// with).
func LoadOrCreateCA(dir, sandbox string, domains ...string) (*CA, error) {
	certFile, keyFile := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	if certPEM, err := os.ReadFile(certFile); err == nil {
		keyPEM, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, err
		}
		cb, _ := pem.Decode(certPEM)
		kb, _ := pem.Decode(keyPEM)
		if cb == nil || kb == nil {
			return nil, fmt.Errorf("%s: bad PEM", dir)
		}
		cert, err := x509.ParseCertificate(cb.Bytes)
		if err != nil {
			return nil, err
		}
		key, err := x509.ParseECPrivateKey(kb.Bytes)
		if err != nil {
			return nil, err
		}
		if !cert.IsCA || !cert.PermittedDNSDomainsCritical || !slices.Contains(cert.PermittedDNSDomains, BrokerDomain) {
			return nil, fmt.Errorf("%s: not a broker CA", certFile)
		}
		ca := &CA{Cert: cert, Key: key, leafs: map[string]*tls.Certificate{}}
		for _, d := range domains {
			if !ca.covers(d) {
				return nil, fmt.Errorf("%s can't sign for %s, which the broker now serves: the sandbox trusts only this CA; remove the sandbox (mono-jig rm) and create it again", certFile, d)
			}
		}
		return ca, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ca, err := NewCA(sandbox, domains...)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(ca.Key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certFile, ca.CertPEM(), 0o644); err != nil {
		return nil, err
	}
	return ca, nil
}

// CertPEM is what the VM installs as a trusted CA.
func (c *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Cert.Raw})
}

// permitted is the CA's name constraint: broker.internal and domains.
func permitted(domains []string) []string {
	out := []string{BrokerDomain}
	for _, d := range domains {
		if d = strings.ToLower(d); !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// covers reports whether the CA's name constraint permits host.
func (c *CA) covers(host string) bool {
	host = strings.ToLower(host)
	for _, d := range c.Cert.PermittedDNSDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// Leaf returns (and caches) a server certificate for host, which the
// CA's name constraint must permit.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	host = strings.ToLower(host)
	if !c.covers(host) {
		return nil, fmt.Errorf("broker CA signs only names under %s, not %q", strings.Join(c.Cert.PermittedDNSDomains, ", "), host)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.leafs[host]; ok {
		return l, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 3, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, &key.PublicKey, c.Key)
	if err != nil {
		return nil, err
	}
	l := &tls.Certificate{Certificate: [][]byte{der, c.Cert.Raw}, PrivateKey: key}
	c.leafs[host] = l
	return l, nil
}

// TLSConfig serves a certificate for each provider host by SNI and refuses
// connections without a known server name.
func (c *CA) TLSConfig(hosts []string) *tls.Config {
	ok := map[string]bool{}
	for _, h := range hosts {
		ok[strings.ToLower(h)] = true
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := strings.ToLower(hi.ServerName)
			if !ok[name] {
				return nil, fmt.Errorf("unknown server name %q", hi.ServerName)
			}
			return c.Leaf(name)
		},
	}
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}
