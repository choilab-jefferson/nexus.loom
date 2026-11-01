//go:build !unix

package main

import "errors"

var errLocked = errors.New("locked")

// flockPath is a no-op on platforms without flock: same-path protection is
// unix-only (documented in docs/contract.md).
func flockPath(string, bool) (func(), error) { return func() {}, nil }
