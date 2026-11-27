package tsa_test

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/tsa"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

// rejectCase is a token the verifier must refuse. want is the whole error
// text, or only its start when prefix is set (the rest comes from encoding/asn1
// or crypto/x509).
type rejectCase struct {
	name   string
	build  func(t *testing.T, f *fixture) []byte
	want   string
	prefix bool
}

func runRejectCases(t *testing.T, f *fixture, cases []rejectCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			info, err := tsa.Verify(c.build(t, f), f.digest, f.auth.Roots)
			if err == nil {
				t.Fatalf("accepted a bad token: %+v", info)
			}
			ok := err.Error() == c.want
			if c.prefix {
				ok = strings.HasPrefix(err.Error(), c.want)
			}
			if !ok {
				t.Fatalf("error = %q; want %q (prefix %v)", err, c.want, c.prefix)
			}
		})
	}
}

func TestVerifyRejectsBrokenEncoding(t *testing.T) {
	f := newFixture(t)
	cut := func(n *tsatest.Node) { // last child loses its final bytes, the parent keeps a valid header
		last := n.Kids[len(n.Kids)-1]
		last.Raw = last.DER()[:len(last.DER())-3]
	}
	plant := func(n *tsatest.Node) { n.Kids = append(n.Kids, &tsatest.Node{Raw: garbage()}) }
	cases := []rejectCase{
		{name: "empty input", build: func(*testing.T, *fixture) []byte { return nil }, want: "not a TimeStampResp: ", prefix: true},
		{name: "truncated response", build: func(_ *testing.T, f *fixture) []byte { return f.resp[:len(f.resp)-5] }, want: "not a TimeStampResp: ", prefix: true},
		{name: "trailing garbage after the response", build: func(_ *testing.T, f *fixture) []byte { return append(append([]byte(nil), f.resp...), garbage()...) }, want: "not a TimeStampResp: trailing data"},
		{name: "garbage inside the response", build: editor(func(_ *testing.T, r *tsatest.Node) { plant(r) }), want: "not a TimeStampResp: ", prefix: true},
		{name: "truncated ContentInfo", build: editor(func(t *testing.T, r *tsatest.Node) { cut(r.At(t, 1)) }), want: "malformed ContentInfo"},
		{name: "garbage inside ContentInfo", build: editor(func(t *testing.T, r *tsatest.Node) { plant(r.At(t, 1)) }), want: "malformed ContentInfo"},
		{name: "garbage after SignedData in its wrapper", build: editor(func(t *testing.T, r *tsatest.Node) { plant(r.At(t, 1, 1)) }), want: "malformed SignedData wrapper"},
		{name: "truncated SignedData", build: editor(func(t *testing.T, r *tsatest.Node) { cut(signedData(t, r)) }), want: "malformed SignedData"},
		{name: "garbage inside SignedData", build: editor(func(t *testing.T, r *tsatest.Node) { plant(signedData(t, r)) }), want: "malformed SignedData"},
		{name: "garbage inside encapContentInfo", build: editor(func(t *testing.T, r *tsatest.Node) { plant(encapContent(t, r)) }), want: "missing encapsulated TSTInfo"},
		{name: "truncated TSTInfo", build: editor(func(t *testing.T, r *tsatest.Node) { editTST(t, r, cut) }), want: "malformed TSTInfo"},
		{name: "garbage inside TSTInfo", build: editor(func(t *testing.T, r *tsatest.Node) { editTST(t, r, plant) }), want: "malformed TSTInfo"},
		{name: "trailing garbage after TSTInfo", build: editor(func(t *testing.T, r *tsatest.Node) {
			oct := tstOctets(t, r)
			oct.Bytes = append(oct.Bytes, garbage()...)
		}), want: "malformed TSTInfo"},
	}
	runRejectCases(t, f, cases)
}

func TestVerifyRejectsBadStructure(t *testing.T) {
	f := newFixture(t)
	cases := []rejectCase{
		{name: "empty TimeStampResp", build: editor(func(_ *testing.T, r *tsatest.Node) { r.Kids = nil }), want: "not a TimeStampResp: ", prefix: true},
		{name: "empty PKIStatusInfo", build: editor(func(_ *testing.T, r *tsatest.Node) { r.Kids[0].Kids = nil }), want: "malformed PKIStatusInfo"},
		{name: "PKIStatus not an integer", build: editor(func(t *testing.T, r *tsatest.Node) { r.Kids[0].Kids[0] = octets(t, []byte{0}) }), want: "asn1: ", prefix: true},
		{name: "granted status without a token", build: editor(func(_ *testing.T, r *tsatest.Node) { r.Kids = r.Kids[:1] }), want: "TimeStampResp has no token"},
		{name: "ContentInfo of another type", build: editor(func(t *testing.T, r *tsatest.Node) { r.At(t, 1).Kids[0] = objectID(t, idDataOID()) }), want: "token is not CMS SignedData"},
		{name: "ContentInfo type not an OID", build: editor(func(t *testing.T, r *tsatest.Node) { r.At(t, 1).Kids[0] = null(t) }), want: "token is not CMS SignedData"},
		{name: "ContentInfo with one element", build: editor(func(t *testing.T, r *tsatest.Node) { r.At(t, 1).Kids = r.At(t, 1).Kids[:1] }), want: "malformed ContentInfo"},
		{name: "ContentInfo with three elements", build: editor(func(t *testing.T, r *tsatest.Node) { r.At(t, 1).Kids = append(r.At(t, 1).Kids, null(t)) }), want: "malformed ContentInfo"},
		{name: "token not constructed", build: editor(func(t *testing.T, r *tsatest.Node) { r.Kids[1] = integer(t, 1) }), want: "malformed ContentInfo"},
		{name: "empty SignedData wrapper", build: editor(func(t *testing.T, r *tsatest.Node) { r.At(t, 1, 1).Kids = nil }), want: "malformed SignedData wrapper"},
		{name: "two SignedData in the wrapper", build: editor(func(t *testing.T, r *tsatest.Node) {
			w := r.At(t, 1, 1)
			w.Kids = append(w.Kids, w.Kids[0])
		}), want: "malformed SignedData wrapper"},
		{name: "SignedData with three fields", build: editor(func(t *testing.T, r *tsatest.Node) { signedData(t, r).Kids = signedData(t, r).Kids[:3] }), want: "malformed SignedData"},
		{name: "SignedData not constructed", build: editor(func(t *testing.T, r *tsatest.Node) { r.At(t, 1, 1).Kids[0] = integer(t, 3) }), want: "malformed SignedData"},
		{name: "no encapsulated TSTInfo", build: editor(func(t *testing.T, r *tsatest.Node) { encapContent(t, r).Kids = encapContent(t, r).Kids[:1] }), want: "missing encapsulated TSTInfo"},
		{name: "encapsulated content of another type", build: editor(func(t *testing.T, r *tsatest.Node) { encapContent(t, r).Kids[0] = objectID(t, idDataOID()) }), want: "encapsulated content is not TSTInfo"},
		{name: "eContent not an OCTET STRING", build: editor(func(t *testing.T, r *tsatest.Node) { r.At(t, 1, 1, 0, 2, 1).Kids[0] = null(t) }), want: "malformed eContent"},
	}
	runRejectCases(t, f, cases)
}

func TestVerifyRejectsBadTSTInfo(t *testing.T) {
	f := newFixture(t)
	tst := func(fn func(t *testing.T, tst *tsatest.Node)) func(t *testing.T, f *fixture) []byte {
		return editor(func(t *testing.T, r *tsatest.Node) { editTST(t, r, func(n *tsatest.Node) { fn(t, n) }) })
	}
	cases := []rejectCase{
		{name: "TSTInfo with four fields", build: tst(func(_ *testing.T, n *tsatest.Node) { n.Kids = n.Kids[:4] }), want: "malformed TSTInfo"},
		{name: "TSTInfo not constructed", build: tst(func(t *testing.T, n *tsatest.Node) { *n = *integer(t, 1) }), want: "malformed TSTInfo"},
		{name: "policy not an OID", build: tst(func(t *testing.T, n *tsatest.Node) { n.Kids[1] = integer(t, 1) }), want: "asn1: ", prefix: true},
		{name: "message imprint not a sequence", build: tst(func(t *testing.T, n *tsatest.Node) { n.Kids[2] = integer(t, 1) }), want: "asn1: ", prefix: true},
		{name: "serial not an integer", build: tst(func(t *testing.T, n *tsatest.Node) { n.Kids[3] = null(t) }), want: "asn1: ", prefix: true},
		{name: "genTime not a GeneralizedTime", build: tst(func(t *testing.T, n *tsatest.Node) { n.Kids[4] = integer(t, 1) }), want: "genTime: ", prefix: true},
		{name: "imprint algorithm is not SHA-256", build: tst(func(t *testing.T, n *tsatest.Node) { n.At(t, 2, 0, 0).Bytes = objectID(t, sha384OID()).Bytes }), want: "token's message imprint does not match the anchored hash"},
	}
	runRejectCases(t, f, cases)
}

func TestVerifyRejectsWrongImprint(t *testing.T) {
	f := newFixture(t)
	other := sha256.Sum256([]byte("rewritten chain"))
	short := f.digest[:31]
	cases := []struct {
		name   string
		digest []byte
	}{
		{name: "digest differs from the token's", digest: other[:]},
		{name: "digest one byte short", digest: short},
		{name: "no digest", digest: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := tsa.Verify(f.resp, c.digest, f.auth.Roots)
			if err == nil || err.Error() != "token's message imprint does not match the anchored hash" {
				t.Fatalf("err = %v; want imprint mismatch", err)
			}
		})
	}
}

func TestVerifyRejectsRefusedStatus(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{name: "rejection", status: 2, want: "TSA refused the request (status 2)"},
		{name: "waiting", status: 3, want: "TSA refused the request (status 3)"},
		{name: "revocation warning", status: 4, want: "TSA refused the request (status 4)"},
		{name: "revocation notification", status: 5, want: "TSA refused the request (status 5)"},
		{name: "negative status", status: -1, want: "TSA refused the request (status -1)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			// The token is still there and good; only the status says no.
			resp := f.edit(t, func(r *tsatest.Node) { r.Kids[0].Kids[0] = integer(t, c.status) })
			_, err := tsa.Verify(resp, f.digest, f.auth.Roots)
			if err == nil || err.Error() != c.want {
				t.Fatalf("err = %v; want %q", err, c.want)
			}
		})
	}
}

func TestVerifyRejectsBadSignerInfo(t *testing.T) {
	f := newFixture(t)
	other := tsatest.New(t)
	si := func(fn func(t *testing.T, si *tsatest.Node)) func(t *testing.T, f *fixture) []byte {
		return editor(func(t *testing.T, r *tsatest.Node) { fn(t, signerInfo(t, r)) })
	}
	const noCert = "signing certificate not included in token (request certReq=true)"
	cases := []rejectCase{
		{name: "no certificates in the token", build: editor(func(t *testing.T, r *tsatest.Node) {
			sd := signedData(t, r)
			sd.Kids = append(sd.Kids[:3], sd.Kids[4:]...)
		}), want: noCert},
		{name: "certificates of another authority", build: editor(func(t *testing.T, r *tsatest.Node) {
			certSet(t, r).Kids = []*tsatest.Node{{Raw: other.SignerCert(t).Raw}}
		}), want: noCert},
		{name: "certificate that does not parse", build: editor(func(t *testing.T, r *tsatest.Node) {
			certSet(t, r).Kids = []*tsatest.Node{integer(t, 1)}
		}), want: noCert},
		{name: "subject key id names no certificate", build: si(func(_ *testing.T, si *tsatest.Node) {
			si.Kids[siSID] = &tsatest.Node{Class: 2, Tag: 0, Bytes: []byte{1, 2, 3}}
		}), want: noCert},
		{name: "issuerAndSerial malformed", build: si(func(t *testing.T, si *tsatest.Node) { si.Kids[siSID] = tsatest.FromValue(t, []int{1}) }), want: "asn1: ", prefix: true},
		{name: "no SignerInfos field", build: editor(func(t *testing.T, r *tsatest.Node) { signedData(t, r).Kids = signedData(t, r).Kids[:4] }), want: "expected exactly one SignerInfo"},
		{name: "empty SignerInfos", build: editor(func(t *testing.T, r *tsatest.Node) { signerInfos(t, r).Kids = nil }), want: "expected exactly one SignerInfo"},
		{name: "two SignerInfos", build: editor(func(t *testing.T, r *tsatest.Node) {
			set := signerInfos(t, r)
			set.Kids = append(set.Kids, set.Kids[0])
		}), want: "expected exactly one SignerInfo"},
		{name: "SignerInfo with four fields", build: si(func(_ *testing.T, si *tsatest.Node) { si.Kids = si.Kids[:4] }), want: "malformed SignerInfo"},
		{name: "SignerInfo not constructed", build: editor(func(t *testing.T, r *tsatest.Node) { signerInfos(t, r).Kids[0] = integer(t, 1) }), want: "malformed SignerInfo"},
		{name: "SignerInfo without a signature", build: si(func(_ *testing.T, si *tsatest.Node) { si.Kids = si.Kids[:siSig] }), want: "malformed SignerInfo"},
		{name: "digest algorithm not an identifier", build: si(func(t *testing.T, si *tsatest.Node) { si.Kids[siDigAlg] = integer(t, 1) }), want: "asn1: ", prefix: true},
		{name: "unsupported digest algorithm", build: si(func(t *testing.T, si *tsatest.Node) {
			si.At(t, siDigAlg).Kids[0] = objectID(t, md5OID())
		}), want: "unsupported digest algorithm 1.2.840.113549.2.5"},
		{name: "signature algorithm not an identifier", build: si(func(t *testing.T, si *tsatest.Node) { si.Kids[siSigAlg] = integer(t, 1) }), want: "asn1: ", prefix: true},
		{name: "unsupported signature algorithm", build: si(func(t *testing.T, si *tsatest.Node) {
			si.At(t, siSigAlg).Kids[0] = objectID(t, md5OID())
		}), want: "unsupported signature algorithm 1.2.840.113549.2.5/2.16.840.1.101.3.4.2.1"},
		{name: "signature not an OCTET STRING", build: si(func(t *testing.T, si *tsatest.Node) { si.Kids[siSig] = integer(t, 1) }), want: "asn1: ", prefix: true},
	}
	runRejectCases(t, f, cases)
}

func TestVerifyRejectsBadSignedAttributes(t *testing.T) {
	f := newFixture(t)
	cases := []rejectCase{
		{name: "attribute not a sequence", build: editor(func(t *testing.T, r *tsatest.Node) { signedAttrs(t, r).Kids[0] = integer(t, 1) }), want: "asn1: ", prefix: true},
		{name: "messageDigest value not an OCTET STRING", build: editor(func(t *testing.T, r *tsatest.Node) {
			attribute(t, r, messageDigestOID()).At(t, 1).Kids[0] = integer(t, 1)
		}), want: "CMS messageDigest: ", prefix: true},
		{name: "content-type value not an OID", build: editor(func(t *testing.T, r *tsatest.Node) {
			attribute(t, r, contentTypeOID()).At(t, 1).Kids[0] = integer(t, 1)
		}), want: "CMS content-type: ", prefix: true},
		{name: "messageDigest differs from the content", build: editor(func(t *testing.T, r *tsatest.Node) {
			flipLast(attribute(t, r, messageDigestOID()))
		}), want: "CMS messageDigest does not match TSTInfo"},
		{name: "messageDigest missing", build: editor(func(t *testing.T, r *tsatest.Node) {
			dropAttribute(t, r, messageDigestOID())
		}), want: "CMS messageDigest does not match TSTInfo"},
		{name: "content-type names another type", build: editor(func(t *testing.T, r *tsatest.Node) {
			attribute(t, r, contentTypeOID()).At(t, 1).Kids[0] = objectID(t, idDataOID())
		}), want: "CMS content-type attribute is not TSTInfo"},
		{name: "content-type missing", build: editor(func(t *testing.T, r *tsatest.Node) {
			dropAttribute(t, r, contentTypeOID())
		}), want: "CMS content-type attribute is not TSTInfo"},
	}
	runRejectCases(t, f, cases)
}

func TestVerifyRejectsBadSignature(t *testing.T) {
	f := newFixture(t)
	cases := []rejectCase{
		{name: "flipped byte in the signature", build: editor(func(t *testing.T, r *tsatest.Node) { flipLast(signerInfo(t, r).At(t, siSig)) }), want: "TSA signature invalid: ", prefix: true},
		{name: "flipped byte in a signed attribute", build: editor(func(t *testing.T, r *tsatest.Node) {
			flipLast(attribute(t, r, signingCertV2OID()))
		}), want: "TSA signature invalid: ", prefix: true},
		{name: "flipped byte in the TSTInfo", build: editor(func(t *testing.T, r *tsatest.Node) {
			editTST(t, r, func(n *tsatest.Node) { n.Kids[3].Bytes[len(n.Kids[3].Bytes)-1] ^= 0x01 })
		}), want: "CMS messageDigest does not match TSTInfo"},
		{name: "TSTInfo changed and messageDigest recomputed", build: editor(func(t *testing.T, r *tsatest.Node) {
			editTST(t, r, func(n *tsatest.Node) { n.Kids[3].Bytes[len(n.Kids[3].Bytes)-1] ^= 0x01 })
			sum := sha256.Sum256(tstOctets(t, r).Bytes)
			attribute(t, r, messageDigestOID()).At(t, 1).Kids[0] = octets(t, sum[:])
		}), want: "TSA signature invalid: ", prefix: true},
	}
	runRejectCases(t, f, cases)
}
