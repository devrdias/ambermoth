package ops_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/ops"
)

const csvHead = "date,model,input_tokens,output_tokens\n"

func writeCSV(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "usage.csv")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type reconcileCase struct {
	name       string
	csv        string
	calls      []ledgerCall
	tolerance  float64
	result     string
	mismatched int64
	lines      []string
	ledgerIn   int64
	ledgerOut  int64
	exportIn   int64
	exportOut  int64
	from, to   string
}

var (
	day2 = "2026-03-02T10:00:00.000Z"
	day3 = "2026-03-03T10:00:00.000Z"
	day4 = "2026-03-04T10:00:00.000Z"
)

var reconcileCases = []reconcileCase{
	{
		name: "exact match",
		csv:  csvHead + "2026-03-02,m,150,15\n",
		calls: []ledgerCall{
			{day2, "openai", 100, 10},
			{"2026-03-02T11:00:00.000Z", "openai", 50, 5},
		},
		tolerance: 0.02, result: "match",
		lines:    []string{"2026-03-02  ledger in/out 150/15  export in/out 150/15  ok"},
		ledgerIn: 150, ledgerOut: 15, exportIn: 150, exportOut: 15, from: "2026-03-02", to: "2026-03-02",
	},
	{
		name:      "ledger above the export by exactly the tolerance is ok",
		csv:       csvHead + "2026-03-02,m,100,10\n",
		calls:     []ledgerCall{{day2, "openai", 110, 10}},
		tolerance: 0.1, result: "match",
		lines:    []string{"2026-03-02  ledger in/out 110/10  export in/out 100/10  ok"},
		ledgerIn: 110, ledgerOut: 10, exportIn: 100, exportOut: 10, from: "2026-03-02", to: "2026-03-02",
	},
	{
		name:      "ledger above the export by more than the tolerance",
		csv:       csvHead + "2026-03-02,m,100,10\n",
		calls:     []ledgerCall{{day2, "openai", 111, 10}},
		tolerance: 0.1, result: "mismatch", mismatched: 1,
		lines:    []string{"2026-03-02  ledger in/out 111/10  export in/out 100/10  MISMATCH"},
		ledgerIn: 111, ledgerOut: 10, exportIn: 100, exportOut: 10, from: "2026-03-02", to: "2026-03-02",
	},
	{
		name:      "ledger below the export by exactly the tolerance is ok",
		csv:       csvHead + "2026-03-02,m,100,10\n",
		calls:     []ledgerCall{{day2, "openai", 90, 10}},
		tolerance: 0.1, result: "match",
		lines:    []string{"2026-03-02  ledger in/out 90/10  export in/out 100/10  ok"},
		ledgerIn: 90, ledgerOut: 10, exportIn: 100, exportOut: 10, from: "2026-03-02", to: "2026-03-02",
	},
	{
		name:      "ledger below the export by more than the tolerance",
		csv:       csvHead + "2026-03-02,m,100,10\n",
		calls:     []ledgerCall{{day2, "openai", 89, 10}},
		tolerance: 0.1, result: "mismatch", mismatched: 1,
		lines:    []string{"2026-03-02  ledger in/out 89/10  export in/out 100/10  MISMATCH"},
		ledgerIn: 89, ledgerOut: 10, exportIn: 100, exportOut: 10, from: "2026-03-02", to: "2026-03-02",
	},
	{
		name:      "output tokens are compared as well",
		csv:       csvHead + "2026-03-02,m,100,10\n",
		calls:     []ledgerCall{{day2, "openai", 100, 12}},
		tolerance: 0.1, result: "mismatch", mismatched: 1,
		lines:    []string{"2026-03-02  ledger in/out 100/12  export in/out 100/10  MISMATCH"},
		ledgerIn: 100, ledgerOut: 12, exportIn: 100, exportOut: 10, from: "2026-03-02", to: "2026-03-02",
	},
	{
		name:      "zero tolerance accepts only exact totals",
		csv:       csvHead + "2026-03-02,m,100,10\n2026-03-03,m,100,10\n",
		calls:     []ledgerCall{{day2, "openai", 100, 10}, {day3, "openai", 101, 10}},
		tolerance: 0, result: "mismatch", mismatched: 1,
		lines: []string{
			"2026-03-02  ledger in/out 100/10  export in/out 100/10  ok",
			"2026-03-03  ledger in/out 101/10  export in/out 100/10  MISMATCH",
		},
		ledgerIn: 201, ledgerOut: 20, exportIn: 200, exportOut: 20, from: "2026-03-02", to: "2026-03-03",
	},
	{
		name:      "export of zero with ledger tokens is a mismatch at any tolerance",
		csv:       csvHead + "2026-03-02,m,0,0\n",
		calls:     []ledgerCall{{day2, "openai", 1, 0}},
		tolerance: 5, result: "mismatch", mismatched: 1,
		lines:    []string{"2026-03-02  ledger in/out 1/0  export in/out 0/0  MISMATCH"},
		ledgerIn: 1, ledgerOut: 0, exportIn: 0, exportOut: 0, from: "2026-03-02", to: "2026-03-02",
	},
	{
		name:      "zero on both sides is ok",
		csv:       csvHead + "2026-03-02,m,0,0\n",
		tolerance: 0.02, result: "match",
		lines: []string{"2026-03-02  ledger in/out 0/0  export in/out 0/0  ok"},
		from:  "2026-03-02", to: "2026-03-02",
	},
	{
		name:      "a day in the export with no ledger calls is a mismatch",
		csv:       csvHead + "2026-03-02,m,100,10\n2026-03-03,m,40,4\n",
		calls:     []ledgerCall{{day2, "openai", 100, 10}},
		tolerance: 0.02, result: "mismatch", mismatched: 1,
		lines: []string{
			"2026-03-02  ledger in/out 100/10  export in/out 100/10  ok",
			"2026-03-03  ledger in/out 0/0  export in/out 40/4  MISMATCH",
		},
		ledgerIn: 100, ledgerOut: 10, exportIn: 140, exportOut: 14, from: "2026-03-02", to: "2026-03-03",
	},
	{
		// The day is inside the period but has no export row, so it is not
		// compared. Its calls still count in the ledger totals of the entry.
		name: "a ledger day missing from the export is not compared",
		csv:  csvHead + "2026-03-02,m,100,10\n2026-03-04,m,5,1\n",
		calls: []ledgerCall{
			{day2, "openai", 100, 10},
			{day3, "openai", 1000, 100},
			{day4, "openai", 5, 1},
		},
		tolerance: 0.02, result: "match",
		lines: []string{
			"2026-03-02  ledger in/out 100/10  export in/out 100/10  ok",
			"2026-03-04  ledger in/out 5/1  export in/out 5/1  ok",
		},
		ledgerIn: 1105, ledgerOut: 111, exportIn: 105, exportOut: 11, from: "2026-03-02", to: "2026-03-04",
	},
	{
		name: "ledger calls outside the exported period are left out",
		csv:  csvHead + "2026-03-03,m,7,1\n",
		calls: []ledgerCall{
			{day2, "openai", 500, 50},
			{day3, "openai", 7, 1},
			{day4, "openai", 500, 50},
		},
		tolerance: 0.02, result: "match",
		lines:    []string{"2026-03-03  ledger in/out 7/1  export in/out 7/1  ok"},
		ledgerIn: 7, ledgerOut: 1, exportIn: 7, exportOut: 1, from: "2026-03-03", to: "2026-03-03",
	},
	{
		name:      "the whole first and last day of the period count",
		csv:       csvHead + "2026-03-02,m,3,3\n",
		calls:     []ledgerCall{{"2026-03-02T00:00:00.000Z", "openai", 1, 1}, {"2026-03-02T23:59:59.999Z", "openai", 2, 2}, {"2026-03-03T00:00:00.000Z", "openai", 9, 9}},
		tolerance: 0.02, result: "match",
		lines:    []string{"2026-03-02  ledger in/out 3/3  export in/out 3/3  ok"},
		ledgerIn: 3, ledgerOut: 3, exportIn: 3, exportOut: 3, from: "2026-03-02", to: "2026-03-02",
	},
	{
		name: "rows out of order, repeated days and several models add up",
		csv: csvHead + "2026-03-03,a,7,1\n2026-03-02,a,100,10\n2026-03-02,b,50,5\n" +
			"2026-03-03,b,3,2\n",
		calls: []ledgerCall{
			{day3, "openai", 10, 3},
			{day2, "openai", 150, 15},
		},
		tolerance: 0.02, result: "match",
		lines: []string{
			"2026-03-02  ledger in/out 150/15  export in/out 150/15  ok",
			"2026-03-03  ledger in/out 10/3  export in/out 10/3  ok",
		},
		ledgerIn: 160, ledgerOut: 18, exportIn: 160, exportOut: 18, from: "2026-03-02", to: "2026-03-03",
	},
	{
		name:      "extra and reordered columns, header case and spaces are accepted",
		csv:       " Output_Tokens ,Model,extra,DATE, input_tokens\n10,m,x,2026-03-02,150\n",
		calls:     []ledgerCall{{day2, "openai", 150, 10}},
		tolerance: 0.02, result: "match",
		lines:    []string{"2026-03-02  ledger in/out 150/10  export in/out 150/10  ok"},
		ledgerIn: 150, ledgerOut: 10, exportIn: 150, exportOut: 10, from: "2026-03-02", to: "2026-03-02",
	},
	{
		name:      "the model column is not required",
		csv:       "date,input_tokens,output_tokens\n2026-03-02,5,6\n",
		calls:     []ledgerCall{{day2, "openai", 5, 6}},
		tolerance: 0.02, result: "match",
		lines:    []string{"2026-03-02  ledger in/out 5/6  export in/out 5/6  ok"},
		ledgerIn: 5, ledgerOut: 6, exportIn: 5, exportOut: 6, from: "2026-03-02", to: "2026-03-02",
	},
	{
		name:      "a time after the date is cut to the day",
		csv:       csvHead + "2026-03-02T23:59:59Z,m,1,1\n2026-03-02 10:00,m,2,2\n  2026-03-02  ,m,3,3\n",
		calls:     []ledgerCall{{day2, "openai", 6, 6}},
		tolerance: 0.02, result: "match",
		lines:    []string{"2026-03-02  ledger in/out 6/6  export in/out 6/6  ok"},
		ledgerIn: 6, ledgerOut: 6, exportIn: 6, exportOut: 6, from: "2026-03-02", to: "2026-03-02",
	},
	{
		name: "other providers and other entry types are not counted",
		csv:  csvHead + "2026-03-02,m,10,1\n",
		calls: []ledgerCall{
			{day2, "openai", 10, 1},
			{day2, "anthropic", 9999, 999},
		},
		tolerance: 0.02, result: "match",
		lines:    []string{"2026-03-02  ledger in/out 10/1  export in/out 10/1  ok"},
		ledgerIn: 10, ledgerOut: 1, exportIn: 10, exportOut: 1, from: "2026-03-02", to: "2026-03-02",
	},
	{
		name:      "numbers that do not parse count as zero",
		csv:       csvHead + "2026-03-02,m,abc,10\n2026-03-02,m,50,\n2026-03-02,m,1.5,x\n2026-03-02,m, 7 ,-3\n",
		calls:     []ledgerCall{{day2, "openai", 57, 7}},
		tolerance: 0.02, result: "match",
		lines:    []string{"2026-03-02  ledger in/out 57/7  export in/out 57/7  ok"},
		ledgerIn: 57, ledgerOut: 7, exportIn: 57, exportOut: 7, from: "2026-03-02", to: "2026-03-02",
	},
	{
		name:      "a date that does not parse is kept as its own day and never matches the ledger",
		csv:       csvHead + "garbage,m,5,5\n",
		calls:     []ledgerCall{{day2, "openai", 5, 5}},
		tolerance: 0.02, result: "mismatch", mismatched: 1,
		lines:    []string{"garbage  ledger in/out 0/0  export in/out 5/5  MISMATCH"},
		ledgerIn: 0, ledgerOut: 0, exportIn: 5, exportOut: 5, from: "garbage", to: "garbage",
	},
	{
		name:      "a bad date that sorts last empties the ledger totals of the entry",
		csv:       csvHead + "2026-03-02,m,5,5\nzzz,m,1,1\n",
		calls:     []ledgerCall{{day2, "openai", 5, 5}},
		tolerance: 0.02, result: "mismatch", mismatched: 1,
		lines: []string{
			"2026-03-02  ledger in/out 5/5  export in/out 5/5  ok",
			"zzz  ledger in/out 0/0  export in/out 1/1  MISMATCH",
		},
		ledgerIn: 0, ledgerOut: 0, exportIn: 6, exportOut: 6, from: "2026-03-02", to: "zzz",
	},
	{
		name:      "a short date is not cut and does not match",
		csv:       csvHead + "2026-3-2,m,5,5\n",
		tolerance: 0.02, result: "mismatch", mismatched: 1,
		lines:    []string{"2026-3-2  ledger in/out 0/0  export in/out 5/5  MISMATCH"},
		exportIn: 5, exportOut: 5, from: "2026-3-2", to: "2026-3-2",
	},
}

func TestReconcileCases(t *testing.T) {
	for _, c := range reconcileCases {
		t.Run(c.name, func(t *testing.T) {
			s := newFixedStore(t)
			appendCalls(t, s, c.calls)
			if err := s.Append(ledger.Entry{"type": ledger.TypeKeyEvent, "action": "enroll", "key_fp": "sha256:x", "provider": "openai"}); err != nil {
				t.Fatal(err)
			}
			e, lines, err := ops.Reconcile(s, "openai", writeCSV(t, c.csv), c.tolerance)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(lines, c.lines) {
				t.Errorf("lines:\n got %q\nwant %q", lines, c.lines)
			}
			got := []any{e.Str("result"), e.Int("days_mismatched"), e.Int("ledger_tokens_in"), e.Int("ledger_tokens_out"),
				e.Int("export_tokens_in"), e.Int("export_tokens_out"), e.Str("period_from"), e.Str("period_to")}
			want := []any{c.result, c.mismatched, c.ledgerIn, c.ledgerOut, c.exportIn, c.exportOut, c.from, c.to}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("entry:\n got %v\nwant %v", got, want)
			}
		})
	}
}

type reconcileErrCase struct {
	name    string
	csv     string
	missing bool
	want    string
	is      error
}

var reconcileErrCases = []reconcileErrCase{
	{name: "missing file", missing: true, is: os.ErrNotExist},
	{name: "empty file", csv: "", is: io.EOF},
	{name: "header only", csv: csvHead, want: "CSV has no rows"},
	{name: "header and blank lines", csv: csvHead + "\n\n", want: "CSV has no rows"},
	{name: "no date column", csv: "day,model,input_tokens,output_tokens\n2026-03-02,m,1,1\n",
		want: `CSV missing column "date" (need date,model,input_tokens,output_tokens)`},
	{name: "no input column", csv: "date,model,output_tokens\n2026-03-02,m,1\n",
		want: `CSV missing column "input_tokens" (need date,model,input_tokens,output_tokens)`},
	{name: "no output column", csv: "date,model,input_tokens\n2026-03-02,m,1\n",
		want: `CSV missing column "output_tokens" (need date,model,input_tokens,output_tokens)`},
	{name: "first missing column is named", csv: "model\nm\n",
		want: `CSV missing column "date" (need date,model,input_tokens,output_tokens)`},
	{name: "row shorter than the header", csv: csvHead + "2026-03-02,m,1\n", want: "wrong number of fields"},
	{name: "row longer than the header", csv: csvHead + "2026-03-02,m,1,1,9\n", want: "wrong number of fields"},
	{name: "unterminated quote", csv: csvHead + "\"2026-03-02,m,1,1\n", want: "extraneous or missing"},
}

func TestReconcileRefusesBadFiles(t *testing.T) {
	for _, c := range reconcileErrCases {
		t.Run(c.name, func(t *testing.T) {
			s := newFixedStore(t)
			appendCalls(t, s, []ledgerCall{{day2, "openai", 1, 1}})
			before := ledgerBytes(t, s)
			path := filepath.Join(t.TempDir(), "none.csv")
			if !c.missing {
				path = writeCSV(t, c.csv)
			}
			e, lines, err := ops.Reconcile(s, "openai", path, 0.02)
			if err == nil {
				t.Fatal("want an error")
			}
			if e != nil || lines != nil {
				t.Errorf("want no entry and no lines, got %v %v", e, lines)
			}
			if c.is != nil && !errors.Is(err, c.is) {
				t.Errorf("error %v, want %v", err, c.is)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q, want it to contain %q", err, c.want)
			}
			assertAppendOnly(t, before, ledgerBytes(t, s), 0)
		})
	}
}

func TestReconcileRecordsEntry(t *testing.T) {
	s := newFixedStore(t)
	appendCalls(t, s, []ledgerCall{{day2, "openai", 100, 10}, {day3, "openai", 7, 1}})
	body := csvHead + "2026-03-02,m,100,10\n2026-03-03,m,7,1\n"
	path := writeCSV(t, body)
	before := ledgerBytes(t, s)
	head, headHash, err := s.Head()
	if err != nil {
		t.Fatal(err)
	}

	e, _, err := ops.Reconcile(s, "openai", path, 0.02)
	if err != nil {
		t.Fatal(err)
	}
	after := ledgerBytes(t, s)
	assertAppendOnly(t, before, after, 1)

	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	wantKeys := []string{"days_mismatched", "entry_hash", "export_tokens_in", "export_tokens_out", "ledger_tokens_in",
		"ledger_tokens_out", "period_from", "period_to", "prev_hash", "provider", "result", "seq", "source_sha256",
		"tolerance_pct", "ts", "type", "v"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Errorf("fields:\n got %v\nwant %v", keys, wantKeys)
	}
	checks := []struct {
		field     string
		got, want any
	}{
		{"type", e.Type(), ledger.TypeReconciliation},
		{"provider", e.Str("provider"), "openai"},
		{"seq", e.Seq(), head + 1},
		{"prev_hash", e.Str("prev_hash"), headHash},
		{"ts", e.Str("ts"), ledger.FormatTime(clockAt)},
		{"source_sha256", e.Str("source_sha256"), ledger.SHA256Bytes([]byte(body))},
		{"tolerance_pct", e.Int("tolerance_pct"), int64(2)},
		{"result", e.Str("result"), "match"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}

	disk := readLedger(t, s)
	last := disk[len(disk)-1]
	if last.Hash() != e.Hash() || last.Str("result") != "match" || last.Int("export_tokens_in") != 107 {
		t.Errorf("entry on disk differs from the returned one: %v", last)
	}
	if r := verify(t, s, nil); !r.OK() {
		t.Fatalf("ledger does not verify: %v", r.Errors)
	}

	// Running it again appends a second entry and leaves the first alone.
	if _, _, err := ops.Reconcile(s, "openai", path, 0.02); err != nil {
		t.Fatal(err)
	}
	assertAppendOnly(t, after, ledgerBytes(t, s), 1)
}

func TestReconcileTolerancePct(t *testing.T) {
	s := newFixedStore(t)
	path := writeCSV(t, csvHead+"2026-03-02,m,0,0\n")
	// float64 cannot hold these exactly, so the whole percent truncates.
	pcts := []struct {
		tol  float64
		want int64
	}{{0, 0}, {0.02, 2}, {0.1, 10}, {0.5, 50}, {0.29, 28}, {0.07, 7}}
	for _, p := range pcts {
		e, _, err := ops.Reconcile(s, "openai", path, p.tol)
		if err != nil {
			t.Fatal(err)
		}
		if got := e.Int("tolerance_pct"); got != p.want {
			t.Errorf("tolerance %v recorded as %d, want %d", p.tol, got, p.want)
		}
	}
}
