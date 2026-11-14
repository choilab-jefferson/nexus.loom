package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
)

// fakeBin puts an executable shell script named name first on PATH.
func fakeBin(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func roConfig(t *testing.T, enabled ...string) *policy.PolicyConfig {
	t.Helper()
	cfg := policy.DefaultPolicyConfig(t.TempDir())
	cfg.ReadonlyCommands = enabled
	return cfg
}

func TestRunReadonlyDisabledByDefault(t *testing.T) {
	cfg := policy.DefaultPolicyConfig(t.TempDir())
	res, err := Execute(context.Background(), "run_readonly", map[string]any{"argv": []string{"git", "log"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.ViolationKind != "allowlist" || !strings.Contains(res.Output, "not enabled") {
		t.Fatalf("expected recoverable allowlist error, got %+v", res)
	}
}

func TestRunReadonlyDeniedIsRecoverable(t *testing.T) {
	cfg := roConfig(t, "git", "glab")
	for _, argv := range [][]string{
		{"git", "push", "origin", "main"},
		{"git", "config", "user.name", "x"},
		{"glab", "mr", "merge", "1"},
		{"glab", "api", "-X", "POST", "projects/1"},
		{"sh", "-c", "id"},
	} {
		res, err := Execute(context.Background(), "run_readonly", map[string]any{"argv": argv}, cfg)
		if err != nil {
			t.Fatalf("%v: fatal error %v", argv, err)
		}
		if !res.IsError || res.ViolationKind != "allowlist" {
			t.Errorf("%v: want recoverable allowlist error, got %+v", argv, res)
		}
	}
}

func TestRunReadonlyRealGitLog(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cfg := roConfig(t, "git")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = cfg.Workspace
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", ".")
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "f.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "f.txt")
	run("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "first commit subject")

	res, err := Execute(context.Background(), "run_readonly", map[string]any{"argv": []string{"git", "log", "--format=%s"}}, cfg)
	if err != nil || res.IsError {
		t.Fatalf("git log failed: %v %+v", err, res)
	}
	if !strings.Contains(res.Output, "first commit subject") || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	// A write attempt is refused and leaves the repo untouched.
	res, _ = Execute(context.Background(), "run_readonly", map[string]any{"argv": []string{"git", "branch", "x"}}, cfg)
	if !res.IsError || res.ViolationKind != "allowlist" {
		t.Fatalf("git branch x must be refused: %+v", res)
	}
	res, _ = Execute(context.Background(), "run_readonly", map[string]any{"argv": []string{"git", "branch", "--list"}}, cfg)
	if res.IsError || strings.Contains(res.Output, " x") {
		t.Fatalf("branch list: %+v", res)
	}
}

func TestRunReadonlyCwdEnvAndPassthrough(t *testing.T) {
	fakeBin(t, "glab", `pwd; env`)
	cfg := roConfig(t, "glab")
	cfg.PassEnv = []string{"GITLAB_TOKEN", "GLAB_CONFIG_DIR", "AWS_SECRET_ACCESS_KEY"}
	t.Setenv("GITLAB_TOKEN", "tok-passed")
	t.Setenv("GITLAB_HOST", "not-listed.example")
	t.Setenv("ANTHROPIC_API_KEY", "sk-leak-canary")
	t.Setenv("GITHUB_TOKEN", "gh-leak-canary")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-leak-canary")
	t.Setenv("MY_PLAIN_VAR", "plain-leak-canary")

	res, err := Execute(context.Background(), "run_readonly", map[string]any{"argv": []string{"glab", "mr", "list"}}, cfg)
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	wsReal, _ := filepath.EvalSymlinks(cfg.Workspace)
	if first, _, _ := strings.Cut(res.Output, "\n"); first != wsReal {
		t.Errorf("cwd = %q, want %q", first, wsReal)
	}
	if !strings.Contains(res.Output, "GITLAB_TOKEN=tok-passed") {
		t.Error("GITLAB_TOKEN named in pass_env must pass through")
	}
	for _, canary := range []string{"leak-canary", "not-listed.example"} {
		if strings.Contains(res.Output, canary) {
			t.Errorf("env leaked %q:\n%s", canary, res.Output)
		}
	}
	for _, want := range []string{"PATH=", "HOME=", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("env missing %s", want)
		}
	}
}

func TestRunReadonlyTimeout(t *testing.T) {
	fakeBin(t, "git", `case " $* " in *" config "*) exit 0;; *" rev-parse "*) pwd; echo .git; exit 0;; esac; sleep 30`)
	cfg := roConfig(t, "git")
	res, err := Execute(context.Background(), "run_readonly", map[string]any{"argv": []string{"git", "status"}, "timeout_s": 1}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode == nil || *res.ExitCode != -1 {
		t.Fatalf("expected timeout, got %+v", res)
	}
}

func TestRunReadonlyOutputCap(t *testing.T) {
	fakeBin(t, "git", `case " $* " in *" config "*) exit 0;; *" rev-parse "*) pwd; echo .git; exit 0;; esac; head -c 3000000 /dev/zero | tr '\0' 'a'`)
	cfg := roConfig(t, "git")
	res, err := Execute(context.Background(), "run_readonly", map[string]any{"argv": []string{"git", "log"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.BytesRaw > maxReadonlyOutput+200 || res.BytesPassed > cfg.MaxOutputBytes {
		t.Fatalf("output not capped: raw=%d passed=%d truncated=%v", res.BytesRaw, res.BytesPassed, res.Truncated)
	}
}

func TestRunReadonlyGitEnvHardening(t *testing.T) {
	fakeBin(t, "git", `case " $* " in *" config "*) exit 0;; *" rev-parse "*) pwd; echo .git; exit 0;; esac; env`)
	cfg := roConfig(t, "git")
	t.Setenv("GIT_EXTERNAL_DIFF", "/bin/evil")
	t.Setenv("GIT_DIR", "/elsewhere")
	res, err := Execute(context.Background(), "run_readonly", map[string]any{"argv": []string{"git", "log"}}, cfg)
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	for _, want := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CEILING_DIRECTORIES=", "GIT_DISCOVERY_ACROSS_FILESYSTEM=0"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("env missing %s", want)
		}
	}
	for _, bad := range []string{"GIT_EXTERNAL_DIFF", "GIT_DIR=/elsewhere"} {
		if strings.Contains(res.Output, bad) {
			t.Errorf("parent env leaked %s", bad)
		}
	}
}

func TestRunReadonlyGitExfilRefusedEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cfg := roConfig(t, "git")
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET-CANARY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRepo(t, cfg.Workspace)
	for _, argv := range [][]string{
		{"git", "diff", secret, "/dev/null"},
		{"git", "diff", "/dev/null", secret},
		{"git", "diff", "--", secret, "/dev/null"},
		{"git", "diff", "--no-index", secret, "/dev/null"},
		{"git", "blame", secret},
	} {
		res, err := Execute(context.Background(), "run_readonly", map[string]any{"argv": argv}, cfg)
		if err != nil {
			t.Fatalf("%v: %v", argv, err)
		}
		if !res.IsError || res.ViolationKind != "allowlist" || strings.Contains(res.Output, "TOP-SECRET-CANARY") {
			t.Errorf("%v: not refused: %+v", argv, res)
		}
	}
}

// gitRepo makes dir a git repo with one commit.
func gitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", ".")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "f.txt")
	run("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "first commit subject")
}

func TestRunReadonlyRefusesDangerousRepoConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cfg := roConfig(t, "git")
	gitRepo(t, cfg.Workspace)
	call := func() ToolResult {
		res, err := Execute(context.Background(), "run_readonly", map[string]any{"argv": []string{"git", "status", "--porcelain"}}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := call(); res.IsError {
		t.Fatalf("clean repo refused: %+v", res)
	}
	for _, kv := range [][2]string{
		{"filter.evil.clean", "touch /tmp/pwned-canary"},
		{"diff.external", "/bin/true"},
		{"diff.evil.textconv", "cat"},
		{"diff.evil.command", "cat"},
		{"core.fsmonitor", "/bin/true"},
		{"core.hooksPath", "/tmp"},
		{"core.pager", "cat"},
		{"include.path", "/etc/hostname"},
		{"includeIf.gitdir:/x/.path", "/etc/hostname"},
	} {
		c := exec.Command("git", "config", "--local", kv[0], kv[1])
		c.Dir = cfg.Workspace
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("set %s: %v %s", kv[0], err, out)
		}
		res := call()
		if !res.IsError || res.ViolationKind != "allowlist" || !strings.Contains(res.Output, "can run external programs") {
			t.Errorf("%s: repo with dangerous config not refused: %+v", kv[0], res)
		}
		c = exec.Command("git", "config", "--local", "--unset-all", kv[0])
		c.Dir = cfg.Workspace
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("unset %s: %v %s", kv[0], err, out)
		}
	}
	if res := call(); res.IsError {
		t.Fatalf("repo refused after cleanup: %+v", res)
	}
	if _, err := os.Stat("/tmp/pwned-canary"); err == nil {
		t.Error("filter driver ran")
	}
}

func TestRunReadonlyNonRepoWorkspaceRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cfg := roConfig(t, "git")
	res, err := Execute(context.Background(), "run_readonly", map[string]any{"argv": []string{"git", "log"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.ViolationKind != "allowlist" || !strings.Contains(res.Output, "own git repository") {
		t.Fatalf("expected refusal outside a repo: %+v", res)
	}
}

// A workspace that is a plain subdirectory of a repository must not read the parent.
func TestRunReadonlyParentRepoRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	parent := t.TempDir()
	gitRepo(t, parent)
	if err := os.MkdirAll(filepath.Join(parent, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secrets", "prod.env"), []byte("PARENT-REPO-CANARY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("git", "add", "secrets/prod.env")
	c.Dir = parent
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	c = exec.Command("git", "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "secrets")
	c.Dir = parent
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	sub := filepath.Join(parent, "proj")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := policy.DefaultPolicyConfig(sub)
	cfg.ReadonlyCommands = []string{"git"}
	for _, argv := range [][]string{
		{"git", "show", "HEAD:secrets/prod.env"},
		{"git", "log", "-p", "--all"},
		{"git", "status"},
		{"git", "rev-parse", "--show-toplevel"},
	} {
		res, err := Execute(context.Background(), "run_readonly", map[string]any{"argv": argv}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || res.ViolationKind != "allowlist" || strings.Contains(res.Output, "CANARY") {
			t.Errorf("%v: parent repo reachable: %+v", argv, res)
		}
	}
}

// A linked worktree has its common dir outside the workspace: refused unless the
// task names that common dir explicitly.
func TestRunReadonlyLinkedWorktreeNeedsExplicitCommonDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	main := filepath.Join(base, "main")
	wt := filepath.Join(base, "wt")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRepo(t, main)
	c := exec.Command("git", "worktree", "add", "-q", "-b", "feat", wt)
	c.Dir = main
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v %s", err, out)
	}
	cfg := policy.DefaultPolicyConfig(wt)
	cfg.ReadonlyCommands = []string{"git"}
	call := func() ToolResult {
		res, err := Execute(context.Background(), "run_readonly", map[string]any{"argv": []string{"git", "log", "--format=%s"}}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := call(); !res.IsError || res.ViolationKind != "allowlist" || !strings.Contains(res.Output, "common dir") {
		t.Fatalf("linked worktree must be refused by default: %+v", res)
	}
	cfg.ReadonlyGitCommonDirs = []string{filepath.Join(base, "other", ".git")}
	if res := call(); !res.IsError {
		t.Fatalf("wrong common dir must still be refused: %+v", res)
	}
	cfg.ReadonlyGitCommonDirs = []string{filepath.Join(main, ".git")}
	if res := call(); res.IsError || !strings.Contains(res.Output, "first commit subject") {
		t.Fatalf("explicit common dir must be accepted: %+v", res)
	}
}

func TestRunReadonlyExcludesWritingTools(t *testing.T) {
	cfg := roConfig(t, "git")
	for name, args := range map[string]map[string]any{
		"write_file":  {"path": "x.txt", "content": "x"},
		"edit_file":   {"path": "x.txt", "target": "a", "replacement": "b"},
		"run_command": {"argv": []string{"echo", "hi"}},
		"job_start":   {"argv": []string{"echo", "hi"}},
	} {
		res, err := Execute(context.Background(), name, args, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || res.ViolationKind != "allowlist" || !strings.Contains(res.Output, "run_readonly") {
			t.Errorf("%s not refused in a read-only task: %+v", name, res)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.Workspace, "x.txt")); err == nil {
		t.Error("write_file wrote in a read-only task")
	}
}

func TestRunReadonlyRefusesBinaryInsideWorkspace(t *testing.T) {
	cfg := roConfig(t, "git")
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "git"), []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", cfg.Workspace+string(os.PathListSeparator)+os.Getenv("PATH"))
	res, err := Execute(context.Background(), "run_readonly", map[string]any{"argv": []string{"git", "status"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || strings.Contains(res.Output, "pwned") {
		t.Fatalf("workspace-planted git must not run: %+v", res)
	}
}

func TestRunReadonlySchemaListed(t *testing.T) {
	found := false
	for _, s := range BuiltinSchemas() {
		if s.Name == "run_readonly" {
			found = true
		}
	}
	if !found {
		t.Fatal("run_readonly missing from BuiltinSchemas")
	}
}
