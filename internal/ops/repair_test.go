package ops_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/ops"
)

const wideProvider = "pro€vider😀"

// repairFixture is a ledger of a genesis and three calls, split into what
// precedes its last line and the last line itself (with its newline).
type repairFixture struct {
	dir    string
	before []byte // everything up to the last line
	last   []byte // the last line, newline included
	prevTS string // ts of the entry before the last
}

func newRepairFixture(t *testing.T) repairFixture {
	t.Helper()
	s := newFixedStore(t)
	appendCalls(t, s, []ledgerCall{
		{ts: "2026-03-10T08:00:00.000Z", provider: "openai", in: 10, out: 2},
		{ts: "2026-03-10T08:01:00.000Z", provider: "openai", in: 11, out: 3},
		{ts: "2026-03-10T08:02:00.000Z", provider: wideProvider, in: 12, out: 4},
	})
	b := ledgerBytes(t, s)
	i := bytes.LastIndexByte(b[:len(b)-1], '\n') + 1
	return repairFixture{dir: s.Dir, before: b[:i], last: b[i:], prevTS: "2026-03-10T08:01:00.000Z"}
}

func (f repairFixture) path() string { return filepath.Join(f.dir, ledger.LedgerFile) }

func (f repairFixture) write(t *testing.T, data []byte) {
	t.Helper()
	if err := os.WriteFile(f.path(), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// tear leaves the ledger with only the first n bytes of its last line.
func (f repairFixture) tear(t *testing.T, n int) []byte {
	t.Helper()
	f.write(t, append(append([]byte(nil), f.before...), f.last[:n]...))
	return f.last[:n]
}

func (f repairFixture) bytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(f.path())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f repairFixture) sidecars(t *testing.T) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(f.dir, ledger.LedgerFile+".torn-*"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(m)
	return m
}

// names lists everything in the ledger directory.
func (f repairFixture) names(t *testing.T) []string {
	t.Helper()
	es, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func (f repairFixture) verify(t *testing.T) *ledger.Report {
	t.Helper()
	fh, err := os.Open(f.path())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	r, err := ledger.VerifyReader(fh, ledger.VerifyOptions{AnchorDir: filepath.Join(f.dir, ledger.AnchorDir)})
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK() {
		t.Fatalf("ledger does not verify: %v", r.Errors)
	}
	return r
}

func (f repairFixture) repair(t *testing.T, dryRun bool) *ops.RepairResult {
	t.Helper()
	r, err := ops.Repair(f.dir, fixedClock(clockAt), dryRun)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	return r
}

func (f repairFixture) refused(t *testing.T) error {
	t.Helper()
	_, err := ops.Repair(f.dir, fixedClock(clockAt), false)
	if !errors.Is(err, ops.ErrRepairRefused) {
		t.Fatalf("Repair = %v, want ErrRepairRefused", err)
	}
	return err
}

func (f repairFixture) assertUntouched(t *testing.T, want []byte, names []string) {
	t.Helper()
	if !bytes.Equal(f.bytes(t), want) {
		t.Error("ledger bytes changed")
	}
	if got := f.names(t); strings.Join(got, ",") != strings.Join(names, ",") {
		t.Errorf("directory changed: %v, was %v", got, names)
	}
}

func gapEntries(t *testing.T, f repairFixture) []ledger.Entry {
	t.Helper()
	es, _, err := ledger.ReadFile(f.path())
	if err != nil {
		t.Fatal(err)
	}
	var out []ledger.Entry
	for _, e := range es {
		if e.Type() == ledger.TypeGap {
			out = append(out, e)
		}
	}
	return out
}

func wantCause(frag []byte, afterSeq int64, sidecar string) string {
	return "torn final line removed by repair: " + strconv.Itoa(len(frag)) + " bytes, " + ledger.SHA256Bytes(frag) +
		", after seq " + strconv.FormatInt(afterSeq, 10) + ", saved as " + sidecar
}

type cutCase struct {
	name string
	at   func(last []byte) int
}

func cutCases() []cutCase {
	return []cutCase{
		{"one byte", func([]byte) int { return 1 }},
		{"in the middle of the JSON", func(l []byte) int { return len(l) / 2 }},
		{"in the middle of the entry hash", func(l []byte) int { return bytes.Index(l, []byte(`"entry_hash":"sha256:`)) + 40 }},
		{"before the closing brace", func(l []byte) int { return len(l) - 2 }},
		{"inside a multi-byte character", func(l []byte) int { return bytes.Index(l, []byte("€")) + 1 }},
		{"inside a four-byte character", func(l []byte) int { return bytes.Index(l, []byte("😀")) + 2 }},
	}
}

func TestRepairCutsATornFinalLine(t *testing.T) {
	for _, c := range cutCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newRepairFixture(t)
			frag := append([]byte(nil), f.tear(t, c.at(f.last))...)

			res := f.repair(t, false)

			if res.Action != ops.RepairCut || res.DryRun {
				t.Fatalf("result %+v", res)
			}
			after := f.bytes(t)
			if !bytes.HasPrefix(after, f.before) {
				t.Fatal("bytes before the cut changed")
			}
			assertAppendOnly(t, f.before, after, 1)
			r := f.verify(t)
			if r.LastSeq != 3 || r.DroppedCalls != 1 {
				t.Errorf("report last seq %d dropped %d", r.LastSeq, r.DroppedCalls)
			}
			side := f.sidecars(t)
			if len(side) != 1 {
				t.Fatalf("sidecars %v", side)
			}
			got, err := os.ReadFile(side[0])
			if err != nil || !bytes.Equal(got, frag) {
				t.Errorf("sidecar holds %q (%v), want the removed %q", got, err, frag)
			}
			if runtime.GOOS != "windows" {
				if st, _ := os.Stat(side[0]); st.Mode().Perm() != 0o600 {
					t.Errorf("sidecar mode %v", st.Mode().Perm())
				}
			}
			gaps := gapEntries(t, f)
			if len(gaps) != 1 {
				t.Fatalf("%d gap entries", len(gaps))
			}
			g := gaps[0]
			if g.Seq() != 3 || g.Int("dropped") != 1 || g.Str("since") != f.prevTS {
				t.Errorf("gap %v", g)
			}
			if want := wantCause(frag, 2, filepath.Base(side[0])); g.Str("cause") != want {
				t.Errorf("cause %q, want %q", g.Str("cause"), want)
			}
			if res.RemovedBytes != len(frag) || res.RemovedSHA256 != ledger.SHA256Bytes(frag) ||
				res.Sidecar != filepath.Base(side[0]) || res.LastSeq != 2 || res.Gaps != 1 {
				t.Errorf("result %+v", res)
			}
			if len(frag) > 20 && bytes.Contains(after[len(f.before):], frag) {
				t.Error("the gap entry quotes the removed line")
			}
		})
	}
}

func TestRepairAddsMissingNewlineWithoutDroppingTheEntry(t *testing.T) {
	t.Parallel()
	f := newRepairFixture(t)
	f.write(t, append(append([]byte(nil), f.before...), bytes.TrimSuffix(f.last, []byte("\n"))...))
	if _, err := ledger.Open(f.dir, fixedClock(clockAt)); err == nil {
		t.Fatal("precondition: Open accepts a last line with no newline")
	}

	res := f.repair(t, false)

	if res.Action != ops.RepairNewline || res.Gaps != 0 || res.LastSeq != 3 {
		t.Fatalf("result %+v", res)
	}
	want := append(append([]byte(nil), f.before...), f.last...)
	if !bytes.Equal(f.bytes(t), want) {
		t.Error("ledger is not the original with its newline")
	}
	if len(f.sidecars(t)) != 0 {
		t.Error("a sidecar was written")
	}
	f.verify(t)
	if _, err := ledger.Open(f.dir, fixedClock(clockAt)); err != nil {
		t.Errorf("Open after repair: %v", err)
	}
}

func TestRepairAddsMissingNewlineToAGenesisOnlyLedger(t *testing.T) {
	t.Parallel()
	s := newFixedStore(t)
	b := ledgerBytes(t, s)
	if err := os.WriteFile(s.LedgerPath(), bytes.TrimSuffix(b, []byte("\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := ops.Repair(s.Dir, fixedClock(clockAt), false)
	if err != nil || res.Action != ops.RepairNewline {
		t.Fatalf("result %+v, error %v", res, err)
	}
	if !bytes.Equal(ledgerBytes(t, s), b) {
		t.Error("ledger is not the original")
	}
}

func TestRepairRefusesWhatIsNotATornLine(t *testing.T) {
	cases := []struct {
		name  string
		build func(f repairFixture) []byte
	}{
		{"whole JSON that fails its hash", func(f repairFixture) []byte {
			bad := bytes.Replace(f.last, []byte(`"tokens_in":12`), []byte(`"tokens_in":13`), 1)
			return append(append([]byte(nil), f.before...), bytes.TrimSuffix(bad, []byte("\n"))...)
		}},
		{"a hash failure on a line that ends in a newline", func(f repairFixture) []byte {
			bad := bytes.Replace(f.last, []byte(`"tokens_in":12`), []byte(`"tokens_in":13`), 1)
			return append(append([]byte(nil), f.before...), bad...)
		}},
		{"garbage that ends in a newline", func(f repairFixture) []byte {
			return append(append([]byte(nil), f.before...), "{torn\n"...)
		}},
		{"a last line that does not follow the chain", func(f repairFixture) []byte {
			bad := bytes.Replace(f.last, []byte(`"seq":3`), []byte(`"seq":9`), 1)
			return append(append([]byte(nil), f.before...), bytes.TrimSuffix(bad, []byte("\n"))...)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newRepairFixture(t)
			f.write(t, c.build(f))
			before, names := f.bytes(t), f.names(t)
			err := f.refused(t)
			if !strings.Contains(err.Error(), "ambermoth verify") {
				t.Errorf("error %q does not point at verify", err)
			}
			f.assertUntouched(t, before, names)
		})
	}
}

func TestRepairRefusesDamageBeforeTheLastLine(t *testing.T) {
	t.Parallel()
	f := newRepairFixture(t)
	damaged := bytes.Replace(f.before, []byte(`"tokens_in":10`), []byte(`"tokens_in":99`), 1)
	f.write(t, append(damaged, f.last[:20]...))
	before, names := f.bytes(t), f.names(t)

	err := f.refused(t)

	if !strings.Contains(err.Error(), "ambermoth verify") {
		t.Errorf("error %q does not point at verify", err)
	}
	f.assertUntouched(t, before, names)
}

func TestRepairRefusesWhenThereIsNothingToSave(t *testing.T) {
	cases := []struct {
		name  string
		build func(f repairFixture) []byte
	}{
		{"an empty file", func(repairFixture) []byte { return nil }},
		{"only a torn first line", func(f repairFixture) []byte {
			g := bytes.SplitAfter(f.before, []byte("\n"))[0]
			return g[:len(g)/2]
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newRepairFixture(t)
			f.write(t, c.build(f))
			before, names := f.bytes(t), f.names(t)
			_ = f.refused(t)
			f.assertUntouched(t, before, names)
		})
	}
}

func TestRepairWithoutALedgerIsAnError(t *testing.T) {
	t.Parallel()
	_, err := ops.Repair(t.TempDir(), fixedClock(clockAt), false)
	if err == nil {
		t.Fatal("Repair succeeded with no ledger")
	}
}

func TestRepairNeedsAClock(t *testing.T) {
	t.Parallel()
	f := newRepairFixture(t)
	if _, err := ops.Repair(f.dir, nil, false); !errors.Is(err, ledger.ErrNilClock) {
		t.Errorf("Repair = %v, want ErrNilClock", err)
	}
}

func TestRepairLeavesAHealthyLedgerAlone(t *testing.T) {
	t.Parallel()
	f := newRepairFixture(t)
	before := f.bytes(t)
	for i := 0; i < 2; i++ {
		res := f.repair(t, false)
		if res.Action != ops.RepairNone || res.LastSeq != 3 {
			t.Fatalf("run %d: %+v", i, res)
		}
	}
	if !bytes.Equal(f.bytes(t), before) || len(f.sidecars(t)) != 0 {
		t.Error("a healthy ledger was changed")
	}
}

func TestRepairIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newRepairFixture(t)
	f.tear(t, 50)
	f.repair(t, false)
	once, names := f.bytes(t), f.names(t)

	res := f.repair(t, false)

	if res.Action != ops.RepairNone {
		t.Errorf("second run %+v", res)
	}
	f.assertUntouched(t, once, names)
}

func TestRepairDryRunChangesNothing(t *testing.T) {
	cases := []struct {
		name   string
		damage func(t *testing.T, f repairFixture)
		want   ops.RepairAction
	}{
		{"a torn line", func(t *testing.T, f repairFixture) { f.tear(t, 50) }, ops.RepairCut},
		{"a missing newline", func(t *testing.T, f repairFixture) { f.tear(t, len(f.last)-1) }, ops.RepairNewline},
		{"a healthy ledger", func(*testing.T, repairFixture) {}, ops.RepairNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newRepairFixture(t)
			c.damage(t, f)
			before, names := f.bytes(t), f.names(t)

			res := f.repair(t, true)

			if res.Action != c.want || !res.DryRun {
				t.Errorf("result %+v", res)
			}
			f.assertUntouched(t, before, names)
		})
	}
}

func TestRepairDryRunSaysWhatItWouldDo(t *testing.T) {
	t.Parallel()
	f := newRepairFixture(t)
	frag := f.tear(t, 50)
	res := f.repair(t, true)
	if res.RemovedBytes != 50 || res.RemovedSHA256 != ledger.SHA256Bytes(frag) || res.LastSeq != 2 || res.Gaps != 1 ||
		!strings.HasPrefix(res.Sidecar, ledger.LedgerFile+".torn-3-") {
		t.Errorf("result %+v", res)
	}
}

func TestRepairRefusesWhileTheLedgerIsLocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no file locks on this platform")
	}
	t.Parallel()
	f := newRepairFixture(t)
	f.tear(t, 50)
	before, names := f.bytes(t), f.names(t)
	unlock, err := ledger.TryLock(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	_, err = ops.Repair(f.dir, fixedClock(clockAt), false)

	if !errors.Is(err, ledger.ErrLocked) {
		t.Fatalf("Repair = %v, want ErrLocked", err)
	}
	f.assertUntouched(t, before, names)
}

func TestRepairReportsAnchorProblemsWithoutRefusing(t *testing.T) {
	t.Parallel()
	s := newFixedStore(t)
	appendCalls(t, s, []ledgerCall{{ts: "2026-03-10T08:00:00.000Z", provider: "openai", in: 1, out: 1}})
	_, head, err := s.Head()
	if err != nil {
		t.Fatal(err)
	}
	anchor := ledger.Entry{"type": ledger.TypeAnchor, "target_seq": 1, "target_hash": head, "tsa": "https://tsa.invalid",
		"token_file": "1-0.tsr", "token_sha256": "sha256:00", "gen_time": "2026-03-10T08:00:01.000Z"}
	if err := s.Append(anchor); err != nil {
		t.Fatal(err)
	}
	b := ledgerBytes(t, s)
	if err := os.WriteFile(s.LedgerPath(), append(b, `{"seq":3,"ty`...), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := ops.Repair(s.Dir, fixedClock(clockAt), false)

	if err != nil {
		t.Fatal(err)
	}
	if res.Action != ops.RepairCut || res.AnchorProblems != 1 {
		t.Errorf("result %+v", res)
	}
}

func TestRepairFinishesAfterACrashBetweenSidecarAndTruncate(t *testing.T) {
	t.Parallel()
	f := newRepairFixture(t)
	frag := f.tear(t, 70)
	name := ledger.LedgerFile + ".torn-3-123"
	if err := os.WriteFile(filepath.Join(f.dir, name), frag, 0o600); err != nil {
		t.Fatal(err)
	}

	res := f.repair(t, false)

	if res.Action != ops.RepairCut || res.Sidecar != name {
		t.Fatalf("result %+v", res)
	}
	if got := f.sidecars(t); len(got) != 1 {
		t.Errorf("sidecars %v: the existing one should be reused", got)
	}
	gaps := gapEntries(t, f)
	if len(gaps) != 1 || gaps[0].Str("cause") != wantCause(frag, 2, name) {
		t.Errorf("gaps %v", gaps)
	}
	f.verify(t)
}

func TestRepairFinishesAfterACrashBetweenTruncateAndGap(t *testing.T) {
	t.Parallel()
	f := newRepairFixture(t)
	frag := f.tear(t, 70)
	name := ledger.LedgerFile + ".torn-3-123"
	if err := os.WriteFile(filepath.Join(f.dir, name), frag, 0o600); err != nil {
		t.Fatal(err)
	}
	f.write(t, f.before)

	res := f.repair(t, false)

	if res.Action != ops.RepairGap || res.Gaps != 1 || res.Sidecar != name {
		t.Fatalf("result %+v", res)
	}
	assertAppendOnly(t, f.before, f.bytes(t), 1)
	gaps := gapEntries(t, f)
	if len(gaps) != 1 || gaps[0].Str("cause") != wantCause(frag, 2, name) {
		t.Errorf("gaps %v", gaps)
	}
	f.verify(t)
	if again := f.repair(t, false); again.Action != ops.RepairNone {
		t.Errorf("second run %+v", again)
	}
}

func TestRepairDryRunSeesAGapStillOwed(t *testing.T) {
	t.Parallel()
	f := newRepairFixture(t)
	frag := f.tear(t, 70)
	if err := os.WriteFile(filepath.Join(f.dir, ledger.LedgerFile+".torn-3-123"), frag, 0o600); err != nil {
		t.Fatal(err)
	}
	f.write(t, f.before)
	before, names := f.bytes(t), f.names(t)
	if res := f.repair(t, true); res.Action != ops.RepairGap || res.Gaps != 1 {
		t.Errorf("result %+v", res)
	}
	f.assertUntouched(t, before, names)
}

func TestRepairFinishesAfterACrashInsideTheGapAppend(t *testing.T) {
	t.Parallel()
	f := newRepairFixture(t)
	f.tear(t, 70)
	f.repair(t, false)
	done := f.bytes(t)
	gapStart := len(f.before)

	// The gap line itself was only half written.
	f.write(t, done[:gapStart+30])
	res := f.repair(t, false)

	if res.Action != ops.RepairCut || res.Gaps != 2 {
		t.Fatalf("result %+v", res)
	}
	f.verify(t)
	side := f.sidecars(t)
	if len(side) != 2 {
		t.Fatalf("sidecars %v", side)
	}
	gaps := gapEntries(t, f)
	if len(gaps) != 2 {
		t.Fatalf("gaps %v", gaps)
	}
	for _, s := range side {
		n := 0
		for _, g := range gaps {
			if strings.HasSuffix(g.Str("cause"), "saved as "+filepath.Base(s)) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s is named by %d gap entries, want 1", filepath.Base(s), n)
		}
	}
}

func TestRepairDoesNotRepeatAGapThatOnlyLacksItsNewline(t *testing.T) {
	t.Parallel()
	f := newRepairFixture(t)
	f.tear(t, 70)
	f.repair(t, false)
	done := f.bytes(t)
	f.write(t, bytes.TrimSuffix(done, []byte("\n")))

	res := f.repair(t, false)

	if res.Action != ops.RepairNewline || res.Gaps != 0 {
		t.Fatalf("result %+v", res)
	}
	if !bytes.Equal(f.bytes(t), done) {
		t.Error("ledger is not the repaired one")
	}
	if len(gapEntries(t, f)) != 1 {
		t.Error("a second gap entry was written")
	}
}

func TestRepairedLedgerTakesNewEntriesInTheSameChain(t *testing.T) {
	t.Parallel()
	f := newRepairFixture(t)
	f.tear(t, 70)
	f.repair(t, false)

	s, err := ledger.Open(f.dir, fixedClock(clockAt))
	if err != nil {
		t.Fatalf("Open after repair: %v", err)
	}
	if err := s.Append(ledger.Entry{"type": ledger.TypeCall, "provider": "openai"}); err != nil {
		t.Fatal(err)
	}
	if r := f.verify(t); r.LastSeq != 4 || r.Entries != 5 {
		t.Errorf("report %+v", r)
	}
}
