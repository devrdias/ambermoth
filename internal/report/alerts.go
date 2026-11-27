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
	day := func(t time.Time) string { return t.UTC().Format("2006-01-02") }
	winStart := now.AddDate(0, 0, -c.WindowDays)
	eff := EffectivePurposes(entries)

	type kp struct{ key, purpose string }
	daily := map[kp]map[string]int64{}
	winEndpoint := map[string]map[string]int64{}
	winTotal := map[string]int64{}
	var out []Alert
	for _, e := range entries {
		if e == nil {
			continue
		}
		t := e.Time()
		if e.Type() == ledger.TypeKeyEvent && e.Str("action") == "auto_enroll" && t.After(winStart) {
			out = append(out, Alert{Kind: "new_key", KeyFP: e.Str("key_fp"),
				Message: fmt.Sprintf("Key %s (%s) was first seen on %s. Review: is this key expected? If it may be leaked, rotate it and record the rotation.", ledger.ShortFP(e.Str("key_fp")), e.Str("provider"), day(t))})
		}
		if e.Type() != ledger.TypeCall || t.After(now) {
			continue
		}
		k := kp{e.Str("key_fp"), eff[e.Seq()]}
		if daily[k] == nil {
			daily[k] = map[string]int64{}
		}
		daily[k][day(t)]++
		if t.After(winStart) {
			fp := e.Str("key_fp")
			if winEndpoint[fp] == nil {
				winEndpoint[fp] = map[string]int64{}
			}
			winEndpoint[fp][e.Str("endpoint")+" / "+eff[e.Seq()]]++
			winTotal[fp]++
		}
	}
	for k, days := range daily {
		for d := 0; d < c.WindowDays; d++ {
			dt := now.AddDate(0, 0, -d)
			n := days[day(dt)]
			if n < c.VolumeMinCalls {
				continue
			}
			var base []int64
			for b := 1; b <= c.BaselineDays; b++ {
				base = append(base, days[day(dt.AddDate(0, 0, -b))])
			}
			m := median(base)
			if float64(n) > c.VolumeFactor*m {
				out = append(out, Alert{Kind: "volume", KeyFP: k.key, Day: day(dt),
					Message: fmt.Sprintf("Key %s, purpose %q: %d calls on %s vs a %d-day median of %.0f. Review: is this an expected run (e.g. a benchmark)? Tag it so the record explains it.", ledger.ShortFP(k.key), k.purpose, n, day(dt), c.BaselineDays, m)})
			}
		}
	}
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Day != out[j].Day {
			return out[i].Day > out[j].Day
		}
		return out[i].Message < out[j].Message
	})
	return out
}
