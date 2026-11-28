package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/cli"
	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/proxy"
)

func intentFor(id, endpoint string) ledger.Entry {
	return ledger.Entry{"type": ledger.TypeIntent, "call_id": id, "provider": "openai", "api_style": "openai",
		"endpoint": endpoint, "method": "POST", "path": "/v1/chat/completions", "purpose": "untagged"}
}

// appendTo adds entries to the harness ledger as the proxy would.
func (h *harness) appendTo(t *testing.T, es ...ledger.Entry) {
	t.Helper()
	s, err := ledger.Open(h.dir, fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(es...); err != nil {
		t.Fatal(err)
	}
}

func writeAheadOn(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, ledger.ConfigFile)
	cfg, err := proxy.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.WriteAhead = true
	if err := os.WriteFile(path, proxy.MarshalConfig(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func chatThroughProxy(t *testing.T, base string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-wa")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// 'ambermoth proxy' reads write_ahead from the config: a call is recorded as
// an intent and its call entry, and verify finds nothing open.
func TestProxyCommandRunsWriteAhead(t *testing.T) {
	h := newHarness(t)
	h.check(t, runCase{args: []string{"init"}, wantCode: cli.ExitOK})
	pointAt(t, h.dir, fakeUpstream(t).URL)
	writeAheadOn(t, h.dir)

	base, stop := startProxy(t, h.dir)
	chatThroughProxy(t, base)
	if code := stop(); code != cli.ExitOK {
		t.Fatalf("proxy exit code %d", code)
	}

	s, _ := ledger.Open(h.dir, fixedClock)
	es, _ := s.Entries()
	var types []string
	for _, e := range es {
		types = append(types, e.Type())
	}
	if got := strings.Join(types, ","); got != "genesis,intent,key_event,call" {
		t.Errorf("entries %s, want genesis,intent,key_event,call", got)
	}
	code, out, _ := h.run("verify")
	if code != cli.ExitOK || strings.Contains(out, "intents:") {
		t.Errorf("verify: exit %d\n%s", code, out)
	}
}

// Verify prints orphans as information. The exit status stays 0.
func TestVerifyPrintsOrphanIntentsAndExitsZero(t *testing.T) {
	h := newHarness(t)
	h.recordCall(t)
	h.appendTo(t, intentFor(strings.Repeat("a", 32), "realtime"), intentFor(strings.Repeat("b", 32), "chat.completions"))

	code, out, _ := h.run("verify")

	if code != cli.ExitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	wants := []string{"Ledger verification: OK", "intents:   2 without a call entry", "a running proxy has calls in flight",
		"seq 3 ", "realtime", "seq 4 ", "chat.completions"}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "warning") || strings.Contains(out, "ERROR") {
		t.Errorf("an orphan is information only:\n%s", out)
	}

	code, out, _ = h.run("verify", "--json")
	var rep struct {
		OrphanIntents int `json:"orphan_intents"`
		Orphans       []struct {
			Seq      int64  `json:"seq"`
			TS       string `json:"ts"`
			Endpoint string `json:"endpoint"`
		} `json:"orphans"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != cli.ExitOK {
		t.Fatalf("exit %d: %v\n%s", code, err, out)
	}
	if rep.OrphanIntents != 2 || len(rep.Orphans) != 2 || rep.Orphans[0].Seq != 3 || rep.Orphans[0].Endpoint != "realtime" || rep.Orphans[0].TS == "" {
		t.Errorf("json orphans: %+v", rep)
	}
}

func TestVerifyCapsTheOrphanList(t *testing.T) {
	h := newHarness(t)
	h.recordCall(t)
	var es []ledger.Entry
	for i := 0; i < 25; i++ {
		es = append(es, intentFor(strings.Repeat(string(rune('a'+i%26)), 31)+string(rune('a'+i/26)), "chat.completions"))
	}
	h.appendTo(t, es...)
	code, out, _ := h.run("verify")
	if code != cli.ExitOK || !strings.Contains(out, "25 without a call entry") || !strings.Contains(out, "and 5 more") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if n := strings.Count(out, "chat.completions"); n != 20 {
		t.Errorf("%d orphans listed, want 20", n)
	}
}

// A call that names the wrong intent fails verify: the chain is fine, the records disagree.
func TestVerifyFailsOnACallThatNamesTheWrongIntent(t *testing.T) {
	h := newHarness(t)
	h.recordCall(t)
	h.appendTo(t, intentFor(strings.Repeat("a", 32), "chat.completions"),
		ledger.Entry{"type": ledger.TypeCall, "call_id": strings.Repeat("b", 32), "intent_seq": 3})
	code, out, _ := h.run("verify")
	if code != cli.ExitFail || !strings.Contains(out, "Ledger verification: FAILED") || !strings.Contains(out, "not an open intent") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// The proxy logs how many intents it finds open when it starts.
func TestProxyLogsOpenIntentsAtStart(t *testing.T) {
	h := newHarness(t)
	h.recordCall(t)
	h.appendTo(t, intentFor(strings.Repeat("a", 32), "chat.completions"))
	_, stop, logs := startProxyLogs(t, h.dir)
	defer stop()
	if got := logs(); !strings.Contains(got, "open intents") || !strings.Contains(got, "count=1") {
		t.Errorf("start-up log lacks the open intents:\n%s", got)
	}

	clean := newHarness(t)
	clean.recordCall(t)
	_, stop2, logs2 := startProxyLogs(t, clean.dir)
	defer stop2()
	if strings.Contains(logs2(), "open intents") {
		t.Errorf("logged open intents for a ledger with none:\n%s", logs2())
	}
}

// A torn last line that is an intent: the call was not sent, since the forward waits for the fsync,
// so repair removes it and nothing is open. A torn completion leaves its intent as an orphan.
func TestRepairATornIntentAndATornCompletion(t *testing.T) {
	a := strings.Repeat("a", 32)
	done := ledger.Entry{"type": ledger.TypeCall, "call_id": a, "intent_seq": 3, "provider": "openai"}
	cases := []struct {
		name        string
		entries     []ledger.Entry // the last one is torn
		wantOrphans int
	}{
		{"torn intent", []ledger.Entry{intentFor(a, "chat.completions"), done, intentFor(strings.Repeat("c", 32), "chat.completions")}, 0},
		{"torn completion", []ledger.Entry{intentFor(a, "chat.completions"), done}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.recordCall(t)
			h.appendTo(t, c.entries...)
			path := filepath.Join(h.dir, ledger.LedgerFile)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			start := bytes.LastIndexByte(b[:len(b)-1], '\n') + 1
			if err := os.WriteFile(path, b[:start+40], 0o600); err != nil {
				t.Fatal(err)
			}
			if code, out, _ := h.run("repair"); code != cli.ExitOK || !strings.Contains(out, "repaired") {
				t.Fatalf("repair: exit %d\n%s", code, out)
			}
			code, out, _ := h.run("verify", "--json")
			var rep struct {
				OrphanIntents int `json:"orphan_intents"`
			}
			if err := json.Unmarshal([]byte(out), &rep); err != nil || code != cli.ExitOK {
				t.Fatalf("verify after repair: exit %d %v\n%s", code, err, out)
			}
			if rep.OrphanIntents != c.wantOrphans {
				t.Errorf("%d orphans after repair, want %d", rep.OrphanIntents, c.wantOrphans)
			}
		})
	}
}
