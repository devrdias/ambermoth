package canon_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"os"
	"strconv"
	"testing"

	"github.com/devrdias/ambermoth/internal/canon"
)

// corpusLines reads testdata/entries.jsonl: entry lines (with and without
// entry_hash) that the ledger tests, the integration tests and the example
// provider produced, kept as they were written.
func corpusLines(tb testing.TB) [][]byte {
	tb.Helper()
	raw, err := os.ReadFile("testdata/entries.jsonl")
	if err != nil {
		tb.Fatal(err)
	}
	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	if err := sc.Err(); err != nil {
		tb.Fatal(err)
	}
	return lines
}

func entryHash(tb testing.TB, entry map[string]any) string {
	tb.Helper()
	body := make(map[string]any, len(entry))
	for k, v := range entry {
		if k != "entry_hash" {
			body[k] = v
		}
	}
	b, err := canon.Marshal(body)
	if err != nil {
		tb.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// checkRoundTrip parses a canonical line, writes it again and expects the
// same bytes, the same bytes as the reference writer, and a stable hash.
func checkRoundTrip(t *testing.T, line []byte) map[string]any {
	t.Helper()
	v, err := canon.Parse(line)
	if err != nil {
		t.Fatalf("parse %q: %v", line, err)
	}
	again, err := canon.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %q: %v", line, err)
	}
	if !bytes.Equal(again, line) {
		t.Fatalf("round trip changed the line:\nin  %q\nout %q", line, again)
	}
	sameAsReference(t, v)
	entry, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("line is not an object: %q", line)
	}
	first := entryHash(t, entry)
	if second := entryHash(t, entry); first != second {
		t.Fatalf("hash of %q is not stable: %s then %s", line, first, second)
	}
	return entry
}

func TestRecordedEntriesRoundTrip(t *testing.T) {
	t.Parallel()
	lines := corpusLines(t)
	if len(lines) < 100 {
		t.Fatalf("only %d corpus lines", len(lines))
	}
	hashed := 0
	for _, line := range lines {
		entry := checkRoundTrip(t, line)
		claimed, ok := entry["entry_hash"].(string)
		if !ok {
			continue
		}
		hashed++
		if got := entryHash(t, entry); got != claimed {
			t.Errorf("recomputed %s, line says %s: %q", got, claimed, line)
		}
	}
	if hashed == 0 {
		t.Error("no corpus line carries an entry_hash")
	}
}

var purposes = []string{"production-inference", "eval-regression", "eval-benchmark", "rag-indexing", "rag-query", "batch-job", "dev-experiment", "other-declared", "untagged", "x-r\u00e9sum\u00e9"}

var notes = []string{"", "plain note", "quote \" and backslash \\", "tab\tnewline\nreturn\r", "ctl \x01\x1f del \x7f", "<b>&amp;</b>", "caf\u00e9 \u65e5\u672c", "emoji \U0001f600 \U0001f9d1\u200d\U0001f4bb", "sep \u2028\u2029", "bom \ufeff nul \x00"}

type entryGen struct {
	rng *rand.Rand
}

func (g entryGen) pick(options []string) string {
	return options[g.rng.Intn(len(options))]
}

func (g entryGen) hex(n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[g.rng.Intn(len(digits))]
	}
	return string(b)
}

func (g entryGen) count() int {
	switch g.rng.Intn(4) {
	case 0:
		return 0
	case 1:
		return g.rng.Intn(100)
	case 2:
		return g.rng.Intn(1 << 20)
	}
	return 9007199254740991 - g.rng.Intn(3)
}

func (g entryGen) base(seq int, typ string) map[string]any {
	return map[string]any{
		"v": 1, "seq": seq, "type": typ,
		"ts":        "2026-03-0" + strconv.Itoa(1+g.rng.Intn(9)) + "T00:00:00." + strconv.Itoa(100+g.rng.Intn(900)) + "Z",
		"prev_hash": "sha256:" + g.hex(64),
	}
}

func (g entryGen) call(seq int) map[string]any {
	e := g.base(seq, "call")
	e["provider"] = g.pick([]string{"openai", "anthropic", "custom-\u00fc"})
	e["api_style"] = g.pick([]string{"openai", "anthropic"})
	e["endpoint"] = g.pick([]string{"chat.completions", "responses", "messages", "embeddings", "realtime", "other"})
	e["method"] = "POST"
	e["path"] = "/v1/" + g.hex(6)
	e["model"] = g.pick([]string{"", "gpt-4o", "claude-sonnet"})
	e["stream"] = g.rng.Intn(2) == 0
	e["status"] = []int{0, 200, 429, 500, 101}[g.rng.Intn(5)]
	e["tokens_in"], e["tokens_out"], e["cached_in"] = g.count(), g.count(), g.count()
	e["usage_reported"] = g.rng.Intn(2) == 0
	e["latency_ms"] = g.count()
	e["key_fp"] = "sha256:" + g.hex(64)
	e["purpose"] = g.pick(purposes)
	e["salt_id"] = g.hex(32)
	e["prompt_hmac"] = "hmac-sha256:" + g.hex(64)
	e["output_hmac"] = "hmac-sha256:" + g.hex(64)
	e["bytes_in"], e["bytes_out"] = g.count(), g.count()
	e["client_aborted"] = g.rng.Intn(2) == 0
	if g.rng.Intn(2) == 0 {
		e["note"] = g.pick(notes)
		e["git_ref"] = g.hex(40)
	}
	return e
}

func (g entryGen) other(seq int) map[string]any {
	switch g.rng.Intn(6) {
	case 0:
		e := g.base(seq, "amendment")
		e["target_from"], e["target_to"] = g.count(), g.count()
		e["new_purpose"], e["annotation"], e["reason"], e["by"] = g.pick(purposes), g.pick(notes), g.pick(notes), g.pick(notes)
		return e
	case 1:
		e := g.base(seq, "key_event")
		e["action"] = g.pick([]string{"enroll", "rotate", "revoke", "auto_enroll"})
		e["key_fp"], e["provider"], e["label"], e["reason"] = "sha256:"+g.hex(64), "openai", g.pick(notes), g.pick(notes)
		e["replaced_by"] = "sha256:" + g.hex(64)
		return e
	case 2:
		e := g.base(seq, "code_link")
		e["purpose"], e["repo"], e["commit"], e["path"] = g.pick(purposes), "https://example.test/r/"+g.hex(4), g.hex(40), g.pick(notes)
		e["first_commit_ts"], e["commit_count"] = "2026-01-01T00:00:00.000Z", g.count()
		return e
	case 3:
		e := g.base(seq, "anchor")
		e["target_seq"], e["target_hash"], e["tsa"] = g.count(), "sha256:"+g.hex(64), "https://tsa.example.test/tsr"
		e["token_file"], e["token_sha256"], e["gen_time"] = "anchors/"+g.hex(3)+".tsr", g.hex(64), "2026-03-01T00:00:01Z"
		return e
	case 4:
		e := g.base(seq, "gap")
		e["dropped"], e["since"], e["until"], e["cause"] = g.count(), "2026-03-01T00:00:00.000Z", "2026-03-01T00:00:09.000Z", g.pick(notes)
		return e
	}
	e := g.base(seq, "reconciliation")
	e["provider"], e["period_from"], e["period_to"] = "openai", "2026-03-01", "2026-03-31"
	e["source_sha256"], e["days_mismatched"], e["result"] = g.hex(64), g.rng.Intn(31), g.pick([]string{"match", "mismatch"})
	e["ledger_tokens_in"], e["ledger_tokens_out"], e["export_tokens_in"], e["export_tokens_out"] = g.count(), g.count(), g.count(), g.count()
	return e
}

func (g entryGen) entry(seq int) map[string]any {
	if g.rng.Intn(3) > 0 {
		return g.call(seq)
	}
	return g.other(seq)
}

func TestGeneratedEntriesRoundTrip(t *testing.T) {
	t.Parallel()
	g := entryGen{rng: rand.New(rand.NewSource(20260101))}
	for seq := 1; seq <= 400; seq++ {
		e := g.entry(seq)
		e["entry_hash"] = entryHash(t, e)
		line, err := canon.Marshal(e)
		if err != nil {
			t.Fatalf("marshal entry %d: %v", seq, err)
		}
		entry := checkRoundTrip(t, line)
		if entry["entry_hash"] != e["entry_hash"] {
			t.Fatalf("entry %d: hash changed on the way through", seq)
		}
	}
}
