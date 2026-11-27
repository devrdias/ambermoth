package tsa_test

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509/pkix"
	"encoding/asn1"
	"testing"

	"github.com/devrdias/ambermoth/internal/tsa"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

// resigned returns a builder that edits the good token with fn and signs it again.
func resigned(fn func(t *testing.T, f *fixture, r *tsatest.Node)) func(t *testing.T, f *fixture) []byte {
	return func(t *testing.T, f *fixture) []byte {
		t.Helper()
		return f.edit(t, func(r *tsatest.Node) {
			fn(t, f, r)
			resign(t, f, r)
		})
	}
}

func sum1(b []byte) []byte   { s := sha1.Sum(b); return s[:] }
func sum256(b []byte) []byte { s := sha256.Sum256(b); return s[:] }
func sum384(b []byte) []byte { s := sha512.Sum384(b); return s[:] }
func sum512(b []byte) []byte { s := sha512.Sum512(b); return s[:] }

func sha384() asn1.ObjectIdentifier { return sha384OID() }

func algID(t *testing.T, oid asn1.ObjectIdentifier) *tsatest.Node {
	t.Helper()
	return tsatest.FromValue(t, pkix.AlgorithmIdentifier{Algorithm: oid})
}

// setV2 gives the signing-certificate-v2 attribute these certID fields.
func setV2(t *testing.T, r *tsatest.Node, kids ...*tsatest.Node) {
	t.Helper()
	certID(t, r, signingCertV2OID()).Kids = kids
}

// asV1 turns the signing-certificate-v2 attribute into a v1 one for the hash.
func asV1(t *testing.T, r *tsatest.Node, certHash []byte) {
	t.Helper()
	attribute(t, r, signingCertV2OID()).Kids[0] = objectID(t, signingCertV1OID())
	certID(t, r, signingCertV1OID()).Kids = []*tsatest.Node{octets(t, certHash)}
}

// issuerSerial builds an IssuerSerial for issuerDER and serial.
func issuerSerial(t *testing.T, issuerDER []byte, serial any) *tsatest.Node {
	t.Helper()
	dir := &tsatest.Node{Class: asn1.ClassContextSpecific, Tag: 4, Compound: true, Kids: []*tsatest.Node{tsatest.ParseDER(t, issuerDER)}}
	names := &tsatest.Node{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, Compound: true, Kids: []*tsatest.Node{dir}}
	return &tsatest.Node{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, Compound: true, Kids: []*tsatest.Node{names, tsatest.FromValue(t, serial)}}
}

func TestVerifyAcceptsSigningCertificate(t *testing.T) {
	f := newFixture(t)
	signer := f.auth.SignerCert(t)
	der := signer.Raw
	withAlg := func(sum func([]byte) []byte, oid asn1.ObjectIdentifier) func(t *testing.T, f *fixture) []byte {
		return resigned(func(t *testing.T, _ *fixture, r *tsatest.Node) {
			setV2(t, r, algID(t, oid), octets(t, sum(der)))
		})
	}
	cases := []struct {
		name  string
		build func(t *testing.T, f *fixture) []byte
	}{
		{"v2 without a hash algorithm, as openssl writes it", func(*testing.T, *fixture) []byte { return f.resp }},
		{"v2 with SHA-256 named", withAlg(sum256, sha256OID())},
		{"v2 with SHA-384", withAlg(sum384, sha384())},
		{"v2 with SHA-512", withAlg(sum512, sha512OID())},
		{"v1 with SHA-1", resigned(func(t *testing.T, _ *fixture, r *tsatest.Node) { asV1(t, r, sum1(der)) })},
		{"v2 with a matching issuerSerial", resigned(func(t *testing.T, _ *fixture, r *tsatest.Node) {
			setV2(t, r, octets(t, sum256(der)), issuerSerial(t, signer.RawIssuer, signer.SerialNumber))
		})},
		{"v1 and v2 both present and matching", resigned(func(t *testing.T, _ *fixture, r *tsatest.Node) {
			v1 := tsatest.ParseDER(t, attribute(t, r, signingCertV2OID()).DER())
			v1.Kids[0] = objectID(t, signingCertV1OID())
			v1.At(t, 1, 0, 0, 0).Kids = []*tsatest.Node{octets(t, sum1(der))}
			attrs := signedAttrs(t, r)
			attrs.Kids = append(attrs.Kids, v1)
		})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info, err := tsa.Verify(c.build(t, f), f.digest, f.auth.Roots)
			if err != nil || !info.ChainVerified {
				t.Fatalf("info %+v, err %v", info, err)
			}
		})
	}
}

func TestVerifyRejectsSigningCertificate(t *testing.T) {
	f := newFixture(t)
	signer := f.auth.SignerCert(t)
	der := signer.Raw
	otherDER := tsatest.New(t).SignerCert(t).Raw
	const (
		noHash   = "tsa: signing-certificate hash does not match the signer certificate"
		noSerial = "tsa: signing-certificate issuerSerial does not match the signer certificate"
		bad      = "tsa: malformed signing-certificate attribute"
	)
	v2 := func(fn func(t *testing.T, r *tsatest.Node)) func(t *testing.T, f *fixture) []byte {
		return resigned(func(t *testing.T, _ *fixture, r *tsatest.Node) { fn(t, r) })
	}
	otherIssuer := func(t *testing.T) []byte {
		n := tsatest.ParseDER(t, signer.RawIssuer)
		flipLast(n)
		return n.DER()
	}
	cases := []rejectCase{
		{name: "no signing-certificate attribute", build: resigned(func(t *testing.T, _ *fixture, r *tsatest.Node) {
			dropAttribute(t, r, signingCertV2OID())
		}), want: "tsa: token has no signing-certificate attribute"},
		{name: "v2 hash of another certificate", build: v2(func(t *testing.T, r *tsatest.Node) { setV2(t, r, octets(t, sum256(otherDER))) }), want: noHash},
		{name: "v2 hash made with another algorithm than named", build: v2(func(t *testing.T, r *tsatest.Node) {
			setV2(t, r, algID(t, sha512OID()), octets(t, sum256(der)))
		}), want: noHash},
		{name: "v1 hash of another certificate", build: v2(func(t *testing.T, r *tsatest.Node) { asV1(t, r, sum1(otherDER)) }), want: noHash},
		{name: "v1 hash made with SHA-256", build: v2(func(t *testing.T, r *tsatest.Node) { asV1(t, r, sum256(der)) }), want: noHash},
		{name: "v1 and v2 present, v1 for another certificate", build: v2(func(t *testing.T, r *tsatest.Node) {
			v1 := tsatest.ParseDER(t, attribute(t, r, signingCertV2OID()).DER())
			v1.Kids[0] = objectID(t, signingCertV1OID())
			v1.At(t, 1, 0, 0, 0).Kids = []*tsatest.Node{octets(t, sum1(otherDER))}
			attrs := signedAttrs(t, r)
			attrs.Kids = append(attrs.Kids, v1)
		}), want: noHash},
		{name: "issuerSerial with another serial number", build: v2(func(t *testing.T, r *tsatest.Node) {
			other := signer.SerialNumber.Int64() + 1
			setV2(t, r, octets(t, sum256(der)), issuerSerial(t, signer.RawIssuer, other))
		}), want: noSerial},
		{name: "issuerSerial with another issuer", build: v2(func(t *testing.T, r *tsatest.Node) {
			setV2(t, r, octets(t, sum256(der)), issuerSerial(t, otherIssuer(t), signer.SerialNumber))
		}), want: noSerial},
		{name: "issuerSerial without a serial number", build: v2(func(t *testing.T, r *tsatest.Node) {
			is := issuerSerial(t, signer.RawIssuer, 1)
			is.Kids = is.Kids[:1]
			setV2(t, r, octets(t, sum256(der)), is)
		}), want: bad},
		{name: "issuerSerial serial not an INTEGER", build: v2(func(t *testing.T, r *tsatest.Node) {
			setV2(t, r, octets(t, sum256(der)), issuerSerial(t, signer.RawIssuer, []byte{1}))
		}), want: bad},
		{name: "v2 hash algorithm MD5", build: v2(func(t *testing.T, r *tsatest.Node) {
			setV2(t, r, algID(t, md5OID()), octets(t, sum256(der)))
		}), want: "tsa: unsupported signing-certificate hash algorithm 1.2.840.113549.2.5"},
		{name: "v2 hash algorithm SHA-1", build: v2(func(t *testing.T, r *tsatest.Node) {
			setV2(t, r, algID(t, sha1OID()), octets(t, sum1(der)))
		}), want: "tsa: unsupported signing-certificate hash algorithm 1.3.14.3.2.26"},
		{name: "hash algorithm not an identifier", build: v2(func(t *testing.T, r *tsatest.Node) {
			alg := algID(t, sha256OID())
			alg.Kids[0] = integer(t, 1)
			setV2(t, r, alg, octets(t, sum256(der)))
		}), want: bad},
		{name: "certHash not an OCTET STRING", build: v2(func(t *testing.T, r *tsatest.Node) { setV2(t, r, integer(t, 1)) }), want: bad},
		{name: "certID without fields", build: v2(func(t *testing.T, r *tsatest.Node) { setV2(t, r) }), want: bad},
		{name: "certID not constructed", build: v2(func(t *testing.T, r *tsatest.Node) {
			attribute(t, r, signingCertV2OID()).At(t, 1, 0, 0).Kids[0] = integer(t, 1)
		}), want: bad},
		{name: "no certIDs", build: v2(func(t *testing.T, r *tsatest.Node) { attribute(t, r, signingCertV2OID()).At(t, 1, 0, 0).Kids = nil }), want: bad},
		{name: "empty SigningCertificate", build: v2(func(t *testing.T, r *tsatest.Node) { attribute(t, r, signingCertV2OID()).At(t, 1, 0).Kids = nil }), want: bad},
		{name: "SigningCertificate not constructed", build: v2(func(t *testing.T, r *tsatest.Node) {
			attribute(t, r, signingCertV2OID()).At(t, 1).Kids[0] = integer(t, 1)
		}), want: bad},
		{name: "attribute without values", build: v2(func(t *testing.T, r *tsatest.Node) { attribute(t, r, signingCertV2OID()).At(t, 1).Kids = nil }), want: bad},
	}
	runRejectCases(t, f, cases)
}
