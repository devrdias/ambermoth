package canon_test

import (
	"testing"

	"github.com/devrdias/ambermoth/internal/canon"
)

type canonCase struct {
	name string
	in   string
	want string
}

func TestCanonical(t *testing.T) {
	t.Parallel()
	cases := []canonCase{
		{name: "sorts keys and keeps HTML characters", in: `{"b":1, "a":"x<&>é"}`, want: `{"a":"x<&>é","b":1}`},
		{name: "sorts keys inside arrays", in: `{"z":[true,null,{"y":2,"x":-3}]}`, want: `{"z":[true,null,{"x":-3,"y":2}]}`},
		{name: "escapes control characters", in: "{\"s\":\"a\\u0001\\n\"}", want: `{"s":"a\u0001\n"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			v, err := canon.Parse([]byte(c.in))
			if err != nil {
				t.Fatal(err)
			}
			got, err := canon.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("canon(%s) = %s, want %s", c.in, got, c.want)
			}
		})
	}
}

type rejectCase struct {
	name string
	in   string
}

func TestRejects(t *testing.T) {
	t.Parallel()
	cases := []rejectCase{
		{name: "rejects a fraction", in: `{"a":1.5}`},
		{name: "rejects an exponent", in: `{"a":1e3}`},
		{name: "rejects a duplicate key", in: `{"a":1,"a":2}`},
		{name: "rejects an integer beyond 2^53", in: `{"a":9007199254740993}`},
		{name: "rejects a non-string key", in: `{1:2}`},
		{name: "rejects trailing data", in: `{"a":1} {}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			v, err := canon.Parse([]byte(c.in))
			if err == nil {
				_, err = canon.Marshal(v)
			}
			if err == nil {
				t.Errorf("expected rejection for %s", c.in)
			}
		})
	}
}
