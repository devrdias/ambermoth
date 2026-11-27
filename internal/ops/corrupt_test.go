package ops_test

import (
	"context"
	"testing"

	"github.com/devrdias/ambermoth/internal/ops"
)

// A ledger that cannot be read is an error for every operation that writes,
// and the operation adds nothing to it.
func TestOperationsRefuseAnUnreadableLedger(t *testing.T) {
	path := writeCSV(t, csvHead+"2026-03-02,m,1,1\n")
	runs := map[string]func(t *testing.T) error{
		"anchor": func(t *testing.T) error {
			s := callStore(t)
			corrupt(t, s)
			_, err := ops.Anchor(context.Background(), s, []string{downURL}, nil)
			return err
		},
		"amend": func(t *testing.T) error {
			s := amendStore(t)
			corrupt(t, s)
			_, err := ops.Amend(s, 1, 1, "eval-regression", "", "why", "me")
			return err
		},
		"reconcile": func(t *testing.T) error {
			s := callStore(t)
			corrupt(t, s)
			_, _, err := ops.Reconcile(s, "openai", path, 0.02)
			return err
		},
	}
	for name, run := range runs {
		t.Run(name, func(t *testing.T) {
			if err := run(t); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}
