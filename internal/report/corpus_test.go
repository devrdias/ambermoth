package report_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/report"
)

var updateGolden = flag.Bool("update", false, "rewrite the summarize corpus golden files")

const corpusDir = "testdata/summarize-corpus"

// built is a ledger under construction: seq counts up unless a test sets it.
type built struct {
	es  []ledger.Entry
	seq int64
}

func (b *built) add(e ledger.Entry) int64 {
	b.seq++
	if _, ok := e["seq"]; !ok {
		e["seq"] = b.seq
	}
	b.es = append(b.es, e)
	return e.Seq()
}

type callSpec struct {
	provider, purpose, key, endpoint, model string
	day                                     int
	hour                                    int
	status                                  int64
	in, out, cached                         int64
}

func at(day, hour int) string {
	return ledger.FormatTime(time.Date(2026, 3, day, hour, 0, 0, 0, time.UTC))
}

func (b *built) call(c callSpec) int64 {
	if c.provider == "" {
		c.provider = "openai"
	}
	if c.purpose == "" {
		c.purpose = "untagged"
	}
	if c.key == "" {
		c.key = "sha256:k1"
	}
	if c.endpoint == "" {
		c.endpoint = "chat"
	}
	if c.model == "" {
		c.model = "m1"
	}
	if c.status == 0 {
		c.status = 200
	}
	return b.add(ledger.Entry{"type": "call", "ts": at(c.day, c.hour), "provider": c.provider, "purpose": c.purpose,
		"key_fp": c.key, "endpoint": c.endpoint, "model": c.model, "status": c.status,
		"tokens_in": c.in, "tokens_out": c.out, "cached_in": c.cached,
		"prompt_hmac": "p", "output_hmac": "o"})
}

func (b *built) amend(from, to int64, purpose string) int64 {
	return b.add(ledger.Entry{"type": "amendment", "ts": at(20, 0), "target_from": from, "target_to": to,
		"new_purpose": purpose, "reason": "r", "by": "b"})
}

func (b *built) keyEvent(provider, key, action string, day int) int64 {
	return b.add(ledger.Entry{"type": "key_event", "ts": at(day, 1), "provider": provider, "key_fp": key, "action": action})
}

type corpusCase struct {
	name   string
	build  func(b *built)
	filter report.Filter
}

func day(d int) time.Time { return time.Date(2026, 3, d, 0, 0, 0, 0, time.UTC) }

func corpusCases() []corpusCase {
	return []corpusCase{
		{name: "empty ledger", build: func(b *built) {}},
		{name: "calls without amendments", build: func(b *built) {
			b.call(callSpec{day: 1, in: 10, out: 5})
			b.call(callSpec{day: 1, hour: 2, purpose: "prod", in: 7, out: 3, cached: 2})
			b.call(callSpec{day: 2, purpose: "prod", endpoint: "embed", model: "m2"})
		}},
		{name: "amendment after the calls", build: func(b *built) {
			a := b.call(callSpec{day: 1})
			c := b.call(callSpec{day: 1, hour: 1})
			b.call(callSpec{day: 2, purpose: "prod"})
			b.amend(a, c, "eval")
		}},
		{name: "amendment before the calls retags nothing later", build: func(b *built) {
			b.call(callSpec{day: 1})
			b.amend(1, 10, "eval")
			b.call(callSpec{day: 2})
			b.call(callSpec{day: 3})
		}},
		{name: "amendment targeting only a call outside the provider filter", build: func(b *built) {
			b.call(callSpec{day: 1, provider: "openai"})
			o := b.call(callSpec{day: 1, provider: "anthropic"})
			b.amend(o, o, "eval")
		}, filter: report.Filter{Provider: "openai"}},
		{name: "amendment spanning matched and unmatched calls", build: func(b *built) {
			a := b.call(callSpec{day: 1, provider: "openai"})
			c := b.call(callSpec{day: 1, provider: "anthropic"})
			b.amend(a, c, "eval")
		}, filter: report.Filter{Provider: "openai"}},
		{name: "amendment of an amendment and a second retag", build: func(b *built) {
			a := b.call(callSpec{day: 1})
			c := b.call(callSpec{day: 1, hour: 1})
			first := b.amend(a, c, "eval")
			b.amend(first, first, "ignored")
			b.amend(a, a, "prod")
		}},
		{name: "amendment with an empty new purpose", build: func(b *built) {
			a := b.call(callSpec{day: 1, purpose: "prod"})
			b.amend(a, a, "")
		}},
		{name: "amendment range includes a later call it must not retag", build: func(b *built) {
			a := b.call(callSpec{day: 1})
			am := b.amend(a, 100, "eval")
			b.call(callSpec{day: 2})
			_ = am
		}},
		{name: "amendment whose only matched target comes after it", build: func(b *built) {
			b.amend(5, 6, "eval")
			b.add(ledger.Entry{"type": "call", "seq": int64(5), "ts": at(1, 0), "provider": "openai", "purpose": "untagged"})
		}},
		{name: "retag moves a call to a new purpose bucket", build: func(b *built) {
			for i := 0; i < 5; i++ {
				b.call(callSpec{day: 1, hour: i, purpose: "prod", in: int64(i)})
			}
			b.amend(2, 4, "eval")
		}},
		{name: "provider filter drops other providers and their reconciliations", build: func(b *built) {
			b.call(callSpec{day: 1, provider: "openai", key: "sha256:a"})
			b.call(callSpec{day: 1, provider: "anthropic", key: "sha256:b"})
			b.keyEvent("openai", "sha256:a", "enroll", 1)
			b.keyEvent("anthropic", "sha256:b", "enroll", 1)
			b.add(ledger.Entry{"type": "reconciliation", "provider": "openai", "ts": at(2, 0)})
			b.add(ledger.Entry{"type": "reconciliation", "provider": "anthropic", "ts": at(2, 0)})
		}, filter: report.Filter{Provider: "openai"}},
		{name: "period filter with an inclusive start and an exclusive end", build: func(b *built) {
			for d := 1; d <= 6; d++ {
				b.call(callSpec{day: d})
				b.call(callSpec{day: d, hour: 23})
			}
			b.amend(1, 12, "eval")
		}, filter: report.Filter{From: day(2), To: day(5)}},
		{name: "calls with an unparsable or missing timestamp", build: func(b *built) {
			b.add(ledger.Entry{"type": "call", "ts": "not a time", "provider": "openai", "purpose": "prod"})
			b.add(ledger.Entry{"type": "call", "provider": "openai", "purpose": "prod"})
			b.call(callSpec{day: 3})
		}, filter: report.Filter{From: day(2)}},
		{name: "key events with several keys and a rotation", build: func(b *built) {
			b.keyEvent("openai", "sha256:old", "enroll", 1)
			b.call(callSpec{day: 2, key: "sha256:old"})
			b.keyEvent("openai", "sha256:new", "enroll", 3)
			b.keyEvent("openai", "sha256:old", "revoke", 3)
			b.call(callSpec{day: 4, key: "sha256:new"})
			b.call(callSpec{day: 4, key: "sha256:new", hour: 1})
			b.keyEvent("openai", "sha256:old", "enroll", 5)
			b.keyEvent("anthropic", "sha256:solo", "auto_enroll", 5)
		}},
		{name: "keys tied on calls order by fingerprint", build: func(b *built) {
			tied := []string{"sha256:cc", "sha256:aa", "sha256:bb"}
			for _, k := range tied {
				b.call(callSpec{day: 1, key: k})
			}
			b.keyEvent("openai", "sha256:zz", "enroll", 1)
		}},
		{name: "a key first seen on a call keeps that call's provider", build: func(b *built) {
			b.call(callSpec{day: 1, provider: "openai", key: "sha256:x"})
			b.keyEvent("anthropic", "sha256:x", "enroll", 2)
		}},
		{name: "calls without a key", build: func(b *built) {
			b.add(ledger.Entry{"type": "call", "ts": at(1, 0), "provider": "openai", "purpose": "prod", "status": int64(200)})
		}},
		{name: "gaps code links and reconciliations", build: func(b *built) {
			b.call(callSpec{day: 1})
			b.add(ledger.Entry{"type": "gap", "ts": at(1, 5), "missed": int64(3)})
			b.add(ledger.Entry{"type": "code_link", "ts": at(1, 6), "repo": "r", "commit": "c"})
			b.add(ledger.Entry{"type": "reconciliation", "ts": at(2, 0), "provider": "openai", "billed_calls": int64(1)})
			b.add(ledger.Entry{"type": "genesis", "ts": at(1, 0)})
			b.add(ledger.Entry{"type": "anchor", "ts": at(2, 0)})
		}},
		{name: "exemplars keep the first three per bucket", build: func(b *built) {
			for i := 0; i < 8; i++ {
				b.call(callSpec{day: 1, hour: i, purpose: "prod", model: "m" + string(rune('a'+i))})
			}
			for i := 0; i < 2; i++ {
				b.call(callSpec{day: 2, hour: i, purpose: "eval", provider: "anthropic"})
			}
			b.amend(1, 3, "eval")
		}},
		{name: "error statuses", build: func(b *built) {
			statuses := []int64{200, 399, 400, 429, 500, 0}
			for _, s := range statuses {
				b.add(ledger.Entry{"type": "call", "ts": at(1, 0), "provider": "openai", "purpose": "p", "status": s})
			}
		}},
		{name: "nil entries are skipped", build: func(b *built) {
			b.call(callSpec{day: 1})
			b.es = append(b.es, nil)
			b.call(callSpec{day: 1, hour: 1})
		}},
		{name: "repeated seq in a hostile ledger", build: func(b *built) {
			b.add(ledger.Entry{"type": "call", "seq": int64(1), "ts": at(1, 0), "provider": "openai", "purpose": "a"})
			b.add(ledger.Entry{"type": "call", "seq": int64(1), "ts": at(1, 1), "provider": "anthropic", "purpose": "b"})
			b.add(ledger.Entry{"type": "call", "seq": int64(1), "ts": at(1, 2), "provider": "openai", "purpose": "c"})
			b.add(ledger.Entry{"type": "amendment", "seq": int64(9), "target_from": int64(1), "target_to": int64(1), "new_purpose": "z"})
		}, filter: report.Filter{Provider: "openai"}},
		{name: "seqs out of order", build: func(b *built) {
			b.add(ledger.Entry{"type": "call", "seq": int64(7), "ts": at(1, 0), "provider": "openai", "purpose": "a"})
			b.add(ledger.Entry{"type": "call", "seq": int64(3), "ts": at(1, 1), "provider": "openai", "purpose": "a"})
			b.add(ledger.Entry{"type": "amendment", "seq": int64(5), "target_from": int64(1), "target_to": int64(9), "new_purpose": "z"})
		}},
	}
}

func TestSummarizeCorpus(t *testing.T) {
	for _, c := range corpusCases() {
		t.Run(c.name, func(t *testing.T) {
			var b built
			c.build(&b)
			got, err := json.MarshalIndent(report.Summarize(b.es, c.filter), "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join(corpusDir, slug(c.name)+".json")
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
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if string(got) != string(want) {
				t.Errorf("summary differs from %s\n got: %s\nwant: %s", path, got, want)
			}
		})
	}
}

func slug(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			out = append(out, c)
		} else if len(out) > 0 && out[len(out)-1] != '-' {
			out = append(out, '-')
		}
	}
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	return string(out)
}
