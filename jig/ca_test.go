package jig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func certPEM(t *testing.T, cn string, ca bool) string {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: ca}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestLoadCAs(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, b := certPEM(t, "a", true), certPEM(t, "b", true)
	two := write("two.pem", "# comment\n"+a+"\n"+b)
	one := write("one.pem", certPEM(t, "c", true))
	cas, err := loadCAs([]string{two, one})
	if err != nil || len(cas) != 3 || cas[0] != a || cas[1] != b {
		t.Fatalf("%d %v", len(cas), err)
	}
	for name, body := range map[string]string{"leaf.pem": certPEM(t, "leaf", false), "empty.pem": "no pem"} {
		if _, err := loadCAs([]string{write(name, body)}); err == nil {
			t.Errorf("%s loaded", name)
		}
	}
	if _, err := loadCAs([]string{filepath.Join(dir, "missing")}); err == nil {
		t.Error("missing file loaded")
	}

	// Recorded in settings, under JIG_EXTRA_CAS.
	t.Setenv("JIG_STATE", t.TempDir())
	os.Unsetenv("JIG_EXTRA_CAS")
	SaveSettings(Settings{CAs: []string{one}})
	if got := extraCAs(); len(got) != 1 || !strings.Contains(got[0], "BEGIN CERTIFICATE") {
		t.Errorf("from settings: %v", got)
	}
	t.Setenv("JIG_EXTRA_CAS", "")
	if got := extraCAs(); len(got) != 0 {
		t.Errorf("JIG_EXTRA_CAS empty: %v", got)
	}
}
