package pack_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/pack"
)

// The golden test builds packs from four stored ledgers and compares the
// SHA-256 of every zip with testdata/golden/packs.json. A change to how a pack
// is built must not change a byte of it.
//
//	go test ./internal/pack -run TestGoldenPacks -update-fixtures   # new ledgers (needs openssl)
//	go test ./internal/pack -run TestGoldenPacks -update            # new hashes
var (
	updateGolden   = flag.Bool("update", false, "rewrite the pack hashes in testdata/golden/packs.json")
	updateFixtures = flag.Bool("update-fixtures", false, "regenerate the ledgers in testdata/golden (needs openssl); implies -update")
)

const goldenDir = "testdata/golden"

// goldenFixtures fill a ledger. Days are from 2026-03-01.
var goldenFixtures = map[string]func(e *env){
	"rich": fillRich, "plain": fillPlain, "anchored": fillAnchored,
}

// crlfFixture is the rich ledger with CRLF line ends and a blank line after every
// fifth line. It verifies, and a pack from it holds the lines as the reader
// sees them.
const crlfFixture = "crlf"

// fillRich has anchors in the middle (one and two TSAs), context entries
// before them, gaps, amendments after the calls they retag, and an unanchored
// tail.
func fillRich(e *env) {
	e.keyEvent(0)
	e.call(1, "anthropic", "alpha")
	e.call(1, "openai", "beta")
	e.link(1, "alpha", "github.com/x/alpha", "0123456789abcdef0123", "2026-01-02T03:04:05Z")
	e.reconcile(1)
	e.anchor(2, 2)
	e.call(3, "anthropic", "alpha")
	e.call(3, "openai", "beta")
	e.keyEvent(4)
	e.call(4, "anthropic", "gamma")
	e.link(4, "gamma", "github.com/x/gamma", "fedcba9876543210fedc", "")
	e.call(5, "openai", "beta")
	e.anchor(6, 1)
	a := e.call(7, "anthropic", "alpha").Seq()
	e.call(7, "openai", "beta")
	c := e.call(8, "anthropic", "gamma").Seq()
	e.gap(8)
	e.call(8, "anthropic", "alpha")
	e.amend(9, a, a, "alpha-v2")
	e.anchor(10, 2)
	e.call(11, "anthropic", "gamma")
	e.call(12, "openai", "beta")
	e.amend(13, c, c, "gamma-v2")
	e.amend(13, 1, 2, "late")
	e.anchor(14, 1)
	e.call(15, "anthropic", "alpha")
	e.gap(16)
	e.call(16, "openai", "beta")
	e.call(17, "anthropic", "alpha")
	e.call(17, "anthropic", "gamma")
}

// fillPlain has no anchor at all, so every pack is the whole ledger, unanchored.
func fillPlain(e *env) {
	e.keyEvent(0)
	e.call(1, "anthropic", "alpha")
	e.call(2, "openai", "beta")
	e.gap(2)
	e.call(3, "anthropic", "alpha")
	e.amend(4, 1, 1, "alpha-v2")
}

// fillAnchored ends on an anchor, so the whole ledger is anchored at the end.
func fillAnchored(e *env) {
	e.keyEvent(0)
	e.call(1, "anthropic", "alpha")
	e.call(2, "anthropic", "beta")
	e.anchor(3, 2)
}

type goldenCase struct {
	name, fixture string
	opts          func(o pack.Options) pack.Options
	disclose      []int64
}

func whole(o pack.Options) pack.Options { return o }

func days(from, to int) func(pack.Options) pack.Options {
	return func(o pack.Options) pack.Options { return period(o, from, to) }
}

func provider(p string, f func(pack.Options) pack.Options) func(pack.Options) pack.Options {
	return func(o pack.Options) pack.Options {
		o = f(o)
		o.Provider = p
		return o
	}
}

var goldenCases = []goldenCase{
	{"rich whole", "rich", whole, nil},
	{"rich whole with account", "rich", func(o pack.Options) pack.Options { o.Account = "Acme Ltd"; return o }, nil},
	{"rich whole disclosed", "rich", whole, []int64{31, 2, 31, 3}},
	{"rich provider filter", "rich", provider("openai", whole), nil},
	{"rich first days, end anchored", "rich", days(0, 1), nil},
	{"rich both ends anchored", "rich", days(3, 5), nil},
	{"rich both ends anchored with disclosure", "rich", days(3, 5), []int64{9}},
	{"rich context entries, no later amendment", "rich", days(4, 5), nil},
	{"rich context entries and an amendment after the last call", "rich", days(7, 8), nil},
	{"rich amendment after the last call, disclosed", "rich", days(7, 8), []int64{17}},
	{"rich amendment after the last call, other provider", "rich", provider("openai", days(7, 8)), nil},
	{"rich amendment after the last call, same provider", "rich", provider("anthropic", days(7, 8)), nil},
	{"rich later amendment of earlier calls", "rich", days(1, 1), nil},
	{"rich unanchored end", "rich", days(15, 16), nil},
	{"rich last day only", "rich", days(17, 17), nil},
	{"rich single day with context", "rich", days(4, 4), nil},
	{"rich two TSAs at the end", "rich", days(8, 9), nil},
	{"plain whole", "plain", whole, nil},
	{"plain provider filter", "plain", provider("openai", whole), nil},
	{"plain period", "plain", days(2, 3), []int64{2}},
	{"crlf whole", crlfFixture, whole, []int64{2}},
	{"crlf partial with context", crlfFixture, days(4, 5), []int64{9}},
	{"crlf partial with a later amendment", crlfFixture, days(7, 8), nil},
	{"crlf unanchored end", crlfFixture, days(17, 17), nil},
	{"anchored whole", "anchored", whole, nil},
	{"anchored period", "anchored", days(2, 2), nil},
}

func TestGoldenPacks(t *testing.T) {
	if *updateFixtures {
		writeGoldenFixtures(t)
		*updateGolden = true
	}
	got := map[string]string{}
	for _, c := range goldenCases {
		got[c.name] = buildGolden(t, c)
	}
	path := filepath.Join(goldenDir, "packs.json")
	if *updateGolden {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if got[n] != want[n] {
			t.Errorf("%s: pack sha256 %s, want %s", n, got[n], want[n])
		}
	}
	if len(want) != len(got) {
		t.Errorf("%d golden packs, %d cases", len(want), len(got))
	}
}

func buildGolden(t *testing.T, c goldenCase) string {
	t.Helper()
	out, _ := goldenZip(t, c)
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// goldenZip builds the pack of c from its stored ledger and returns its path
// and the TSA roots of that ledger.
func goldenZip(t *testing.T, c goldenCase) (string, *x509.CertPool) {
	t.Helper()
	dir := t.TempDir()
	copyTree(t, filepath.Join(goldenDir, c.fixture), dir)
	pem, err := os.ReadFile(filepath.Join(dir, "roots.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	now := day0.AddDate(0, 0, 40)
	s, err := ledger.Open(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	o := pack.Options{From: day0, To: day0.AddDate(0, 0, 60), Roots: roots, Generator: "test",
		Out: filepath.Join(t.TempDir(), "pack.zip"), Disclose: c.disclose}
	if _, err := pack.Build(context.Background(), s, c.opts(o)); err != nil {
		t.Fatalf("%s: Build: %v", c.name, err)
	}
	return o.Out, roots
}

func writeGoldenFixtures(t *testing.T) {
	t.Helper()
	for name, fill := range goldenFixtures {
		e := newEnv(t)
		fill(e)
		dst := filepath.Join(goldenDir, name)
		if err := os.RemoveAll(dst); err != nil {
			t.Fatal(err)
		}
		copyTree(t, e.s.Dir, dst)
		if err := os.WriteFile(filepath.Join(dst, "roots.pem"), e.auth.CAPEM, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeCRLFFixture(t)
}

func writeCRLFFixture(t *testing.T) {
	t.Helper()
	src, dst := filepath.Join(goldenDir, "rich"), filepath.Join(goldenDir, crlfFixture)
	if err := os.RemoveAll(dst); err != nil {
		t.Fatal(err)
	}
	copyTree(t, src, dst)
	b, err := os.ReadFile(filepath.Join(src, ledger.LedgerFile))
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	lines := bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
	for i, l := range lines {
		out = append(out, l...)
		out = append(out, "\r\n"...)
		if i%5 == 4 && i < len(lines)-1 {
			out = append(out, "\r\n"...)
		}
	}
	if err := os.WriteFile(filepath.Join(dst, ledger.LedgerFile), out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if info.Name() == "ledger.lock" {
			return nil
		}
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}
