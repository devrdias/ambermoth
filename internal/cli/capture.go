package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/ops"
	"github.com/devrdias/ambermoth/internal/proxy"
)

// keyActions are the subcommands of 'ambermoth key'.
//
//rules:readonly lookup table, never written.
var keyActions = map[string]bool{"enroll": true, "rotate": true, "revoke": true}

// Proxy timeouts.
const (
	writerQueue       = 10000
	readHeaderTimeout = 30 * time.Second
	shutdownGrace     = 30 * time.Second
)

func (a *App) cmdInit(_ context.Context, args []string) error {
	fs, dir := a.newFlags("init")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	s, err := ledger.Init(*dir, a.generator(), a.env.Clock)
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(*dir, ledger.ConfigFile)
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(cfgPath, proxy.MarshalConfig(proxy.DefaultConfig()), 0o600); err != nil {
			return fmt.Errorf("writing config: %w", err)
		}
	}
	fmt.Fprintf(a.env.Stdout, `Ledger created in %s
  ledger id: %s
  master key: %s/secret.key  (back it up; keep it private — it never leaves this machine)

Next:
  ambermoth proxy
  export OPENAI_BASE_URL=http://127.0.0.1:8787/openai/v1
  export ANTHROPIC_BASE_URL=http://127.0.0.1:8787/anthropic
`, *dir, s.LedgerID, *dir)
	return nil
}

// cmdProxy runs the recording proxy until ctx is cancelled (SIGINT/SIGTERM in
// main), then lets in-flight calls finish and be recorded before returning.
func (a *App) cmdProxy(ctx context.Context, args []string) error {
	fs, dir := a.newFlags("proxy")
	listen := fs.String("listen", "", "listen address (default from config, 127.0.0.1:8787)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	s, cfg, err := a.open(*dir)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	interval, err := anchorInterval(cfg)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	return a.serveProxy(ctx, s, cfg, ln, loops{anchorEvery: interval})
}

func anchorInterval(cfg proxy.Config) (time.Duration, error) {
	if cfg.AnchorInterval == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(cfg.AnchorInterval)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("config anchor_interval %q: want a positive duration such as 1h", cfg.AnchorInterval)
	}
	return d, nil
}

// loops are the optional background loops next to the proxy.
type loops struct {
	anchorEvery time.Duration // 0 = no anchor loop
}

// serveProxy owns up to three goroutines: the ledger writer, the anchor loop,
// and the HTTP server. All have exited when it returns.
func (a *App) serveProxy(ctx context.Context, s *ledger.Store, cfg proxy.Config, ln net.Listener, l loops) error {
	w := proxy.NewWriter(s, writerQueue, a.env.Logger)
	p, err := proxy.New(cfg, s, w, nil, a.env.Logger)
	if err != nil {
		ln.Close()
		return err
	}
	// The writer must outlive the server so late calls are still recorded, so
	// it stops only through w.Close, not through ctx.
	loopCtx, stopLoops := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.Run(context.WithoutCancel(ctx))
	}()
	defer func() {
		stopLoops()
		w.Close()
		wg.Wait()
	}()
	a.startLoops(loopCtx, &wg, s, cfg, l)
	srv := &http.Server{Handler: p, ReadHeaderTimeout: readHeaderTimeout}
	srv.RegisterOnShutdown(p.CloseUpgrades)
	a.printListening(ln.Addr().String(), cfg)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	fmt.Fprintln(a.env.Stderr, "ambermoth: shutting down, waiting for in-flight calls…")
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		fmt.Fprintln(a.env.Stderr, "ambermoth: some calls were still running at shutdown:", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// startLoops starts the configured loops; the caller waits for them on wg.
func (a *App) startLoops(ctx context.Context, wg *sync.WaitGroup, s *ledger.Store, cfg proxy.Config, l loops) {
	if l.anchorEvery > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.anchorLoop(ctx, s, cfg.TSAs, l.anchorEvery)
		}()
	}
}

func (a *App) printListening(addr string, cfg proxy.Config) {
	names := make([]string, 0, len(cfg.Upstreams))
	for n := range cfg.Upstreams {
		names = append(names, "/"+n)
	}
	fmt.Fprintf(a.env.Stderr, "ambermoth proxy listening on http://%s (upstreams: %s)\n", addr, strings.Join(names, " "))
}

// anchorLoop anchors the chain head every interval until ctx is cancelled.
func (a *App) anchorLoop(ctx context.Context, s *ledger.Store, tsas []string, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := ops.Anchor(ctx, s, tsas, nil); err != nil {
				a.env.Logger.ErrorContext(ctx, "anchoring failed", "error", err)
			}
		}
	}
}

func (a *App) cmdKey(_ context.Context, args []string) error {
	if len(args) < 1 || !keyActions[args[0]] {
		return errors.New("usage: ambermoth key enroll|rotate|revoke [flags]")
	}
	action := args[0]
	fs, dir := a.newFlags("key " + action)
	provider := fs.String("provider", "", "provider name (e.g. openai, anthropic)")
	label := fs.String("label", "", "human label, e.g. prod-backend")
	reason := fs.String("reason", "", "why")
	keyEnv := fs.String("key-env", "", "read the API key from this environment variable")
	fp := fs.String("fp", "", "use an existing sha256: fingerprint instead of the key")
	replacedBy := fs.String("replaced-by", "", "rotate: fingerprint of the new key")
	if _, err := parse(fs, args[1:]); err != nil {
		return err
	}
	s, _, err := a.open(*dir)
	if err != nil {
		return err
	}
	key, err := a.readKey(*keyEnv, *fp)
	if err != nil {
		return err
	}
	e, err := ops.KeyEvent(s, action, key, *fp, *provider, *label, *reason, *replacedBy)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stdout, "recorded key %s: %s (seq %d)\n", action, e.Str("key_fp"), e.Seq())
	return nil
}

// readKey returns the API key from the named environment variable, or from
// the terminal when no fingerprint was given, or "" when one was.
func (a *App) readKey(keyEnv, fp string) (string, error) {
	if keyEnv != "" {
		key := a.env.Getenv(keyEnv)
		if key == "" {
			return "", fmt.Errorf("$%s is empty", keyEnv)
		}
		return key, nil
	}
	if fp != "" {
		return "", nil
	}
	fmt.Fprint(a.env.Stderr, "API key (read from stdin, not stored): ")
	return a.env.ReadSecret()
}

func (a *App) cmdLink(_ context.Context, args []string) error {
	fs, dir := a.newFlags("link")
	purpose := fs.String("purpose", "", "purpose tag the code produces (required)")
	repo := fs.String("repo", ".", "git repository")
	path := fs.String("path", "", "optional subdirectory within the repo")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *purpose == "" {
		return errors.New("--purpose is required")
	}
	s, _, err := a.open(*dir)
	if err != nil {
		return err
	}
	e, err := ops.Link(s, *purpose, *repo, *path)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stdout, "linked %s → %s @ %.12s (history since %s, %d commits; seq %d)\n", *purpose, e.Str("repo"),
		e.Str("commit"), e.Str("first_commit_ts"), e.Int("commit_count"), e.Seq())
	return nil
}

func (a *App) cmdAmend(_ context.Context, args []string) error {
	fs, dir := a.newFlags("amend")
	seq := fs.Int64("seq", 0, "single target seq")
	from := fs.Int64("from-seq", 0, "first target seq")
	to := fs.Int64("to-seq", 0, "last target seq (inclusive)")
	purpose := fs.String("purpose", "", "new purpose tag")
	ann := fs.String("annotation", "", "annotation text")
	reason := fs.String("reason", "", "why (required)")
	by := fs.String("by", "", "who (required)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *seq > 0 {
		*from, *to = *seq, *seq
	}
	if *purpose != "" && !proxy.ValidPurpose(*purpose) {
		return fmt.Errorf("invalid purpose %q (taxonomy: %s, or x-custom)", *purpose, strings.Join(proxy.Taxonomy(), ", "))
	}
	s, _, err := a.open(*dir)
	if err != nil {
		return err
	}
	e, err := ops.Amend(s, *from, *to, *purpose, *ann, *reason, *by)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stdout, "amendment recorded (seq %d) for entries %d–%d\n", e.Seq(), *from, *to)
	return nil
}
