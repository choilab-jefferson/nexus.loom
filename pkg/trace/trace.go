package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Writer writes structured JSONL events per Loom v5.1 §5.4.
type Writer struct {
	mu   sync.Mutex
	out  io.Writer
	file *os.File
}

// NewFileWriter opens or creates a JSONL trace file at path.
// If path is empty, a no-op Writer is returned.
func NewFileWriter(path string) (*Writer, error) {
	if path == "" {
		return &Writer{}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	// OpenFile's mode only applies on create; tighten pre-existing files too.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	return &Writer{out: f, file: f}, nil
}

// NewWriter wraps an existing io.Writer.
func NewWriter(w io.Writer) *Writer {
	return &Writer{out: w}
}

// Emit serializes event as a single JSON line. Safe to call on a nil *Writer.
func (w *Writer) Emit(event map[string]any) error {
	if w == nil || w.out == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = w.out.Write(raw)
	return err
}

// Close closes the underlying file if opened via NewFileWriter.
func (w *Writer) Close() error {
	if w == nil || w.file == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.file.Sync()
	return w.file.Close()
}

// HashArgs returns a short SHA-256 hex digest of a tool argument map (§5.4).
func HashArgs(args map[string]any) string {
	raw, _ := json.Marshal(args)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:8]
}
