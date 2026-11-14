//go:build !unix

package tools

import "os"

// openLocked: no advisory file locks on this platform (as for the run locks).
func openLocked(path string, create bool) (*os.File, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	return os.OpenFile(path, flags, 0o644)
}
