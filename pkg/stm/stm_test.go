package stm

import (
	"os"
	"path/filepath"
	"strings"
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

func TestResolveProjectIDDirectAndWorktreeGitFile(t *testing.T) {
	root := t.TempDir()
	pid := "p601d8f44903f4fe1f167"

	// 1. Direct .git/nexus/project-id
	gitNexus := filepath.Join(root, ".git", "nexus")
	if err := os.MkdirAll(gitNexus, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitNexus, "project-id"), []byte(pid+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := ResolveProjectID(root); got != pid {
		t.Fatalf("expected %s from .git/nexus/project-id, got %q", pid, got)
	}

	// 2. Worktree with .git file pointing to <root>/.git/worktrees/w1
	wtGitDir := filepath.Join(root, ".git", "worktrees", "w1")
	if err := os.MkdirAll(wtGitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wtDir := filepath.Join(t.TempDir(), "wt-checkout")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte("gitdir: "+wtGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := ResolveProjectID(wtDir); got != pid {
		t.Fatalf("expected %s via worktree gitdir resolution, got %q", pid, got)
	}
}

func TestLoadBriefStandardAndPHIClamp(t *testing.T) {
	ws := t.TempDir()
	pid := "p601d8f44903f4fe1f167"
	gitNexus := filepath.Join(ws, ".git", "nexus")
	if err := os.MkdirAll(gitNexus, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitNexus, "project-id"), []byte(pid), 0o644); err != nil {
		t.Fatal(err)
	}

	stmRoot := t.TempDir()
	t.Setenv("NEXUS_STM_DIR", stmRoot)
	nodeDir := filepath.Join(stmRoot, "stm-"+pid, "node")
	if err := os.MkdirAll(nodeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	rootMD := `---
id: p601d8f44903f4fe1f167
kind: project
status: active
sensitivity: standard
seq: 10
summary: 'loom: 1 active (1 task), last=n123'
updated: '2026-10-29T02:30:00Z'
---
`
	if err := os.WriteFile(filepath.Join(nodeDir, pid+".md"), []byte(rootMD), 0o644); err != nil {
		t.Fatal(err)
	}

	nodeActive := `---
id: n11111111111111111111
kind: task
status: done
seq: 9
agent: mini-worker
summary: 'Implemented Go v0.1 engine
  and provider tests'
updated: '2026-10-29T02:29:00Z'
invalid_at: null
---
`
	if err := os.WriteFile(filepath.Join(nodeDir, "n11111111111111111111.md"), []byte(nodeActive), 0o644); err != nil {
		t.Fatal(err)
	}

	nodeInvalidated := `---
id: n22222222222222222222
kind: task
status: superseded
seq: 11
summary: 'Old superseded task'
invalid_at: '2026-10-29T02:25:00Z'
---
`
	if err := os.WriteFile(filepath.Join(nodeDir, "n22222222222222222222.md"), []byte(nodeInvalidated), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Standard mode preserves node summary and excludes invalidated node
	brief, err := LoadBrief(ws, false)
	if err != nil {
		t.Fatalf("LoadBrief failed: %v", err)
	}
	if brief.ProjectID != pid || len(brief.RecentNodes) != 1 {
		t.Fatalf("expected 1 active node, got %+v", brief.RecentNodes)
	}
	if !strings.Contains(brief.RecentNodes[0].Summary, "Implemented Go v0.1 engine and provider tests") {
		t.Errorf("unexpected multiline summary: %q", brief.RecentNodes[0].Summary)
	}
	fenced := brief.FormatFenced()
	if !strings.Contains(fenced, `<nexus_stm project="p601d8f44903f4fe1f167" sensitivity="standard">`) ||
		!strings.Contains(fenced, "- [9] n11111111111111111111 (task/done) Implemented Go v0.1 engine and provider tests") {
		t.Errorf("unexpected FormatFenced output:\n%s", fenced)
	}

	// 2. PHI clamp mode redacts node Summary
	phiBrief, err := LoadBrief(ws, true)
	if err != nil {
		t.Fatalf("LoadBrief(phiClamp=true) failed: %v", err)
	}
	if phiBrief.RecentNodes[0].Summary != "" || phiBrief.RecentNodes[0].Agent != "" {
		t.Errorf("expected Summary and Agent to be redacted under phiClamp, got %+v", phiBrief.RecentNodes[0])
	}
	phiFenced := phiBrief.FormatFenced()
	if strings.Contains(phiFenced, "Implemented Go v0.1 engine") {
		t.Errorf("expected summary redacted from fenced output in phiClamp mode, got:\n%s", phiFenced)
	}

	// 3. Optional STM: workspace without project-id or STM directory returns Available=false and no error
	plainWS := t.TempDir()
	optBrief, err := LoadBrief(plainWS, false)
	if err != nil {
		t.Fatalf("expected nil error when STM is absent, got: %v", err)
	}
	if optBrief.Available || optBrief.FormatFenced() != "" {
		t.Fatalf("expected Available=false and empty FormatFenced() when STM is absent, got %+v", optBrief)
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
