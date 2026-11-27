package report_test

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/report"
)

// alertsNow is the fixed end of the window in every alert test.
var alertsNow = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

const (
	keyA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	keyB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	keyC = "sha256:cccccccccccccccccccccccccccc"
	// short forms as ledger.ShortFP prints them
	shortA = "sha256:aaaaaaaaaaaa…"
	shortB = "sha256:bbbbbbbbbbbb…"
	shortC = "sha256:cccccccccccc…"
)

// dayAt is hour h UTC, d days before alertsNow's date.
func dayAt(d, h int) time.Time {
	return time.Date(2026, 3, 15-d, h, 0, 0, 0, time.UTC)
}

type feed struct{ es []ledger.Entry }

func (f *feed) add(e ledger.Entry) int64 {
	seq := int64(len(f.es) + 1)
	e["seq"] = seq
	f.es = append(f.es, e)
	return seq
}

type batch struct {
	key, purpose string
	endpoints    []string
	at           time.Time
	n            int
}

// calls adds b.n calls, cycling through b.endpoints, and returns the first and last seq.
func (f *feed) calls(b batch) (int64, int64) {
	eps := b.endpoints
	if len(eps) == 0 {
		eps = []string{"chat"}
	}
	var first, last int64
	for i := 0; i < b.n; i++ {
		last = f.add(ledger.Entry{"type": ledger.TypeCall, "ts": ledger.FormatTime(b.at), "key_fp": b.key,
			"purpose": b.purpose, "endpoint": eps[i%len(eps)]})
		if first == 0 {
			first = last
		}
	}
	return first, last
}

func (f *feed) amend(from, to int64, purpose string) {
	e := ledger.Entry{"type": ledger.TypeAmendment, "target_from": from, "target_to": to, "reason": "r", "by": "b"}
	if purpose != "" {
		e["new_purpose"] = purpose
	}
	f.add(e)
}

func (f *feed) keyEvent(action, key string, at time.Time) {
	f.add(ledger.Entry{"type": ledger.TypeKeyEvent, "action": action, "key_fp": key, "provider": "openai", "ts": ledger.FormatTime(at)})
}

// history adds counts[i] calls on the day i+start days back.
func (f *feed) history(key, purpose string, start int, counts []int) {
	for i, n := range counts {
		f.calls(batch{key: key, purpose: purpose, at: dayAt(start+i, 10), n: n})
	}
}

func rep(n, count int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = count
	}
	return out
}

func volumeMsg(short, purpose string, n int, day string, baseDays, median int) string {
	return "Key " + short + ", purpose \"" + purpose + "\": " + strconv.Itoa(n) + " calls on " + day + " vs a " + strconv.Itoa(baseDays) +
		"-day median of " + strconv.Itoa(median) + ". Review: is this an expected run (e.g. a benchmark)? Tag it so the record explains it."
}

func concentrationMsg(short string, pct, total, days int, class string) string {
	return "Key " + short + ": " + strconv.Itoa(pct) + "% of " + strconv.Itoa(total) + " calls in the last " + strconv.Itoa(days) + " days are " + class +
		". Narrow, high-volume traffic resembles published distillation signatures. Review: make sure its purpose and code link are recorded."
}

func messages(as []report.Alert, kind string) []string {
	var out []string
	for _, a := range as {
		if a.Kind == kind {
			out = append(out, a.Message)
		}
	}
	return out
}

// onlyVolume keeps the concentration rule from firing.
var onlyVolume = report.AlertConfig{ConcentrationMin: 1 << 40}

// onlyConcentration keeps the volume rule from firing.
var onlyConcentration = report.AlertConfig{VolumeMinCalls: 1 << 40, ConcentrationMin: 100}

type volumeCase struct {
	name     string
	base     []int // calls per day, starting the day before the spike
	spike    int
	spikeAt  int // days before alertsNow; 0 means yesterday
	today    bool
	cfg      report.AlertConfig
	days     []string // days expected to alert, newest first
	baseDays int
	median   int
}

var volumeCases = []volumeCase{
	{name: "exactly three times the median", base: rep(14, 20), spike: 60},
	{name: "just under three times the median", base: rep(14, 20), spike: 59},
	{name: "just over three times the median", base: rep(14, 20), spike: 61, days: []string{"2026-03-14"}, baseDays: 14, median: 20},
	{name: "a day under the minimum is ignored", base: rep(14, 10), spike: 49},
	{name: "a day at the minimum is checked", base: rep(14, 10), spike: 50, days: []string{"2026-03-14"}, baseDays: 14, median: 10},
	{name: "no history at all", base: nil, spike: 50, days: []string{"2026-03-14"}, baseDays: 14, median: 0},
	{name: "no history and under the minimum", base: nil, spike: 49},
	{name: "days with no calls count as zero in a short history", base: rep(5, 20), spike: 50, days: []string{"2026-03-14"}, baseDays: 14, median: 0},
	{name: "seven days of history leave a median of half", base: rep(7, 20), spike: 31 + 20, days: []string{"2026-03-14"}, baseDays: 14, median: 10},
	{name: "eight days of history already give the full median", base: rep(8, 20), spike: 60},
	{name: "zero-call days pull the median down", base: []int{40, 0, 40, 0, 40, 0, 40, 0, 40, 0, 40, 0, 40, 0}, spike: 61,
		days: []string{"2026-03-14"}, baseDays: 14, median: 20},
	{name: "zero-call days, at three times the lowered median", base: []int{40, 0, 40, 0, 40, 0, 40, 0, 40, 0, 40, 0, 40, 0}, spike: 60},
	{name: "today counts", base: rep(14, 20), spike: 61, today: true, days: []string{"2026-03-15"}, baseDays: 14, median: 20},
	{name: "the oldest day of the window counts", base: rep(14, 20), spike: 61, spikeAt: 6, days: []string{"2026-03-09"}, baseDays: 14, median: 20},
	{name: "a day before the window is not reported", base: rep(14, 20), spike: 61, spikeAt: 7},
	{name: "thresholds from the config", base: rep(7, 10), spike: 21, spikeAt: 2,
		cfg:  report.AlertConfig{VolumeFactor: 2, VolumeMinCalls: 5, BaselineDays: 7, WindowDays: 3, ConcentrationMin: 1 << 40},
		days: []string{"2026-03-13"}, baseDays: 7, median: 10},
	{name: "the config window ends before an older spike", base: rep(7, 10), spike: 40, spikeAt: 3,
		cfg: report.AlertConfig{VolumeFactor: 2, VolumeMinCalls: 5, BaselineDays: 7, WindowDays: 3, ConcentrationMin: 1 << 40}},
}

func TestAlertsVolumeRule(t *testing.T) {
	for _, c := range volumeCases {
		t.Run(c.name, func(t *testing.T) {
			at := c.spikeAt
			if at == 0 && !c.today {
				at = 1
			}
			var f feed
			f.history(keyA, "prod", at+1, c.base)
			f.calls(batch{key: keyA, purpose: "prod", at: dayAt(at, 10), n: c.spike})
			cfg := c.cfg
			if cfg == (report.AlertConfig{}) {
				cfg = onlyVolume
			}
			got := messages(report.Alerts(f.es, alertsNow, cfg), "volume")
			var want []string
			for _, d := range c.days {
				want = append(want, volumeMsg(shortA, "prod", c.spike, d, c.baseDays, c.median))
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got  %q\nwant %q", got, want)
			}
		})
	}
}

func TestAlertsVolumeFields(t *testing.T) {
	var f feed
	f.history(keyA, "prod", 2, rep(14, 20))
	f.calls(batch{key: keyA, purpose: "prod", at: dayAt(1, 10), n: 61})
	got := report.Alerts(f.es, alertsNow, onlyVolume)
	want := []report.Alert{{Kind: "volume", KeyFP: keyA, Day: "2026-03-14", Message: volumeMsg(shortA, "prod", 61, "2026-03-14", 14, 20)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestAlertsVolumePerKeyAndPurpose(t *testing.T) {
	var f feed
	// Each key and purpose is its own series: alpha on key A and beta on key B spike.
	f.history(keyA, "alpha", 2, rep(14, 20))
	f.history(keyA, "beta", 2, rep(14, 20))
	f.history(keyB, "alpha", 2, rep(14, 20))
	f.calls(batch{key: keyA, purpose: "alpha", at: dayAt(1, 10), n: 70})
	f.calls(batch{key: keyA, purpose: "beta", at: dayAt(1, 10), n: 20})
	f.calls(batch{key: keyB, purpose: "alpha", at: dayAt(1, 10), n: 20})
	f.calls(batch{key: keyB, purpose: "beta", at: dayAt(1, 10), n: 90})
	got := messages(report.Alerts(f.es, alertsNow, onlyVolume), "volume")
	want := []string{
		volumeMsg(shortA, "alpha", 70, "2026-03-14", 14, 20),
		volumeMsg(shortB, "beta", 90, "2026-03-14", 14, 0),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestAlertsIgnoreCallsAfterNow(t *testing.T) {
	var f feed
	f.calls(batch{key: keyA, purpose: "prod", at: alertsNow.Add(time.Second), n: 500})
	if got := report.Alerts(f.es, alertsNow, report.AlertConfig{}); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

type concentrationCase struct {
	name  string
	top   int // calls to chat / prod
	other int // calls to embed / prod
	cfg   report.AlertConfig
	pct   int // expected percentage; 0 means no alert
	total int
	days  int
}

var concentrationCases = []concentrationCase{
	{name: "exactly 80 percent", top: 80, other: 20, cfg: onlyConcentration},
	{name: "just under 80 percent", top: 79, other: 21, cfg: onlyConcentration},
	{name: "just over 80 percent", top: 81, other: 19, cfg: onlyConcentration, pct: 81, total: 100, days: 7},
	{name: "everything on one endpoint", top: 100, cfg: onlyConcentration, pct: 100, total: 100, days: 7},
	{name: "one call short of the minimum", top: 99, cfg: onlyConcentration},
	{name: "exactly the minimum", top: 100, cfg: onlyConcentration, pct: 100, total: 100, days: 7},
	{name: "thresholds from the config", top: 60, other: 40,
		cfg: report.AlertConfig{VolumeMinCalls: 1 << 40, ConcentrationMin: 10, ConcentrationPct: 0.5, WindowDays: 3},
		pct: 60, total: 100, days: 3},
}

func TestAlertsConcentrationRule(t *testing.T) {
	for _, c := range concentrationCases {
		t.Run(c.name, func(t *testing.T) {
			var f feed
			f.calls(batch{key: keyA, purpose: "prod", endpoints: []string{"chat"}, at: dayAt(1, 10), n: c.top})
			f.calls(batch{key: keyA, purpose: "prod", endpoints: []string{"embed"}, at: dayAt(2, 10), n: c.other})
			got := report.Alerts(f.es, alertsNow, c.cfg)
			if c.pct == 0 {
				if len(got) != 0 {
					t.Fatalf("want no alert, got %+v", got)
				}
				return
			}
			want := []report.Alert{{Kind: "concentration", KeyFP: keyA, Message: concentrationMsg(shortA, c.pct, c.total, c.days, "chat / prod")}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestAlertsConcentrationWindow(t *testing.T) {
	var f feed
	f.calls(batch{key: keyA, purpose: "prod", endpoints: []string{"chat"}, at: dayAt(1, 10), n: 81})
	f.calls(batch{key: keyA, purpose: "prod", endpoints: []string{"embed"}, at: dayAt(1, 11), n: 19})
	// Exactly seven days back, and older, is outside the window; so is the future.
	f.calls(batch{key: keyA, purpose: "prod", endpoints: []string{"embed"}, at: alertsNow.AddDate(0, 0, -7), n: 200})
	f.calls(batch{key: keyA, purpose: "prod", endpoints: []string{"embed"}, at: dayAt(20, 10), n: 200})
	f.calls(batch{key: keyA, purpose: "prod", endpoints: []string{"embed"}, at: alertsNow.Add(time.Hour), n: 200})
	got := messages(report.Alerts(f.es, alertsNow, onlyConcentration), "concentration")
	want := []string{concentrationMsg(shortA, 81, 100, 7, "chat / prod")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}

	// One millisecond inside the window is counted.
	f.calls(batch{key: keyA, purpose: "prod", endpoints: []string{"embed"}, at: alertsNow.AddDate(0, 0, -7).Add(time.Millisecond), n: 100})
	if got := report.Alerts(f.es, alertsNow, onlyConcentration); len(got) != 0 {
		t.Errorf("want the extra calls to dilute the share, got %+v", got)
	}
}

func TestAlertsConcentrationByKeyAndPurpose(t *testing.T) {
	var f feed
	f.calls(batch{key: keyC, purpose: "prod", at: dayAt(1, 10), n: 85})
	f.calls(batch{key: keyC, purpose: "prod", endpoints: []string{"embed"}, at: dayAt(1, 11), n: 15})
	f.calls(batch{key: keyA, purpose: "prod", at: dayAt(1, 10), n: 90})
	f.calls(batch{key: keyA, purpose: "prod", endpoints: []string{"embed"}, at: dayAt(1, 11), n: 10})
	// Key B is spread over four endpoints; the same endpoint under two purposes is two classes.
	f.calls(batch{key: keyB, purpose: "prod", endpoints: []string{"chat", "embed", "images", "audio"}, at: dayAt(1, 10), n: 100})
	got := messages(report.Alerts(f.es, alertsNow, onlyConcentration), "concentration")
	want := []string{
		concentrationMsg(shortA, 90, 100, 7, "chat / prod"),
		concentrationMsg(shortC, 85, 100, 7, "chat / prod"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}

	var g feed
	g.calls(batch{key: keyA, purpose: "one", at: dayAt(1, 10), n: 50})
	g.calls(batch{key: keyA, purpose: "two", at: dayAt(1, 10), n: 50})
	if got := report.Alerts(g.es, alertsNow, onlyConcentration); len(got) != 0 {
		t.Errorf("the same endpoint under two purposes is two classes, got %+v", got)
	}
}

func TestAlertsNewKey(t *testing.T) {
	var f feed
	f.keyEvent("auto_enroll", keyA, dayAt(2, 9))
	f.keyEvent("auto_enroll", keyB, alertsNow.AddDate(0, 0, -7))
	f.keyEvent("auto_enroll", keyC, dayAt(10, 9))
	f.keyEvent("enroll", keyC, dayAt(1, 9))
	got := report.Alerts(f.es, alertsNow, report.AlertConfig{})
	want := []report.Alert{{Kind: "new_key", KeyFP: keyA, Message: "Key " + shortA +
		" (openai) was first seen on 2026-03-13. Review: is this key expected? If it may be leaked, rotate it and record the rotation."}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestAlertsOrder(t *testing.T) {
	var f feed
	f.keyEvent("auto_enroll", keyC, dayAt(3, 9))
	// Key A: spread over four endpoints, spikes on the 14th and the 12th.
	f.history(keyA, "prod", 2, rep(14, 10))
	f.calls(batch{key: keyA, purpose: "prod", endpoints: []string{"chat", "embed", "images", "audio"}, at: dayAt(1, 11), n: 60})
	f.calls(batch{key: keyA, purpose: "prod", endpoints: []string{"chat", "embed", "images", "audio"}, at: dayAt(3, 11), n: 70})
	// Key B: one purpose on one endpoint, a spike on the 14th.
	f.calls(batch{key: keyB, purpose: "bench", at: dayAt(1, 10), n: 80})
	got := report.Alerts(f.es, alertsNow, report.AlertConfig{ConcentrationMin: 50})
	var order []string
	for _, a := range got {
		order = append(order, a.Kind+" "+a.Day+" "+ledger.ShortFP(a.KeyFP))
	}
	want := []string{
		"concentration  " + shortB,
		"new_key  " + shortC,
		"volume 2026-03-14 " + shortA,
		"volume 2026-03-14 " + shortB,
		"volume 2026-03-12 " + shortA,
	}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order:\n got %q\nwant %q", order, want)
	}
}

func TestAlertsUseEffectivePurposes(t *testing.T) {
	var f feed
	f.history(keyA, "prod", 2, rep(14, 10))
	first, last := f.calls(batch{key: keyA, purpose: "untagged", at: dayAt(1, 10), n: 60})
	f.amend(first, last, "eval-benchmark")
	got := messages(report.Alerts(f.es, alertsNow, onlyVolume), "volume")
	want := []string{volumeMsg(shortA, "eval-benchmark", 60, "2026-03-14", 14, 0)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestAlertsAmendmentSplitsACount(t *testing.T) {
	var f feed
	first, _ := f.calls(batch{key: keyA, purpose: "untagged", at: dayAt(1, 10), n: 60})
	f.amend(first, first+29, "eval-benchmark")
	// 30 calls under each purpose is under the minimum for both.
	if got := report.Alerts(f.es, alertsNow, onlyVolume); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestAlertsLaterAmendmentWins(t *testing.T) {
	var f feed
	first, last := f.calls(batch{key: keyA, purpose: "untagged", at: dayAt(1, 10), n: 60})
	f.amend(first, last, "eval-regression")
	f.amend(first, last, "eval-benchmark")
	f.amend(first, last, "") // an annotation changes nothing
	got := messages(report.Alerts(f.es, alertsNow, onlyVolume), "volume")
	want := []string{volumeMsg(shortA, "eval-benchmark", 60, "2026-03-14", 14, 0)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestAlertsAmendmentBeforeTheCallsDoesNotApply(t *testing.T) {
	var f feed
	f.amend(1, 100, "eval-benchmark")
	f.calls(batch{key: keyA, purpose: "untagged", at: dayAt(1, 10), n: 60})
	got := messages(report.Alerts(f.es, alertsNow, onlyVolume), "volume")
	want := []string{volumeMsg(shortA, "untagged", 60, "2026-03-14", 14, 0)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestAlertsConcentrationUsesEffectivePurposes(t *testing.T) {
	var f feed
	first, last := f.calls(batch{key: keyA, purpose: "untagged", at: dayAt(1, 10), n: 90})
	f.calls(batch{key: keyA, purpose: "untagged", endpoints: []string{"embed"}, at: dayAt(1, 11), n: 10})
	f.amend(first, last, "eval-benchmark")
	got := messages(report.Alerts(f.es, alertsNow, onlyConcentration), "concentration")
	want := []string{concentrationMsg(shortA, 90, 100, 7, "chat / eval-benchmark")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestAlertsNothingToReport(t *testing.T) {
	var steady feed
	steady.history(keyA, "prod", 0, rep(15, 20))
	steady.add(ledger.Entry{"type": ledger.TypeCall, "ts": "not a time", "key_fp": keyA, "purpose": "prod", "endpoint": "chat"})
	steady.es = append(steady.es, nil)
	steady.add(ledger.Entry{"type": ledger.TypeGap, "ts": ledger.FormatTime(dayAt(1, 9)), "dropped": int64(5)})
	steady.keyEvent("enroll", keyA, dayAt(1, 9))
	cases := map[string][]ledger.Entry{
		"nil":    nil,
		"empty":  {},
		"steady": steady.es,
	}
	for name, es := range cases {
		t.Run(name, func(t *testing.T) {
			if got := report.Alerts(es, alertsNow, report.AlertConfig{}); len(got) != 0 {
				t.Errorf("got %+v", got)
			}
		})
	}
}
