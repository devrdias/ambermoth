// Package proxy is the pass-through recording proxy (SPEC §6).
package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/devrdias/ambermoth/internal/ledger"
)

const (
	maxReqCapture  = 1 << 20 // bytes of request body kept for parsing model/stream
	maxRespCapture = 4 << 20 // bytes of non-streaming response kept for usage parsing
	maxNoteBytes   = 512     // X-Provenance-Note is truncated to this
	maxGitRefBytes = 256     // X-Provenance-Git-Ref is truncated to this
)

// Proxy records every call it forwards.
type Proxy struct {
	cfg    Config
	store  *ledger.Store
	writer *Writer
	rps    map[string]*httputil.ReverseProxy
	ups    map[string]*url.URL

	mu       sync.Mutex
	known    map[string]bool // enrolled key fingerprints
	upgrades map[*upgradeConn]struct{}
	log      *slog.Logger
}

type ctxKey struct{}

// call carries per-request recording state.
type call struct {
	upstream, style, path, method string
	transport                     string // "" for HTTP, "websocket" for upgrades
	start                         time.Time
	purpose, note, gitRef, keyFP  string
	saltID                        string
	entryKey                      []byte

	reqMAC   hash.Hash
	reqBytes int64
	reqCap   []byte
	reqDone  bool

	once sync.Once
}

// New builds a proxy. Known key fingerprints are loaded from the ledger.
func New(cfg Config, s *ledger.Store, w *Writer, transport http.RoundTripper, log *slog.Logger) (*Proxy, error) {
	p := &Proxy{cfg: cfg, store: s, writer: w, rps: map[string]*httputil.ReverseProxy{},
		ups: map[string]*url.URL{}, known: map[string]bool{}, upgrades: map[*upgradeConn]struct{}{}, log: log}
	entries, err := s.Entries()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e != nil && e.Type() == ledger.TypeKeyEvent {
			p.known[e.Str("key_fp")] = e.Str("action") != "revoke"
		}
	}
	for name, u := range cfg.Upstreams {
		target, err := url.Parse(u.BaseURL)
		if err != nil {
			return nil, err
		}
		p.ups[name] = target
		rp := &httputil.ReverseProxy{
			FlushInterval: -1, // stream every write immediately
			Transport:     transport,
			Rewrite: func(pr *httputil.ProxyRequest) {
				c := pr.In.Context().Value(ctxKey{}).(*call)
				pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
				pr.Out.URL.Path = strings.TrimRight(target.Path, "/") + c.path
				pr.Out.URL.RawPath = ""
				pr.Out.Host = target.Host
			},
			ModifyResponse: p.modifyResponse,
			ErrorHandler:   p.errorHandler,
		}
		p.rps[name] = rp
	}
	return p, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	name, rest, _ := strings.Cut(trimmed, "/")
	if name == "_ambermoth" && rest == "health" {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
		return
	}
	rp, ok := p.rps[name]
	if !ok {
		http.Error(w, `{"error":"ambermoth: unknown upstream `+name+`; configure it in .ambermoth/config.json"}`, http.StatusNotFound)
		return
	}
	up := p.cfg.Upstreams[name]
	salt := ledger.NewSaltID()
	ek := ledger.EntryKey(p.store.MasterKey, salt)
	c := &call{upstream: name, style: up.Style, path: "/" + rest, method: r.Method,
		start: p.store.Now(), saltID: salt, entryKey: ek, reqMAC: hmac.New(sha256.New, ek)}

	// Provenance headers: read, then strip before forwarding.
	c.purpose = strings.TrimSpace(r.Header.Get("X-Provenance-Purpose"))
	c.note = truncateUTF8(r.Header.Get("X-Provenance-Note"), maxNoteBytes)
	c.gitRef = truncateUTF8(r.Header.Get("X-Provenance-Git-Ref"), maxGitRefBytes)
	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-provenance-") {
			r.Header.Del(k)
		}
	}
	c.keyFP = ledger.KeyFingerprint(extractKey(r))
	p.resolvePurpose(c)
	p.maybeEnroll(c.keyFP, name)

	if r.Body != nil && r.Body != http.NoBody {
		r.Body = &teeBody{rc: r.Body, c: c}
	} else {
		c.reqDone = true
	}
	ctx := context.WithValue(r.Context(), ctxKey{}, c)
	rp.ServeHTTP(w, r.WithContext(ctx))
}

func extractKey(r *http.Request) string {
	if a := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(a), "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	if v := firstHeader(r.Header, "X-Api-Key", "X-Goog-Api-Key", "Api-Key"); v != "" {
		return v
	}
	return r.URL.Query().Get("key")
}

// firstHeader returns the first non-empty value among names.
func firstHeader(h http.Header, names ...string) string {
	for _, n := range names {
		if v := h.Get(n); v != "" {
			return v
		}
	}
	return ""
}

func (p *Proxy) resolvePurpose(c *call) {
	if c.purpose != "" {
		if ValidPurpose(c.purpose) {
			if c.purpose == "other-declared" && c.note == "" {
				c.note = "other-declared without a note"
			}
			return
		}
		c.note = strings.TrimSpace(c.note + " invalid purpose header: " + c.purpose)
		c.purpose = "untagged"
		return
	}
	for _, rr := range p.cfg.Routes {
		if (rr.Upstream == "" || rr.Upstream == c.upstream) && strings.HasPrefix(c.path, rr.Prefix) {
			c.purpose = rr.Purpose
			return
		}
	}
	if d, ok := p.cfg.KeyDefaults[c.keyFP]; ok {
		c.purpose = d
		return
	}
	c.purpose = p.cfg.DefaultPurpose
	if c.purpose == "" {
		c.purpose = "untagged"
	}
}

func (p *Proxy) maybeEnroll(fp, upstream string) {
	if fp == "" || !p.cfg.AutoEnroll {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, seen := p.known[fp]; seen {
		return
	}
	// Mark the key known only if the enrolment was accepted, so a dropped one
	// is retried on the key's next call.
	if p.writer.Enqueue(ledger.Entry{"type": ledger.TypeKeyEvent, "action": "auto_enroll",
		"key_fp": fp, "provider": upstream, "label": "auto-enrolled by proxy", "reason": "first call seen"}) {
		p.known[fp] = true
	}
}

// truncateUTF8 cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "…[truncated]"
}

// teeBody hashes and partially captures the request body as it is forwarded.
type teeBody struct {
	rc io.ReadCloser
	c  *call
}

func (t *teeBody) Read(b []byte) (int, error) {
	n, err := t.rc.Read(b)
	if n > 0 {
		t.c.reqMAC.Write(b[:n])
		t.c.reqBytes += int64(n)
		if room := maxReqCapture - len(t.c.reqCap); room > 0 {
			k := n
			if k > room {
				k = room
			}
			t.c.reqCap = append(t.c.reqCap, b[:k]...)
		}
	}
	if err == io.EOF {
		t.c.reqDone = true
	}
	return n, err
}

func (t *teeBody) Close() error { return t.rc.Close() }

// respBody hashes the response as it streams to the client and records the
// entry when the body ends (EOF) or is closed early.
type respBody struct {
	rc     io.ReadCloser
	p      *Proxy
	c      *call
	status int
	stream bool
	mac    hash.Hash
	n      int64
	buf    []byte
	sse    *sseParser
	u      usage
	sawEOF bool
}

func (rb *respBody) Read(b []byte) (int, error) {
	n, err := rb.rc.Read(b)
	if n > 0 {
		rb.mac.Write(b[:n])
		rb.n += int64(n)
		if rb.stream {
			rb.sse.Write(b[:n])
		} else if len(rb.buf)+n <= maxRespCapture {
			rb.buf = append(rb.buf, b[:n]...)
		}
	}
	if err == io.EOF {
		rb.sawEOF = true
		rb.finish(false)
	}
	return n, err
}

func (rb *respBody) Close() error {
	err := rb.rc.Close()
	rb.finish(!rb.sawEOF)
	return err
}

func (rb *respBody) finish(aborted bool) {
	rb.c.once.Do(func() {
		if rb.stream {
			rb.sse.Flush()
		} else if rb.n <= maxRespCapture {
			rb.u.absorbJSON(rb.buf)
		}
		rb.p.record(rb.c, rb.status, rb.stream, &rb.u, rb.mac.Sum(nil), rb.n, aborted)
	})
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	c, ok := resp.Request.Context().Value(ctxKey{}).(*call)
	if !ok {
		return nil
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		// ReverseProxy uses the body as the raw backend connection. Wrap it so
		// both directions are counted and HMACed, and record on close.
		rwc, ok := resp.Body.(io.ReadWriteCloser)
		if !ok {
			return nil // ReverseProxy will reject this response itself
		}
		c.transport = "websocket"
		uc := &upgradeConn{rwc: rwc, p: p, c: c, out: hmac.New(sha256.New, c.entryKey)}
		p.mu.Lock()
		p.upgrades[uc] = struct{}{}
		p.mu.Unlock()
		resp.Body = uc
		return nil
	}
	stream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	rb := &respBody{rc: resp.Body, p: p, c: c, status: resp.StatusCode, stream: stream,
		mac: hmac.New(sha256.New, c.entryKey)}
	rb.sse = &sseParser{u: &rb.u}
	resp.Body = rb
	return nil
}

// upgradeConn wraps the backend side of an upgraded (WebSocket) connection.
// Reads are upstream→client (output); writes are client→upstream (input).
type upgradeConn struct {
	rwc io.ReadWriteCloser
	p   *Proxy
	c   *call
	mu  sync.Mutex
	out hash.Hash
	n   int64
}

func (u *upgradeConn) Read(b []byte) (int, error) {
	n, err := u.rwc.Read(b)
	if n > 0 {
		u.mu.Lock()
		u.out.Write(b[:n])
		u.n += int64(n)
		u.mu.Unlock()
	}
	return n, err
}

func (u *upgradeConn) Write(b []byte) (int, error) {
	n, err := u.rwc.Write(b)
	if n > 0 {
		u.mu.Lock()
		u.c.reqMAC.Write(b[:n])
		u.c.reqBytes += int64(n)
		u.mu.Unlock()
	}
	return n, err
}

func (u *upgradeConn) Close() error {
	err := u.rwc.Close()
	u.c.once.Do(func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		u.c.reqDone = true
		u.p.record(u.c, http.StatusSwitchingProtocols, false, &usage{}, u.out.Sum(nil), u.n, false)
	})
	u.p.mu.Lock()
	delete(u.p.upgrades, u)
	u.p.mu.Unlock()
	return err
}

// CloseUpgrades closes every open upgraded connection, which records them.
// http.Server.Shutdown does not track hijacked connections, so call this from
// RegisterOnShutdown.
func (p *Proxy) CloseUpgrades() {
	p.mu.Lock()
	open := make([]*upgradeConn, 0, len(p.upgrades))
	for u := range p.upgrades {
		open = append(open, u)
	}
	p.mu.Unlock()
	for _, u := range open {
		u.Close()
	}
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	c, _ := r.Context().Value(ctxKey{}).(*call)
	if c != nil {
		empty := hmac.New(sha256.New, c.entryKey).Sum(nil)
		c.once.Do(func() { p.record(c, 0, false, &usage{}, empty, 0, false) })
	}
	p.log.ErrorContext(r.Context(), "upstream error", "error", err)
	w.WriteHeader(http.StatusBadGateway)
}

func (p *Proxy) record(c *call, status int, stream bool, u *usage, outMAC []byte, outBytes int64, aborted bool) {
	ri := parseRequest(c.reqCap, c.reqDone && int64(len(c.reqCap)) == c.reqBytes)
	model := ri.Model
	if model == "" {
		model = u.Model
	}
	e := ledger.Entry{
		"type": ledger.TypeCall, "ts": ledger.FormatTime(c.start),
		"provider": c.upstream, "api_style": c.style, "endpoint": endpointClass(c.path),
		"method": c.method, "path": c.path, "model": model, "stream": stream || ri.Stream,
		"status": status, "tokens_in": u.In, "tokens_out": u.Out, "cached_in": u.Cached,
		"usage_reported": u.Reported, "latency_ms": p.store.Now().Sub(c.start).Milliseconds(),
		"key_fp": c.keyFP, "purpose": c.purpose, "salt_id": c.saltID,
		"prompt_hmac": ledger.FormatHMAC(c.reqMAC.Sum(nil)), "output_hmac": ledger.FormatHMAC(outMAC),
		"bytes_in": c.reqBytes, "bytes_out": outBytes, "client_aborted": aborted,
	}
	if c.note != "" {
		e["note"] = c.note
	}
	if c.gitRef != "" {
		e["git_ref"] = c.gitRef
	}
	if c.transport != "" {
		e["transport"] = c.transport
		e["endpoint"] = "realtime"
	}
	p.writer.Enqueue(e)
}

// MarshalConfig is a helper for `ambermoth init`.
func MarshalConfig(c Config) []byte {
	b, _ := json.MarshalIndent(c, "", "  ")
	return append(b, '\n')
}
