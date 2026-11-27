package report

import (
	"fmt"
	"sort"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
)

// AlertConfig holds thresholds (SPEC §10).
type AlertConfig struct {
	VolumeFactor     float64 // default 3
	VolumeMinCalls   int64   // ignore days below this; default 50
	BaselineDays     int     // default 14
	ConcentrationPct float64 // default 0.8
	ConcentrationMin int64   // default 1000 calls in window
	WindowDays       int     // default 7
}

func (c *AlertConfig) defaults() {
	if c.VolumeFactor == 0 {
		c.VolumeFactor = 3
	}
	if c.VolumeMinCalls == 0 {
		c.VolumeMinCalls = 50
	}
	if c.BaselineDays == 0 {
		c.BaselineDays = 14
	}
	if c.ConcentrationPct == 0 {
		c.ConcentrationPct = 0.8
	}
	if c.ConcentrationMin == 0 {
		c.ConcentrationMin = 1000
	}
	if c.WindowDays == 0 {
		c.WindowDays = 7
	}
}

// Alert is advisory: "review this", never an accusation.
type Alert struct {
	Kind    string `json:"kind"` // volume | concentration | new_key
	KeyFP   string `json:"key_fp"`
	Day     string `json:"day,omitempty"`
	Message string `json:"message"`
}

func median(xs []int64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	n := len(s)
	if n%2 == 1 {
		return float64(s[n/2])
	}
	return float64(s[n/2-1]+s[n/2]) / 2
}

// Alerts evaluates the window ending at now.
func Alerts(entries []ledger.Entry, now time.Time, c AlertConfig) []Alert {
	c.defaults()
	sc := scanCalls(entries, now, c)
	out := sc.newKeys
	out = append(out, volumeAlerts(sc.daily, now, c)...)
	out = append(out, concentrationAlerts(sc.winEndpoint, sc.winTotal, c)...)
	sort.Slice(out, func(i, j int) bool { return alertLess(out[i], out[j]) })
	return out
}

func alertDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

type keyPurpose struct{ key, purpose string }

// callScan is what one pass over the ledger collects for the alert rules.
type callScan struct {
	newKeys     []Alert
	daily       map[keyPurpose]map[string]int64 // calls per key and effective purpose, by day
	winEndpoint map[string]map[string]int64     // calls in the window per key, by endpoint / purpose
	winTotal    map[string]int64                // calls in the window per key
}

func scanCalls(entries []ledger.Entry, now time.Time, c AlertConfig) *callScan {
	winStart := now.AddDate(0, 0, -c.WindowDays)
	eff := EffectivePurposes(entries)
	sc := &callScan{daily: map[keyPurpose]map[string]int64{},
		winEndpoint: map[string]map[string]int64{}, winTotal: map[string]int64{}}
	for _, e := range entries {
		if e == nil {
			continue
		}
		t := e.Time()
		if a, ok := newKeyAlert(e, t, winStart); ok {
			sc.newKeys = append(sc.newKeys, a)
		}
		if e.Type() != ledger.TypeCall || t.After(now) {
			continue
		}
		sc.addCall(e, t, eff[e.Seq()], winStart)
	}
	return sc
}

func (sc *callScan) addCall(e ledger.Entry, t time.Time, purpose string, winStart time.Time) {
	fp := e.Str("key_fp")
	k := keyPurpose{fp, purpose}
	if sc.daily[k] == nil {
		sc.daily[k] = map[string]int64{}
	}
	sc.daily[k][alertDay(t)]++
	if !t.After(winStart) {
		return
	}
	if sc.winEndpoint[fp] == nil {
		sc.winEndpoint[fp] = map[string]int64{}
	}
	sc.winEndpoint[fp][e.Str("endpoint")+" / "+purpose]++
	sc.winTotal[fp]++
}

func newKeyAlert(e ledger.Entry, t, winStart time.Time) (Alert, bool) {
	if e.Type() != ledger.TypeKeyEvent || e.Str("action") != "auto_enroll" || !t.After(winStart) {
		return Alert{}, false
	}
	return Alert{Kind: "new_key", KeyFP: e.Str("key_fp"),
		Message: fmt.Sprintf("Key %s (%s) was first seen on %s. Review: is this key expected? If it may be leaked, rotate it and record the rotation.", ledger.ShortFP(e.Str("key_fp")), e.Str("provider"), alertDay(t))}, true
}

func volumeAlerts(daily map[keyPurpose]map[string]int64, now time.Time, c AlertConfig) []Alert {
	var out []Alert
	for k, days := range daily {
		for d := 0; d < c.WindowDays; d++ {
			if a, ok := volumeAlert(k, days, now.AddDate(0, 0, -d), c); ok {
				out = append(out, a)
			}
		}
	}
	return out
}

// volumeAlert checks one day of one key and purpose against the median of the
// BaselineDays before it. Days with no calls count as zero.
func volumeAlert(k keyPurpose, days map[string]int64, dt time.Time, c AlertConfig) (Alert, bool) {
	n := days[alertDay(dt)]
	if n < c.VolumeMinCalls {
		return Alert{}, false
	}
	var base []int64
	for b := 1; b <= c.BaselineDays; b++ {
		base = append(base, days[alertDay(dt.AddDate(0, 0, -b))])
	}
	m := median(base)
	if float64(n) > c.VolumeFactor*m {
		return Alert{Kind: "volume", KeyFP: k.key, Day: alertDay(dt),
			Message: fmt.Sprintf("Key %s, purpose %q: %d calls on %s vs a %d-day median of %.0f. Review: is this an expected run (e.g. a benchmark)? Tag it so the record explains it.", ledger.ShortFP(k.key), k.purpose, n, alertDay(dt), c.BaselineDays, m)}, true
	}
	return Alert{}, false
}

func concentrationAlerts(winEndpoint map[string]map[string]int64, winTotal map[string]int64, c AlertConfig) []Alert {
	var out []Alert
	for fp, m := range winEndpoint {
		if winTotal[fp] < c.ConcentrationMin {
			continue
		}
		for cls, n := range m {
			share := float64(n) / float64(winTotal[fp])
			if share > c.ConcentrationPct {
				out = append(out, Alert{Kind: "concentration", KeyFP: fp,
					Message: fmt.Sprintf("Key %s: %.0f%% of %d calls in the last %d days are %s. Narrow, high-volume traffic resembles published distillation signatures. Review: make sure its purpose and code link are recorded.", ledger.ShortFP(fp), share*100, winTotal[fp], c.WindowDays, cls)})
			}
		}
	}
	return out
}

// alertLess orders by kind, then newest day first, then message.
func alertLess(a, b Alert) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Day != b.Day {
		return a.Day > b.Day
	}
	return a.Message < b.Message
}
