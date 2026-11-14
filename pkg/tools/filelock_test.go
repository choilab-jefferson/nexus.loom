//go:build unix

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
)

// Concurrent edit_file calls on one file must not lose each other's edits (read-modify-write under a per-file lock).
func TestConcurrentEditsToOneFileAreNotLost(t *testing.T) {
	ws := t.TempDir()
	const n = 40
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "item %03d todo\n", i)
	}
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := policy.DefaultPolicyConfig(ws)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			args := map[string]any{"path": "f.txt", "target": fmt.Sprintf("item %03d todo", i),
				"replacement": fmt.Sprintf("item %03d done", i)}
			if _, err := execEditFile(args, cfg); err != nil {
				t.Errorf("edit %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	got, _ := os.ReadFile(filepath.Join(ws, "f.txt"))
	if c := strings.Count(string(got), "done"); c != n {
		t.Fatalf("lost edits: %d of %d done\n%s", c, n, got)
	}
}

// A write waits while another process holds the file's lock, then completes.
func TestWriteWaitsForTheFileLock(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(ws, "r.md")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	cfg := policy.DefaultPolicyConfig(ws)
	if _, err := execReadFile(map[string]any{"path": "r.md"}, cfg); err != nil { // overwriting needs a current read
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := execWriteFile(map[string]any{"path": "r.md", "content": "new"}, cfg)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("write did not wait for the lock (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	f.Close() // releases the lock
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write did not finish after the lock was released")
	}
	if got, _ := os.ReadFile(p); string(got) != "new" {
		t.Fatalf("content %q", got)
	}
}

// edit_file still refuses a missing file (the lock must not create it).
func TestEditOfAMissingFileCreatesNothing(t *testing.T) {
	ws := t.TempDir()
	_, err := execEditFile(map[string]any{"path": "nope.txt", "target": "a", "replacement": "b"}, policy.DefaultPolicyConfig(ws))
	if err == nil {
		t.Fatal("edit of a missing file succeeded")
	}
	if _, serr := os.Stat(filepath.Join(ws, "nope.txt")); !os.IsNotExist(serr) {
		t.Fatal("edit created the file")
	}
}
