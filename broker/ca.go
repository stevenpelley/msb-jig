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
	"strings"
	"sync"
	"time"
)

// BrokerDomain is the only DNS domain the broker's CA can sign for.
const BrokerDomain = "broker.internal"

// CA is the broker's own certificate authority. Its certificate carries a
// critical X.509 name constraint, so even a stolen key could sign only
// names under broker.internal. The VM trusts it; the key stays in
// investigator-net.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey

	mu    sync.Mutex
	leafs map[string]*tls.Certificate
}

// NewCA creates a CA named for one sandbox.
func NewCA(sandbox string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: "investigator broker CA (" + sandbox + ")"},
		NotBefore:                   time.Now().Add(-time.Hour),
		NotAfter:                    time.Now().AddDate(5, 0, 0),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{BrokerDomain},
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
// creates one the first time.
func LoadOrCreateCA(dir, sandbox string) (*CA, error) {
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
		if !cert.IsCA || !cert.PermittedDNSDomainsCritical || len(cert.PermittedDNSDomains) != 1 || cert.PermittedDNSDomains[0] != BrokerDomain {
			return nil, fmt.Errorf("%s: not a broker CA", certFile)
		}
		return &CA{Cert: cert, Key: key, leafs: map[string]*tls.Certificate{}}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ca, err := NewCA(sandbox)
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

// Leaf returns (and caches) a server certificate for host, which must be
// under broker.internal.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	host = strings.ToLower(host)
	if !strings.HasSuffix(host, "."+BrokerDomain) {
		return nil, fmt.Errorf("broker CA signs only *.%s, not %q", BrokerDomain, host)
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
