// Package ledger implements the append-only, hash-chained ledger (SPEC §2–§4).
package ledger

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/devrdias/ambermoth/internal/canon"
)

const (
	FormatVersion = 1
	ZeroHash      = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	TimeLayout    = "2006-01-02T15:04:05.000Z"
)

// Entry types (SPEC §4).
const (
	TypeGenesis        = "genesis"
	TypeCall           = "call"
	TypeAmendment      = "amendment"
	TypeKeyEvent       = "key_event"
	TypeCodeLink       = "code_link"
	TypeAnchor         = "anchor"
	TypeGap            = "gap"
	TypeReconciliation = "reconciliation"
)

// Entry is a ledger entry. It is a generic map so that unknown fields from
// newer writers survive verification and are covered by the hash.
type Entry map[string]any

func (e Entry) Str(k string) string {
	s, _ := e[k].(string)
	return s
}

func (e Entry) Int(k string) int64 {
	switch v := e[k].(type) {
	case json.Number:
		n, _ := v.Int64()
		return n
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

func (e Entry) Bool(k string) bool {
	b, _ := e[k].(bool)
	return b
}

func (e Entry) Type() string { return e.Str("type") }
func (e Entry) Seq() int64   { return e.Int("seq") }
func (e Entry) Hash() string { return e.Str("entry_hash") }

func (e Entry) Time() time.Time { return parseTS(e.Str("ts")) }

// parseTS parses an entry timestamp; the zero time when it does not parse.
func parseTS(ts string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, ts)
	return t
}

// Day is the entry's UTC date (YYYY-MM-DD), or "" if its ts does not parse.
// Use it instead of slicing ts: a pack from someone else can hold any string.
func (e Entry) Day() string {
	t := e.Time()
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}

// ComputeHash returns the entry_hash for e (SPEC §3.2).
func ComputeHash(e Entry) (string, error) {
	c := make(map[string]any, len(e))
	for k, v := range e {
		if k != "entry_hash" {
			c[k] = v
		}
	}
	b, err := canon.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// HashBytes decodes "sha256:<hex>" into raw bytes.
func HashBytes(h string) ([]byte, error) {
	if !strings.HasPrefix(h, "sha256:") {
		return nil, fmt.Errorf("not a sha256 hash: %q", h)
	}
	b, err := hex.DecodeString(h[7:])
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("malformed hash %q", h)
	}
	return b, nil
}

// Clock returns the current time. Stores, writers and packs take one so that
// tests can fix the time.
type Clock func() time.Time

// FormatTime formats t in ledger format.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// NewSaltID returns a fresh 16-byte random salt id (hex).
func NewSaltID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// EntryKey derives the per-entry HMAC key (SPEC §3.3).
func EntryKey(master []byte, saltID string) []byte {
	m := hmac.New(sha256.New, master)
	m.Write([]byte("ambermoth/entry-key/v1:" + saltID))
	return m.Sum(nil)
}

// KeyCommitment returns the genesis commitment to the master key.
func KeyCommitment(master []byte) string {
	h := sha256.New()
	h.Write([]byte("ambermoth/key-commit/v1:"))
	h.Write(master)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// FormatHMAC renders a content HMAC.
func FormatHMAC(sum []byte) string { return "hmac-sha256:" + hex.EncodeToString(sum) }

// KeyFingerprint returns the fingerprint of an API key (SPEC §3.4).
func KeyFingerprint(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ShortFP shortens a fingerprint for display.
func ShortFP(fp string) string {
	if len(fp) > 19 {
		return fp[:19] + "…"
	}
	return fp
}
