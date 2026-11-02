package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverAgentsMDWalksUpWorktree(t *testing.T) {
	root := t.TempDir()
	agentsContent := "# Nexus Agent Protocol\nFollow Karpathy discipline.\n"
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(agentsContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(root, ".worktrees", "worker-123")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}

	gotPath, gotContent := DiscoverAgentsMD(worktree)
	if gotPath == "" || gotContent != agentsContent {
		t.Fatalf("expected AGENTS.md discovered from parent root, got path=%q content=%q", gotPath, gotContent)
	}
}

func TestDiscoverAgentsMDConfinedToGitRoot(t *testing.T) {
	outer := t.TempDir()
	if err := os.WriteFile(filepath.Join(outer, "AGENTS.md"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(outer, "repo")
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if p, c := DiscoverAgentsMD(sub); p != "" || c != "" {
		t.Fatalf("AGENTS.md above git root must not load, got %q", p)
	}
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, c := DiscoverAgentsMD(sub); c != "inside" {
		t.Fatalf("want in-root AGENTS.md, got %q", c)
	}
}
