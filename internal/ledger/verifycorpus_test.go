package ledger_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/canon"
	"github.com/devrdias/ambermoth/internal/ledger"
)

// The verify corpus pins what Verify reports for a set of good and damaged
// ledgers. Each case has a golden report in testdata/verify-corpus. Run
// `go test ./internal/ledger -run TestVerifyCorpus -update` to rewrite them
// and read the diff: a change in a golden is a change in what verify says.
var updateGolden = flag.Bool("update", false, "rewrite the verify corpus golden files")

const corpusDir = "testdata/verify-corpus"

// corpusInput is one ledger as bytes, with the anchor tokens it refers to and
// the token checker to run it with (nil skips the cryptographic check).
type corpusInput struct {
	data   []byte
	tokens map[string][]byte
	check  ledger.AnchorVerifier
}

type corpusCase struct {
	name  string
	build func(t *testing.T) corpusInput
}

// fakeAnchorCheck reads the TSA time from a token written by corpusBuilder.
// Any other token is rejected.
func fakeAnchorCheck(tok []byte, _ string) (ledger.AnchorCheck, error) {
	gen, ok := strings.CutPrefix(string(tok), "gen=")
	if !ok {
		return ledger.AnchorCheck{}, errors.New("token rejected")
	}
	at, err := time.Parse(ledger.TimeLayout, gen)
	if err != nil {
		return ledger.AnchorCheck{}, err
	}
	return ledger.AnchorCheck{GenTime: at, Signer: "Test TSA", Pinned: true}, nil
}

// corpusBuilder writes a ledger through the Store with a fixed key, ledger id
// and timestamps, so the bytes are the same on every run.
type corpusBuilder struct {
	t       *testing.T
	s       *ledger.Store
	pending []ledger.Entry
	hashes  []string
	tokens  map[string][]byte
}

func newCorpusBuilder(t *testing.T) *corpusBuilder {
	t.Helper()
	dir := t.TempDir()
	key := bytes.Repeat([]byte{0xab}, 32)
	g := ledger.Entry{
		"v": ledger.FormatVersion, "seq": 0, "ts": ledger.FormatTime(epoch), "type": ledger.TypeGenesis,
		"prev_hash": ledger.ZeroHash, "ledger_id": "00000000-0000-4000-8000-000000000001",
		"hmac_key_commitment": ledger.KeyCommitment(key), "generator": "ambermoth/corpus",
	}
	h, err := ledger.ComputeHash(g)
	if err != nil {
		t.Fatal(err)
	}
	g["entry_hash"] = h
	line, err := canon.Marshal(map[string]any(g))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, keyPath(dir), []byte(hex.EncodeToString(key)+"\n"))
	writeFile(t, ledgerPath(dir), append(line, '\n'))
	s, err := ledger.Open(dir, fixedClock(epoch))
	if err != nil {
		t.Fatal(err)
	}
	return &corpusBuilder{t: t, s: s, hashes: []string{h}, tokens: map[string][]byte{}}
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (b *corpusBuilder) next() int64 { return int64(len(b.hashes) + len(b.pending)) }

// put queues e, timestamped at after the epoch unless it carries a ts already.
func (b *corpusBuilder) put(at time.Duration, e ledger.Entry) {
	if _, ok := e["ts"]; !ok {
		e["ts"] = ledger.FormatTime(epoch.Add(at))
	}
	b.pending = append(b.pending, e)
}

func (b *corpusBuilder) flush() {
	b.t.Helper()
	if len(b.pending) == 0 {
		return
	}
	if err := b.s.Append(b.pending...); err != nil {
		b.t.Fatal(err)
	}
	for _, e := range b.pending {
		b.hashes = append(b.hashes, e.Hash())
	}
	b.pending = nil
}

func (b *corpusBuilder) hashAt(seq int64) string {
	b.flush()
	if seq < 0 || seq >= int64(len(b.hashes)) {
		return ledger.ZeroHash
	}
	return b.hashes[seq]
}

// anchor queues an anchor of target whose token says the TSA time is gen.
func (b *corpusBuilder) anchor(target int64, at, gen time.Duration, tweak func(ledger.Entry)) {
	b.anchorToken(target, at, []byte("gen="+ledger.FormatTime(epoch.Add(gen))), tweak)
}

func (b *corpusBuilder) anchorToken(target int64, at time.Duration, tok []byte, tweak func(ledger.Entry)) {
	name := fmt.Sprintf("anchor-%d.tsr", b.next())
	b.tokens[name] = tok
	e := ledger.Entry{
		"type": ledger.TypeAnchor, "target_seq": target, "target_hash": b.hashAt(target),
		"token_file": name, "token_sha256": ledger.SHA256Bytes(tok), "tsa": "test",
	}
	if tweak != nil {
		tweak(e)
	}
	b.put(at, e)
}

// input flushes and returns the ledger as written so far.
func (b *corpusBuilder) input(check ledger.AnchorVerifier) corpusInput {
	b.flush()
	return corpusInput{data: readBytes(b.t, b.s.LedgerPath()), tokens: b.tokens, check: check}
}

func minutes(n int) time.Duration { return time.Duration(n) * time.Minute }

func callEntry(i int) ledger.Entry {
	return ledger.Entry{
		"type": ledger.TypeCall, "provider": "openai", "model": fmt.Sprintf("model-%d", i%3),
		"purpose": "support", "status": int64(200), "tokens_in": int64(i * 3), "tokens_out": int64(i),
		"key_fp": "sha256:" + strings.Repeat("0", 63) + fmt.Sprint(i%10),
	}
}

func keyEvent() ledger.Entry {
	return ledger.Entry{"type": ledger.TypeKeyEvent, "action": "rotate", "key_fp": ledger.KeyFingerprint("k1"),
		"provider": "openai", "label": "prod", "reason": "scheduled"}
}

func codeLink() ledger.Entry {
	return ledger.Entry{"type": ledger.TypeCodeLink, "purpose": "support", "repo": "https://example.com/r.git",
		"commit": strings.Repeat("c", 40), "first_commit_ts": "2026-01-01T00:00:00.000Z", "commit_count": int64(12),
		"path": ".", "dirty": false}
}

func amendment(from, to int64) ledger.Entry {
	return ledger.Entry{"type": ledger.TypeAmendment, "target_from": from, "target_to": to,
		"reason": "wrong purpose", "by": "ops", "new_purpose": "research"}
}

func gapEntry(dropped int64) ledger.Entry {
	return ledger.Entry{"type": ledger.TypeGap, "dropped": dropped, "since": ledger.FormatTime(epoch),
		"until": ledger.FormatTime(epoch.Add(time.Minute)), "cause": "disk full"}
}

// fillBasic adds seq 1 to 7: every entry type but anchor, no anchors.
func fillBasic(b *corpusBuilder) {
	b.put(minutes(1), callEntry(1))
	b.put(minutes(2), callEntry(2))
	b.put(minutes(3), keyEvent())
	b.put(minutes(4), codeLink())
	b.put(minutes(5), amendment(1, 2))
	b.put(minutes(6), gapEntry(7))
	b.put(minutes(7), callEntry(7))
}

// fillAnchored adds seq 8 to 12: an anchor of seq 5, calls, an anchor of
// seq 10 and one call after it.
func fillAnchored(b *corpusBuilder) {
	b.anchor(5, minutes(8), minutes(8), nil)
	b.put(minutes(9), callEntry(9))
	b.put(minutes(10), callEntry(10))
	b.anchor(10, minutes(11), minutes(11), nil)
	b.put(minutes(12), callEntry(12))
}

func basicInput(t *testing.T) corpusInput {
	b := newCorpusBuilder(t)
	fillBasic(b)
	return b.input(fakeAnchorCheck)
}

func anchoredInput(t *testing.T) corpusInput {
	b := newCorpusBuilder(t)
	fillBasic(b)
	fillAnchored(b)
	return b.input(fakeAnchorCheck)
}

// anchoredWith builds the basic ledger and lets fn add what follows.
func anchoredWith(fn func(b *corpusBuilder)) func(t *testing.T) corpusInput {
	return func(t *testing.T) corpusInput {
		b := newCorpusBuilder(t)
		fillBasic(b)
		fn(b)
		return b.input(fakeAnchorCheck)
	}
}

// largeEntry is the entry at seq i of the large ledger.
func largeEntry(i int) ledger.Entry {
	switch {
	case i%401 == 0:
		return gapEntry(int64(i % 13))
	case i%503 == 0:
		return amendment(int64(i-10), int64(i-1))
	case i%211 == 0:
		return codeLink()
	case i%97 == 0:
		return keyEvent()
	}
	return callEntry(i)
}

const largeEntries = 3400

func largeInput(t *testing.T) corpusInput {
	b := newCorpusBuilder(t)
	for i := 1; i < largeEntries; i++ {
		if i%300 == 0 {
			b.anchor(int64(i-1), minutes(i), minutes(i)+30*time.Second, nil)
			continue
		}
		b.put(minutes(i), largeEntry(i))
	}
	return b.input(fakeAnchorCheck)
}

// withField returns a tweak that sets one field of an entry.
func withField(k string, v any) func(ledger.Entry) {
	return func(e ledger.Entry) { e[k] = v }
}

func anchorCases() []corpusCase {
	last := func(b *corpusBuilder) { b.anchor(7, minutes(8), minutes(8), nil) }
	several := func(b *corpusBuilder) {
		b.anchor(3, minutes(8), minutes(8), nil)
		b.anchor(5, minutes(9), minutes(9), nil)
		b.anchor(7, minutes(10), minutes(10), nil)
	}
	return []corpusCase{
		{"anchor-one-last", anchoredWith(last)},
		{"anchor-several-last", anchoredWith(several)},
		{"anchor-not-last-with-tail", anchoredInput},
		{"anchor-token-file-missing", func(t *testing.T) corpusInput {
			in := anchoredWith(last)(t)
			in.tokens = map[string][]byte{}
			return in
		}},
		{"anchor-token-hash-wrong", anchoredWith(func(b *corpusBuilder) {
			b.anchor(7, minutes(8), minutes(8), withField("token_sha256", ledger.SHA256Bytes([]byte("other"))))
		})},
		{"anchor-target-hash-wrong", anchoredWith(func(b *corpusBuilder) {
			b.anchor(7, minutes(8), minutes(8), withField("target_hash", ledger.SHA256Bytes([]byte("other"))))
		})},
		{"anchor-target-seq-outside-range", anchoredWith(func(b *corpusBuilder) {
			b.anchor(7, minutes(8), minutes(8), withField("target_seq", int64(99)))
		})},
		{"anchor-without-target-fields", anchoredWith(func(b *corpusBuilder) {
			b.anchor(7, minutes(8), minutes(8), func(e ledger.Entry) { delete(e, "target_seq"); delete(e, "target_hash") })
		})},
		{"anchor-check-nil", func(t *testing.T) corpusInput {
			in := anchoredInput(t)
			in.check = nil
			return in
		}},
		{"anchor-check-rejects-token", anchoredWith(func(b *corpusBuilder) {
			b.anchorToken(7, minutes(8), []byte("not a token"), nil)
		})},
		{"anchor-after-entries-clock-ahead", anchoredWith(func(b *corpusBuilder) {
			b.anchor(7, minutes(8), minutes(3), nil)
		})},
		{"anchor-lag-over-24h", anchoredWith(func(b *corpusBuilder) {
			b.anchor(7, minutes(8), 48*time.Hour, nil)
		})},
		{"anchor-token-file-with-directory", anchoredWith(func(b *corpusBuilder) {
			b.anchor(7, minutes(8), minutes(8), func(e ledger.Entry) { e["token_file"] = "../elsewhere/" + e.Str("token_file") })
		})},
		{"anchor-duplicate-target", anchoredWith(func(b *corpusBuilder) {
			b.anchor(7, minutes(8), minutes(8), nil)
			b.anchor(7, minutes(9), minutes(9), nil)
		})},
	}
}

func timeCases() []corpusCase {
	at := func(name string, e ledger.Entry, when time.Duration) corpusCase {
		return corpusCase{name, anchoredWith(func(b *corpusBuilder) { b.put(when, e) })}
	}
	return []corpusCase{
		at("time-goes-backwards", callEntry(8), minutes(1)),
		at("time-backwards-within-tolerance", callEntry(8), minutes(7)-3*time.Second),
		at("time-unparseable", ledger.Entry{"type": ledger.TypeCall, "ts": "yesterday"}, 0),
		{"gap-entries-sum-dropped-calls", anchoredWith(func(b *corpusBuilder) {
			b.put(minutes(8), gapEntry(3))
			b.put(minutes(9), ledger.Entry{"type": ledger.TypeGap})
			b.put(minutes(10), gapEntry(1000000))
		})},
	}
}

func cleanCases() []corpusCase {
	return []corpusCase{
		{"clean-small", basicInput},
		{"clean-small-anchored", anchoredInput},
		{"clean-large-mixed", largeInput},
		{"genesis-only", func(t *testing.T) corpusInput { return newCorpusBuilder(t).input(fakeAnchorCheck) }},
	}
}

// lineEdit changes the lines of a ledger that was built clean.
type lineEdit func(t *testing.T, lines [][]byte) [][]byte

func edited(name string, edit lineEdit) corpusCase {
	return corpusCase{name, func(t *testing.T) corpusInput {
		in := anchoredInput(t)
		in.data = joinLines(edit(t, splitLines(in.data)))
		return in
	}}
}

func splitLines(b []byte) [][]byte {
	return bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
}

func joinLines(lines [][]byte) []byte {
	if len(lines) == 0 {
		return nil
	}
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

// withLine replaces line i with fn(line).
func withLine(i int, fn func(t *testing.T, line []byte) []byte) lineEdit {
	return func(t *testing.T, lines [][]byte) [][]byte {
		lines[i] = fn(t, lines[i])
		return lines
	}
}

func literal(s string) lineEdit {
	return withLine(4, func(*testing.T, []byte) []byte { return []byte(s) })
}

func replaceText(i int, old, repl string) lineEdit {
	return withLine(i, func(t *testing.T, line []byte) []byte {
		if !bytes.Contains(line, []byte(old)) {
			t.Fatalf("line %d has no %q", i, old)
		}
		return bytes.Replace(line, []byte(old), []byte(repl), 1)
	})
}

// rewrite changes fields of line i and seals it again, so the only fault is
// the one fn makes.
func rewrite(i int, fn func(e ledger.Entry)) lineEdit {
	return withLine(i, func(t *testing.T, line []byte) []byte {
		v, err := canon.Parse(line)
		if err != nil {
			t.Fatal(err)
		}
		e := ledger.Entry(v.(map[string]any))
		fn(e)
		delete(e, "entry_hash")
		h, err := ledger.ComputeHash(e)
		if err != nil {
			t.Fatal(err)
		}
		e["entry_hash"] = h
		out, err := canon.Marshal(map[string]any(e))
		if err != nil {
			t.Fatal(err)
		}
		return out
	})
}

func dropLine(i int) lineEdit {
	return func(_ *testing.T, lines [][]byte) [][]byte {
		return append(lines[:i:i], lines[i+1:]...)
	}
}

func swapLines(i, j int) lineEdit {
	return func(_ *testing.T, lines [][]byte) [][]byte {
		lines[i], lines[j] = lines[j], lines[i]
		return lines
	}
}

func duplicateLine(i int) lineEdit {
	return func(_ *testing.T, lines [][]byte) [][]byte {
		out := append([][]byte{}, lines[:i+1]...)
		return append(out, lines[i:]...)
	}
}

func fromLine(i int) lineEdit {
	return func(_ *testing.T, lines [][]byte) [][]byte { return lines[i:] }
}

func truncateLast(n int) lineEdit {
	return func(_ *testing.T, lines [][]byte) [][]byte {
		last := lines[len(lines)-1]
		lines[len(lines)-1] = last[:len(last)-n]
		return lines
	}
}

// insertBlank puts text on its own line after line i.
func insertBlank(i int, text string) lineEdit {
	return func(_ *testing.T, lines [][]byte) [][]byte {
		out := append([][]byte{}, lines[:i+1]...)
		out = append(out, []byte(text))
		return append(out, lines[i+1:]...)
	}
}

func everyLine(fn func(line []byte) []byte) lineEdit {
	return func(_ *testing.T, lines [][]byte) [][]byte {
		for i := range lines {
			lines[i] = fn(lines[i])
		}
		return lines
	}
}

func inOrder(edits ...lineEdit) lineEdit {
	return func(t *testing.T, lines [][]byte) [][]byte {
		for _, e := range edits {
			lines = e(t, lines)
		}
		return lines
	}
}

// movePairToEnd puts the first key of a canonical line last.
func movePairToEnd(line []byte) []byte {
	body := bytes.TrimSuffix(bytes.TrimPrefix(line, []byte("{")), []byte("}"))
	first, rest, _ := bytes.Cut(body, []byte(`,"`))
	return []byte("{\"" + string(rest) + "," + string(first) + "}")
}

func damageCases() []corpusCase {
	return []corpusCase{
		edited("damage-byte-flipped-in-value", replaceText(2, `"provider":"openai"`, `"provider":"openaj"`)),
		edited("damage-byte-flipped-breaks-json", replaceText(3, `{"`, `x"`)),
		edited("damage-line-deleted", dropLine(3)),
		edited("damage-anchor-target-line-deleted", dropLine(5)),
		edited("damage-first-line-deleted", dropLine(0)),
		edited("damage-last-line-deleted", dropLine(12)),
		edited("damage-two-lines-swapped", swapLines(3, 4)),
		edited("damage-line-duplicated", duplicateLine(4)),
		edited("damage-last-line-truncated", truncateLast(20)),
		edited("damage-blank-lines-in-middle", inOrder(insertBlank(2, ""), insertBlank(6, "   "))),
		edited("damage-line-is-array", literal(`[1,2]`)),
		edited("damage-line-is-number", literal(`42`)),
		edited("damage-line-is-null", literal(`null`)),
		edited("damage-line-is-partial-json", literal(`{"seq":`)),
		edited("damage-whitespace-in-line", withLine(3, func(_ *testing.T, l []byte) []byte {
			return bytes.ReplaceAll(l, []byte(`","`), []byte(`", "`))
		})),
		edited("damage-key-order", withLine(3, func(_ *testing.T, l []byte) []byte { return movePairToEnd(l) })),
		edited("damage-trailing-spaces", everyLine(func(l []byte) []byte { return append(l, "  "...) })),
		edited("damage-crlf-line-endings", everyLine(func(l []byte) []byte { return append(l, '\r') })),
		edited("damage-entry-hash-wrong", withLine(3, func(_ *testing.T, l []byte) []byte {
			return replaceHashValue(l, "sha256:"+strings.Repeat("a", 64))
		})),
		edited("damage-prev-hash-wrong", rewrite(4, withField("prev_hash", "sha256:"+strings.Repeat("b", 64)))),
		edited("damage-seq-jump", rewrite(4, withField("seq", int64(40)))),
		edited("damage-required-field-missing", rewrite(3, func(e ledger.Entry) { delete(e, "v") })),
		edited("damage-genesis-seq-not-zero", rewrite(0, withField("seq", int64(1)))),
		edited("damage-genesis-without-ledger-id", rewrite(0, func(e ledger.Entry) { delete(e, "ledger_id") })),
		edited("damage-genesis-entry-hash-wrong", withLine(0, func(_ *testing.T, l []byte) []byte {
			return replaceHashValue(l, "sha256:"+strings.Repeat("c", 64))
		})),
		edited("segment-from-mid-chain", fromLine(4)),
		edited("segment-from-second-entry", fromLine(1)),
		edited("segment-from-last-entry", fromLine(12)),
		edited("empty-input", func(*testing.T, [][]byte) [][]byte { return nil }),
		edited("blank-lines-only", func(*testing.T, [][]byte) [][]byte { return [][]byte{[]byte(""), []byte("  ")} }),
	}
}

// replaceHashValue sets the entry_hash of a canonical line without sealing it.
func replaceHashValue(line []byte, hash string) []byte {
	const key = `"entry_hash":"`
	i := bytes.Index(line, []byte(key)) + len(key)
	j := i + bytes.IndexByte(line[i:], '"')
	return append(append(append([]byte{}, line[:i]...), hash...), line[j:]...)
}

func corpus() []corpusCase {
	all := cleanCases()
	all = append(all, anchorCases()...)
	all = append(all, timeCases()...)
	all = append(all, damageCases()...)
	all = append(all, intentCorpus()...)
	return append(all, largeDamageCases()...)
}

// largeDamageCases breaks the large ledger in the middle.
func largeDamageCases() []corpusCase {
	broken := func(name string, edit lineEdit) corpusCase {
		return corpusCase{name, func(t *testing.T) corpusInput {
			in := largeInput(t)
			in.data = joinLines(edit(t, splitLines(in.data)))
			return in
		}}
	}
	return []corpusCase{
		broken("large-line-deleted", dropLine(1700)),
		broken("large-segment-from-mid-chain", fromLine(1700)),
		broken("large-anchor-target-flipped", replaceText(2999, `"provider":"openai"`, `"provider":"openaj"`)),
	}
}

// projectReport renders a report as indented JSON with sorted keys. Gaps is
// json:"-" on the report, so it is added here.
func projectReport(t *testing.T, r *ledger.Report) []byte {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	m := map[string]any{}
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	gaps := r.Gaps
	if gaps == nil {
		gaps = []ledger.Entry{}
	}
	m["gaps"] = gaps
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// verifyCorpusInput reads in as the verify command does and runs Verify.
func verifyCorpusInput(t *testing.T, in corpusInput) *ledger.Report {
	t.Helper()
	es, lines, err := ledger.Read(bytes.NewReader(in.data))
	if err != nil {
		t.Fatal(err)
	}
	return ledger.Verify(es, lines, corpusOptions(in))
}

func corpusOptions(in corpusInput) ledger.VerifyOptions {
	return ledger.VerifyOptions{CheckAnchor: in.check, LoadToken: func(name string) ([]byte, error) {
		tok, ok := in.tokens[name]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return tok, nil
	}}
}

func goldenPath(name string) string { return filepath.Join(corpusDir, name+".json") }

func compareGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := goldenPath(name)
	if *updateGolden {
		if err := os.MkdirAll(corpusDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no golden file (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("report differs from %s\n%s", path, firstDifference(got, want))
	}
}

func firstDifference(got, want []byte) string {
	g, w := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		if i >= len(g) || i >= len(w) || g[i] != w[i] {
			return fmt.Sprintf("first difference at line %d\n got: %s\nwant: %s", i+1, lineAt(g, i), lineAt(w, i))
		}
	}
	return "no difference"
}

func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return "(end of file)"
}

func TestVerifyCorpus(t *testing.T) {
	for _, c := range corpus() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			compareGolden(t, c.name, projectReport(t, verifyCorpusInput(t, c.build(t))))
		})
	}
}

func TestVerifyCorpusHasNoStaleGoldens(t *testing.T) {
	if *updateGolden {
		t.Skip("rewriting goldens")
	}
	want := map[string]bool{}
	for _, c := range corpus() {
		want[c.name+".json"] = true
	}
	files, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	var stale []string
	for _, f := range files {
		if !want[f.Name()] {
			stale = append(stale, f.Name())
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("golden files with no case: %v", stale)
	}
}

// A segment checked inside a longer read, the way a pack does it, must give
// the report that the same lines give on their own.
func TestVerifyCorpusSegmentInsideLongerLedger(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) corpusInput
		from  int
	}{
		{"small", anchoredInput, 4},
		{"large", largeInput, 1700},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := c.build(t)
			es, lines, err := ledger.Read(bytes.NewReader(in.data))
			if err != nil {
				t.Fatal(err)
			}
			inSlice := ledger.Verify(es[c.from:], lines[c.from:], corpusOptions(in))
			alone := verifyCorpusInput(t, corpusInput{data: joinLines(lines[c.from:]), tokens: in.tokens, check: in.check})
			if got, want := projectReport(t, inSlice), projectReport(t, alone); !bytes.Equal(got, want) {
				t.Errorf("sub-slice report differs from the report of the same lines read alone\n%s", firstDifference(got, want))
			}
			if inSlice.StartsAtGenesis {
				t.Error("a segment from mid-chain reports a genesis start")
			}
		})
	}
}

// verifyReaderCorpusInput runs VerifyReader over in.
func verifyReaderCorpusInput(t *testing.T, in corpusInput) *ledger.Report {
	t.Helper()
	r, err := ledger.VerifyReader(bytes.NewReader(in.data), corpusOptions(in))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// VerifyReader must give the golden report of every corpus case, byte for
// byte. It never rewrites a golden.
func TestVerifyReaderCorpus(t *testing.T) {
	for _, c := range corpus() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := projectReport(t, verifyReaderCorpusInput(t, c.build(t)))
			want, err := os.ReadFile(goldenPath(c.name))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("VerifyReader report differs from %s\n%s", goldenPath(c.name), firstDifference(got, want))
			}
		})
	}
}
