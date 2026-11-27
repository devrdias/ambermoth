package ledger_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
)

const hour = time.Hour

// anchorSpec is one anchor entry: which seq it covers, the TSA time in its
// token, and whether token verification fails.
type anchorSpec struct {
	target int64
	gen    time.Duration // from epoch
	fails  bool
}

type lagCase struct {
	name       string
	genesis    time.Duration   // genesis ts, from epoch
	calls      []time.Duration // ts of seq 1.., from epoch; a negative value writes an unparseable ts
	anchors    []anchorSpec    // appended in this order after the calls
	skipChecks bool            // run Verify without a token checker
	wantOK     bool

	wantMaxSec   int64
	wantMaxSeq   int64
	wantOver24h  int
	wantTail     int
	wantLastAnch int64
	wantWarning  string // substring of the lag warning; empty means none
}

const unparseable = -time.Duration(1)

var lagCases = []lagCase{
	{name: "no anchors", calls: []time.Duration{1 * hour, 2 * hour}, wantOK: true,
		wantMaxSec: -1, wantMaxSeq: -1, wantTail: 3, wantLastAnch: -1},
	{name: "one anchor over everything", calls: []time.Duration{1 * hour, 2 * hour, 3 * hour},
		anchors: []anchorSpec{{3, 4 * hour, false}}, wantOK: true,
		wantMaxSec: 4 * 3600, wantMaxSeq: 0, wantTail: 0, wantLastAnch: 3},
	{name: "entries after the last anchor are the unanchored tail",
		calls:   []time.Duration{1 * hour, 2 * hour, 3 * hour, 4 * hour},
		anchors: []anchorSpec{{2, 3 * hour, false}}, wantOK: true,
		wantMaxSec: 3 * 3600, wantMaxSeq: 0, wantTail: 2, wantLastAnch: 2},
	{name: "late anchor counts entries over 24h and reports the largest gap",
		calls:   []time.Duration{1 * hour, 2 * hour, 3 * hour},
		anchors: []anchorSpec{{1, 2 * hour, false}, {3, 30 * hour, false}}, wantOK: true,
		wantMaxSec: 28 * 3600, wantMaxSeq: 2, wantOver24h: 2, wantLastAnch: 3,
		wantWarning: "2 entries were first anchored more than 24h after their own timestamps (largest gap 28h0m0s, seq 2)"},
	{name: "a gap of exactly 24h is not over",
		calls:   []time.Duration{1 * hour},
		anchors: []anchorSpec{{1, 24 * hour, false}}, wantOK: true,
		wantMaxSec: 24 * 3600, wantMaxSeq: 0, wantLastAnch: 1},
	{name: "one second over 24h is counted",
		calls:   []time.Duration{1 * hour},
		anchors: []anchorSpec{{1, 24*hour + time.Second, false}}, wantOK: true,
		wantMaxSec: 24*3600 + 1, wantMaxSeq: 0, wantOver24h: 1, wantLastAnch: 1,
		wantWarning: "1 entries were first anchored more than 24h after"},
	{name: "anchors written out of order sort by target",
		calls:   []time.Duration{1 * hour, 2 * hour, 3 * hour},
		anchors: []anchorSpec{{3, 10 * hour, false}, {1, 2 * hour, false}}, wantOK: true,
		wantMaxSec: 8 * 3600, wantMaxSeq: 2, wantLastAnch: 3},
	{name: "an earlier TSA time on a later anchor covers earlier entries",
		calls:   []time.Duration{1 * hour, 2 * hour, 3 * hour},
		anchors: []anchorSpec{{1, 20 * hour, false}, {3, 5 * hour, false}}, wantOK: true,
		wantMaxSec: 5 * 3600, wantMaxSeq: 0, wantLastAnch: 3},
	{name: "a failed anchor is not counted",
		calls:      []time.Duration{1 * hour, 2 * hour, 3 * hour},
		anchors:    []anchorSpec{{1, 2 * hour, false}, {3, 90 * hour, true}},
		wantMaxSec: 2 * 3600, wantMaxSeq: 0, wantTail: 2, wantLastAnch: 1},
	{name: "only failed anchors leave nothing anchored",
		calls:      []time.Duration{1 * hour, 2 * hour},
		anchors:    []anchorSpec{{2, 3 * hour, true}},
		wantMaxSec: -1, wantMaxSeq: -1, wantTail: 3, wantLastAnch: -1},
	{name: "anchors without token checks carry no TSA time",
		calls:   []time.Duration{1 * hour, 2 * hour},
		anchors: []anchorSpec{{2, 3 * hour, false}}, skipChecks: true, wantOK: true,
		wantMaxSec: -1, wantMaxSeq: -1, wantTail: 0, wantLastAnch: 2},
	{name: "entries with an unparseable ts are skipped",
		calls:      []time.Duration{1 * hour, unparseable, 3 * hour},
		anchors:    []anchorSpec{{3, 4 * hour, false}},
		wantMaxSec: 4 * 3600, wantMaxSeq: 0, wantLastAnch: 3},
	{name: "a TSA time before every entry gives lag zero and no seq",
		genesis: 10 * hour, calls: []time.Duration{11 * hour},
		anchors: []anchorSpec{{1, 5 * hour, false}}, wantOK: true,
		wantMaxSec: 0, wantMaxSeq: -1, wantLastAnch: 1},
	{name: "a zero lag is reported against its entry",
		calls:   []time.Duration{1 * hour},
		anchors: []anchorSpec{{1, 1 * hour, false}}, wantOK: true,
		wantMaxSec: 3600, wantMaxSeq: 0, wantLastAnch: 1},
}

// buildLagLedger writes the case's ledger and verifies it.
func buildLagLedger(t *testing.T, c lagCase) *ledger.Report {
	t.Helper()
	s := initStoreAt(t, epoch.Add(c.genesis))
	for _, off := range c.calls {
		ts := ledger.FormatTime(epoch.Add(off))
		if off == unparseable {
			ts = "not a time"
		}
		if err := s.Append(ledger.Entry{"type": ledger.TypeCall, "ts": ts}); err != nil {
			t.Fatal(err)
		}
	}
	es, err := s.Entries()
	if err != nil {
		t.Fatal(err)
	}
	genTimes := map[string]anchorSpec{}
	anchorDir := filepath.Join(s.Dir, ledger.AnchorDir)
	for i, a := range c.anchors {
		token := fmt.Sprintf("token-%d", i)
		genTimes[token] = a
		e := ledger.Entry{
			"type": ledger.TypeAnchor, "ts": ledger.FormatTime(epoch.Add(200 * hour)),
			"target_seq": a.target, "target_hash": es[a.target].Hash(),
			"token_file": token + ".tsr", "token_sha256": ledger.SHA256Bytes([]byte(token)), "tsa": "test",
		}
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(anchorDir, token+".tsr"), []byte(token), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	opt := ledger.VerifyOptions{AnchorDir: anchorDir}
	if !c.skipChecks {
		opt.CheckAnchor = func(tok []byte, _ string) (ledger.AnchorCheck, error) {
			a := genTimes[string(tok)]
			if a.fails {
				return ledger.AnchorCheck{}, fmt.Errorf("bad token")
			}
			return ledger.AnchorCheck{GenTime: epoch.Add(a.gen), Signer: "test", Pinned: true}, nil
		}
	}
	all, lines, err := ledger.ReadFile(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	return ledger.Verify(all, lines, opt)
}

func lagWarnings(r *ledger.Report) []string {
	var out []string
	for _, w := range r.Warnings {
		if strings.Contains(w, "first anchored more than 24h") {
			out = append(out, w)
		}
	}
	return out
}

func TestAnchorLagCases(t *testing.T) {
	for _, c := range lagCases {
		t.Run(c.name, func(t *testing.T) {
			r := buildLagLedger(t, c)
			if r.OK() != c.wantOK {
				t.Fatalf("OK = %v, want %v (errors %v)", r.OK(), c.wantOK, r.Errors)
			}
			if r.MaxAnchorLagSeconds != c.wantMaxSec || r.MaxAnchorLagSeq != c.wantMaxSeq {
				t.Errorf("max lag = %ds at seq %d, want %ds at seq %d",
					r.MaxAnchorLagSeconds, r.MaxAnchorLagSeq, c.wantMaxSec, c.wantMaxSeq)
			}
			if r.EntriesLaggingOver24h != c.wantOver24h {
				t.Errorf("over 24h = %d, want %d", r.EntriesLaggingOver24h, c.wantOver24h)
			}
			if r.UnanchoredTail != c.wantTail || r.LastAnchoredSeq != c.wantLastAnch {
				t.Errorf("tail %d last anchored %d, want %d and %d", r.UnanchoredTail, r.LastAnchoredSeq, c.wantTail, c.wantLastAnch)
			}
			ws := lagWarnings(r)
			switch {
			case c.wantWarning == "" && len(ws) != 0:
				t.Errorf("unexpected lag warning %q", ws)
			case c.wantWarning != "" && (len(ws) != 1 || !strings.Contains(ws[0], c.wantWarning)):
				t.Errorf("lag warnings %q, want one containing %q", ws, c.wantWarning)
			}
		})
	}
}
