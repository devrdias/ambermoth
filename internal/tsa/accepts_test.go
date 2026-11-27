package tsa_test

import (
	"crypto/x509"
	"encoding/asn1"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/tsa"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

// acceptCase is a token the verifier takes today. Some of these are right
// (RFC 3161 allows status 1); others are looser than RFC 3161 or RFC 5652 ask
// for. They pin the current behaviour so that a change to it shows in a diff.
type acceptCase struct {
	name  string
	build func(t *testing.T, f *fixture) []byte
	roots func(f *fixture) *x509.CertPool
}

func trusted(f *fixture) *x509.CertPool { return f.auth.Roots }

func noRoots(*fixture) *x509.CertPool { return nil }

// resign signs the signed attributes again with the TSA key, as a TSA that
// built the token differently would have.
func resign(t *testing.T, f *fixture, r *tsatest.Node) {
	t.Helper()
	set := *signedAttrs(t, r)
	set.Class, set.Tag = asn1.ClassUniversal, asn1.TagSet
	signerInfo(t, r).Kids[siSig] = octets(t, f.auth.Sign(t, set.DER()))
}

func TestVerifyAccepts(t *testing.T) {
	f := newFixture(t)
	gen := f.genTime(t)
	stamping := []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping}
	around := tsatest.CertOptions{NotBefore: gen.Add(-time.Hour), NotAfter: gen.Add(time.Hour)}
	noUsage := around
	twoUsages := around
	twoUsages.ExtKeyUsage = append([]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, stamping...)
	expired := around
	expired.NotBefore, expired.NotAfter = gen.Add(-48*time.Hour), gen.Add(-24*time.Hour)

	cases := []acceptCase{
		{name: "status granted with modifications", roots: trusted, build: editor(func(t *testing.T, r *tsatest.Node) { r.Kids[0].Kids[0] = integer(t, 1) })},
		{name: "signer named by subject key id", roots: trusted, build: editor(func(t *testing.T, r *tsatest.Node) {
			ski := f.auth.SignerCert(t).SubjectKeyId
			if len(ski) == 0 {
				t.Skip("openssl issued no subject key id")
			}
			signerInfo(t, r).Kids[siSID] = &tsatest.Node{Class: asn1.ClassContextSpecific, Tag: 0, Bytes: ski}
		})},
		{name: "extra element after the token", roots: trusted, build: editor(func(t *testing.T, r *tsatest.Node) { r.Kids = append(r.Kids, null(t)) })},
		{name: "extra element after the SignerInfos", roots: trusted, build: editor(func(t *testing.T, r *tsatest.Node) {
			sd := signedData(t, r)
			sd.Kids = append(sd.Kids, null(t))
		})},
		{name: "no signing-certificate attribute", roots: trusted, build: editor(func(t *testing.T, r *tsatest.Node) {
			dropAttribute(t, r, signingCertV2OID())
			resign(t, f, r)
		})},
		{name: "signer without an extended key usage", roots: trusted, build: func(t *testing.T, f *fixture) []byte { return f.withSigner(t, noUsage) }},
		{name: "signer allowed for server auth and time stamping", roots: trusted, build: func(t *testing.T, f *fixture) []byte { return f.withSigner(t, twoUsages) }},
		{name: "expired signer when no roots are given", roots: noRoots, build: func(t *testing.T, f *fixture) []byte { return f.withSigner(t, expired) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info, err := tsa.Verify(c.build(t, f), f.digest, c.roots(f))
			if err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if info.ChainVerified != (c.roots(f) != nil) {
				t.Fatalf("ChainVerified = %v with roots %v", info.ChainVerified, c.roots(f) != nil)
			}
		})
	}
}
