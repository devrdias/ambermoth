// Package trust holds the Time Stamping Authority root certificates that
// ambermoth pins by default (SPEC §8). A token whose signer does not chain to
// one of these roots, or to a root the reviewer adds with --tsa-ca, fails
// verification unless --allow-unpinned-tsa is given.
package trust

import (
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"sort"
)

//go:embed roots/*.pem
var rootsFS embed.FS

// Root describes one pinned root certificate.
type Root struct {
	File        string
	Subject     string
	SHA256      string // fingerprint of the DER certificate, lowercase hex
	Certificate *x509.Certificate
}

// Roots returns the embedded roots, sorted by file name.
func Roots() ([]Root, error) {
	entries, err := rootsFS.ReadDir("roots")
	if err != nil {
		return nil, err
	}
	var out []Root
	for _, e := range entries {
		b, err := rootsFS.ReadFile("roots/" + e.Name())
		if err != nil {
			return nil, err
		}
		certs, err := parsePEM(b)
		if err != nil || len(certs) != 1 {
			return nil, fmt.Errorf("trust: %s must hold exactly one certificate", e.Name())
		}
		sum := sha256.Sum256(certs[0].Raw)
		out = append(out, Root{File: e.Name(), Subject: certs[0].Subject.CommonName,
			SHA256: hex.EncodeToString(sum[:]), Certificate: certs[0]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

// Pool returns the embedded roots plus the certificates in extraPEMFiles.
func Pool(extraPEMFiles []string) (*x509.CertPool, error) {
	roots, err := Roots()
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	for _, r := range roots {
		pool.AddCert(r.Certificate)
	}
	for _, p := range extraPEMFiles {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		certs, err := parsePEM(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if len(certs) == 0 {
			return nil, fmt.Errorf("%s: no PEM certificates", p)
		}
		for _, c := range certs {
			pool.AddCert(c)
		}
	}
	return pool, nil
}

func parsePEM(b []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return out, nil
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
}
