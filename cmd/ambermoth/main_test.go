//go:build integration

package main_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

// TestCLIWorkflow builds the binary and runs the documented workflow end to
// end: init, proxy (with a graceful SIGINT shutdown), key, link, amend,
// anchor, verify, pack, verify-pack, report, alerts.
func TestCLIWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	work := t.TempDir()
	bin := filepath.Join(work, "ambermoth")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	dir := filepath.Join(work, "ledger")
	run := func(wantOK bool, args ...string) string {
		t.Helper()
		c := exec.Command(bin, append(args, "--dir", dir)...)
		c.Dir = work
		c.Env = append(os.Environ(), "OPENAI_KEY=sk-cli-test")
		out, err := c.CombinedOutput()
		if (err == nil) != wantOK {
			t.Fatalf("ambermoth %v: err=%v (want ok=%v)\n%s", args, err, wantOK, out)
		}
		return string(out)
	}

	auth := tsatest.New(t)
	tsaSrv := auth.Server(t)
	ca := filepath.Join(work, "ca.pem")
	os.WriteFile(ca, auth.CAPEM, 0o600)

	slow := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		if strings.Contains(r.URL.Path, "slow") {
			close(slow)
			time.Sleep(300 * time.Millisecond) // still running when SIGINT arrives
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"gpt-test","usage":{"prompt_tokens":100,"completion_tokens":20}}`)
	}))
	defer up.Close()

	run(true, "init")
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	cfgPath := filepath.Join(dir, ledger.ConfigFile)
	var cfg map[string]any
	b, _ := os.ReadFile(cfgPath)
	json.Unmarshal(b, &cfg)
	cfg["listen"] = addr
	cfg["tsas"] = []string{tsaSrv.URL}
	cfg["upstreams"] = map[string]any{"openai": map[string]string{"base_url": up.URL, "style": "openai"}}
	b, _ = json.Marshal(cfg)
	os.WriteFile(cfgPath, b, 0o600)

	// Proxy: one normal call, one in flight at SIGINT. Both must be recorded.
	px := exec.Command(bin, "proxy", "--dir", dir)
	px.Stderr = io.Discard
	if err := px.Start(); err != nil {
		t.Fatal(err)
	}
	base := "http://" + addr
	for i := 0; ; i++ {
		if resp, err := http.Get(base + "/_ambermoth/health"); err == nil {
			resp.Body.Close()
			break
		}
		if i > 100 {
			t.Fatal("proxy did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	post := func(path string) error {
		req, _ := http.NewRequest("POST", base+"/openai/v1/"+path, strings.NewReader(`{"model":"gpt-test"}`))
		req.Header.Set("Authorization", "Bearer sk-cli-test")
		req.Header.Set("X-Provenance-Purpose", "eval-regression")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		return err
	}
	if err := post("chat/completions"); err != nil {
		t.Fatal(err)
	}
	slowDone := make(chan error, 1)
	go func() { slowDone <- post("slow/chat/completions") }()
	<-slow
	px.Process.Signal(syscall.SIGINT)
	if err := <-slowDone; err != nil {
		t.Fatalf("in-flight call cut off at shutdown: %v", err)
	}
	if err := px.Wait(); err != nil {
		t.Fatalf("proxy exit: %v", err)
	}
	s, err := ledger.Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	es, _ := s.Entries()
	var nCalls int
	for _, e := range es {
		if e != nil && e.Type() == ledger.TypeCall {
			nCalls++
		}
	}
	if nCalls != 2 {
		t.Fatalf("want 2 recorded calls (including the in-flight one), got %d", nCalls)
	}

	out := run(true, "key", "enroll", "--provider", "openai", "--label", "ci", "--key-env", "OPENAI_KEY")
	if !strings.Contains(out, "recorded key enroll") {
		t.Errorf("key enroll output: %s", out)
	}
	run(false, "amend", "--seq", "1", "--purpose", "x") // missing --reason/--by
	run(true, "anchor")
	run(true, "verify", "--tsa-ca", ca)
	run(false, "verify") // test TSA is not pinned by default
	out = run(true, "verify", "--allow-unpinned-tsa")
	if !strings.Contains(out, "NOT PINNED") {
		t.Errorf("unpinned anchors should be labelled:\n%s", out)
	}
	if !strings.Contains(out, "anchor lag") {
		t.Errorf("verify should report anchor lag:\n%s", out)
	}

	today := time.Now().UTC().Format("2006-01-02")
	pack := filepath.Join(work, "pack.zip")
	run(true, "pack", "--from", today, "--to", today, "--out", pack, "--tsa-ca", ca)
	report := filepath.Join(work, "report.html")
	out = run(true, "verify-pack", pack, "--tsa-ca", ca, "--report", report)
	if !strings.Contains(out, "summary.html: identical") {
		t.Errorf("verify-pack output:\n%s", out)
	}
	rb, err := os.ReadFile(report)
	if err != nil || !strings.Contains(string(rb), "Verified</span></strong>") {
		t.Errorf("verifier report missing or not verified: %v", err)
	}
	run(false, "verify-pack", pack) // test TSA not pinned
	out = run(true, "report", "--from", today, "--to", today)
	if !strings.Contains(out, "eval-regression") {
		t.Errorf("report output:\n%s", out)
	}
	run(true, "alerts")
	if out := run(true, "version"); !strings.HasPrefix(out, "ambermoth ") {
		t.Errorf("version: %s", out)
	}
}

// A reader that goes away ('ambermoth report | head -0') must give the same
// result as a failing writer in the CLI tests: exit 1, nothing on stderr, and
// no death by SIGPIPE.
func TestBrokenStdoutPipeExitsWithFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	work := t.TempDir()
	bin := filepath.Join(work, "ambermoth")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	dir := filepath.Join(work, "ledger")
	if out, err := exec.Command(bin, "init", "--dir", dir).CombinedOutput(); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	defer w.Close()
	var stderr strings.Builder
	c := exec.Command(bin, "report", "--dir", dir)
	c.Stdout, c.Stderr = w, &stderr
	err = c.Run()
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("err = %v, want an exit error", err)
	}
	if ws, _ := ee.Sys().(syscall.WaitStatus); ws.Signaled() {
		t.Fatalf("killed by %v, want a plain exit", ws.Signal())
	}
	if ee.ExitCode() != 1 || stderr.Len() != 0 {
		t.Errorf("exit %d, stderr %q; want exit 1 and nothing on stderr", ee.ExitCode(), stderr.String())
	}
}
