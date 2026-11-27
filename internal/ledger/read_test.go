package ledger_test

import (
	"bytes"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
)

func TestReadNamesTheLineThatIsTooLong(t *testing.T) {
	t.Parallel()
	in := append([]byte("{\"a\":1}\n\n"), bytes.Repeat([]byte("a"), ledger.MaxLineBytes+1)...)
	es, ls, err := ledger.Read(bytes.NewReader(in))
	if err == nil || err.Error() != "line 3 is longer than 16 MiB" {
		t.Fatalf("error = %v, want line 3 is longer than 16 MiB", err)
	}
	if len(es) != 1 || len(ls) != 1 {
		t.Errorf("read %d entries and %d lines before the bad line, want 1 and 1", len(es), len(ls))
	}
}
