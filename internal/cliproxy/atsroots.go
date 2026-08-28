package cliproxy

import (
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"sort"
	"sync"
)

// The embedded Amazon Trust Services root certificates. All AWS
// service endpoints chain to one of these five roots, so a verified
// chain ending elsewhere means a TLS-intercepting proxy re-signed the
// connection. The files were fetched from
// https://www.amazontrust.com/repository/ and byte-compared against
// the Ubuntu ca-certificates copies before being committed;
// TestATSRootsArePinned pins their SHA-256 fingerprints.
//
//go:embed certs/*.pem
var atsCertFS embed.FS

var (
	atsOnce  sync.Once
	atsCerts []*x509.Certificate
	atsErr   error
)

// atsRoots parses the embedded roots once.
func atsRoots() ([]*x509.Certificate, error) {
	atsOnce.Do(func() {
		entries, err := atsCertFS.ReadDir("certs")
		if err != nil {
			atsErr = fmt.Errorf("read embedded ATS roots: %w", err)
			return
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			raw, err := atsCertFS.ReadFile("certs/" + name)
			if err != nil {
				atsErr = fmt.Errorf("read embedded ATS root %s: %w", name, err)
				return
			}
			block, _ := pem.Decode(raw)
			if block == nil || block.Type != "CERTIFICATE" {
				atsErr = fmt.Errorf("embedded ATS root %s is not a PEM certificate", name)
				return
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				atsErr = fmt.Errorf("parse embedded ATS root %s: %w", name, err)
				return
			}
			atsCerts = append(atsCerts, cert)
		}
	})
	return atsCerts, atsErr
}

// isATSRoot reports whether c is one of the embedded Amazon Trust
// Services roots, compared by certificate fingerprint.
func isATSRoot(c *x509.Certificate) (bool, error) {
	roots, err := atsRoots()
	if err != nil {
		return false, err
	}
	fp := fingerprint(c)
	for _, r := range roots {
		if fingerprint(r) == fp {
			return true, nil
		}
	}
	return false, nil
}

// fingerprint returns the hex SHA-256 of the certificate's DER bytes.
func fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}
