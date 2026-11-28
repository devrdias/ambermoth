package cli_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/cli"
	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/ops"
)

// tornHarness has a ledger with a recorded call and its last line cut to n bytes.
func tornHarness(t *testing.T, n int) (*harness, []byte) {
	t.Helper()
	h := newHarness(t)
	h.recordCall(t)
	path := filepath.Join(h.dir, ledger.LedgerFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.LastIndexByte(b[:len(b)-1], '\n') + 1
	torn := b[:start+n]
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatal(err)
	}
	return h, torn
}

func TestRepairCommandRecoversATornLedger(t *testing.T) {
	h, _ := tornHarness(t, 60)
	if code, _, _ := h.run("verify"); code != cli.ExitFail {
		t.Fatalf("precondition: verify exit %d", code)
	}

	code, out, errOut := h.run("repair")

	if code != cli.ExitOK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	wants := []string{"repaired: removed a torn last line of 60 bytes", "after seq 1", "ledger.jsonl.torn-2-", "gap entries to record: 1"}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if code, out, _ := h.run("verify"); code != cli.ExitOK || !strings.Contains(out, "1 gap entries") {
		t.Errorf("verify after repair: exit %d\n%s", code, out)
	}
	if code, out, _ := h.run("repair"); code != cli.ExitOK || !strings.Contains(out, "nothing to repair") {
		t.Errorf("second repair: exit %d\n%s", code, out)
	}
}

func TestRepairCommandDryRunChangesNothing(t *testing.T) {
	h, torn := tornHarness(t, 60)

	code, out, _ := h.run("repair", "--dry-run")

	if code != cli.ExitOK || !strings.Contains(out, "dry run, nothing changed: would remove a torn last line of 60 bytes") {
		t.Errorf("exit %d\n%s", code, out)
	}
	after, err := os.ReadFile(filepath.Join(h.dir, ledger.LedgerFile))
	if err != nil || !bytes.Equal(after, torn) {
		t.Errorf("ledger changed (%v)", err)
	}
	if m, _ := filepath.Glob(filepath.Join(h.dir, "*.torn-*")); len(m) != 0 {
		t.Errorf("sidecars %v", m)
	}
}

func TestRepairCommandJSON(t *testing.T) {
	h, _ := tornHarness(t, 60)
	code, out, errOut := h.run("repair", "--json")
	if code != cli.ExitOK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	var res ops.RepairResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if res.Action != ops.RepairCut || res.RemovedBytes != 60 || res.LastSeq != 1 || res.Gaps != 1 {
		t.Errorf("result %+v", res)
	}
}

func TestRepairCommandNewlineOnly(t *testing.T) {
	h := newHarness(t)
	h.recordCall(t)
	path := filepath.Join(h.dir, ledger.LedgerFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.TrimSuffix(b, []byte("\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := h.run("repair")
	if code != cli.ExitOK || !strings.Contains(out, "only lacks its newline") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, b) {
		t.Error("ledger is not the original")
	}
}

func TestRepairCommandRefusals(t *testing.T) {
	t.Run("damage before the last line", func(t *testing.T) {
		h, _ := tornHarness(t, 60)
		path := filepath.Join(h.dir, ledger.LedgerFile)
		b, _ := os.ReadFile(path)
		b = bytes.Replace(b, []byte(`"generator":"ambermoth/test"`), []byte(`"generator":"ambermoth/tesT"`), 1)
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		code, _, errOut := h.run("repair")
		if code != cli.ExitFail || !strings.Contains(errOut, "ambermoth verify") {
			t.Errorf("exit %d\n%s", code, errOut)
		}
		if after, _ := os.ReadFile(path); !bytes.Equal(after, b) {
			t.Error("ledger changed")
		}
	})
	t.Run("a locked ledger", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no file locks on this platform")
		}
		h, _ := tornHarness(t, 60)
		unlock, err := ledger.TryLock(h.dir)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		code, _, errOut := h.run("repair")
		if code != cli.ExitFail || !strings.Contains(errOut, ledger.ErrLocked.Error()) {
			t.Errorf("exit %d\n%s", code, errOut)
		}
	})
	t.Run("no ledger", func(t *testing.T) {
		code, _, errOut := newHarness(t).run("repair")
		if code != cli.ExitFail || errOut == "" {
			t.Errorf("exit %d\n%s", code, errOut)
		}
	})
	t.Run("a bad flag", func(t *testing.T) {
		if code, _, _ := newHarness(t).run("repair", "--nope"); code != cli.ExitUsage {
			t.Errorf("exit %d", code)
		}
	})
}

func TestRepairIsListedInTheUsage(t *testing.T) {
	_, out, _ := newHarness(t).run("help")
	if !strings.Contains(out, "repair") {
		t.Errorf("usage does not list repair:\n%s", out)
	}
}

func TestProxyContinuesTheChainOnARepairedLedger(t *testing.T) {
	h, _ := tornHarness(t, 60)
	if code, _, errOut := h.run("repair"); code != cli.ExitOK {
		t.Fatalf("repair exit %d\n%s", code, errOut)
	}
	pointAt(t, h.dir, fakeUpstream(t).URL)

	base, stop := startProxy(t, h.dir)
	req, err := http.NewRequest(http.MethodPost, base+"/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-fixture")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if code := stop(); code != cli.ExitOK {
		t.Fatalf("proxy exit %d", code)
	}

	if code, out, _ := h.run("verify"); code != cli.ExitOK {
		t.Fatalf("verify exit %d\n%s", code, out)
	}
	seqs := callSeqs(t, h.dir)
	if len(seqs) == 0 || seqs[len(seqs)-1] <= 2 {
		t.Errorf("call seqs %v: the new call should come after the gap entry", seqs)
	}
	if got := entriesOfType(t, h.dir, ledger.TypeGap); len(got) != 1 {
		t.Errorf("%d gap entries, want 1", len(got))
	}
}

func TestProxyRefusesToStartOnATornLedgerUntilItIsRepaired(t *testing.T) {
	h, _ := tornHarness(t, 60)
	code, _, errOut := h.run("proxy", "--listen", "127.0.0.1:0")
	if code != cli.ExitFail || !strings.Contains(errOut, "corrupt") {
		t.Errorf("exit %d\n%s", code, errOut)
	}
}
