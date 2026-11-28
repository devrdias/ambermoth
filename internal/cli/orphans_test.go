package cli_test

import (
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/cli"
)

const orphanSentence = "intents have no call entry"

func TestReportCountsOrphanIntentsOnlyWhenThereAreSome(t *testing.T) {
	h := recordedHarness(t)
	code, out, _ := h.run("report")
	if code != cli.ExitOK || strings.Contains(out, "intent") {
		t.Fatalf("a ledger without intents: exit %d\n%s", code, out)
	}

	h.appendTo(t, intentFor(strings.Repeat("a", 32), "chat.completions"), intentFor(strings.Repeat("b", 32), "chat.completions"))
	code, out, _ = h.run("report")

	want := "2 intents have no call entry. Each is a request the proxy began to send. Whether it was sent, or answered, is not known."
	if code != cli.ExitOK || !strings.Contains(out, want) {
		t.Errorf("exit %d, output lacks %q:\n%s", code, want, out)
	}
	if !strings.Contains(out, "1 calls · 100 tokens in") {
		t.Errorf("orphans are not calls:\n%s", out)
	}
}

func TestPackAndVerifyPackWithAnOrphanIntent(t *testing.T) {
	h := recordedHarness(t)
	h.appendTo(t, intentFor(strings.Repeat("c", 32), "chat.completions"))
	out := h.workFile("wa.zip")

	code, stdout, stderr := h.run("pack", "--from", "2026-03-01", "--to", "2026-03-31", "--no-tsa", "--out", out)
	if code != cli.ExitOK || !strings.Contains(stdout, "1 intent has no call entry in this segment") {
		t.Fatalf("pack: exit %d\n%s%s", code, stdout, stderr)
	}

	code, stdout, _ = h.run("verify-pack", out)
	wants := []string{"Evidence pack verification: OK", "format ambermoth-pack/2", "1 intent has no call entry in this segment", "intents:   1 without a call entry"}
	for _, want := range wants {
		if code != cli.ExitOK || !strings.Contains(stdout, want) {
			t.Errorf("verify-pack: exit %d, output lacks %q:\n%s", code, want, stdout)
		}
	}
}

func TestPackWithoutIntentsSaysNothingOfThem(t *testing.T) {
	h := recordedHarness(t)
	out := h.workFile("plain.zip")
	_, stdout, _ := h.run("pack", "--from", "2026-03-01", "--to", "2026-03-31", "--no-tsa", "--out", out)
	_, vout, _ := h.run("verify-pack", out)
	outputs := []string{stdout, vout}
	for _, o := range outputs {
		if strings.Contains(o, "intent") || strings.Contains(o, orphanSentence) {
			t.Errorf("output mentions intents:\n%s", o)
		}
	}
	if !strings.Contains(vout, "format ambermoth-pack/1") {
		t.Errorf("not a /1 pack:\n%s", vout)
	}
}

func TestReconcilePrintsTheOrphanHintOnlyNextToAMismatch(t *testing.T) {
	h := recordedHarness(t)
	h.appendTo(t, intentFor(strings.Repeat("d", 32), "chat.completions"))
	csv := h.workFile("usage.csv")
	writeFile(t, csv, "date,model,input_tokens,output_tokens\n2026-03-04,m,500,50\n")

	code, out, _ := h.run("reconcile", "--provider", "openai", "--csv", csv)
	if code != cli.ExitOK || !strings.Contains(out, "MISMATCH") || !strings.Contains(out, "1 intent in this period has no call entry.") {
		t.Errorf("exit %d\n%s", code, out)
	}

	writeFile(t, csv, "date,model,input_tokens,output_tokens\n2026-03-04,m,100,20\n")
	code, out, _ = h.run("reconcile", "--provider", "openai", "--csv", csv)
	if code != cli.ExitOK || strings.Contains(out, "intent") {
		t.Errorf("a match: exit %d\n%s", code, out)
	}
}
