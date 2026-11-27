package ops_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/ops"
	"github.com/devrdias/ambermoth/internal/trust"
	"github.com/devrdias/ambermoth/internal/tsa"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

const downURL = "http://127.0.0.1:1/down"

func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func goodTSA(t *testing.T, a *tsatest.TestAuthority) string { return a.Server(t).URL }

func brokenTSA(t *testing.T, status int, body string) string {
	return serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/timestamp-reply")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// staticTSA always answers with token, whatever it was asked.
func staticTSA(t *testing.T, token []byte) string {
	return serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/timestamp-reply")
		_, _ = w.Write(token)
	}))
}

// tokenFor asks the authority for a token over digest with a nonce of its own.
func tokenFor(t *testing.T, a *tsatest.TestAuthority, digest []byte) []byte {
	t.Helper()
	req, _, err := tsa.BuildRequest(digest)
	if err != nil {
		t.Fatal(err)
	}
	return a.Reply(t, req)
}

func headDigest(t *testing.T, s *ledger.Store) []byte {
	t.Helper()
	_, h, err := s.Head()
	if err != nil {
		t.Fatal(err)
	}
	d, err := ledger.HashBytes(h)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func anchorFiles(t *testing.T, s *ledger.Store) []string {
	t.Helper()
	des, err := os.ReadDir(filepath.Join(s.Dir, ledger.AnchorDir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range des {
		names = append(names, d.Name())
	}
	return names
}

func hostPart(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(u.Host, ":", "_")
}

func callStore(t *testing.T) *ledger.Store {
	t.Helper()
	s := newFixedStore(t)
	appendCalls(t, s, []ledgerCall{{day2, "openai", 1, 1}, {day3, "openai", 2, 2}})
	return s
}

type anchorCase struct {
	name string
	// urls builds the TSA list; fresh servers per case.
	urls func(t *testing.T, a *tsatest.TestAuthority, s *ledger.Store) []string
	// recorded lists the positions in urls that end up anchored, in order.
	recorded []int
	// errHas are fragments of the error; none means Anchor succeeds.
	errHas []string
}

func urlsOf(fns ...func(t *testing.T, a *tsatest.TestAuthority, s *ledger.Store) string) func(t *testing.T, a *tsatest.TestAuthority, s *ledger.Store) []string {
	return func(t *testing.T, a *tsatest.TestAuthority, s *ledger.Store) []string {
		var out []string
		for _, f := range fns {
			out = append(out, f(t, a, s))
		}
		return out
	}
}

func good(t *testing.T, a *tsatest.TestAuthority, _ *ledger.Store) string { return goodTSA(t, a) }
func down(*testing.T, *tsatest.TestAuthority, *ledger.Store) string       { return downURL }
func http500(t *testing.T, _ *tsatest.TestAuthority, _ *ledger.Store) string {
	return brokenTSA(t, http.StatusInternalServerError, "boom")
}
func garbage(t *testing.T, _ *tsatest.TestAuthority, _ *ledger.Store) string {
	return brokenTSA(t, http.StatusOK, "not a token")
}

// replayed hands out a genuine token whose nonce is not the one Anchor sent.
func replayed(t *testing.T, a *tsatest.TestAuthority, s *ledger.Store) string {
	return staticTSA(t, tokenFor(t, a, headDigest(t, s)))
}

// otherDigest hands out a genuine token over some other digest.
func otherDigest(t *testing.T, a *tsatest.TestAuthority, _ *ledger.Store) string {
	return staticTSA(t, tokenFor(t, a, make([]byte, 32)))
}

var anchorCases = []anchorCase{
	{name: "one TSA", urls: urlsOf(good), recorded: []int{0}},
	{name: "two TSAs", urls: urlsOf(good, good), recorded: []int{0, 1}},
	{name: "first of two is down", urls: urlsOf(down, good), recorded: []int{1},
		errHas: []string{"some TSAs failed: " + downURL + ": "}},
	{name: "second of two is down", urls: urlsOf(good, down), recorded: []int{0},
		errHas: []string{"some TSAs failed: " + downURL + ": "}},
	{name: "a TSA answering with an HTTP error", urls: urlsOf(http500, good), recorded: []int{1},
		errHas: []string{"some TSAs failed: ", "returned HTTP 500"}},
	{name: "a reply that is not a token", urls: urlsOf(garbage, good), recorded: []int{1},
		errHas: []string{"some TSAs failed: "}},
	{name: "a genuine token with the wrong nonce", urls: urlsOf(replayed, good), recorded: []int{1},
		errHas: []string{"some TSAs failed: ", "nonce does not match"}},
	{name: "a genuine token over another digest", urls: urlsOf(otherDigest, good), recorded: []int{1},
		errHas: []string{"some TSAs failed: "}},
	{name: "both fail", urls: urlsOf(down, http500), recorded: nil,
		errHas: []string{"all TSAs failed: " + downURL + ": ", "; ", "returned HTTP 500"}},
	{name: "the only TSA fails verification", urls: urlsOf(replayed), recorded: nil,
		errHas: []string{"all TSAs failed: ", "nonce does not match"}},
}

func TestAnchorOutcomes(t *testing.T) {
	auth := tsatest.New(t)
	for _, c := range anchorCases {
		t.Run(c.name, func(t *testing.T) {
			s := callStore(t)
			urls := c.urls(t, auth, s)
			before := ledgerBytes(t, s)
			head, headHash, err := s.Head()
			if err != nil {
				t.Fatal(err)
			}

			got, err := ops.Anchor(context.Background(), s, urls, nil)

			if len(c.errHas) == 0 && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, frag := range c.errHas {
				if err == nil || !strings.Contains(err.Error(), frag) {
					t.Errorf("error %v, want it to contain %q", err, frag)
				}
			}
			if len(got) != len(c.recorded) {
				t.Fatalf("returned %d entries, want %d", len(got), len(c.recorded))
			}
			var wantFiles []string
			for n, idx := range c.recorded {
				name := fmt.Sprintf("%d-%d-%s.tsr", head, idx, hostPart(t, urls[idx]))
				wantFiles = append(wantFiles, name)
				e := got[n]
				if e.Str("tsa") != urls[idx] || e.Str("token_file") != name || e.Int("target_seq") != head || e.Str("target_hash") != headHash {
					t.Errorf("entry %d: %v", n, e)
				}
			}
			sort.Strings(wantFiles)
			if files := anchorFiles(t, s); !reflect.DeepEqual(files, wantFiles) {
				t.Errorf("token files %v, want %v", files, wantFiles)
			}
			assertAppendOnly(t, before, ledgerBytes(t, s), len(c.recorded))
			if r := verify(t, s, ops.AnchorVerifier(auth.Roots, false)); !r.OK() {
				t.Errorf("ledger does not verify: %v", r.Errors)
			}
		})
	}
}

func TestAnchorEntryAndToken(t *testing.T) {
	auth := tsatest.New(t)
	s := callStore(t)
	u := goodTSA(t, auth)
	head, headHash, _ := s.Head()
	digest := headDigest(t, s)

	got, err := ops.Anchor(context.Background(), s, []string{u}, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("anchor: %v %v", got, err)
	}
	e := got[0]

	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	wantKeys := []string{"entry_hash", "gen_time", "kind", "prev_hash", "seq", "target_hash", "target_seq", "token_file",
		"token_sha256", "ts", "tsa", "tsa_signer", "type", "v"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Errorf("fields:\n got %v\nwant %v", keys, wantKeys)
	}

	path := filepath.Join(s.Dir, ledger.AnchorDir, e.Str("token_file"))
	token, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v, %v", st.Mode(), err)
	}
	info, err := tsa.Verify(token, digest, auth.Roots)
	if err != nil || !info.ChainVerified {
		t.Fatalf("recorded token does not verify against the head: %v", err)
	}
	checks := []struct {
		field     string
		got, want any
	}{
		{"type", e.Type(), ledger.TypeAnchor},
		{"kind", e.Str("kind"), "rfc3161"},
		{"target_seq", e.Int("target_seq"), head},
		{"target_hash", e.Str("target_hash"), headHash},
		{"seq", e.Seq(), head + 1},
		{"prev_hash", e.Str("prev_hash"), headHash},
		{"ts", e.Str("ts"), ledger.FormatTime(clockAt)},
		{"tsa", e.Str("tsa"), u},
		{"token_sha256", e.Str("token_sha256"), ledger.SHA256Bytes(token)},
		{"gen_time", e.Str("gen_time"), ledger.FormatTime(info.GenTime)},
		{"tsa_signer", e.Str("tsa_signer"), info.Signer},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
	if disk := readLedger(t, s); disk[len(disk)-1].Hash() != e.Hash() {
		t.Error("entry on disk differs from the returned one")
	}
}

// Anchor takes any token that answers its request; whether the signer is
// trusted is decided when the ledger is verified.
func TestAnchorRecordsUnpinnedTSAAndVerifyDecides(t *testing.T) {
	auth := tsatest.New(t)
	s := callStore(t)
	if _, err := ops.Anchor(context.Background(), s, []string{goodTSA(t, auth)}, nil); err != nil {
		t.Fatal(err)
	}
	builtin, err := trust.Pool(nil)
	if err != nil {
		t.Fatal(err)
	}

	r := verify(t, s, ops.AnchorVerifier(auth.Roots, false))
	if !r.OK() || len(r.Anchors) != 1 || !r.Anchors[0].Pinned {
		t.Errorf("pinned: %v %+v", r.Errors, r.Anchors)
	}
	if r := verify(t, s, ops.AnchorVerifier(builtin, false)); r.OK() {
		t.Error("a TSA outside the pinned roots was accepted")
	}
	r = verify(t, s, ops.AnchorVerifier(builtin, true))
	if !r.OK() || len(r.Anchors) != 1 || r.Anchors[0].Pinned {
		t.Errorf("unpinned: %v %+v", r.Errors, r.Anchors)
	}
}

func TestAnchorWithNothingNew(t *testing.T) {
	auth := tsatest.New(t)
	s := callStore(t)
	u := goodTSA(t, auth)
	first, err := ops.Anchor(context.Background(), s, []string{u}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := ledgerBytes(t, s)

	// The head is now the first anchor entry; a second run anchors that.
	second, err := ops.Anchor(context.Background(), s, []string{u}, nil)
	if err != nil || len(second) != 1 {
		t.Fatalf("second anchor: %v %v", second, err)
	}
	if second[0].Int("target_seq") != first[0].Seq() || second[0].Str("target_hash") != first[0].Hash() {
		t.Errorf("second anchor targets %v, want the first anchor entry", second[0])
	}
	assertAppendOnly(t, before, ledgerBytes(t, s), 1)
	if files := anchorFiles(t, s); len(files) != 2 {
		t.Errorf("token files %v, want two", files)
	}
}

func TestAnchorEmptyLedger(t *testing.T) {
	auth := tsatest.New(t)
	s := newFixedStore(t)
	_, genesis, err := s.Head()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ops.Anchor(context.Background(), s, []string{goodTSA(t, auth)}, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("anchor: %v %v", got, err)
	}
	if got[0].Int("target_seq") != 0 || got[0].Str("target_hash") != genesis {
		t.Errorf("want the genesis entry anchored, got %v", got[0])
	}
}

func TestAnchorWithoutTSAs(t *testing.T) {
	s := callStore(t)
	before := ledgerBytes(t, s)
	nobody := [][]string{nil, {}}
	for _, urls := range nobody {
		got, err := ops.Anchor(context.Background(), s, urls, nil)
		if got != nil || err == nil || err.Error() != "no TSA configured" {
			t.Errorf("got %v, %v", got, err)
		}
	}
	assertAppendOnly(t, before, ledgerBytes(t, s), 0)
}

func TestAnchorCancelledContext(t *testing.T) {
	auth := tsatest.New(t)
	s := callStore(t)
	before := ledgerBytes(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := ops.Anchor(ctx, s, []string{goodTSA(t, auth)}, nil)
	if got != nil || err == nil || !strings.Contains(err.Error(), "all TSAs failed") || !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("got %v, %v", got, err)
	}
	assertAppendOnly(t, before, ledgerBytes(t, s), 0)
}

func TestAnchorStopsWhenTokenCannotBeStored(t *testing.T) {
	auth := tsatest.New(t)
	s := callStore(t)
	dir := filepath.Join(s.Dir, ledger.AnchorDir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := ledgerBytes(t, s)
	got, err := ops.Anchor(context.Background(), s, []string{goodTSA(t, auth), goodTSA(t, auth)}, nil)
	var pe *fs.PathError
	if len(got) != 0 || !errors.As(err, &pe) {
		t.Errorf("got %v, %v; want no entries and a path error", got, err)
	}
	assertAppendOnly(t, before, ledgerBytes(t, s), 0)
}

func TestAnchorUsesTheGivenClient(t *testing.T) {
	auth := tsatest.New(t)
	s := callStore(t)
	var seen int
	client := &http.Client{Timeout: time.Minute, Transport: countingTransport{n: &seen, next: http.DefaultTransport}}
	if _, err := ops.Anchor(context.Background(), s, []string{goodTSA(t, auth)}, client); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Errorf("client used %d times, want 1", seen)
	}
}

type countingTransport struct {
	n    *int
	next http.RoundTripper
}

func (c countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*c.n++
	return c.next.RoundTrip(r)
}
