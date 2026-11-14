package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
)

// Edit and write semantics after earendil-works/pi (edit.ts / edit-diff.ts): several exact, unique, non-overlapping
// blocks per call, all or nothing, a whitespace/typography-tolerant fallback, BOM and line endings kept, a diff back.
// Plus (PI 2026-11-13) a stale-write guard pi does not have: an existing file is overwritten only when this run read
// it and nobody changed it since.

func wsWith(t *testing.T, name, content string) (string, *policy.PolicyConfig) {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws, policy.DefaultPolicyConfig(ws)
}

func readBack(t *testing.T, ws, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ws, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func edits(pairs ...string) []any {
	var out []any
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{"target": pairs[i], "replacement": pairs[i+1]})
	}
	return out
}

func TestEditSeveralBlocksAtOnce(t *testing.T) {
	ws, cfg := wsWith(t, "a.py", "x = 1\ny = 2\nz = 3\n")
	out, err := execEditFile(map[string]any{"path": "a.py", "edits": edits("x = 1", "x = 10", "z = 3", "z = 30")}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, ws, "a.py"); got != "x = 10\ny = 2\nz = 30\n" {
		t.Fatalf("content %q", got)
	}
	if !strings.Contains(out, "replaced 2 block(s)") || !strings.Contains(out, "-x = 1") || !strings.Contains(out, "+z = 30") {
		t.Fatalf("result should report the blocks and a diff:\n%s", out)
	}
}

func TestEditLegacyTargetReplacementStillWorks(t *testing.T) {
	ws, cfg := wsWith(t, "a.txt", "hello world\n")
	if _, err := execEditFile(map[string]any{"path": "a.txt", "target": "world", "replacement": "loom"}, cfg); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, ws, "a.txt"); got != "hello loom\n" {
		t.Fatalf("content %q", got)
	}
}

func TestEditRefusesAnAmbiguousTarget(t *testing.T) {
	ws, cfg := wsWith(t, "a.txt", "n = 0\nn = 0\n")
	_, err := execEditFile(map[string]any{"path": "a.txt", "target": "n = 0", "replacement": "n = 1"}, cfg)
	if err == nil || !strings.Contains(err.Error(), "2 occurrences") || !strings.Contains(err.Error(), "more context") {
		t.Fatalf("want an ambiguity error, got %v", err)
	}
	if got := readBack(t, ws, "a.txt"); got != "n = 0\nn = 0\n" {
		t.Fatalf("file changed: %q", got)
	}
}

func TestEditIsAllOrNothing(t *testing.T) {
	ws, cfg := wsWith(t, "a.txt", "one\ntwo\n")
	_, err := execEditFile(map[string]any{"path": "a.txt", "edits": edits("one", "1", "three", "3")}, cfg)
	if err == nil || !strings.Contains(err.Error(), "edits[1]") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want edits[1] not found, got %v", err)
	}
	if got := readBack(t, ws, "a.txt"); got != "one\ntwo\n" {
		t.Fatalf("a failed call must change nothing: %q", got)
	}
}

func TestEditRefusesOverlappingBlocks(t *testing.T) {
	_, cfg := wsWith(t, "a.txt", "alpha beta gamma\n")
	_, err := execEditFile(map[string]any{"path": "a.txt", "edits": edits("alpha beta", "A", "beta gamma", "B")}, cfg)
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("want overlap error, got %v", err)
	}
}

func TestEditRefusesANoOpAndAnEmptyTarget(t *testing.T) {
	_, cfg := wsWith(t, "a.txt", "same\n")
	if _, err := execEditFile(map[string]any{"path": "a.txt", "target": "same", "replacement": "same"}, cfg); err == nil ||
		!strings.Contains(err.Error(), "no change") {
		t.Fatalf("want no-change error, got %v", err)
	}
	if _, err := execEditFile(map[string]any{"path": "a.txt", "target": "", "replacement": "x"}, cfg); err == nil ||
		!strings.Contains(err.Error(), "empty") {
		t.Fatalf("want empty-target error, got %v", err)
	}
}

func TestEditFallsBackToTolerantMatchingAndKeepsOtherBytes(t *testing.T) {
	// the file has trailing spaces and typographic quotes/dashes; the model wrote plain ASCII without them
	ws, cfg := wsWith(t, "a.md", "keep  \nsay “hi” — now   \nend\n")
	if _, err := execEditFile(map[string]any{"path": "a.md", "target": "say \"hi\" - now", "replacement": "say bye"}, cfg); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, ws, "a.md"); got != "keep  \nsay bye\nend\n" {
		t.Fatalf("content %q", got)
	}
}

func TestEditKeepsBomAndCrlf(t *testing.T) {
	ws, cfg := wsWith(t, "w.txt", "\ufeffa\r\nb\r\nc\r\n")
	if _, err := execEditFile(map[string]any{"path": "w.txt", "target": "b\nc", "replacement": "B\nC"}, cfg); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, ws, "w.txt"); got != "\ufeffa\r\nB\r\nC\r\n" {
		t.Fatalf("content %q", got)
	}
}

func TestWriteOverAnUnreadFileIsRefused(t *testing.T) {
	ws, cfg := wsWith(t, "r.md", "theirs\n")
	_, err := execWriteFile(map[string]any{"path": "r.md", "content": "mine\n"}, cfg)
	if err == nil || !strings.Contains(err.Error(), "read it first") {
		t.Fatalf("want read-first error, got %v", err)
	}
	if got := readBack(t, ws, "r.md"); got != "theirs\n" {
		t.Fatalf("file changed: %q", got)
	}
}

func TestWriteAfterAReadSucceedsUntilSomeoneElseChangesTheFile(t *testing.T) {
	ws, cfg := wsWith(t, "r.md", "v1\n")
	if _, err := execReadFile(map[string]any{"path": "r.md"}, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := execWriteFile(map[string]any{"path": "r.md", "content": "v2\n"}, cfg); err != nil {
		t.Fatalf("write after read: %v", err)
	}
	if _, err := execWriteFile(map[string]any{"path": "r.md", "content": "v3\n"}, cfg); err != nil {
		t.Fatalf("a run's own write keeps its view current: %v", err)
	}
	// another member changes the file
	if err := os.WriteFile(filepath.Join(ws, "r.md"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := execWriteFile(map[string]any{"path": "r.md", "content": "v4\n"}, cfg)
	if err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("want changed-since error, got %v", err)
	}
	if got := readBack(t, ws, "r.md"); got != "other\n" {
		t.Fatalf("the other member's change was lost: %q", got)
	}
}

func TestWriteOfANewFileNeedsNoRead(t *testing.T) {
	ws, cfg := wsWith(t, "x", "")
	if _, err := execWriteFile(map[string]any{"path": "sub/new.md", "content": "n\n"}, cfg); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, ws, "sub/new.md"); got != "n\n" {
		t.Fatalf("content %q", got)
	}
}
