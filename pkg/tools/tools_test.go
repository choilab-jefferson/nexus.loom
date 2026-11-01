package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
)

func TestBuiltinToolsAndPolicyEnforcement(t *testing.T) {
	ws := t.TempDir()
	cfg := policy.DefaultPolicyConfig(ws)
	cfg.AllowCommands = [][]string{{"echo", "hello"}}

	ctx := context.Background()

	// 1. write_file
	_, err := Execute(ctx, "write_file", map[string]any{
		"path":    "src/hello.go",
		"content": "package src\n\nfunc Greet() string { return \"old\" }\n",
	}, cfg)
	if err != nil {
		t.Fatalf("write_file failed: %v", err)
	}

	// 2. edit_file
	_, err = Execute(ctx, "edit_file", map[string]any{
		"path":        "src/hello.go",
		"target":      `"old"`,
		"replacement": `"new"`,
	}, cfg)
	if err != nil {
		t.Fatalf("edit_file failed: %v", err)
	}

	// 3. read_file
	readRes, err := Execute(ctx, "read_file", map[string]any{
		"path":       "src/hello.go",
		"start_line": 1,
		"end_line":   10,
	}, cfg)
	if err != nil {
		t.Fatalf("read_file failed: %v", err)
	}
	if !strings.Contains(readRes.Output, `"new"`) {
		t.Errorf("expected edited content in read_file output, got %q", readRes.Output)
	}

	// 4. search (skips .git and binaries)
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git", "HEAD"), []byte("Greet"), 0o644); err != nil {
		t.Fatal(err)
	}
	searchRes, err := Execute(ctx, "search", map[string]any{
		"pattern": "Greet",
	}, cfg)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if !strings.Contains(searchRes.Output, "src/hello.go:3:") || strings.Contains(searchRes.Output, ".git") {
		t.Errorf("unexpected search output: %q", searchRes.Output)
	}

	// 5. run_command allowed vs denied
	cmdRes, err := Execute(ctx, "run_command", map[string]any{
		"argv": []string{"echo", "hello", "world"},
	}, cfg)
	if err != nil {
		t.Fatalf("expected allowed run_command to succeed, got %v", err)
	}
	if !strings.Contains(cmdRes.Output, "hello world") {
		t.Errorf("unexpected run_command output: %q", cmdRes.Output)
	}

	disallowed, err := Execute(ctx, "run_command", map[string]any{
		"argv": []string{"rm", "-rf", "/"},
	}, cfg)
	if err != nil {
		t.Errorf("disallowed run_command must be a recoverable tool error, got %v", err)
	}
	if !disallowed.IsError || disallowed.ViolationKind != "allowlist" {
		t.Errorf("expected IsError result with kind allowlist, got %+v", disallowed)
	}

	// 6. escalate
	escRes, err := Execute(ctx, "escalate", map[string]any{
		"reason":          "missing_schema",
		"minimal_context": "src/hello.go",
		"attempted":       []string{"read_file", "search"},
	}, cfg)
	if err != nil {
		t.Fatalf("escalate returned unexpected error: %v", err)
	}
	if escRes.Escalation == nil || escRes.Escalation.Reason != "missing_schema" {
		t.Fatalf("expected EscalationSignal with reason=missing_schema, got %+v", escRes.Escalation)
	}
}

func TestCanonicalToolSchemasJSONDeterministic(t *testing.T) {
	a := CanonicalToolSchemasJSON()
	b := CanonicalToolSchemasJSON()
	if a != b || a == "[]" {
		t.Fatalf("expected non-empty deterministic tool schema JSON, got %s", a)
	}
}

func TestScrubSubprocessEnvAndIKBTokenInjection(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("NEXUS_CONFIG_HOME", tmp)
	t.Setenv("IKB_MCP_TOKEN", "")
	t.Setenv("IKB_API_TOKEN", "")
	if err := os.WriteFile(filepath.Join(tmp, "tokens.env"), []byte("IKB_MCP_TOKEN=ikb-secret-777\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rawEnv := []string{
		"PATH=/usr/bin:/bin",
		"NEXUS_ANTHROPIC_API_KEY=sk-ant-secret",
		"NEXUS_GEMINI_API_KEY=AIzaSecret",
		"NEXUS_GOOGLE_AGENT_API_KEY=AIzaGAPSecret",
		"TYPESAFE_API_KEY=ts-secret",
	}

	scrubbedOther := strings.Join(scrubSubprocessEnv(rawEnv, "/usr/bin/pytest"), "\n")
	if strings.Contains(scrubbedOther, "sk-ant-secret") ||
		strings.Contains(scrubbedOther, "AIzaSecret") ||
		strings.Contains(scrubbedOther, "ts-secret") ||
		strings.Contains(scrubbedOther, "ikb-secret-777") {
		t.Fatalf("expected API keys to be scrubbed from non-ikb subprocess env, got:\n%s", scrubbedOther)
	}

	scrubbedIKB := strings.Join(scrubSubprocessEnv(rawEnv, "/usr/local/bin/ikb"), "\n")
	if !strings.Contains(scrubbedIKB, "IKB_MCP_TOKEN=ikb-secret-777") || !strings.Contains(scrubbedIKB, "IKB_API_TOKEN=ikb-secret-777") {
		t.Fatalf("expected IKB_MCP_TOKEN and IKB_API_TOKEN injected for ikb binary, got:\n%s", scrubbedIKB)
	}
}

func TestWriteFileSymlinkEscapes(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	cfg := policy.DefaultPolicyConfig(ws)
	ctx := context.Background()
	target := filepath.Join(outside, "pwned.txt")

	if err := os.Symlink(target, filepath.Join(ws, "dangle")); err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(ctx, "write_file", map[string]any{"path": "dangle", "content": "x"}, cfg); err == nil {
		t.Fatal("write through dangling symlink succeeded")
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("outside file was created")
	}

	if err := os.Symlink(outside, filepath.Join(ws, "outdir")); err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(ctx, "write_file", map[string]any{"path": "outdir/pwned2.txt", "content": "x"}, cfg); err == nil {
		t.Fatal("write through symlinked parent succeeded")
	}
	if _, err := os.Lstat(filepath.Join(outside, "pwned2.txt")); err == nil {
		t.Fatal("outside file via parent was created")
	}

	if err := os.WriteFile(filepath.Join(ws, "real.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(ws, "inlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(ctx, "write_file", map[string]any{"path": "inlink", "content": "b"}, cfg); err == nil {
		t.Fatal("write through in-workspace symlink succeeded")
	}
	if _, err := Execute(ctx, "read_file", map[string]any{"path": "inlink"}, cfg); err != nil {
		t.Fatalf("read via in-workspace symlink failed: %v", err)
	}
}

// TOCTOU: leaf swapped to a symlink after policy resolution must not be followed.
func TestWriteNoFollowLeafSwap(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "pwned.txt")
	leaf := filepath.Join(ws, "f.txt")
	if err := os.Symlink(target, leaf); err != nil {
		t.Fatal(err)
	}
	if err := writeNoFollow(leaf, []byte("x")); err == nil {
		t.Fatal("writeNoFollow followed symlink")
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("outside file created")
	}
}

func TestScrubEnvIsAllowlist(t *testing.T) {
	raw := []string{"PATH=/bin", "HOME=/h", "LC_ALL=C", "GOOGLE_API_KEY=a", "HF_TOKEN=b", "GH_TOKEN=c",
		"GITLAB_TOKEN=d", "AWS_SECRET_ACCESS_KEY=e", "IKB_MCP_TOKEN=f"}
	got := strings.Join(scrubSubprocessEnv(raw, "/bin/ls"), "\n")
	for _, s := range []string{"GOOGLE_API_KEY", "HF_TOKEN", "GH_TOKEN", "GITLAB_TOKEN", "AWS_", "IKB_MCP_TOKEN"} {
		if strings.Contains(got, s) {
			t.Errorf("env leaked %s: %s", s, got)
		}
	}
	if !strings.Contains(got, "PATH=/bin") || !strings.Contains(got, "LC_ALL=C") {
		t.Errorf("safe keys dropped: %s", got)
	}
}

func TestRunCommandBoundedOutputAndKilledChildren(t *testing.T) {
	ws := t.TempDir()
	cfg := policy.DefaultPolicyConfig(ws)
	cfg.AllowCommands = [][]string{{"yes"}, {"sleep"}}
	start := time.Now()
	res, err := Execute(context.Background(), "run_command", map[string]any{"argv": []string{"yes"}, "timeout_s": 1}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Output) > maxRunOutput+256 || !strings.Contains(res.Output, "truncated") {
		t.Fatalf("output not bounded: %d bytes", len(res.Output))
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("runaway command not killed at timeout")
	}
	// timeout_s cap
	if _, err := Execute(context.Background(), "run_command", map[string]any{"argv": []string{"sleep", "0"}, "timeout_s": 999999}, cfg); err != nil {
		t.Fatal(err)
	}
}

// Multi-call binaries (busybox) dispatch on argv[0]; run_command must not
// replace the caller's name with the symlink target.
func TestRunCommandPreservesArgv0ForMultiCall(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(self, filepath.Join(bin, "loom-applet")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := policy.DefaultPolicyConfig(t.TempDir())
	cfg.AllowCommands = [][]string{{"loom-applet"}}
	res, err := Execute(context.Background(), "run_command", map[string]any{"argv": []string{"loom-applet"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "applet-ok") {
		t.Fatalf("argv0 not preserved: %q", res.Output)
	}
}

func TestClampResultKeepsHeadAndTail(t *testing.T) {
	tb := "Traceback (most recent call last):\n  File \"x.py\", line 1\nValueError: boom\n"
	raw := strings.Repeat("line of noise\n", 8000) + tb // ~110 KB
	res := clampResult(raw, false, 16384)
	if !res.Truncated || res.BytesRaw != len(raw) {
		t.Fatalf("flags: %+v", res)
	}
	if !strings.HasSuffix(res.Output, tb) || !strings.Contains(res.Output, "[loom:truncated ") || !strings.Contains(res.Output, " bytes in middle]") {
		t.Fatalf("tail/marker missing: %q", res.Output[len(res.Output)-200:])
	}
	if !strings.HasPrefix(res.Output, "line of noise") || res.BytesPassed > 16384 {
		t.Fatalf("head/passed wrong: %d", res.BytesPassed)
	}
}

func TestClampResultUTF8Boundaries(t *testing.T) {
	raw := strings.Repeat("한", 5000)
	for max := 100; max < 112; max++ {
		res := clampResult(raw, false, max)
		if !utf8.ValidString(res.Output) {
			t.Fatalf("max=%d invalid utf8", max)
		}
	}
}

func TestCappedBufferHeadAndRingTail(t *testing.T) {
	c := &cappedBuffer{max: 300}
	var all strings.Builder
	for i := 0; i < 200; i++ {
		s := fmt.Sprintf("row-%03d;", i)
		all.WriteString(s)
		c.Write([]byte(s))
	}
	out := c.String()
	if !c.truncated() || !strings.HasPrefix(out, "row-000;") || !strings.HasSuffix(out, "row-199;") || !strings.Contains(out, "bytes in middle]") {
		t.Fatalf("bad: %q", out)
	}
	small := &cappedBuffer{max: 300}
	small.Write([]byte("hello"))
	if small.truncated() || small.String() != "hello" {
		t.Fatalf("small: %q", small.String())
	}
}

func TestRunCommandStructuredResult(t *testing.T) {
	ws := t.TempDir()
	cfg := policy.DefaultPolicyConfig(ws)
	self, _ := os.Executable()
	cfg.AllowCommands = [][]string{{self}, {"sleep"}, {"yes"}}

	res, err := Execute(context.Background(), "run_command", map[string]any{"argv": []string{self, "-test.run=TestHelperExit", "--", "loom-exit3"}}, cfg)
	if err != nil || res.ExitCode == nil || *res.ExitCode != 3 || res.TimedOut {
		t.Fatalf("exit3: %+v err=%v", res, err)
	}
	res, _ = Execute(context.Background(), "run_command", map[string]any{"argv": []string{"sleep", "30"}, "timeout_s": 1}, cfg)
	if !res.TimedOut || res.ExitCode == nil || *res.ExitCode != -1 {
		t.Fatalf("timeout: %+v", res)
	}
	res, _ = Execute(context.Background(), "run_command", map[string]any{"argv": []string{"sleep", "0"}}, cfg)
	if res.ExitCode == nil || *res.ExitCode != 0 || res.TimedOut || res.Truncated {
		t.Fatalf("ok: %+v", res)
	}
	res, _ = Execute(context.Background(), "run_command", map[string]any{"argv": []string{"yes"}, "timeout_s": 1}, cfg)
	if !res.Truncated {
		t.Fatalf("big output not truncated: %+v", res)
	}
}

func TestHelperExit(t *testing.T) {
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "loom-exit3" {
		os.Exit(3)
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestScrubEnvMLDefaultsAndPassEnv(t *testing.T) {
	raw := []string{
		"PATH=/bin", "PYTHONPATH=/p", "PYTHONUNBUFFERED=1", "CUDA_VISIBLE_DEVICES=0", "CUDA_HOME=/c", "CUDA_FOO=1",
		"HF_HOME=/h", "TRANSFORMERS_CACHE=/t", "OMP_NUM_THREADS=4", "HF_TOKEN=hf-secret", "HF_FOO=1",
		"MYAPP_A=1", "MYAPP_API_KEY=k", "MYAPP_TOKEN=tt", "OTHER=1",
	}
	m := envMap(scrubSubprocessEnv(raw, "/bin/x"))
	for _, k := range []string{"PYTHONPATH", "PYTHONUNBUFFERED", "CUDA_VISIBLE_DEVICES", "CUDA_HOME", "CUDA_FOO", "HF_HOME", "TRANSFORMERS_CACHE", "OMP_NUM_THREADS"} {
		if _, ok := m[k]; !ok {
			t.Errorf("default %s dropped", k)
		}
	}
	for _, k := range []string{"HF_TOKEN", "HF_FOO", "MYAPP_A", "OTHER"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s should be dropped by default", k)
		}
	}
	m = envMap(scrubSubprocessEnv(raw, "/bin/x", "HF_*", "MYAPP_*"))
	if _, ok := m["HF_FOO"]; !ok {
		t.Error("HF_* prefix should pass HF_FOO")
	}
	if _, ok := m["MYAPP_A"]; !ok {
		t.Error("MYAPP_* prefix should pass MYAPP_A")
	}
	for _, k := range []string{"HF_TOKEN", "MYAPP_API_KEY", "MYAPP_TOKEN", "OTHER"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s must stay blocked under a prefix", k)
		}
	}
	m = envMap(scrubSubprocessEnv(raw, "/bin/x", "HF_*", "HF_TOKEN"))
	if m["HF_TOKEN"] != "hf-secret" {
		t.Error("exact HF_TOKEN should pass")
	}
}
