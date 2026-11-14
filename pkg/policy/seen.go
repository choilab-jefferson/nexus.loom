package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// SeenFiles records, per resolved path, the sha256 of the content this run last read or wrote. write_file uses it
// to refuse overwriting a file the run never read, or one that changed since (another process or member wrote it).
type SeenFiles struct {
	mu sync.Mutex
	m  map[string]string
}

func NewSeenFiles() *SeenFiles { return &SeenFiles{m: map[string]string{}} }

// ContentHash is the hash SeenFiles stores.
func ContentHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *SeenFiles) Record(path string, content []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.m[path] = ContentHash(content)
	s.mu.Unlock()
}

// Lookup returns the recorded hash and whether the path was recorded.
func (s *SeenFiles) Lookup(path string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.m[path]
	return h, ok
}
