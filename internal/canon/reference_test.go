package canon_test

// This file is the canonical writer as it was before it was split into
// smaller functions (write and writeString in internal/canon/canon.go).
// It is frozen on purpose and kept only to compare the package against: every
// entry_hash in every existing ledger is the SHA-256 of this output, so the
// package must produce the same bytes and the same errors for every input.
// Only the identifiers are renamed. Do not edit the logic. If a difference
// shows up, the package is wrong, not this file.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

const referenceMaxSafeInt = 1<<53 - 1

var referenceIntRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

func referenceMarshal(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := referenceWrite(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func referenceWrite(b *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		return referenceWriteString(b, t)
	case json.Number:
		s := string(t)
		if !referenceIntRe.MatchString(s) || s == "-0" {
			return fmt.Errorf("non-integer or non-canonical number %q", s)
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n > referenceMaxSafeInt || n < -referenceMaxSafeInt {
			return fmt.Errorf("number out of range %q", s)
		}
		b.WriteString(s)
	case int:
		return referenceWrite(b, json.Number(strconv.FormatInt(int64(t), 10)))
	case int64:
		return referenceWrite(b, json.Number(strconv.FormatInt(t, 10)))
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return referenceUTF16Less(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := referenceWriteString(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := referenceWrite(b, t[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := referenceWrite(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	default:
		return fmt.Errorf("unsupported type %T", v)
	}
	return nil
}

func referenceWriteString(b *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return errors.New("invalid UTF-8 in string")
	}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return nil
}

func referenceUTF16Less(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}
