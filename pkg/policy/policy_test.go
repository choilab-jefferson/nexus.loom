package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntersectAllowCommands(t *testing.T) {
	global := [][]string{{"pytest"}, {"go", "test"}, {"ruff"}}
	agent := [][]string{{"pytest"}, {"go", "test", "./..."}}
	task := [][]string{{"pytest", "-q"}, {"go", "test", "./..."}}

	got := IntersectAllowCommands(global, agent, task)
	if len(got) != 2 {
		t.Fatalf("expected 2 intersected commands, got %v", got)
	}
	if got[0][0] != "pytest" || got[0][1] != "-q" {
		t.Errorf("unexpected first command: %v", got[0])
	}
	if got[1][0] != "go" || got[1][1] != "test" || got[1][2] != "./..." {
		t.Errorf("unexpected second command: %v", got[1])
	}

	empty := IntersectAllowCommands(global, agent, [][]string{{"cargo", "test"}})
	if len(empty) != 0 {
		t.Errorf("expected empty intersection, got %v", empty)
	}
}

func TestResolveScopedPath(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()

	cfg := DefaultPolicyConfig(ws)
	cfg.WriteAllow = []string{"src/**"}
	cfg.ReadAllow = []string{"src/**", "tests/**"}

	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Valid read and write inside src/
	if _, err := ResolveScopedPath(ws, "src/main.go", false, cfg); err != nil {
		t.Errorf("expected valid read, got %v", err)
	}
	if _, err := ResolveScopedPath(ws, "src/new.go", true, cfg); err != nil {
		t.Errorf("expected valid write, got %v", err)
	}

	// Traversal outside workspace must fail with ViolationError (ExitCode 2)
	if _, err := ResolveScopedPath(ws, "../escape.txt", false, cfg); !IsViolation(err) {
		t.Errorf("expected ViolationError on ../escape.txt, got %v", err)
	}

	// DenyGlobs (.git and .env) must fail
	if _, err := ResolveScopedPath(ws, ".git/config", false, cfg); !IsViolation(err) {
		t.Errorf("expected ViolationError on .git/config, got %v", err)
	}
	if _, err := ResolveScopedPath(ws, ".env.local", false, cfg); !IsViolation(err) {
		t.Errorf("expected ViolationError on .env.local, got %v", err)
	}

	// Write outside WriteAllow (e.g. tests/foo_test.go) must fail
	if _, err := ResolveScopedPath(ws, "tests/foo_test.go", true, cfg); !IsViolation(err) {
		t.Errorf("expected ViolationError writing to tests/, got %v", err)
	}

	// Symlink escape test
	outsideSecret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(ws, "src", "leak.txt")
	if err := os.Symlink(outsideSecret, symlinkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveScopedPath(ws, "src/leak.txt", false, cfg); !IsViolation(err) {
		t.Errorf("expected ViolationError on symlink escaping workspace, got %v", err)
	}
}

// fakeBins puts empty executables on PATH so tests do not depend on what the
// host/CI image has installed (alpine has no pytest or python).
func fakeBins(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestValidateCommandArgv(t *testing.T) {
	fakeBins(t, "pytest")
	cfg := DefaultPolicyConfig(t.TempDir())
	cfg.AllowCommands = [][]string{{"pytest"}, {"go", "test"}}

	if err := ValidateCommandArgv([]string{"pytest", "-q", "tests/"}, cfg); err != nil {
		t.Errorf("expected pytest to be allowed, got %v", err)
	}
	// pytest-wrapper must NOT match pytest (§9 argv token matching)
	if err := ValidateCommandArgv([]string{"pytest-wrapper", "-q"}, cfg); !IsViolation(err) {
		t.Errorf("expected pytest-wrapper to be blocked, got %v", err)
	}
	// Shell wrappers must be blocked even if in AllowCommands
	cfgWithMake := DefaultPolicyConfig("/tmp")
	cfgWithMake.AllowCommands = [][]string{{"make"}, {"bash"}}
	if err := ValidateCommandArgv([]string{"make", "test"}, cfgWithMake); !IsViolation(err) {
		t.Errorf("expected make to be blocked, got %v", err)
	}
	if err := ValidateCommandArgv([]string{"bash", "-c", "echo hi"}, cfgWithMake); !IsViolation(err) {
		t.Errorf("expected bash -c to be blocked, got %v", err)
	}
}

func TestWriteSymlinkEscapes(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	cfg := DefaultPolicyConfig(ws)

	// dangling leaf pointing outside
	if err := os.Symlink(filepath.Join(outside, "new.txt"), filepath.Join(ws, "dangle")); err != nil {
		t.Fatal(err)
	}
	for _, write := range []bool{true, false} {
		if _, err := ResolveScopedPath(ws, "dangle", write, cfg); err == nil {
			t.Fatalf("dangling symlink accepted (write=%v)", write)
		}
	}
	// dangling intermediate
	if err := os.Symlink(filepath.Join(outside, "nodir"), filepath.Join(ws, "dlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveScopedPath(ws, "dlink/sub/f.txt", true, cfg); err == nil {
		t.Fatal("dangling intermediate symlink accepted")
	}
	// symlink to existing inside file: write refused, read allowed
	if err := os.WriteFile(filepath.Join(ws, "real.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(ws, "inlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveScopedPath(ws, "inlink", true, cfg); err == nil {
		t.Fatal("write through in-workspace symlink accepted")
	}
	if _, err := ResolveScopedPath(ws, "inlink", false, cfg); err != nil {
		t.Fatalf("read through in-workspace symlink refused: %v", err)
	}
	// symlinked parent resolving outside
	if err := os.Symlink(outside, filepath.Join(ws, "outdir")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveScopedPath(ws, "outdir/f.txt", true, cfg); err == nil {
		t.Fatal("symlinked parent escape accepted")
	}
}

func TestCommandRejectionMessagesAreActionable(t *testing.T) {
	fakeBins(t, "python", "python3")
	err := ValidateCommandArgv([]string{"python3", "-m", "pytest"}, &PolicyConfig{})
	if err == nil || !strings.Contains(err.Error(), "-allow-cmd") {
		t.Fatalf("empty allowlist message should name -allow-cmd, got %v", err)
	}
	cfg := &PolicyConfig{AllowCommands: [][]string{{"python3", "-m", "pytest"}, {"go", "test"}}}
	err = ValidateCommandArgv([]string{"python", "-m", "pytest"}, cfg)
	if err == nil || !strings.Contains(err.Error(), `"python3 -m pytest"`) || !strings.Contains(err.Error(), `"go test"`) {
		t.Fatalf("mismatch message should list allowed prefixes, got %v", err)
	}
}

func TestModelCausedPathErrorsAreRecoverable(t *testing.T) {
	ws := t.TempDir()
	cfg := DefaultPolicyConfig(ws)
	if err := os.WriteFile(filepath.Join(ws, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Empty path and path-through-a-file (ENOTDIR) are model mistakes, not violations.
	for _, target := range []string{"", "file.txt/child"} {
		_, err := ResolveScopedPath(ws, target, false, cfg)
		if err == nil || IsViolation(err) {
			t.Errorf("target %q: expected plain recoverable error, got %v", target, err)
		}
	}
	// A bad workspace root stays a violation.
	if _, err := ResolveScopedPath("", "a", false, cfg); !IsViolation(err) {
		t.Errorf("empty workspace must be a violation, got %v", err)
	}
}

func TestEffectiveAllowCommandsLayers(t *testing.T) {
	task := [][]string{{"go", "test"}, {"ruff"}}
	t.Setenv("LOOM_GLOBAL_ALLOW_COMMANDS", "")
	t.Setenv("LOOM_AGENT_ALLOW_COMMANDS", "")
	if got := EffectiveAllowCommands(task); len(got) != 2 {
		t.Fatalf("unset layers must not restrict, got %v", got)
	}
	t.Setenv("LOOM_GLOBAL_ALLOW_COMMANDS", "go test; pytest")
	got := EffectiveAllowCommands(task)
	if len(got) != 1 || got[0][0] != "go" {
		t.Fatalf("global layer must narrow task list, got %v", got)
	}
	t.Setenv("LOOM_AGENT_ALLOW_COMMANDS", "cargo")
	if got := EffectiveAllowCommands(task); len(got) != 0 {
		t.Fatalf("agent layer must narrow to empty, got %v", got)
	}
}

func TestWorkspaceBinaryDoesNotSatisfyBareAllowEntry(t *testing.T) {
	ws := t.TempDir()
	bin := filepath.Join(ws, "echo")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultPolicyConfig(ws)
	cfg.AllowCommands = [][]string{{"echo"}}
	if err := ValidateCommandArgv([]string{"./echo", "x"}, cfg); err == nil {
		t.Fatal("./echo in workspace must not satisfy allow entry echo")
	}
	if err := ValidateCommandArgv([]string{"echo", "x"}, cfg); err != nil {
		t.Fatalf("PATH echo should pass: %v", err)
	}
}

func TestInterpreterInlineFlagsBlocked(t *testing.T) {
	cfg := DefaultPolicyConfig(t.TempDir())
	cfg.AllowCommands = [][]string{{"python3"}, {"node"}, {"perl"}}
	for _, a := range [][]string{{"python3", "-c", "1"}, {"node", "-e", "1"}, {"perl", "-e", "1"}} {
		if err := ValidateCommandArgv(a, cfg); err == nil {
			t.Errorf("%v must be rejected", a)
		}
	}
}

func TestViolationKinds(t *testing.T) {
	cfg := DefaultPolicyConfig(t.TempDir())
	cfg.AllowCommands = [][]string{{"make"}}
	cases := []struct {
		argv []string
		want ViolationKind
	}{
		{[]string{"rm", "-rf", "/"}, KindAllowlist},
		{[]string{"bash", "-c", "echo hi"}, KindEvalFlag},
		{[]string{"python3", "-c", "1"}, KindEvalFlag},
	}
	for _, c := range cases {
		k, ok := ViolationKindOf(ValidateCommandArgv(c.argv, cfg))
		if !ok || k != c.want || !k.Recoverable() {
			t.Errorf("%v: got kind %v ok=%v, want %v", c.argv, k, ok, c.want)
		}
	}
	_, err := ResolveScopedPath(t.TempDir(), "../escape", false, cfg)
	if k, ok := ViolationKindOf(err); !ok || k.Recoverable() {
		t.Errorf("path escape must stay fatal, got %v %v", k, ok)
	}
}
