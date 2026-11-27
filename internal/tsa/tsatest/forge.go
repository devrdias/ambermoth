package tsatest

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// SignerCert is the certificate the authority signs tokens with.
func (a *TestAuthority) SignerCert(t testing.TB) *x509.Certificate {
	t.Helper()
	return a.readCert(t, "tsa.crt")
}

func (a *TestAuthority) readCert(t testing.TB, name string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(a.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// Sign returns an RSA PKCS#1 v1.5 SHA-256 signature over data made with the
// signer's key, for tests that rebuild parts of a token.
func (a *TestAuthority) Sign(t testing.TB, data []byte) []byte {
	t.Helper()
	dir := t.TempDir() // per call, so parallel subtests do not share files
	in, out := filepath.Join(dir, "data.bin"), filepath.Join(dir, "data.sig")
	if err := os.WriteFile(in, data, 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, a.dir, "dgst", "-sha256", "-sign", "tsa.key", "-out", out, in)
	sig, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// CertOptions sets what ReissueSigner changes. An empty ExtKeyUsage leaves
// the extension out.
type CertOptions struct {
	NotBefore, NotAfter time.Time
	ExtKeyUsage         []x509.ExtKeyUsage
}

// ReissueSigner returns the DER of a certificate for the signer's key with the
// same subject, serial, issuer and key identifiers but the given validity and
// extended key usage, signed by the authority's root. Swapped into a token it
// still matches the SignerInfo and the signature.
func (a *TestAuthority) ReissueSigner(t testing.TB, o CertOptions) []byte {
	t.Helper()
	signer := a.SignerCert(t)
	ca := a.readCert(t, "ca.crt")
	keyPEM, err := os.ReadFile(filepath.Join(a.dir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(keyPEM)
	caKey, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          new(big.Int).Set(signer.SerialNumber),
		Subject:               signer.Subject,
		SubjectKeyId:          signer.SubjectKeyId,
		NotBefore:             o.NotBefore,
		NotAfter:              o.NotAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           o.ExtKeyUsage,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, signer.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return der
}
