//go:build unix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// errLocked reports that another live loom process holds the lock.
var errLocked = errors.New("locked")

// flockPath takes a non-blocking exclusive advisory flock on path (file or
// directory). The kernel releases it when the process dies, so locks never go
// stale. The returned func releases it. No file is created for directories.
func flockPath(path string, createFile bool) (func(), error) {
	flags := os.O_RDONLY
	if createFile {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		flags = os.O_CREATE | os.O_WRONLY
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}
