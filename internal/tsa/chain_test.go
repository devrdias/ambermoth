package tsa_test

import (
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/tsa"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

// withSigner swaps the signer certificate in the token for one with the same
// key and names but the given validity and key usage. The signing-certificate
// attribute is rewritten for it and the token signed again, so it still verifies.
func (f *fixture) withSigner(t *testing.T, o tsatest.CertOptions) []byte {
	t.Helper()
	der := f.auth.ReissueSigner(t, o)
	return f.edit(t, func(r *tsatest.Node) {
		certSet(t, r).Kids = []*tsatest.Node{{Raw: der}}
		sum := sha256.Sum256(der)
		certID(t, r, signingCertV2OID()).Kids[0] = octets(t, sum[:])
		resign(t, f, r)
	})
}

func (f *fixture) genTime(t *testing.T) time.Time {
	t.Helper()
	info, err := tsa.Verify(f.resp, f.digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	return info.GenTime
}

func TestVerifyChain(t *testing.T) {
	f := newFixture(t)
	gen := f.genTime(t)
	stamping := []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping}
	valid := tsatest.CertOptions{NotBefore: gen.Add(-time.Hour), NotAfter: gen.Add(time.Hour), ExtKeyUsage: stamping}

	t.Run("accepts a re-issued signer with a window around genTime", func(t *testing.T) {
		// Control for the cases below: only the field they change may fail.
		info, err := tsa.Verify(f.withSigner(t, valid), f.digest, f.auth.Roots)
		if err != nil || !info.ChainVerified {
			t.Fatalf("info %+v, err %v", info, err)
		}
	})

	other := tsatest.New(t)
	expired := valid
	expired.NotBefore, expired.NotAfter = gen.Add(-48*time.Hour), gen.Add(-24*time.Hour)
	early := valid
	early.NotBefore, early.NotAfter = gen.Add(24*time.Hour), gen.Add(48*time.Hour)
	wrongUsage := valid
	wrongUsage.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}

	noUsage := valid
	noUsage.ExtKeyUsage = nil
	anyUsage := valid
	anyUsage.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny}

	cases := []struct {
		name  string
		resp  []byte
		roots *x509.CertPool
		want  string
	}{
		{name: "signer chains to another root", resp: f.resp, roots: other.Roots, want: "unknown authority"},
		{name: "empty root pool", resp: f.resp, roots: x509.NewCertPool(), want: "unknown authority"},
		{name: "signer expired at genTime", resp: f.withSigner(t, expired), roots: f.auth.Roots, want: "is after"},
		{name: "signer not yet valid at genTime", resp: f.withSigner(t, early), roots: f.auth.Roots, want: "is before"},
		{name: "signer allowed only for server auth", resp: f.withSigner(t, wrongUsage), roots: f.auth.Roots, want: "incompatible key usage"},
		{name: "signer without an extended key usage", resp: f.withSigner(t, noUsage), roots: f.auth.Roots, want: "no timeStamping extended key usage"},
		{name: "signer allowed for any usage", resp: f.withSigner(t, anyUsage), roots: f.auth.Roots, want: "no timeStamping extended key usage"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := tsa.Verify(c.resp, f.digest, c.roots)
			if !errors.Is(err, tsa.ErrUntrustedChain) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v; want ErrUntrustedChain containing %q", err, c.want)
			}
		})
	}
}
