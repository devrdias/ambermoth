package ledger

import "os"

// SetSync replaces the fsync of the store, for the black-box tests in this directory.
func (s *Store) SetSync(fn func(*os.File) error) { s.sync = fn }
