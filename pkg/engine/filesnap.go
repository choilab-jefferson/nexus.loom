package engine

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// MaxFilesChanged caps the total entries across added+modified+deleted.
	MaxFilesChanged = 1000
	// MaxSnapshotEntries bounds the snapshot walk (and the baseline stored in checkpoints).
	MaxSnapshotEntries = 20000
)

// FileStat is the per-file fingerprint used to detect changes.
type FileStat struct {
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	Mode    uint32 `json:"mode"`
}

// Snapshot is a bounded workspace listing keyed by slash-separated relative path.
type Snapshot struct {
	Files     map[string]FileStat `json:"files"`
	Truncated bool                `json:"truncated,omitempty"`
}

// FilesChanged is the workspace change manifest in TaskResult.
type FilesChanged struct {
	Added     []string `json:"added"`
	Modified  []string `json:"modified"`
	Deleted   []string `json:"deleted"`
	Truncated bool     `json:"truncated"`
}

func emptyFilesChanged() FilesChanged {
	return FilesChanged{Added: []string{}, Modified: []string{}, Deleted: []string{}}
}

// loomOwnedExcluder returns a predicate matching loom's own bookkeeping files for a trace path:
// the trace, <trace>.ckpt, <trace>.lock and checkpoint temp files (.ckpt-*) in the trace dir.
// Nil when tracePath is empty.
func loomOwnedExcluder(tracePath string) func(string) bool {
	if tracePath == "" {
		return nil
	}
	abs, err := filepath.Abs(tracePath)
	if err != nil {
		return nil
	}
	dir := resolveDir(filepath.Dir(abs))
	base := filepath.Base(abs)
	return func(p string) bool {
		if resolveDir(filepath.Dir(p)) != dir {
			return false
		}
		n := filepath.Base(p)
		return n == base || n == base+".ckpt" || n == base+".lock" || strings.HasPrefix(n, ".ckpt-")
	}
}

func resolveDir(d string) string {
	if r, err := filepath.EvalSymlinks(d); err == nil {
		return r
	}
	return d
}

// TakeSnapshot walks root recording regular files and symlinks (links are recorded, never
// followed). .git is skipped, as is anything exclude (optional) matches (absolute path).
func TakeSnapshot(root string, exclude ...func(string) bool) *Snapshot {
	s := &Snapshot{Files: map[string]FileStat{}}
	if root == "" {
		return s
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		for _, ex := range exclude {
			if ex != nil && ex(p) {
				return nil
			}
		}
		if len(s.Files) >= MaxSnapshotEntries {
			s.Truncated = true
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		s.Files[filepath.ToSlash(rel)] = FileStat{Size: info.Size(), MtimeNs: info.ModTime().UnixNano(), Mode: uint32(info.Mode())}
		return nil
	})
	return s
}

// DiffSnapshots computes the manifest between a baseline and the current snapshot.
func DiffSnapshots(base, cur *Snapshot) FilesChanged {
	fc := emptyFilesChanged()
	if base == nil || cur == nil {
		return fc
	}
	for p, st := range cur.Files {
		if b, ok := base.Files[p]; !ok {
			fc.Added = append(fc.Added, p)
		} else if b != st {
			fc.Modified = append(fc.Modified, p)
		}
	}
	for p := range base.Files {
		if _, ok := cur.Files[p]; !ok {
			fc.Deleted = append(fc.Deleted, p)
		}
	}
	sort.Strings(fc.Added)
	sort.Strings(fc.Modified)
	sort.Strings(fc.Deleted)
	fc.Truncated = base.Truncated || cur.Truncated
	budget := MaxFilesChanged
	for _, l := range []*[]string{&fc.Added, &fc.Modified, &fc.Deleted} {
		if len(*l) > budget {
			*l = (*l)[:budget]
			fc.Truncated = true
		}
		budget -= len(*l)
	}
	return fc
}
