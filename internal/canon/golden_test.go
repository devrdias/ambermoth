package canon_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/canon"
)

// goldenCase holds an input and the exact bytes (or error text) SPEC section 3.1
// requires for it. The expected values are written by hand, not produced by
// the code under test.
type goldenCase struct {
	name string
	in   any
	want string
	err  string
}

type namedMap map[string]any

func nestedArrays(depth int) (any, string) {
	var v any = []any{}
	for i := 1; i < depth; i++ {
		v = []any{v}
	}
	return v, strings.Repeat("[", depth) + strings.Repeat("]", depth)
}

func nestedObjects(depth int) (any, string) {
	var v any = map[string]any{}
	for i := 1; i < depth; i++ {
		v = map[string]any{"k": v}
	}
	return v, strings.Repeat(`{"k":`, depth-1) + "{}" + strings.Repeat("}", depth-1)
}

func deepGolden() []goldenCase {
	arr, arrWant := nestedArrays(100)
	obj, objWant := nestedObjects(100)
	deep, deepWant := nestedArrays(1000)
	return []goldenCase{
		{name: "writes 100 nested arrays", in: arr, want: arrWant},
		{name: "writes 100 nested objects", in: obj, want: objWant},
		{name: "writes 1000 nested arrays", in: deep, want: deepWant},
	}
}

var structureGolden = []goldenCase{
	{name: "writes an empty object", in: map[string]any{}, want: `{}`},
	{name: "writes an empty array", in: []any{}, want: `[]`},
	{name: "writes a nil map as an empty object", in: map[string]any(nil), want: `{}`},
	{name: "writes a nil slice as an empty array", in: []any(nil), want: `[]`},
	{name: "writes nil as null", in: nil, want: `null`},
	{name: "writes nested empties", in: map[string]any{"b": []any{}, "a": map[string]any{}}, want: `{"a":{},"b":[]}`},
	{name: "writes a nil map and a nil slice inside a value", in: []any{map[string]any(nil), []any(nil)}, want: `[{},[]]`},
	{name: "writes true", in: true, want: `true`},
	{name: "writes false", in: false, want: `false`},
	{name: "writes every JSON type", in: map[string]any{
		"s": "x", "n": json.Number("1"), "i": 2, "i64": int64(3), "t": true, "f": false, "z": nil, "a": []any{}, "o": map[string]any{},
	}, want: `{"a":[],"f":false,"i":2,"i64":3,"n":1,"o":{},"s":"x","t":true,"z":null}`},
	{name: "writes no whitespace between elements", in: []any{1, "a", nil, true, []any{}}, want: `[1,"a",null,true,[]]`},
	{name: "writes nested objects and arrays", in: map[string]any{
		"z": map[string]any{"b": []any{map[string]any{"y": 1, "x": 2}}, "a": nil},
	}, want: `{"z":{"a":null,"b":[{"x":2,"y":1}]}}`},
	{name: "keeps array order", in: []any{3, 1, 2}, want: `[3,1,2]`},
}

var keyOrderGolden = []goldenCase{
	{name: "sorts keys", in: map[string]any{"b": 1, "a": 2, "c": 3}, want: `{"a":2,"b":1,"c":3}`},
	{name: "sorts upper case before lower case", in: map[string]any{"a": 1, "B": 2, "A": 3, "b": 4}, want: `{"A":3,"B":2,"a":1,"b":4}`},
	{name: "sorts a prefix before the longer key", in: map[string]any{"ab": 1, "a": 2, "abc": 3, "": 4}, want: `{"":4,"a":2,"ab":1,"abc":3}`},
	{name: "sorts digits before letters and underscore between", in: map[string]any{"a": 1, "_": 2, "Z": 3, "9": 4}, want: `{"9":4,"Z":3,"_":2,"a":1}`},
	{name: "sorts non-ASCII keys after ASCII", in: map[string]any{
		"\u65e5": 5, "\u00e9": 4, "z": 2, "Z": 1, "\u00e4": 3,
	}, want: "{\"Z\":1,\"z\":2,\"\u00e4\":3,\"\u00e9\":4,\"\u65e5\":5}"},
	{name: "sorts a supplementary character before U+FFFF (UTF-16 order, not bytes)", in: map[string]any{
		"\uffff": 2, "\U00010000": 1,
	}, want: "{\"\U00010000\":1,\"\uffff\":2}"},
	{name: "sorts a supplementary character before U+E000 (UTF-16 order, not bytes)", in: map[string]any{
		"\ue000": 2, "\U0001f600": 1,
	}, want: "{\"\U0001f600\":1,\"\ue000\":2}"},
	{name: "sorts two supplementary characters by code point", in: map[string]any{
		"\U00010001": 2, "\U00010000": 1,
	}, want: "{\"\U00010000\":1,\"\U00010001\":2}"},
	{name: "sorts U+D7FF before a supplementary character", in: map[string]any{
		"\U0001f600": 2, "\ud7ff": 1,
	}, want: "{\"\ud7ff\":1,\"\U0001f600\":2}"},
	{name: "sorts nested objects independently", in: map[string]any{
		"b": map[string]any{"y": 1, "x": 2}, "a": map[string]any{"d": 3, "c": 4},
	}, want: `{"a":{"c":4,"d":3},"b":{"x":2,"y":1}}`},
	{name: "escapes keys like strings", in: map[string]any{"b\n": 1, "a\"": 2}, want: `{"a\"":2,"b\n":1}`},
}

var intGolden = []goldenCase{
	{name: "writes int zero", in: 0, want: `0`},
	{name: "writes a negative int", in: -1, want: `-1`},
	{name: "writes int64 zero", in: int64(0), want: `0`},
	{name: "writes the largest safe int", in: 9007199254740991, want: `9007199254740991`},
	{name: "writes the smallest safe int", in: -9007199254740991, want: `-9007199254740991`},
	{name: "writes the largest safe int64", in: int64(9007199254740991), want: `9007199254740991`},
	{name: "writes the smallest safe int64", in: int64(-9007199254740991), want: `-9007199254740991`},
	{name: "rejects an int one past the safe range", in: 9007199254740992, err: `number out of range "9007199254740992"`},
	{name: "rejects an int one below the safe range", in: -9007199254740992, err: `number out of range "-9007199254740992"`},
	{name: "rejects MaxInt64", in: int64(math.MaxInt64), err: `number out of range "9223372036854775807"`},
	{name: "rejects MinInt64", in: int64(math.MinInt64), err: `number out of range "-9223372036854775808"`},
	{name: "rejects MaxInt64 as an int", in: math.MaxInt64, err: `number out of range "9223372036854775807"`},
}

var numberGolden = []goldenCase{
	{name: "writes number 0", in: json.Number("0"), want: `0`},
	{name: "writes a negative number", in: json.Number("-1"), want: `-1`},
	{name: "writes a multi-digit number", in: json.Number("1234567890"), want: `1234567890`},
	{name: "writes the largest safe number", in: json.Number("9007199254740991"), want: `9007199254740991`},
	{name: "writes the smallest safe number", in: json.Number("-9007199254740991"), want: `-9007199254740991`},
	{name: "rejects negative zero", in: json.Number("-0"), err: `non-integer or non-canonical number "-0"`},
	{name: "rejects a leading zero", in: json.Number("01"), err: `non-integer or non-canonical number "01"`},
	{name: "rejects a double zero", in: json.Number("00"), err: `non-integer or non-canonical number "00"`},
	{name: "rejects a negative leading zero", in: json.Number("-01"), err: `non-integer or non-canonical number "-01"`},
	{name: "rejects a plus sign", in: json.Number("+1"), err: `non-integer or non-canonical number "+1"`},
	{name: "rejects a trailing .0", in: json.Number("1.0"), err: `non-integer or non-canonical number "1.0"`},
	{name: "rejects a fraction", in: json.Number("1.5"), err: `non-integer or non-canonical number "1.5"`},
	{name: "rejects an exponent", in: json.Number("1e3"), err: `non-integer or non-canonical number "1e3"`},
	{name: "rejects an upper case exponent", in: json.Number("1E3"), err: `non-integer or non-canonical number "1E3"`},
	{name: "rejects hex", in: json.Number("0x10"), err: `non-integer or non-canonical number "0x10"`},
	{name: "rejects a leading space", in: json.Number(" 1"), err: `non-integer or non-canonical number " 1"`},
	{name: "rejects a trailing space", in: json.Number("1 "), err: `non-integer or non-canonical number "1 "`},
	{name: "rejects a trailing newline", in: json.Number("1\n"), err: `non-integer or non-canonical number "1\n"`},
	{name: "rejects an empty number", in: json.Number(""), err: `non-integer or non-canonical number ""`},
	{name: "rejects a lone minus", in: json.Number("-"), err: `non-integer or non-canonical number "-"`},
	{name: "rejects NaN", in: json.Number("NaN"), err: `non-integer or non-canonical number "NaN"`},
	{name: "rejects Infinity", in: json.Number("Infinity"), err: `non-integer or non-canonical number "Infinity"`},
	{name: "rejects a non-ASCII digit", in: json.Number("\u0661"), err: "non-integer or non-canonical number \"\u0661\""},
	{name: "rejects one past the safe range", in: json.Number("9007199254740992"), err: `number out of range "9007199254740992"`},
	{name: "rejects one below the safe range", in: json.Number("-9007199254740992"), err: `number out of range "-9007199254740992"`},
	{name: "rejects MaxInt64", in: json.Number("9223372036854775807"), err: `number out of range "9223372036854775807"`},
	{name: "rejects MinInt64", in: json.Number("-9223372036854775808"), err: `number out of range "-9223372036854775808"`},
	{name: "rejects a number beyond int64", in: json.Number("99999999999999999999"), err: `number out of range "99999999999999999999"`},
	{name: "rejects a negative number beyond int64", in: json.Number("-99999999999999999999"), err: `number out of range "-99999999999999999999"`},
}

var stringGolden = []goldenCase{
	{name: "writes an empty string", in: "", want: `""`},
	{name: "writes plain ASCII", in: "hello world", want: `"hello world"`},
	{name: "escapes a quote", in: `a"b`, want: `"a\"b"`},
	{name: "escapes a backslash", in: `a\b`, want: `"a\\b"`},
	{name: "leaves a slash alone", in: "a/b", want: `"a/b"`},
	{name: "escapes a backslash before a quote", in: `\"`, want: `"\\\""`},
	{name: "escapes backspace as \\b", in: "\b", want: `"\b"`},
	{name: "escapes form feed as \\f", in: "\f", want: `"\f"`},
	{name: "escapes newline as \\n", in: "\n", want: `"\n"`},
	{name: "escapes carriage return as \\r", in: "\r", want: `"\r"`},
	{name: "escapes tab as \\t", in: "\t", want: `"\t"`},
	{name: "escapes NUL as \\u0000", in: "\x00", want: `"\u0000"`},
	{name: "escapes 0x01 as \\u0001", in: "\x01", want: `"\u0001"`},
	{name: "escapes vertical tab as \\u000b", in: "\v", want: `"\u000b"`},
	{name: "escapes 0x0e with lower case hex", in: "\x0e", want: `"\u000e"`},
	{name: "escapes 0x1a with lower case hex", in: "\x1a", want: `"\u001a"`},
	{name: "escapes 0x1b with lower case hex", in: "\x1b", want: `"\u001b"`},
	{name: "escapes 0x1f as \\u001f", in: "\x1f", want: `"\u001f"`},
	{name: "leaves space alone", in: " ", want: `" "`},
	{name: "leaves DEL alone", in: "\x7f", want: "\"\x7f\""},
	{name: "leaves less-than alone", in: "<", want: `"<"`},
	{name: "leaves greater-than alone", in: ">", want: `">"`},
	{name: "leaves ampersand alone", in: "&", want: `"&"`},
	{name: "leaves an HTML snippet alone", in: "<a href=\"x\">&amp;</a>", want: `"<a href=\"x\">&amp;</a>"`},
	{name: "leaves U+2028 alone", in: "\u2028", want: "\"\u2028\""},
	{name: "leaves U+2029 alone", in: "\u2029", want: "\"\u2029\""},
	{name: "leaves U+0080 alone", in: "\u0080", want: "\"\u0080\""},
	{name: "leaves U+00A0 alone", in: "\u00a0", want: "\"\u00a0\""},
	{name: "leaves a two-byte character alone", in: "\u00e9", want: "\"\u00e9\""},
	{name: "leaves a three-byte character alone", in: "\u65e5\u672c", want: "\"\u65e5\u672c\""},
	{name: "leaves a four-byte character alone", in: "\U0001f600", want: "\"\U0001f600\""},
	{name: "leaves U+FFFF alone", in: "\uffff", want: "\"\uffff\""},
	{name: "leaves U+10FFFF alone", in: "\U0010ffff", want: "\"\U0010ffff\""},
	{name: "leaves a byte order mark alone", in: "\ufeff", want: "\"\ufeff\""},
	{name: "leaves a literal U+FFFD alone", in: "\ufffd", want: "\"\ufffd\""},
	{name: "escapes a control character between letters", in: "a\x02b", want: `"a\u0002b"`},
	{name: "escapes every kind in one string", in: "\"\\\b\f\n\r\t\x00\x1f<>&\u2028", want: "\"\\\"\\\\\\b\\f\\n\\r\\t\\u0000\\u001f<>&\u2028\""},
	{name: "writes a string with a NUL in the middle", in: "a\x00b", want: `"a\u0000b"`},
}

var invalidUTF8Golden = []goldenCase{
	{name: "rejects a lone continuation byte", in: "\x80", err: "invalid UTF-8 in string"},
	{name: "rejects 0xff", in: "\xff", err: "invalid UTF-8 in string"},
	{name: "rejects an overlong slash", in: "a\xc0\xafb", err: "invalid UTF-8 in string"},
	{name: "rejects an encoded surrogate", in: "\xed\xa0\x80", err: "invalid UTF-8 in string"},
	{name: "rejects a truncated sequence", in: "\xe6\x97", err: "invalid UTF-8 in string"},
	{name: "rejects a code point above U+10FFFF", in: "\xf4\x90\x80\x80", err: "invalid UTF-8 in string"},
	{name: "rejects invalid UTF-8 after valid text", in: "ok\xff", err: "invalid UTF-8 in string"},
	{name: "rejects invalid UTF-8 in a key", in: map[string]any{"\xff": 1}, err: "invalid UTF-8 in string"},
	{name: "rejects invalid UTF-8 in a nested array", in: []any{[]any{"\xc3"}}, err: "invalid UTF-8 in string"},
	{name: "writes the key before checking the value", in: map[string]any{"\xff": 1.5}, err: "invalid UTF-8 in string"},
}

var unsupportedGolden = []goldenCase{
	{name: "rejects a float64", in: 1.5, err: "unsupported type float64"},
	{name: "rejects a whole float64", in: float64(1), err: "unsupported type float64"},
	{name: "rejects a float32", in: float32(1), err: "unsupported type float32"},
	{name: "rejects an int32", in: int32(1), err: "unsupported type int32"},
	{name: "rejects an int8", in: int8(1), err: "unsupported type int8"},
	{name: "rejects a uint", in: uint(1), err: "unsupported type uint"},
	{name: "rejects a uint64", in: uint64(1), err: "unsupported type uint64"},
	{name: "rejects a string slice", in: []string{"a"}, err: "unsupported type []string"},
	{name: "rejects a byte slice", in: []byte("a"), err: "unsupported type []uint8"},
	{name: "rejects a raw message", in: json.RawMessage(`1`), err: "unsupported type json.RawMessage"},
	{name: "rejects a string map", in: map[string]string{"a": "b"}, err: "unsupported type map[string]string"},
	{name: "rejects a named map type", in: namedMap{"a": 1}, err: "unsupported type canon_test.namedMap"},
	{name: "rejects a struct", in: struct{}{}, err: "unsupported type struct {}"},
	{name: "rejects a pointer", in: new(int), err: "unsupported type *int"},
	{name: "rejects a nil pointer", in: (*int)(nil), err: "unsupported type *int"},
	{name: "rejects a nil typed map of another type", in: map[string]int(nil), err: "unsupported type map[string]int"},
	{name: "rejects a float inside an array", in: []any{1, 2.5}, err: "unsupported type float64"},
	{name: "rejects a float inside an object", in: map[string]any{"a": map[string]any{"b": 0.5}}, err: "unsupported type float64"},
	{name: "reports the first bad value in key order", in: map[string]any{"b": 1.5, "a": json.Number("1.5")}, err: `non-integer or non-canonical number "1.5"`},
	{name: "reports the first bad array element", in: []any{json.Number("-0"), 1.5}, err: `non-integer or non-canonical number "-0"`},
	{name: "reports a bad value before a later bad key", in: map[string]any{"a": 1.5, "\xff": 1}, err: "unsupported type float64"},
}

func goldenCases() []goldenCase {
	groups := [][]goldenCase{
		structureGolden, keyOrderGolden, intGolden, numberGolden,
		stringGolden, invalidUTF8Golden, unsupportedGolden, deepGolden(),
	}
	var all []goldenCase
	for _, g := range groups {
		all = append(all, g...)
	}
	return all
}

func TestGolden(t *testing.T) {
	t.Parallel()
	for _, c := range goldenCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := canon.Marshal(c.in)
			if c.err != "" {
				if err == nil || err.Error() != c.err {
					t.Fatalf("error = %v, want %q", err, c.err)
				}
				if got != nil {
					t.Errorf("bytes = %q on error, want nil", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

// hashCase pins the SHA-256 of a canonical line. The digests were computed
// with sha256sum over the bytes in canon, not with this package.
type hashCase struct {
	name  string
	in    any
	canon string
	sha   string
}

var hashGolden = []hashCase{
	{name: "hashes a small object", in: map[string]any{"b": 1, "a": "x"}, canon: `{"a":"x","b":1}`,
		sha: "cdab067e9f3beb32d1252cfd63e492592fecbf591b0d08cadb24bb17f3864246"},
	{name: "hashes an entry with escapes and non-ASCII", in: map[string]any{
		"v": 1, "seq": 7, "type": "call", "note": "caf\u00e9 <b>&\n\u2028\U0001f600", "stream": false, "model": nil,
	}, canon: "{\"model\":null,\"note\":\"caf\u00e9 <b>&\\n\u2028\U0001f600\",\"seq\":7,\"stream\":false,\"type\":\"call\",\"v\":1}",
		sha: "5749b03de6400361534eb3cdb82805565dfdbe78611105b8ff2bdabe6188f157"},
}

func TestHashIsPinned(t *testing.T) {
	t.Parallel()
	for _, c := range hashGolden {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := canon.Marshal(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.canon {
				t.Fatalf("canonical bytes %q, want %q", got, c.canon)
			}
			sum := sha256.Sum256(got)
			if h := hex.EncodeToString(sum[:]); h != c.sha {
				t.Errorf("sha256 %s, want %s", h, c.sha)
			}
		})
	}
}

func TestDoesNotModifyItsInput(t *testing.T) {
	t.Parallel()
	in := map[string]any{"b": []any{map[string]any{"y": 1, "x": 2}}, "a": "\u00e9"}
	first, err := canon.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := canon.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("second call differs: %q vs %q", first, second)
	}
}
