package tsa_test

import (
	"crypto"
	"crypto/x509"
	"encoding/asn1"
	"testing"

	"github.com/devrdias/ambermoth/internal/tsa"
)

// OIDs written out here, independently of the package, so a typo in its
// tables fails a test.
var (
	dSHA1, dSHA256, dSHA384, dSHA512 = oid(1, 3, 14, 3, 2, 26), oid(2, 16, 840, 1, 101, 3, 4, 2, 1), oid(2, 16, 840, 1, 101, 3, 4, 2, 2), oid(2, 16, 840, 1, 101, 3, 4, 2, 3)
	rsaKey, ecKey                    = oid(1, 2, 840, 113549, 1, 1, 1), oid(1, 2, 840, 10045, 2, 1)
	dMD5                             = oid(1, 2, 840, 113549, 2, 5)
)

func oid(n ...int) asn1.ObjectIdentifier { return n }

type hashCase struct {
	name string
	oid  asn1.ObjectIdentifier
	want crypto.Hash
	ok   bool
}

func TestHashFor(t *testing.T) {
	t.Parallel()
	cases := []hashCase{
		{name: "SHA-256", oid: dSHA256, want: crypto.SHA256, ok: true},
		{name: "SHA-384", oid: dSHA384, want: crypto.SHA384, ok: true},
		{name: "SHA-512", oid: dSHA512, want: crypto.SHA512, ok: true},
		{name: "SHA-1", oid: dSHA1, want: crypto.SHA1, ok: true},
		{name: "rejects MD5", oid: dMD5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := tsa.HashFor(c.oid)
			if (err == nil) != c.ok || got != c.want {
				t.Fatalf("HashFor(%v) = %v, %v; want %v, ok=%v", c.oid, got, err, c.want, c.ok)
			}
		})
	}
}

type sigCase struct {
	name        string
	digest, sig asn1.ObjectIdentifier
	want        x509.SignatureAlgorithm
	ok          bool
}

func TestSigAlg(t *testing.T) {
	t.Parallel()
	cases := []sigCase{
		{name: "RSA with SHA-256", sig: oid(1, 2, 840, 113549, 1, 1, 11), want: x509.SHA256WithRSA, ok: true},
		{name: "RSA with SHA-384", sig: oid(1, 2, 840, 113549, 1, 1, 12), want: x509.SHA384WithRSA, ok: true},
		{name: "RSA with SHA-512", sig: oid(1, 2, 840, 113549, 1, 1, 13), want: x509.SHA512WithRSA, ok: true},
		{name: "RSA with SHA-1", sig: oid(1, 2, 840, 113549, 1, 1, 5), want: x509.SHA1WithRSA, ok: true},
		{name: "ECDSA with SHA-256", sig: oid(1, 2, 840, 10045, 4, 3, 2), want: x509.ECDSAWithSHA256, ok: true},
		{name: "ECDSA with SHA-384", sig: oid(1, 2, 840, 10045, 4, 3, 3), want: x509.ECDSAWithSHA384, ok: true},
		{name: "ECDSA with SHA-512", sig: oid(1, 2, 840, 10045, 4, 3, 4), want: x509.ECDSAWithSHA512, ok: true},
		{name: "bare RSA key takes SHA-256 from the digest", sig: rsaKey, digest: dSHA256, want: x509.SHA256WithRSA, ok: true},
		{name: "bare RSA key takes SHA-384 from the digest", sig: rsaKey, digest: dSHA384, want: x509.SHA384WithRSA, ok: true},
		{name: "bare RSA key takes SHA-512 from the digest", sig: rsaKey, digest: dSHA512, want: x509.SHA512WithRSA, ok: true},
		{name: "bare EC key takes SHA-256 from the digest", sig: ecKey, digest: dSHA256, want: x509.ECDSAWithSHA256, ok: true},
		{name: "bare EC key takes SHA-384 from the digest", sig: ecKey, digest: dSHA384, want: x509.ECDSAWithSHA384, ok: true},
		{name: "bare EC key takes SHA-512 from the digest", sig: ecKey, digest: dSHA512, want: x509.ECDSAWithSHA512, ok: true},
		{name: "rejects a bare RSA key with SHA-1", sig: rsaKey, digest: dSHA1},
		{name: "rejects a bare key with no digest", sig: ecKey},
		{name: "rejects an unknown algorithm", sig: dMD5, digest: dSHA256},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := tsa.SigAlg(c.digest, c.sig)
			if (err == nil) != c.ok || got != c.want {
				t.Fatalf("SigAlg(%v, %v) = %v, %v; want %v, ok=%v", c.digest, c.sig, got, err, c.want, c.ok)
			}
		})
	}
}
