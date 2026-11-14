package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
)

func jobCfg(t *testing.T, allow ...[]string) *policy.PolicyConfig {
	t.Helper()
	cfg := policy.DefaultPolicyConfig(t.TempDir())
	cfg.AllowCommands = allow
	return cfg
}

func runJob(t *testing.T, name string, args map[string]any, cfg *policy.PolicyConfig) ToolResult {
	t.Helper()
	res, err := Execute(context.Background(), name, args, cfg)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func startJob(t *testing.T, cfg *policy.PolicyConfig, argv []string, detach bool) jobMeta {
	t.Helper()
	res := runJob(t, "job_start", map[string]any{"argv": argv, "detach": detach}, cfg)
	if res.IsError {
		t.Fatalf("job_start error: %s", res.Output)
	}
	var m jobMeta
	if err := json.Unmarshal([]byte(res.Output), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestJobStartRejectsDisallowed(t *testing.T) {
	cfg := jobCfg(t, []string{"echo"})
	for _, argv := range [][]string{{"rm", "-rf", "/"}, {"bash", "-c", "ls"}, {"python3", "-c", "1"}} {
		res := runJob(t, "job_start", map[string]any{"argv": argv}, cfg)
		if !res.IsError || res.ViolationKind == "" {
			t.Errorf("%v must be a recoverable violation, got %+v", argv, res)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.Workspace, ".loom", "jobs")); err == nil {
		ents, _ := os.ReadDir(filepath.Join(cfg.Workspace, ".loom", "jobs"))
		if len(ents) != 0 {
			t.Errorf("rejected start must not leave job files: %v", ents)
		}
	}
}

func TestJobLifecycleAndLog(t *testing.T) {
	cfg := jobCfg(t, []string{"echo"})
	m := startJob(t, cfg, []string{"echo", "line1"}, false)
	w := runJob(t, "job_wait", map[string]any{"job_id": m.JobID, "timeout_s": 10}, cfg)
	if !strings.Contains(w.Output, `"status":"exited"`) || !strings.Contains(w.Output, `"exit_code":0`) || !strings.Contains(w.Output, "line1") {
		t.Errorf("wait output: %q", w.Output)
	}
	st := runJob(t, "job_status", map[string]any{"job_id": m.JobID}, cfg)
	if !strings.Contains(st.Output, `"status":"exited"`) || !strings.Contains(st.Output, `"pid"`) {
		t.Errorf("status: %q", st.Output)
	}
	lg := runJob(t, "job_log", map[string]any{"job_id": m.JobID, "tail_n": 1}, cfg)
	if lg.Output != "line1" {
		t.Errorf("log: %q", lg.Output)
	}
	if bad := runJob(t, "job_status", map[string]any{"job_id": "../../etc/passwd"}, cfg); !bad.IsError {
		t.Error("traversal job_id must be rejected")
	}
	if _, err := os.Stat(filepath.Join(cfg.Workspace, ".loom", "jobs", m.JobID+".log")); err != nil {
		t.Errorf("log not workspace-confined: %v", err)
	}
}

func TestJobLogTail(t *testing.T) {
	cfg := jobCfg(t, []string{"seq"})
	m := startJob(t, cfg, []string{"seq", "1", "100"}, false)
	runJob(t, "job_wait", map[string]any{"job_id": m.JobID, "timeout_s": 10}, cfg)
	lg := runJob(t, "job_log", map[string]any{"job_id": m.JobID, "tail_n": 3}, cfg)
	if lg.Output != "98\n99\n100" {
		t.Errorf("tail: %q", lg.Output)
	}
}

func TestJobWaitTimeoutThenKillGroup(t *testing.T) {
	cfg := jobCfg(t, []string{"sleep"})
	m := startJob(t, cfg, []string{"sleep", "60"}, false)
	w := runJob(t, "job_wait", map[string]any{"job_id": m.JobID, "timeout_s": 1}, cfg)
	if !strings.Contains(w.Output, `"status":"running"`) || !strings.Contains(w.Output, `"timed_out":true`) {
		t.Errorf("wait should time out while running: %q", w.Output)
	}
	// Process group: pgid == pid (Setpgid).
	if pg, _ := syscall.Getpgid(m.PID); pg != m.PID {
		t.Errorf("pgid %d != pid %d", pg, m.PID)
	}
	k := runJob(t, "job_kill", map[string]any{"job_id": m.JobID}, cfg)
	if !strings.Contains(k.Output, `"status":"killed"`) {
		t.Errorf("kill: %q", k.Output)
	}
	if pidAlive(m.PID) {
		t.Error("process still alive after job_kill")
	}
	st := runJob(t, "job_status", map[string]any{"job_id": m.JobID}, cfg)
	if !strings.Contains(st.Output, `"status":"killed"`) {
		t.Errorf("status after kill: %q", st.Output)
	}
}

func TestJobEnvScrubbed(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret-value")
	cfg := jobCfg(t, []string{"env"})
	m := startJob(t, cfg, []string{"env"}, false)
	runJob(t, "job_wait", map[string]any{"job_id": m.JobID, "timeout_s": 10}, cfg)
	lg := runJob(t, "job_log", map[string]any{"job_id": m.JobID, "tail_n": 500}, cfg)
	if strings.Contains(lg.Output, "sk-secret-value") || strings.Contains(lg.Output, "ANTHROPIC_API_KEY") {
		t.Errorf("secret leaked into job env: %q", lg.Output)
	}
}

func TestKillAttachedJobsSparesDetached(t *testing.T) {
	cfg := jobCfg(t, []string{"sleep"})
	att := startJob(t, cfg, []string{"sleep", "60"}, false)
	det := startJob(t, cfg, []string{"sleep", "60"}, true)
	t.Cleanup(func() { _ = syscall.Kill(-det.PID, syscall.SIGKILL) })

	ids := StartedJobIDs()
	if !contains(ids, att.JobID) || !contains(ids, det.JobID) {
		t.Errorf("StartedJobIDs missing jobs: %v", ids)
	}
	KillAttachedJobs()

	if pidAlive(att.PID) {
		t.Error("non-detached job must be killed at run exit")
	}
	if !pidAlive(det.PID) {
		t.Fatal("detached job must survive run exit")
	}
	st := runJob(t, "job_status", map[string]any{"job_id": att.JobID}, cfg)
	if !strings.Contains(st.Output, `"status":"killed"`) {
		t.Errorf("attached status: %q", st.Output)
	}

	// Simulate a new invocation: forget live state, then inspect and kill the
	// detached job purely from the persisted record.
	jobMu.Lock()
	delete(jobLive, det.JobID)
	jobMu.Unlock()
	st = runJob(t, "job_status", map[string]any{"job_id": det.JobID}, cfg)
	if !strings.Contains(st.Output, `"status":"running"`) || !strings.Contains(st.Output, `"detached":true`) {
		t.Errorf("detached status across runs: %q", st.Output)
	}
	runJob(t, "job_kill", map[string]any{"job_id": det.JobID}, cfg)
	deadline := time.Now().Add(3 * time.Second)
	for pidAlive(det.PID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if pidAlive(det.PID) {
		t.Error("persisted detached job not killed")
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
