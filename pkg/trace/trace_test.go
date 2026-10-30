package trace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTraceFileMode0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "t.jsonl")
	w, err := NewFileWriter(p)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode = %v", fi.Mode().Perm())
	}
	// Pre-existing loose file gets tightened.
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err = NewFileWriter(p)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("existing file mode = %v", fi.Mode().Perm())
	}
}
