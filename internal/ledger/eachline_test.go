package ledger_test

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
)

func TestEachLineGivesLinesAndOffsets(t *testing.T) {
	t.Parallel()
	in := "{\"seq\":0}\r\n\n  \n{\"seq\":1} \n[1]\n{\"seq\":3}"
	type got struct {
		seq  int64 // -1 for a line that is not an object
		line string
		off  int64
	}
	var gs []got
	err := ledger.EachLine(strings.NewReader(in), func(line []byte, off int64) {
		seq := int64(-1)
		if e := ledger.ParseEntry(line); e != nil {
			seq = e.Seq()
		}
		gs = append(gs, got{seq, string(line), off})
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []got{{0, `{"seq":0}`, 0}, {1, `{"seq":1} `, 15}, {-1, "[1]", 26}, {3, `{"seq":3}`, 30}}
	if len(gs) != len(want) {
		t.Fatalf("got %d lines, want %d: %v", len(gs), len(want), gs)
	}
	for i := range want {
		if gs[i] != want[i] {
			t.Errorf("line %d: got %v, want %v", i, gs[i], want[i])
		}
		if in[want[i].off:want[i].off+int64(len(want[i].line))] != want[i].line {
			t.Errorf("line %d: offset %d does not point at the line", i, want[i].off)
		}
	}
}

func TestEachLineStopsOnALongLine(t *testing.T) {
	t.Parallel()
	in := "{\"seq\":0}\n" + strings.Repeat("x", ledger.MaxLineBytes+1) + "\n"
	n := 0
	err := ledger.EachLine(bytes.NewReader([]byte(in)), func([]byte, int64) { n++ })
	if err == nil || !strings.Contains(err.Error(), "line 2 is longer than") || n != 1 {
		t.Fatalf("got %d lines and error %v, want one line and a line 2 error", n, err)
	}
}

func TestVerifyReaderCallsOnEntryForEveryLine(t *testing.T) {
	t.Parallel()
	in, err := io.ReadAll(newChainReader(50, 10))
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	opt := ledger.VerifyOptions{OnEntry: func(e ledger.Entry, line []byte, off int64) {
		if e.Seq() != int64(seen) || string(in[off:off+int64(len(line))]) != string(line) {
			t.Errorf("entry %d: seq %d, offset %d does not point at its line", seen, e.Seq(), off)
		}
		seen++
	}}
	with, err := ledger.VerifyReader(bytes.NewReader(in), opt)
	if err != nil {
		t.Fatal(err)
	}
	without, err := ledger.VerifyReader(bytes.NewReader(in), ledger.VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 50 || fmt.Sprint(with) != fmt.Sprint(without) {
		t.Fatalf("OnEntry saw %d lines, want 50; reports differ: %v / %v", seen, with, without)
	}
}
