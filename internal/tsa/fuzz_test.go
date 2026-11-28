package tsa_test

import (
	"bytes"
	"crypto/x509"
	"testing"

	"github.com/devrdias/ambermoth/internal/tsa"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

// FuzzVerify feeds the token verifier bytes that are not what the TSA sent. It
// must never panic, and whatever it accepts must say what the good token says:
// the same time, serial and signer, chained to the root, and only for the
// digest the token was issued for.
func FuzzVerify(f *testing.F) {
	good := newFixture(f)
	want, err := tsa.Verify(good.resp, good.digest, good.auth.Roots)
	if err != nil {
		f.Fatalf("the good token does not verify: %v", err)
	}
	root := tsatest.ParseDER(f, good.resp)
	sig, tst := signerInfo(f, root).At(f, siSig).Bytes, tstOctets(f, root).Bytes
	f.Add(good.resp, good.digest)
	f.Add(good.resp, []byte("another digest"))
	f.Add(good.resp[:len(good.resp)-5], good.digest)
	f.Add(good.resp[:20], good.digest)
	f.Add(append(append([]byte(nil), good.resp...), 0), good.digest)
	flips := []int{4, 30, len(good.resp) / 2, len(good.resp) - 20, len(good.resp) - 1}
	for _, at := range flips {
		flipped := append([]byte(nil), good.resp...)
		flipped[at] ^= 0x01
		f.Add(flipped, good.digest)
	}
	f.Add([]byte{0x30, 0x03, 0x02, 0x01, 0x00}, good.digest)
	f.Add([]byte{0x30, 0x80, 0x00, 0x00}, good.digest)
	f.Add([]byte{}, []byte{})
	f.Fuzz(func(t *testing.T, resp, digest []byte) {
		info, err := tsa.Verify(resp, good.digest, good.auth.Roots)
		if err == nil {
			assertSameAttestation(t, info, want)
			// Container tags are not checked (see SECURITY.md), so other bytes can verify,
			// but only with the signature and the TSTInfo that were issued.
			if !bytes.Contains(resp, sig) || !bytes.Contains(resp, tst) {
				t.Fatalf("accepted %d bytes without the issued signature and TSTInfo", len(resp))
			}
		}
		if info, err := tsa.Verify(resp, good.digest, nil); err == nil && info.ChainVerified {
			t.Fatal("a token reported a verified chain when no roots were given")
		}
		if _, err := tsa.Verify(resp, good.digest, x509.NewCertPool()); err == nil {
			t.Fatal("a token verified against a pool with no roots")
		}
		if !bytes.Equal(digest, good.digest) {
			if _, err := tsa.Verify(good.resp, digest, good.auth.Roots); err == nil {
				t.Fatalf("the token was accepted for the digest %x it was not issued for", digest)
			}
		}
	})
}

func assertSameAttestation(t *testing.T, got, want *tsa.TokenInfo) {
	t.Helper()
	switch {
	case !got.ChainVerified:
		t.Fatal("accepted against roots but the chain is not reported as verified")
	case !got.GenTime.Equal(want.GenTime):
		t.Fatalf("accepted a token that says %v, the good one says %v", got.GenTime, want.GenTime)
	case got.Serial.Cmp(want.Serial) != 0 || got.Policy.String() != want.Policy.String():
		t.Fatalf("accepted a token with serial %v policy %v, want %v %v", got.Serial, got.Policy, want.Serial, want.Policy)
	case (got.Nonce == nil) != (want.Nonce == nil) || (got.Nonce != nil && got.Nonce.Cmp(want.Nonce) != 0):
		t.Fatalf("accepted a token with nonce %v, want %v", got.Nonce, want.Nonce)
	case got.Signer != want.Signer || !bytes.Equal(got.SignerCert.Raw, want.SignerCert.Raw):
		t.Fatalf("accepted a token from signer %q, want %q", got.Signer, want.Signer)
	}
}
