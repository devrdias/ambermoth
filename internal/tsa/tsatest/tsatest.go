// Package tsatest provides an openssl-backed RFC 3161 authority for tests.
package tsatest

import (
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestAuthority is an openssl-backed TSA for tests.
type TestAuthority struct {
	dir   string
	Roots *x509.CertPool
	CAPEM []byte
}

const tsCfg = `
[ tsa ]
default_tsa = tsa_config1
[ tsa_config1 ]
serial = $dir/serial
crypto_device = builtin
default_policy = 1.2.3.4.1
digests = sha256
accuracy = secs:1
ordering = no
tsa_name = no
ess_cert_id_chain = no
ess_cert_id_alg = sha256
signer_digest = sha256
[ v3_tsa ]
basicConstraints = CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = critical, timeStamping
[ v3_ca ]
basicConstraints = critical, CA:TRUE
keyUsage = critical, keyCertSign
`

func run(t testing.TB, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("openssl", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("openssl %v: %v\n%s", args, err, out)
	}
}

func New(t testing.TB) *TestAuthority {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not available")
	}
	dir := t.TempDir()
	cfg := strings.ReplaceAll(tsCfg, "$dir", dir)
	os.WriteFile(filepath.Join(dir, "ts.cnf"), []byte(cfg), 0o600)
	os.WriteFile(filepath.Join(dir, "serial"), []byte("01\n"), 0o600)
	run(t, dir, "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes",
		"-keyout", "ca.key", "-out", "ca.crt", "-subj", "/CN=Test Root", "-days", "30",
		"-config", "ts.cnf", "-extensions", "v3_ca")
	run(t, dir, "req", "-newkey", "rsa:2048", "-nodes", "-keyout", "tsa.key", "-out", "tsa.csr", "-subj", "/CN=Test TSA")
	run(t, dir, "x509", "-req", "-in", "tsa.csr", "-CA", "ca.crt", "-CAkey", "ca.key", "-CAcreateserial",
		"-out", "tsa.crt", "-days", "30", "-extfile", "ts.cnf", "-extensions", "v3_tsa")
	caPEM, _ := os.ReadFile(filepath.Join(dir, "ca.crt"))
	pool := x509.NewCertPool()
	blk, _ := pem.Decode(caPEM)
	ca, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pool.AddCert(ca)
	return &TestAuthority{dir: dir, Roots: pool, CAPEM: caPEM}
}

// Reply answers a DER TimeStampReq.
func (a *TestAuthority) Reply(t testing.TB, reqDER []byte) []byte {
	t.Helper()
	q := filepath.Join(a.dir, "q.tsq")
	os.WriteFile(q, reqDER, 0o600)
	run(t, a.dir, "ts", "-reply", "-config", "ts.cnf", "-queryfile", "q.tsq",
		"-inkey", "tsa.key", "-signer", "tsa.crt", "-out", "r.tsr")
	b, err := os.ReadFile(filepath.Join(a.dir, "r.tsr"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Server starts an HTTP TSA endpoint backed by this authority.
func (a *TestAuthority) Server(t testing.TB) *httptest.Server {
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/timestamp-reply")
		w.Write(a.Reply(t, b))
	}))
	t.Cleanup(srv.Close)
	return srv
}
