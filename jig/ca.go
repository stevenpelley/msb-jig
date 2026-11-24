package jig

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Extra CA certificates: roots the guest must trust besides the public
// ones and the broker's, such as a TLS-inspecting proxy's (Zscaler and
// the like) on a corporate network. mono-jig ca records the PEM files
// (their paths, so a rotated file is read again); JIG_EXTRA_CAS
// overrides them (space-separated paths; empty: none).
//
// They go in the base image (mono-jig image build: the build's own
// downloads need them) and in every sandbox at every start (provision.sh's
// ca component), before anything else is installed. The broker on the Mac
// verifies with the Mac's own trust settings, so the proxy's root belongs
// in the Mac's keychain too (docs/library-split.md).

// guestCAFile is where the supervisor writes the extra CAs for
// provision.sh's ca component.
const guestCAFile = "/root/guest-ca.pem"

// extraCAFiles are the recorded files, under JIG_EXTRA_CAS.
func extraCAFiles() []string {
	if v, ok := os.LookupEnv("JIG_EXTRA_CAS"); ok {
		return strings.Fields(v)
	}
	return LoadSettings().CAs
}

// loadCAs reads files: each CA certificate in them, PEM-encoded.
func loadCAs(files []string) ([]string, error) {
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		n := 0
		for {
			var blk *pem.Block
			blk, b = pem.Decode(b)
			if blk == nil {
				break
			}
			if blk.Type != "CERTIFICATE" {
				continue
			}
			c, err := x509.ParseCertificate(blk.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", f, err)
			}
			if !c.IsCA {
				return nil, fmt.Errorf("%s: %s isn't a CA certificate", f, c.Subject)
			}
			out = append(out, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: blk.Bytes})))
			n++
		}
		if n == 0 {
			return nil, fmt.Errorf("%s: no PEM certificate", f)
		}
	}
	return out, nil
}

// extraCAs are the extra CA certificates, PEM, one each; it fails if a
// file can't be read.
func extraCAs() []string {
	cas, err := loadCAs(extraCAFiles())
	if err != nil {
		fail("extra CAs (mono-jig ca): %v", err)
	}
	return cas
}

// caCmd: mono-jig ca shows the extra CAs; ca <file>... records them;
// ca --clear forgets them.
func caCmd(args []string) {
	s := LoadSettings()
	switch {
	case len(args) == 0:
		if len(s.CAs) == 0 {
			fmt.Println("no extra CAs (mono-jig ca <pem-file>...)")
			return
		}
		cas, err := loadCAs(s.CAs)
		for _, f := range s.CAs {
			fmt.Println(f)
		}
		if err != nil {
			fail("%v", err)
		}
		for _, p := range cas {
			blk, _ := pem.Decode([]byte(p))
			c, _ := x509.ParseCertificate(blk.Bytes)
			fmt.Printf("  %s (until %s)\n", c.Subject, c.NotAfter.Format("2006-01-02"))
		}
		return
	case len(args) == 1 && args[0] == "--clear":
		s.CAs = nil
	default:
		var files []string
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				usage()
			}
			abs, err := filepath.Abs(a)
			if err != nil {
				fail("%v", err)
			}
			files = append(files, abs)
		}
		cas, err := loadCAs(files)
		if err != nil {
			fail("%v", err)
		}
		s.CAs = files
		fmt.Printf("%d CA certificates from %s\n", len(cas), strings.Join(files, " "))
	}
	SaveSettings(s)
	fmt.Println("sandboxes pick this up when they next start; rebuild the base image too (mono-jig image build)")
}
