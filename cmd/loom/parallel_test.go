//go:build unix

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestMain doubles as a lock-holder helper: the test binary re-executed with
// LOOM_TEST_HOLD=<workspace>|<trace> takes the run locks and blocks on stdin.
func TestMain(m *testing.M) {
	if h := os.Getenv("LOOM_TEST_HOLD"); h != "" {
		ws, tr, _ := strings.Cut(h, "|")
		rel, err := acquireRunLocks(ws, tr)
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		defer rel()
		fmt.Println("HELD")
		_, _ = bufio.NewReader(os.Stdin).ReadByte()
		return
	}
	os.Exit(m.Run())
}

func buildLoom(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "loom")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func TestParallelRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and spawns processes")
	}
	bin := buildLoom(t)
	const n = 6
	root := t.TempDir()
	var wg sync.WaitGroup
	errs := make([]string, n)
	for i := 0; i < n; i++ {
		ws := filepath.Join(root, fmt.Sprintf("ws%d", i))
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(i int, ws string) {
			defer wg.Done()
			tr := filepath.Join(root, "traces", fmt.Sprintf("t%d.jsonl", i))
			cmd := exec.Command(bin, "-p", "parallel test", "--provider", "mock", "--workspace", ws, "--trace", tr)
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Run(); err != nil {
				errs[i] = fmt.Sprintf("%v: %s", err, out.String())
			}
		}(i, ws)
	}
	wg.Wait()
	for i, e := range errs {
		if e != "" {
			t.Fatalf("run %d failed: %s", i, e)
		}
	}
	ids := map[string]bool{}
	for i := 0; i < n; i++ {
		raw, err := os.ReadFile(filepath.Join(root, "traces", fmt.Sprintf("t%d.jsonl", i)))
		if err != nil {
			t.Fatal(err)
		}
		var runID string
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var ev map[string]any
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("trace %d invalid JSONL: %v: %q", i, err, line)
			}
			if ev["t"] == "done" {
				runID, _ = ev["run_id"].(string)
			}
		}
		if runID == "" || ids[runID] {
			t.Fatalf("trace %d: missing or duplicate run id %q", i, runID)
		}
		ids[runID] = true
	}
	// no stray .ckpt / temp files in traces dir or workspaces
	var stray []string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Ext(p) != ".jsonl" {
			stray = append(stray, p)
		}
		return nil
	})
	if len(stray) > 0 {
		t.Fatalf("stray files: %v", stray)
	}
}

func startHolder(t *testing.T, ws, tr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "LOOM_TEST_HOLD="+ws+"|"+tr)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Wait() })
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "HELD" {
		t.Fatalf("holder: %q", line)
	}
}

func runLoomExpect2(t *testing.T, bin, ws, tr, want string) {
	t.Helper()
	cmd := exec.Command(bin, "-p", "x", "--provider", "mock", "--workspace", ws, "--trace", tr)
	var out bytes.Buffer
	cmd.Stderr = &out
	err := cmd.Run()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 2 {
		t.Fatalf("want exit 2, got %v (%s)", err, out.String())
	}
	msg := strings.TrimSpace(out.String())
	if strings.Count(msg, "\n") != 0 || !strings.Contains(msg, want) || !strings.Contains(msg, "in use by another loom process") {
		t.Fatalf("bad message %q", msg)
	}
}

func TestSameWorkspaceRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and spawns processes")
	}
	bin := buildLoom(t)
	ws := t.TempDir()
	startHolder(t, ws, "")
	runLoomExpect2(t, bin, ws, filepath.Join(t.TempDir(), "o.jsonl"), "workspace")
}

func TestSameTraceRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and spawns processes")
	}
	bin := buildLoom(t)
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	startHolder(t, t.TempDir(), tr)
	runLoomExpect2(t, bin, t.TempDir(), tr, "trace")
}
