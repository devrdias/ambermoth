package tsa_test

import (
	"crypto/sha256"
	"crypto/x509"
	"github.com/devrdias/ambermoth/internal/tsa"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

// TestTSA builds a throwaway CA + TSA with openssl, has openssl answer a
// request built by tsa.BuildRequest, and verifies the reply with tsa.Verify.
func TestTSA(t *testing.T) {
	a := tsatest.New(t)
	digest := sha256.Sum256([]byte("chain head"))
	reqDER, nonce, err := tsa.BuildRequest(digest[:])
	if err != nil {
		t.Fatal(err)
	}
	resp := a.Reply(t, reqDER)

	info, err := tsa.Verify(resp, digest[:], a.Roots)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !info.ChainVerified || info.Nonce.Cmp(nonce) != 0 || !strings.Contains(info.Signer, "Test TSA") {
		t.Fatalf("unexpected info %+v", info)
	}
	// Wrong digest must fail.
	other := sha256.Sum256([]byte("rewritten chain"))
	if _, err := tsa.Verify(resp, other[:], nil); err == nil {
		t.Fatal("expected imprint mismatch")
	}
	// Untrusted root must fail chain check but pass signature check without roots.
	if _, err := tsa.Verify(resp, digest[:], x509.NewCertPool()); err == nil {
		t.Fatal("expected chain failure with empty root pool")
	}
	if _, err := tsa.Verify(resp, digest[:], nil); err != nil {
		t.Fatalf("signature-only verify failed: %v", err)
	}
	// Flipping a byte in the signature region must fail.
	bad := append([]byte(nil), resp...)
	bad[len(bad)-10] ^= 0xff
	if _, err := tsa.Verify(bad, digest[:], nil); err == nil {
		t.Fatal("expected failure on corrupted token")
	}
}
