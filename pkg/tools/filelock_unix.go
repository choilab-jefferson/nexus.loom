//go:build unix

package tools

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// fileLockWait bounds how long a write waits for another loom process writing the same file.
const fileLockWait = 30 * time.Second

// openLocked opens path for a write and takes an exclusive advisory flock on the file itself (no lock file), waiting
// up to fileLockWait while another process writes it. Parallel runs may share a workspace; only writes to the same
// file are serialised. create=false refuses a missing file. O_NOFOLLOW refuses a symlink at the leaf.
func openLocked(path string, create bool) (*os.File, error) {
	flags := os.O_RDWR | syscall.O_NOFOLLOW
	if create {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(fileLockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, fmt.Errorf("%s is being written by another process; try again", path)
			}
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
