// Package canon implements the canonical JSON form used by the ledger
// (SPEC §3.1): a strict subset of RFC 8785 (JCS) that allows integers only.
package canon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"unicode/utf8"
)

const maxSafeInt = 1<<53 - 1

var intRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// Parse decodes JSON into a generic value, keeping numbers as json.Number
// and rejecting duplicate keys.
func Parse(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after JSON value")
	}
	if _, err := dec.Token(); err == nil {
		return nil, errors.New("trailing data after JSON value")
	}
	return v, nil
}

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil // string, json.Number, bool, nil
	}
	switch d {
	case '{':
		return parseObject(dec)
	case '[':
		return parseArray(dec)
	}
	return nil, fmt.Errorf("unexpected delimiter %v", d)
}

// parseObject reads the members of an object whose '{' was already read,
// rejecting duplicate keys.
func parseObject(dec *json.Decoder) (map[string]any, error) {
	m := map[string]any{}
	for dec.More() {
		k, err := objectKey(dec)
		if err != nil {
			return nil, err
		}
		if _, dup := m[k]; dup {
			return nil, fmt.Errorf("duplicate key %q", k)
		}
		v, err := parseValue(dec)
		if err != nil {
			return nil, err
		}
		m[k] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return m, nil
}

func objectKey(dec *json.Decoder) (string, error) {
	kt, err := dec.Token()
	if err != nil {
		return "", err
	}
	k, ok := kt.(string)
	if !ok {
		return "", errors.New("object key is not a string")
	}
	return k, nil
}

// parseArray reads the elements of an array whose '[' was already read.
func parseArray(dec *json.Decoder) ([]any, error) {
	arr := []any{}
	for dec.More() {
		v, err := parseValue(dec)
		if err != nil {
			return nil, err
		}
		arr = append(arr, v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return arr, nil
}

// Marshal returns the canonical serialisation of v. v may contain
// map[string]any, []any, string, bool, nil, json.Number and Go integer types.
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := write(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// FromStruct converts any JSON-marshalable value to its canonical bytes.
func FromStruct(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	g, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return Marshal(g)
}

func write(b *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		writeBool(b, t)
	case string:
		return writeString(b, t)
	case json.Number:
		return writeNumber(b, string(t))
	case int:
		return writeInt(b, int64(t))
	case int64:
		return writeInt(b, t)
	case map[string]any:
		return writeObject(b, t)
	case []any:
		return writeArray(b, t)
	default:
		return fmt.Errorf("unsupported type %T", v)
	}
	return nil
}

func writeBool(b *bytes.Buffer, v bool) {
	if v {
		b.WriteString("true")
		return
	}
	b.WriteString("false")
}

// writeNumber takes the text of an integer; the range and error text are the
// ones json.Number input gets.
func writeNumber(b *bytes.Buffer, s string) error {
	if !intRe.MatchString(s) || s == "-0" {
		return fmt.Errorf("non-integer or non-canonical number %q", s)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n > maxSafeInt || n < -maxSafeInt {
		return fmt.Errorf("number out of range %q", s)
	}
	b.WriteString(s)
	return nil
}

func writeInt(b *bytes.Buffer, n int64) error {
	return writeNumber(b, strconv.FormatInt(n, 10))
}

func writeObject(b *bytes.Buffer, m map[string]any) error {
	b.WriteByte('{')
	for i, k := range sortedKeys(m) {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := writeString(b, k); err != nil {
			return err
		}
		b.WriteByte(':')
		if err := write(b, m[k]); err != nil {
			return err
		}
	}
	b.WriteByte('}')
	return nil
}

// sortedKeys orders keys by UTF-16 code units (RFC 8785). For ASCII keys that
// is plain byte order, which is what almost every ledger key uses.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	ascii := true
	for k := range m {
		keys = append(keys, k)
		ascii = ascii && isASCII(k)
	}
	if ascii {
		slices.Sort(keys)
		return keys
	}
	slices.SortFunc(keys, utf16Compare)
	return keys
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func writeArray(b *bytes.Buffer, a []any) error {
	b.WriteByte('[')
	for i, e := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := write(b, e); err != nil {
			return err
		}
	}
	b.WriteByte(']')
	return nil
}

func writeString(b *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return errors.New("invalid UTF-8 in string")
	}
	b.WriteByte('"')
	for _, r := range s {
		writeRune(b, r)
	}
	b.WriteByte('"')
	return nil
}

func writeRune(b *bytes.Buffer, r rune) {
	if esc, ok := shortEscape(r); ok {
		b.WriteString(esc)
		return
	}
	if r < 0x20 {
		fmt.Fprintf(b, `\u%04x`, r)
		return
	}
	b.WriteRune(r)
}

func shortEscape(r rune) (string, bool) {
	switch r {
	case '"':
		return `\"`, true
	case '\\':
		return `\\`, true
	case '\b':
		return `\b`, true
	case '\f':
		return `\f`, true
	case '\n':
		return `\n`, true
	case '\r':
		return `\r`, true
	case '\t':
		return `\t`, true
	}
	return "", false
}

// utf16Compare orders a and b by their UTF-16 code units without encoding
// them. Invalid bytes count as U+FFFD, as they do in a []rune conversion.
func utf16Compare(a, b string) int {
	for a != "" && b != "" {
		ra, na := utf8.DecodeRuneInString(a)
		rb, nb := utf8.DecodeRuneInString(b)
		if ra != rb {
			return compareRunes(ra, rb)
		}
		a, b = a[na:], b[nb:]
	}
	return len(a) - len(b)
}

// compareRunes orders two different runes by their UTF-16 encodings. A
// supplementary rune starts with a surrogate (D800-DBFF), so it sorts before
// BMP runes from U+E000 up even though its code point is larger.
func compareRunes(a, b rune) int {
	if ua, ub := firstUnit(a), firstUnit(b); ua != ub {
		return int(ua - ub)
	}
	return int(a - b)
}

func firstUnit(r rune) rune {
	if r < 0x10000 {
		return r
	}
	return 0xD800 + (r-0x10000)>>10
}
