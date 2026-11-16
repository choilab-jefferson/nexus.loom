package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
)

// Background jobs: long ML runs started with job_start outlive a single tool
// call. State lives in workspace-confined .loom/jobs/<id>.{json,log} so a
// resumed run or a later --context-in turn can inspect, wait on, or kill a job.

const (
	jobsRelDir      = ".loom/jobs"
	jobStatusRun    = "running"
	jobStatusExited = "exited"
	jobStatusKilled = "killed"
	jobPollEvery    = 100 * time.Millisecond
	jobKillGrace    = 2 * time.Second
	jobDefaultTail  = 50
	jobDefaultWait  = 30
)

var jobIDRe = regexp.MustCompile(`^job-[0-9a-f]{12}$`)

// jobMeta is the persisted job record (.loom/jobs/<id>.json).
type jobMeta struct {
	JobID    string   `json:"job_id"`
	Argv     []string `json:"argv"`
	PID      int      `json:"pid"`
	Status   string   `json:"status"`
	ExitCode *int     `json:"exit_code,omitempty"`
	Detached bool     `json:"detached"`
	Started  string   `json:"started"`
	Ended    string   `json:"ended,omitempty"`
	TimedOut bool     `json:"timed_out,omitempty"`
}

type liveJob struct {
	cmd      *exec.Cmd
	detached bool
	dir      string
	done     chan struct{}
}

var (
	jobMu   sync.Mutex // guards registry and all .json read-modify-write
	jobLive = map[string]*liveJob{}
	jobSeen []string // IDs started by this process, in order
)

// StartedJobIDs returns the job IDs started by this process (for checkpoints).
func StartedJobIDs() []string {
	jobMu.Lock()
	defer jobMu.Unlock()
	return append([]string(nil), jobSeen...)
}

func jobsDir(cfg *policy.PolicyConfig) (string, error) {
	dir, err := policy.ResolveScopedPath(cfg.Workspace, jobsRelDir, true, cfg)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func newJobID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "job-" + hex.EncodeToString(b), nil
}

func jobIDArg(args map[string]any) (string, error) {
	id := getString(args, "job_id")
	if !jobIDRe.MatchString(id) {
		return "", fmt.Errorf("invalid job_id %q", id)
	}
	return id, nil
}

// writeJobMeta atomically replaces <dir>/<id>.json. Caller holds jobMu.
func writeJobMeta(dir string, m *jobMeta) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".job-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, m.JobID+".json"))
}

func readJobMeta(dir, id string) (*jobMeta, error) {
	raw, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("unknown job_id %q", id)
		}
		return nil, err
	}
	var m jobMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("corrupt job record %s: %w", id, err)
	}
	return &m, nil
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// loadJob reads the record and, for a job marked running that no live process
// backs any more, settles it to exited (exit code unknown across invocations).
func loadJob(dir, id string) (*jobMeta, error) {
	jobMu.Lock()
	defer jobMu.Unlock()
	return loadJobLocked(dir, id)
}

func loadJobLocked(dir, id string) (*jobMeta, error) {
	m, err := readJobMeta(dir, id)
	if err != nil {
		return nil, err
	}
	if m.Status != jobStatusRun {
		return m, nil
	}
	if lj, ok := jobLive[id]; ok {
		select {
		case <-lj.done: // reaped but record still running: settle below
		default:
			return m, nil
		}
	} else if pidAlive(m.PID) {
		return m, nil // foreign (earlier invocation) job still alive
	}
	m.Status = jobStatusExited
	m.Ended = time.Now().UTC().Format(time.RFC3339)
	_ = writeJobMeta(dir, m)
	return m, nil
}

func execJobStart(args map[string]any, cfg *policy.PolicyConfig) (string, error) {
	argv := getStringSlice(args, "argv")
	if err := policy.ValidateCommandArgv(argv, cfg); err != nil {
		return "", err
	}
	wsReal, err := policy.ResolveScopedPath(cfg.Workspace, ".", false, cfg)
	if err != nil {
		return "", err
	}
	resolved, err := policy.ResolveArgv0(argv[0], cfg.Workspace)
	if err != nil {
		return "", &policy.ViolationError{Reason: fmt.Sprintf("cannot resolve executable %q: %v", argv[0], err)}
	}
	dir, err := jobsDir(cfg)
	if err != nil {
		return "", err
	}
	id, err := newJobID()
	if err != nil {
		return "", err
	}
	logf, err := os.OpenFile(filepath.Join(dir, id+".log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	defer logf.Close() // the child holds its own duplicate

	detached, _ := args["detach"].(bool)
	cmd := exec.Command(resolved, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Dir = wsReal
	cmd.Env = scrubSubprocessEnv(os.Environ(), argv[0], cfg.PassEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = logf
	cmd.Stderr = logf

	jobMu.Lock()
	defer jobMu.Unlock()
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start %q: %w", argv[0], err)
	}
	m := &jobMeta{
		JobID: id, Argv: argv, PID: cmd.Process.Pid, Status: jobStatusRun,
		Detached: detached, Started: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeJobMeta(dir, m); err != nil {
		_ = syscall.Kill(-m.PID, syscall.SIGKILL)
		_ = cmd.Wait()
		return "", err
	}
	lj := &liveJob{cmd: cmd, detached: detached, dir: dir, done: make(chan struct{})}
	jobLive[id] = lj
	jobSeen = append(jobSeen, id)
	go reapJob(id, lj)
	return jobJSON(m), nil
}

// reapJob waits for the child and records its exit unless job_kill already did.
func reapJob(id string, lj *liveJob) {
	werr := lj.cmd.Wait()
	jobMu.Lock()
	defer jobMu.Unlock()
	if m, err := readJobMeta(lj.dir, id); err == nil && m.Status == jobStatusRun {
		code := lj.cmd.ProcessState.ExitCode() // -1 when killed by a signal
		_ = werr
		m.Status, m.ExitCode = jobStatusExited, &code
		m.Ended = time.Now().UTC().Format(time.RFC3339)
		_ = writeJobMeta(lj.dir, m)
	}
	close(lj.done)
}

func jobJSON(m *jobMeta) string {
	raw, _ := json.Marshal(m)
	return string(raw)
}

func execJobStatus(args map[string]any, cfg *policy.PolicyConfig) (string, error) {
	id, err := jobIDArg(args)
	if err != nil {
		return "", err
	}
	dir, err := jobsDir(cfg)
	if err != nil {
		return "", err
	}
	m, err := loadJob(dir, id)
	if err != nil {
		return "", err
	}
	return jobJSON(m), nil
}

// tailLines returns the last n lines of the file, reading at most maxRunOutput bytes.
func tailLines(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := int64(0)
	if st.Size() > maxRunOutput {
		off = st.Size() - maxRunOutput
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return "", nil
	}
	lines := strings.Split(s, "\n")
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // first line is likely partial
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

func execJobLog(args map[string]any, cfg *policy.PolicyConfig) (string, error) {
	id, err := jobIDArg(args)
	if err != nil {
		return "", err
	}
	dir, err := jobsDir(cfg)
	if err != nil {
		return "", err
	}
	if _, err := loadJob(dir, id); err != nil {
		return "", err
	}
	n := getInt(args, "tail_n", jobDefaultTail)
	if n <= 0 {
		n = jobDefaultTail
	}
	return tailLines(filepath.Join(dir, id+".log"), n)
}

func execJobWait(ctx context.Context, args map[string]any, cfg *policy.PolicyConfig) (string, error) {
	id, err := jobIDArg(args)
	if err != nil {
		return "", err
	}
	dir, err := jobsDir(cfg)
	if err != nil {
		return "", err
	}
	sec := getInt(args, "timeout_s", jobDefaultWait)
	if sec <= 0 {
		sec = jobDefaultWait
	}
	deadline := time.Now().Add(time.Duration(clampRunTimeout(sec, cfg)) * time.Second)
	var m *jobMeta
	timedOut := false
	for {
		if m, err = loadJob(dir, id); err != nil {
			return "", err
		}
		if m.Status != jobStatusRun {
			break
		}
		if time.Now().After(deadline) {
			timedOut = true
			break
		}
		select {
		case <-ctx.Done():
			timedOut = true
		case <-time.After(jobPollEvery):
		}
		if timedOut {
			break
		}
	}
	out := *m
	out.TimedOut = timedOut
	tail, _ := tailLines(filepath.Join(dir, id+".log"), jobDefaultTail)
	return jobJSON(&out) + "\n" + tail, nil
}

func execJobKill(args map[string]any, cfg *policy.PolicyConfig) (string, error) {
	id, err := jobIDArg(args)
	if err != nil {
		return "", err
	}
	dir, err := jobsDir(cfg)
	if err != nil {
		return "", err
	}
	jobMu.Lock()
	m, err := loadJobLocked(dir, id)
	if err != nil {
		jobMu.Unlock()
		return "", err
	}
	lj := jobLive[id]
	if m.Status == jobStatusRun {
		killJobLocked(dir, m)
	}
	jobMu.Unlock()
	if lj != nil {
		select {
		case <-lj.done:
		case <-time.After(jobKillGrace):
		}
	}
	return jobJSON(m), nil
}

// killJobLocked SIGKILLs the job's process group and records it as killed.
func killJobLocked(dir string, m *jobMeta) {
	_ = syscall.Kill(-m.PID, syscall.SIGKILL)
	m.Status = jobStatusKilled
	m.Ended = time.Now().UTC().Format(time.RFC3339)
	_ = writeJobMeta(dir, m)
}

// KillAttachedJobs kills every non-detached job this process started and still
// has running. Detached jobs are left alone to survive the run.
func KillAttachedJobs() {
	jobMu.Lock()
	var waits []chan struct{}
	for id, lj := range jobLive {
		if lj.detached {
			continue
		}
		select {
		case <-lj.done:
			continue
		default:
		}
		if m, err := readJobMeta(lj.dir, id); err == nil && m.Status == jobStatusRun {
			killJobLocked(lj.dir, m)
		}
		waits = append(waits, lj.done)
	}
	jobMu.Unlock()
	for _, w := range waits {
		select {
		case <-w:
		case <-time.After(jobKillGrace):
		}
	}
}

// JobInfo is a read-only view of a .loom/jobs/<id>.json record for supervisors (loom loop mode).
type JobInfo struct {
	JobID    string
	Status   string // "running", "exited" or "killed"
	ExitCode *int   // nil when unknown (settled across invocations) or killed
	Started  string
	Ended    string
}

// ListJobs returns the workspace's job records in id order. A job marked running whose process is gone
// is settled to exited with an unknown exit code, as job_status does. A missing jobs dir is not an error.
func ListJobs(cfg *policy.PolicyConfig) ([]JobInfo, error) {
	if _, err := os.Stat(filepath.Join(cfg.Workspace, jobsRelDir)); err != nil {
		return nil, nil
	}
	dir, err := jobsDir(cfg)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []JobInfo
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".json")
		if id == e.Name() || !jobIDRe.MatchString(id) {
			continue
		}
		m, err := loadJob(dir, id)
		if err != nil {
			continue
		}
		out = append(out, JobInfo{JobID: m.JobID, Status: m.Status, ExitCode: m.ExitCode, Started: m.Started, Ended: m.Ended})
	}
	return out, nil
}

// JobRecordPath returns the workspace-relative path of a job record.
func JobRecordPath(id string) string { return jobsRelDir + "/" + id + ".json" }
