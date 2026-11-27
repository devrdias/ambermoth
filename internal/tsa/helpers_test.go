package tsa_test

import (
	"crypto/sha256"
	"encoding/asn1"
	"math/big"
	"testing"

	"github.com/devrdias/ambermoth/internal/tsa"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

// fixture is a good token from an openssl TSA plus what it was made for.
type fixture struct {
	auth   *tsatest.TestAuthority
	digest []byte
	nonce  *big.Int
	resp   []byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	auth := tsatest.New(t)
	sum := sha256.Sum256([]byte("chain head"))
	reqDER, nonce, err := tsa.BuildRequest(sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{auth: auth, digest: sum[:], nonce: nonce, resp: auth.Reply(t, reqDER)}
}

// edit decodes the good token, applies fn and encodes it again.
func (f *fixture) edit(t *testing.T, fn func(root *tsatest.Node)) []byte {
	t.Helper()
	root := tsatest.ParseDER(t, f.resp)
	fn(root)
	return root.DER()
}

// editor returns a builder that edits the good token with fn.
func editor(fn func(t *testing.T, root *tsatest.Node)) func(t *testing.T, f *fixture) []byte {
	return func(t *testing.T, f *fixture) []byte {
		t.Helper()
		return f.edit(t, func(root *tsatest.Node) { fn(t, root) })
	}
}

// Paths into a TimeStampResp: status, then ContentInfo { type, [0] SignedData }.
func signedData(t *testing.T, r *tsatest.Node) *tsatest.Node { return r.At(t, 1, 1, 0) }

func encapContent(t *testing.T, r *tsatest.Node) *tsatest.Node { return r.At(t, 1, 1, 0, 2) }

func tstOctets(t *testing.T, r *tsatest.Node) *tsatest.Node { return r.At(t, 1, 1, 0, 2, 1, 0) }

func certSet(t *testing.T, r *tsatest.Node) *tsatest.Node { return r.At(t, 1, 1, 0, 3) }

func signerInfos(t *testing.T, r *tsatest.Node) *tsatest.Node { return r.At(t, 1, 1, 0, 4) }

func signerInfo(t *testing.T, r *tsatest.Node) *tsatest.Node { return r.At(t, 1, 1, 0, 4, 0) }

// SignerInfo fields: version, sid, digestAlgorithm, [0] signedAttrs, signatureAlgorithm, signature.
const (
	siSID    = 1
	siDigAlg = 2
	siAttrs  = 3
	siSigAlg = 4
	siSig    = 5
)

func signedAttrs(t *testing.T, r *tsatest.Node) *tsatest.Node {
	t.Helper()
	return signerInfo(t, r).At(t, siAttrs)
}

// attribute returns the signed attribute of the given type.
func attribute(t *testing.T, r *tsatest.Node, typ asn1.ObjectIdentifier) *tsatest.Node {
	t.Helper()
	for _, a := range signedAttrs(t, r).Kids {
		var got asn1.ObjectIdentifier
		if _, err := asn1.Unmarshal(a.Kids[0].DER(), &got); err == nil && got.Equal(typ) {
			return a
		}
	}
	t.Fatalf("token has no signed attribute %v", typ)
	return nil
}

// certID returns the first certID of the signing-certificate attribute of the given type.
func certID(t *testing.T, r *tsatest.Node, typ asn1.ObjectIdentifier) *tsatest.Node {
	t.Helper()
	return attribute(t, r, typ).At(t, 1, 0, 0, 0)
}

// dropAttribute removes the signed attribute of the given type.
func dropAttribute(t *testing.T, r *tsatest.Node, typ asn1.ObjectIdentifier) {
	t.Helper()
	attrs := signedAttrs(t, r)
	drop := attribute(t, r, typ)
	var keep []*tsatest.Node
	for _, a := range attrs.Kids {
		if a != drop {
			keep = append(keep, a)
		}
	}
	attrs.Kids = keep
}

// editTST decodes the TSTInfo inside the token, applies fn and writes it back.
func editTST(t *testing.T, r *tsatest.Node, fn func(tst *tsatest.Node)) {
	t.Helper()
	oct := tstOctets(t, r)
	tst := oct.Inner(t)
	fn(tst)
	oct.SetInner(tst)
}

// flipLast flips a bit in the last byte under n.
func flipLast(n *tsatest.Node) {
	for len(n.Kids) > 0 {
		n = n.Kids[len(n.Kids)-1]
	}
	n.Bytes[len(n.Bytes)-1] ^= 0x01
}

func objectID(t *testing.T, o asn1.ObjectIdentifier) *tsatest.Node {
	t.Helper()
	return tsatest.FromValue(t, o)
}

func integer(t *testing.T, v int) *tsatest.Node {
	t.Helper()
	return tsatest.FromValue(t, v)
}

func null(t *testing.T) *tsatest.Node {
	t.Helper()
	return tsatest.FromValue(t, asn1.NullRawValue)
}

func octets(t *testing.T, b []byte) *tsatest.Node {
	t.Helper()
	return tsatest.FromValue(t, b)
}

func md5OID() asn1.ObjectIdentifier         { return asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 5} }
func idDataOID() asn1.ObjectIdentifier      { return asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1} }
func sha384OID() asn1.ObjectIdentifier      { return asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 2} }
func contentTypeOID() asn1.ObjectIdentifier { return asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3} }
func messageDigestOID() asn1.ObjectIdentifier {
	return asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
}
func signingCertV1OID() asn1.ObjectIdentifier {
	return asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 12}
}
func sha512OID() asn1.ObjectIdentifier { return asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 3} }
func sha256OID() asn1.ObjectIdentifier { return asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1} }
func sha1OID() asn1.ObjectIdentifier   { return asn1.ObjectIdentifier{1, 3, 14, 3, 2, 26} }
func signingCertV2OID() asn1.ObjectIdentifier {
	return asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 47}
}

// garbage is not valid DER: a constructed private tag with a long-form length
// that runs past the end.
func garbage() []byte { return []byte{0xde, 0xad, 0xbe, 0xef} }
