//go:build unix

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
	"github.com/choilab-jefferson/nexus.loom/pkg/engine"
	"github.com/choilab-jefferson/nexus.loom/pkg/filebus"
	"github.com/choilab-jefferson/nexus.loom/pkg/providers"
	"github.com/choilab-jefferson/nexus.loom/pkg/tools"
)

func toolsJob(status string, code *int) tools.JobInfo {
	return tools.JobInfo{Status: status, ExitCode: code}
}

// ---- crash helper (the test binary re-executed as `loom serve`, see TestMain) ----

type crashProvider struct {
	inner *fakeProvider
	n     int
}

// Chat dies on its second call, i.e. after the first tool turn completed and was checkpointed.
func (c *crashProvider) Chat(ctx context.Context, p compact.PrefixHeader, msgs []compact.Message, max int) (providers.Response, error) {
	c.n++
	if c.n == 2 {
		os.Exit(9)
	}
	return c.inner.Chat(ctx, p, msgs, max)
}

func serveHelper(argsJSON string) int {
	var args []string
	if json.Unmarshal([]byte(argsJSON), &args) != nil {
		return 2
	}
	testProviderHook = func(name string) providers.Provider {
		f := &fakeProvider{name: name}
		if os.Getenv("LOOM_TEST_CRASH") == "1" {
			return &crashProvider{inner: f}
		}
		return f
	}
	return runMain(append([]string{"serve"}, args...))
}

// ---- rig ----

type rig struct {
	t      *testing.T
	dir    string
	ws     string
	host   *filebus.Bus
	name   string
	cancel context.CancelFunc
	done   chan int
	exited bool
	got    []filebus.Message
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	r := &rig{t: t, dir: filepath.Join(root, "bus"), ws: filepath.Join(root, "ws"), name: "loom-a1"}
	if err := os.MkdirAll(r.ws, 0o755); err != nil {
		t.Fatal(err)
	}
	h, err := filebus.Open(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	r.host = h
	return r
}

func (r *rig) cfg() serveConfig {
	return serveConfig{
		BusDir: r.dir, Name: r.name, Workspace: r.ws, ProviderName: "mock", ReportTo: "host",
		Base:             engine.TaskSpec{SchemaVersion: 1, Agent: "worker", Constraints: engine.TaskConstraints{MaxTurns: 8}},
		MaxContextTokens: 28000, Poll: 20 * time.Millisecond, JobPoll: 20 * time.Millisecond,
		Heartbeat: 50 * time.Millisecond, Stderr: io.Discard,
	}
}

func (r *rig) start(cfg serveConfig) {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan int, 1)
	go func() { r.done <- serveLoop(ctx, cfg) }()
	r.t.Cleanup(func() {
		cancel()
		if r.exited {
			return
		}
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
		}
	})
}

func (r *rig) send(kind, body string, data any) filebus.Message {
	r.t.Helper()
	m, err := r.host.Send(filebus.Outgoing{Sender: "host", Target: r.name, Kind: kind, Body: body, Data: data})
	if err != nil {
		r.t.Fatal(err)
	}
	return m
}

func (r *rig) fill() {
	msgs, err := r.host.Unacked("host", "", 0)
	if err != nil {
		r.t.Fatal(err)
	}
	var ids []string
	for _, m := range msgs {
		r.got = append(r.got, m)
		ids = append(ids, m.ID)
	}
	if len(ids) > 0 {
		_ = r.host.Ack("host", "", ids...)
	}
}

func (r *rig) find(pred func(filebus.Message) bool) (filebus.Message, bool) {
	for _, m := range r.got {
		if pred(m) {
			return m, true
		}
	}
	return filebus.Message{}, false
}

func (r *rig) await(what string, pred func(filebus.Message) bool) filebus.Message {
	r.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r.fill()
		if m, ok := r.find(pred); ok {
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("timeout waiting for %s; got %d messages: %s", what, len(r.got), r.dump())
	return filebus.Message{}
}

func (r *rig) dump() string {
	var parts []string
	for _, m := range r.got {
		parts = append(parts, m.Kind+" "+string(m.Data))
	}
	return strings.Join(parts, " | ")
}

func (r *rig) exit() int {
	r.t.Helper()
	select {
	case rc := <-r.done:
		r.exited = true
		return rc
	case <-time.After(15 * time.Second):
		r.t.Fatal("serve did not exit")
	}
	return -1
}

func dataOf(m filebus.Message) map[string]any {
	var d map[string]any
	_ = json.Unmarshal(m.Data, &d)
	return d
}

func result(req string) func(filebus.Message) bool {
	return func(m filebus.Message) bool { return m.Kind == "loom.result" && dataOf(m)["request"] == req }
}

func state(name string) func(filebus.Message) bool {
	return func(m filebus.Message) bool { return m.Kind == "loom.state" && dataOf(m)["state"] == name }
}

func (r *rig) unackedFor(name string) int {
	msgs, err := r.host.Unacked(name, "", 0)
	if err != nil {
		r.t.Fatal(err)
	}
	return len(msgs)
}

func readBlob(t *testing.T, r *rig, rel string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// ---- tests ----

func TestServeTaskInjectExtractRoundTrip(t *testing.T) {
	fakes := useFakes(t)
	r := newRig(t)
	outside := filepath.Join(filepath.Dir(r.ws), "outside.txt")
	os.WriteFile(outside, []byte("outside-secret"), 0o600)
	os.WriteFile(filepath.Join(r.ws, "notes.txt"), []byte("file-secret-123"), 0o600)
	r.start(r.cfg())

	st := r.await("started", state("started"))
	if dataOf(st)["turns_used"] != float64(0) {
		t.Fatalf("started: %s", st.Data)
	}
	sess, _, ok := r.host.ReadSession(r.name)
	if !ok || sess.Status != "live" || sess.Agent["kind"] != "loom" || sess.Agent["loop_mode"] != float64(1) {
		t.Fatalf("session %+v", sess)
	}

	r.send("loom.inject", "", map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "NOTE codeword zebra-9"}},
		"files":    []string{"notes.txt", "../outside.txt", "/etc/hostname"}})
	task := r.send("loom.task", "", map[string]any{"prompt": "hello there"})
	res := r.await("task result", result(task.ID))
	d := dataOf(res)
	if d["status"] != "success" || d["exit_code"] != float64(0) || d["user_turn"] != float64(1) || res.MetaString("corr") != task.ID {
		t.Fatalf("result %s corr=%v", res.Data, res.Meta["corr"])
	}
	if res.Body == "" || res.Target != "host" || res.Sender != r.name {
		t.Fatalf("result routing/body: %+v", res)
	}
	if tr, _ := d["trace"].(string); tr == "" {
		t.Fatalf("no trace pointer: %s", res.Data)
	} else if fi, err := os.Stat(filepath.Join(r.dir, filepath.FromSlash(tr))); err != nil || fi.Size() == 0 {
		t.Fatalf("trace %s: %v", tr, err)
	}
	last := fakes["mock"].seen[0]
	prompt := last[len(last)-1].Content
	for _, want := range []string{"NOTE codeword zebra-9", "File notes.txt", "file-secret-123", "hello there"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("injected context missing %q in %q", want, prompt)
		}
	}
	if strings.Contains(prompt, "outside-secret") {
		t.Fatal("inject read a file outside the workspace")
	}
	r.await("busy/idle", state("idle"))

	ex := r.send("loom.extract", "", map[string]any{"what": "context"})
	got := dataOf(r.await("extracted", func(m filebus.Message) bool { return m.Kind == "loom.extracted" && dataOf(m)["request"] == ex.ID }))
	raw := readBlob(t, r, got["path"].(string))
	sum := sha256.Sum256(raw)
	if got["sha256"] != hex.EncodeToString(sum[:]) || got["schema"] != "loom.context.v1" || !strings.HasPrefix(got["path"].(string), "blob/host/") {
		t.Fatalf("extracted %v", got)
	}
	c, _, err := engine.ParseContext(raw)
	if err != nil || c.UserTurn != 1 || len(c.Runs) != 1 || !strings.Contains(c.Messages[0].Content, "zebra-9") {
		t.Fatalf("extracted context %v %+v", err, c)
	}

	sx := r.send("loom.extract", "", map[string]any{"what": "summary"})
	sg := dataOf(r.await("summary", func(m filebus.Message) bool { return m.Kind == "loom.extracted" && dataOf(m)["request"] == sx.ID }))
	var sm map[string]any
	json.Unmarshal(readBlob(t, r, sg["path"].(string)), &sm)
	if sg["schema"] != "loom.summary.v1" || !strings.Contains(sm["summary"].(string), "user_turns=1") {
		t.Fatalf("summary %v %v", sg, sm)
	}

	// A second turn continues the same context: the first turn's messages are in the history.
	t2 := r.send("loom.prompt", "again please", nil) // alias kind, prompt taken from the body
	if d2 := dataOf(r.await("second result", result(t2.ID))); d2["user_turn"] != float64(2) {
		t.Fatalf("second turn %v", d2)
	}
	if n := len(fakes["mock"].seen); n != 2 || len(fakes["mock"].seen[1]) < 3 {
		t.Fatalf("second turn did not see the first: %d calls", n)
	}

	// Everything was acked; a stop ends the loop with exit 0 and leaves later messages unacked.
	if n := r.unackedFor(r.name); n != 0 {
		t.Fatalf("%d unacked", n)
	}
	r.send("loom.stop", "", map[string]any{"reason": "test done"})
	stopped := r.await("stopped", state("stopped"))
	if dataOf(stopped)["reason"] != "test done" || r.exit() != 0 {
		t.Fatalf("stop: %s", stopped.Data)
	}
	late := r.send("loom.task", "", map[string]any{"prompt": "never runs"})
	if n := r.unackedFor(r.name); n != 1 {
		t.Fatalf("message after stop must stay unacked, %d", n)
	}
	if s, mt, _ := r.host.ReadSession(r.name); s.Status != "unregistered" || s.Live(mt, time.Now()) {
		t.Fatalf("session after stop: %+v", s)
	}

	// Restart on the same bus dir: the persisted context continues and the pending task runs.
	r2 := *r
	r2.done = make(chan int, 1)
	r2.got = nil
	r2.start(r.cfg())
	d3 := dataOf(r2.await("post-restart result", result(late.ID)))
	if d3["user_turn"] != float64(3) {
		t.Fatalf("restart lost the context: %v", d3)
	}
	r2.cancel()
	if rc := r2.exit(); rc != 130 {
		t.Fatalf("signal exit = %d, want 130", rc)
	}
}

// Crash during a turn: the process dies after a tool turn; a restart reports interrupted/130 and acks;
// loom.resume then finishes the turn from the checkpoint.
func TestServeCrashMidTurnThenResume(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a process")
	}
	fakes := useFakes(t)
	_ = fakes
	r := newRig(t)
	args, _ := json.Marshal([]string{"--bus-dir", r.dir, "--name", r.name, "--workspace", r.ws, "--provider", "mock",
		"--poll-interval", "20ms", "--report-to", "host"})
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "LOOM_TEST_SERVE="+string(args), "LOOM_TEST_CRASH=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	task := r.send("loom.task", "", map[string]any{"prompt": "create a file"})
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 9 {
			t.Fatalf("helper exit: %v", err)
		}
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("helper did not crash")
	}
	st, _ := r.host.ReadState(r.name)
	if st["inflight"] != task.ID {
		t.Fatalf("state after crash: %v", st)
	}
	if n := r.unackedFor(r.name); n != 1 {
		t.Fatalf("task must be unacked after the crash, %d", n)
	}
	if _, err := os.Stat(filepath.Join(r.ws, "secret-name-7.txt")); err != nil {
		t.Fatalf("the first tool turn did not run: %v", err)
	}

	r.start(r.cfg())
	d := dataOf(r.await("interrupted result", result(task.ID)))
	if d["status"] != "interrupted" || d["exit_code"] != float64(130) {
		t.Fatalf("interrupted result %v", d)
	}
	if n := r.unackedFor(r.name); n != 0 {
		t.Fatalf("interrupted task must be acked, %d", n)
	}
	if st, _ := r.host.ReadState(r.name); st["inflight"] != nil {
		t.Fatalf("inflight not cleared: %v", st)
	}
	// The turn is not re-run on its own.
	time.Sleep(150 * time.Millisecond)
	if len(fakesSeen(fakes)) != 0 {
		t.Fatalf("the interrupted turn was re-run without loom.resume")
	}

	rs := r.send("loom.resume", "", nil)
	res := r.await("resume result", result(rs.ID))
	rd := dataOf(res)
	if rd["status"] != "success" || !strings.Contains(res.Body, "saw tool result") || rd["user_turn"] != float64(1) {
		t.Fatalf("resume result %v body=%q", rd, res.Body)
	}
	raw, _ := os.ReadFile(filepath.Join(r.dir, "state", r.name+".context.json"))
	c, _, err := engine.ParseContext(raw)
	if err != nil || c.UserTurn != 1 || len(c.Runs) != 1 || len(c.Messages) != 4 {
		t.Fatalf("context after resume: %v %+v", err, c)
	}
	// Resume with nothing to resume is a harness error, not a hang.
	bad := r.send("loom.resume", "", map[string]any{"checkpoint": "state/nope.jsonl"})
	if dataOf(r.await("bad resume", result(bad.ID)))["status"] != "harness_error" {
		t.Fatal("bad checkpoint accepted")
	}
}

// A failed control message answers loom.result harness_error with a reason, is acked, and the loop keeps serving.
func TestServeControlErrorsDoNotEndLoop(t *testing.T) {
	useFakes(t)
	r := newRig(t)
	r.start(r.cfg())
	r.await("started", state("started"))
	bad := map[string]filebus.Message{
		"no checkpoint":  r.send("loom.resume", "", nil),
		"bad checkpoint": r.send("loom.resume", "", map[string]any{"checkpoint": "state/nope.jsonl"}),
		"extract target": r.send("loom.extract", "", map[string]any{"what": "bogus"}),
		"empty prompt":   r.send("loom.task", "", map[string]any{"prompt": "  "}),
	}
	for name, m := range bad {
		d := dataOf(r.await(name, result(m.ID)))
		if d["status"] != "harness_error" || d["exit_code"] != float64(2) || d["reason"] == nil || d["reason"] == "" {
			t.Errorf("%s: %v", name, d)
		}
	}
	task := r.send("loom.task", "", map[string]any{"prompt": "still alive?"})
	if d := dataOf(r.await("normal task", result(task.ID))); d["status"] != "success" || d["exit_code"] != float64(0) {
		t.Fatalf("task after errors: %v", d)
	}
	select {
	case rc := <-r.done:
		t.Fatalf("serve exited (%d) after a control-message failure", rc)
	default:
	}
	if n := r.unackedFor(r.name); n != 0 {
		t.Fatalf("%d unacked", n)
	}
}

func fakesSeen(f map[string]*fakeProvider) [][]compact.Message {
	if p := f["mock"]; p != nil {
		return p.seen
	}
	return nil
}

func TestServeIdleTimeoutAndWallBudget(t *testing.T) {
	useFakes(t)
	r := newRig(t)
	cfg := r.cfg()
	cfg.IdleTimeout = 250 * time.Millisecond
	r.start(cfg)
	ex := r.await("exiting", state("exiting"))
	if dataOf(ex)["reason"] != "idle_timeout" || r.exit() != 0 {
		t.Fatalf("idle: %s", ex.Data)
	}

	r2 := newRig(t)
	cfg = r2.cfg()
	cfg.MaxWall = 250 * time.Millisecond
	r2.start(cfg)
	ex = r2.await("exiting", state("exiting"))
	if dataOf(ex)["reason"] != "budget_exhausted" || r2.exit() != 4 {
		t.Fatalf("wall: %s", ex.Data)
	}
}

func TestServeMaxUserTurns(t *testing.T) {
	useFakes(t)
	r := newRig(t)
	cfg := r.cfg()
	cfg.MaxUserTurns = 1
	r.start(cfg)
	t1 := r.send("loom.task", "", map[string]any{"prompt": "one"})
	r.await("first result", result(t1.ID))
	t2 := r.send("loom.task", "", map[string]any{"prompt": "two"})
	ex := r.await("exiting", state("exiting"))
	if dataOf(ex)["reason"] != "budget_exhausted" || r.exit() != 4 {
		t.Fatalf("budget: %s", ex.Data)
	}
	if _, ok := r.find(result(t2.ID)); ok || r.unackedFor(r.name) != 1 {
		t.Fatal("the over-budget task must stay unacked and un-run")
	}
}

func TestServeConcurrentHostWriters(t *testing.T) {
	useFakes(t)
	r := newRig(t)
	r.start(r.cfg())
	var mu sync.Mutex
	var ids []string
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			h, err := filebus.Open(r.dir) // own writer id, same bus dir
			if err != nil {
				t.Error(err)
				return
			}
			for i := 0; i < 4; i++ {
				m, err := h.Send(filebus.Outgoing{Sender: "host", Target: r.name, Kind: "loom.task", Data: map[string]any{"prompt": "p"}})
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				ids = append(ids, m.ID)
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	for _, id := range ids {
		r.await("result "+id, result(id))
	}
	count := 0
	for _, m := range r.got {
		if m.Kind == "loom.result" {
			count++
		}
	}
	if count != 12 || r.unackedFor(r.name) != 0 {
		t.Fatalf("results=%d unacked=%d", count, r.unackedFor(r.name))
	}
}

func jobScript(argv ...string) *providers.ScriptedMockProvider {
	args := []any{}
	for _, a := range argv {
		args = append(args, a)
	}
	return providers.NewScriptedMockProvider(
		providers.Response{RawStopReason: "stop", ToolCalls: []compact.ToolCall{{ID: "j1", Name: "job_start",
			Arguments: map[string]any{"argv": args, "detach": true}}}},
		providers.Response{Content: "job started", RawStopReason: "stop"},
	)
}

func TestServeJobEvents(t *testing.T) {
	for _, mode := range []string{"notify", "turn"} {
		t.Run(mode, func(t *testing.T) {
			testProviderHook = func(string) providers.Provider { return jobScript("false") }
			t.Cleanup(func() { testProviderHook = nil })
			r := newRig(t)
			cfg := r.cfg()
			cfg.OnJobExit = mode
			cfg.Base.Constraints.AllowCommands = engine.AllowCommandList{{"false"}}
			r.start(cfg)
			task := r.send("loom.task", "", map[string]any{"prompt": "start a job"})
			r.await("task result", result(task.ID))
			isJob := func(verdict string) func(filebus.Message) bool {
				return func(m filebus.Message) bool { return m.Kind == "job" && dataOf(m)["verdict"] == verdict }
			}
			r.await("job RUNNING", isJob("RUNNING"))
			fail := r.await("job FAIL", isJob("FAIL"))
			d := dataOf(fail)
			if d["rc"] != float64(1) || d["supervisor"] != "loom" || !strings.HasPrefix(d["run_dir"].(string), ".loom/jobs/job-") ||
				fail.Summary != d["name"].(string)+" FAIL rc=1" || fail.Body != "" || d["job_id"] != d["name"] {
				t.Fatalf("job message %s summary=%q", fail.Data, fail.Summary)
			}
			synthetic := func(m filebus.Message) bool {
				req, _ := dataOf(m)["request"].(string)
				return m.Kind == "loom.result" && strings.HasPrefix(req, "job-job-")
			}
			if mode == "turn" {
				res := r.await("synthetic turn", synthetic)
				if dataOf(res)["status"] != "success" || dataOf(res)["user_turn"] != float64(2) {
					t.Fatalf("synthetic result %s", res.Data)
				}
			} else {
				time.Sleep(400 * time.Millisecond)
				r.fill()
				if _, ok := r.find(synthetic); ok {
					t.Fatal("notify mode started a synthetic turn")
				}
			}
			// Exactly one terminal job message per job.
			terminal := 0
			for _, m := range r.got {
				if m.Kind == "job" && dataOf(m)["verdict"] != "RUNNING" {
					terminal++
				}
			}
			if terminal != 1 {
				t.Fatalf("%d terminal job messages", terminal)
			}
		})
	}
}

func TestServeJobVerdicts(t *testing.T) {
	one, zero, neg := 1, 0, -1
	for _, tc := range []struct {
		status string
		code   *int
		want   string
	}{{"running", nil, "RUNNING"}, {"exited", &zero, "OK"}, {"exited", &one, "FAIL"}, {"exited", nil, "GONE"}, {"exited", &neg, "KILLED"}, {"killed", nil, "KILLED"}} {
		if v, _ := jobVerdict(toolsJob(tc.status, tc.code)); v != tc.want {
			t.Errorf("%s %v -> %s, want %s", tc.status, tc.code, v, tc.want)
		}
	}
}

func TestServeTightenOnlyLowers(t *testing.T) {
	spec := engine.TaskSpec{Constraints: engine.TaskConstraints{MaxTurns: 8, MaxTotalTokens: 1000}}
	tighten(&spec, json.RawMessage(`{"max_turns": 20, "max_total_tokens": 50, "deadline_ms": 5000, "allow_commands": ["rm"], "tools": ["write_file"]}`))
	k := spec.Constraints
	if k.MaxTurns != 8 || k.MaxTotalTokens != 50 || k.DeadlineMs != 5000 || len(k.AllowCommands) != 0 || len(k.Tools) != 0 {
		t.Fatalf("%+v", k)
	}
}

func TestServeNotifyWakesWithoutPolling(t *testing.T) {
	useFakes(t)
	r := newRig(t)
	cfg := r.cfg()
	cfg.Poll = time.Hour
	r.start(cfg)
	r.await("started", state("started"))
	var sess filebus.Session
	for i := 0; i < 100; i++ {
		sess, _, _ = r.host.ReadSession(r.name)
		if sess.Notify.Unix != "" || i == 99 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sess.Notify.Unix == "" {
		t.Skip("no unix datagram notify on this host")
	}
	task := r.send("loom.task", "", map[string]any{"prompt": "wake me"})
	r.await("push-woken result", result(task.ID))
}

func TestServeHeartbeatAndUnregister(t *testing.T) {
	useFakes(t)
	r := newRig(t)
	r.start(r.cfg())
	r.await("started", state("started"))
	first, _, _ := r.host.ReadSession(r.name)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cur, _, _ := r.host.ReadSession(r.name); cur.Updated > first.Updated {
			r.cancel()
			if r.exit() != 130 {
				t.Fatal("exit code")
			}
			if s, _, _ := r.host.ReadSession(r.name); s.Status != "unregistered" {
				t.Fatalf("session after exit: %+v", s)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no heartbeat")
}

func TestServeSecondInstanceRefused(t *testing.T) {
	r := newRig(t)
	foreign := filebus.Session{V: 1, Name: "taken", Status: "live", PID: 1, Host: "elsewhere", TTLS: 300}
	if err := r.host.WriteSession(foreign); err != nil {
		t.Fatal(err)
	}
	cfg := r.cfg()
	cfg.Name = "taken"
	if rc := serveLoop(context.Background(), cfg); rc != 2 {
		t.Fatalf("rc=%d, want 2 for a name held by a live session", rc)
	}
}

func TestServeConfigErrorsExit2(t *testing.T) {
	r := newRig(t)
	base := r.cfg()
	for name, mut := range map[string]func(*serveConfig){
		"bad name":      func(c *serveConfig) { c.Name = "a/b" },
		"bad workspace": func(c *serveConfig) { c.Workspace = filepath.Join(r.dir, "nope") },
		"bad job mode":  func(c *serveConfig) { c.OnJobExit = "maybe" },
		"bad provider":  func(c *serveConfig) { c.ProviderName = "nonesuch" },
	} {
		cfg := base
		mut(&cfg)
		if rc := serveLoop(context.Background(), cfg); rc != 2 {
			t.Errorf("%s: rc=%d, want 2", name, rc)
		}
	}
	os.WriteFile(filepath.Join(r.dir, "VERSION"), []byte(`{"format":"nexus.filebus","version":9}`), 0o600)
	if rc := serveLoop(context.Background(), base); rc != 2 {
		t.Errorf("newer layout: rc=%d, want 2", rc)
	}
	if rc, _, errS := run(t, "", "serve"); rc != 2 || !strings.Contains(errS, "usage: loom serve") {
		t.Errorf("serve without flags: rc=%d err=%s", rc, errS)
	}
}

func TestCapabilitiesLoopMode(t *testing.T) {
	raw, _ := json.Marshal(capabilitiesMap())
	var m map[string]any
	json.Unmarshal(raw, &m)
	lm, _ := m["loop_mode"].(map[string]any)
	if lm["bus_format"] != "nexus.filebus" || lm["bus_version"] != float64(1) {
		t.Fatalf("loop_mode %v", lm)
	}
	kinds, _ := lm["kinds"].([]any)
	have := map[any]bool{}
	for _, k := range kinds {
		have[k] = true
	}
	for _, k := range []string{"loom.task", "loom.prompt", "loom.inject", "loom.extract", "loom.stop", "loom.resume", "loom.result", "loom.extracted", "loom.state", "job"} {
		if !have[k] {
			t.Errorf("kind %s not advertised", k)
		}
	}
	subs, _ := m["subcommands"].([]any)
	found := false
	for _, s := range subs {
		found = found || s == "serve"
	}
	if !found {
		t.Fatalf("serve not in subcommands: %v", subs)
	}
}

func TestServeTaskReadsLargeBodyViaBodyRef(t *testing.T) {
	fakes := useFakes(t)
	r := newRig(t)
	r.start(r.cfg())
	r.await("started", state("started"))

	big := strings.Repeat("x", 300*1024) + " MARKER-body-ref-7"
	task := r.send("loom.task", big, map[string]any{"prompt": ""})
	if task.BodyRef == nil || *task.BodyRef == "" {
		t.Fatalf("expected a body_ref for a %d-byte body", len(big))
	}
	res := r.await("task result", result(task.ID))
	// A 300 KiB prompt exceeds the default context budget, so the turn escalates instead of calling the
	// model; the point here is that the prompt was read from the blob, not rejected as empty.
	d := dataOf(res)
	if d["status"] == "harness_error" || strings.Contains(res.Body, "empty task prompt") {
		t.Fatalf("body_ref prompt was not read: %s", res.Data)
	}
	if esc, _ := d["escalation"].(map[string]any); d["status"] != "success" &&
		(esc == nil || !strings.HasPrefix(fmt.Sprint(esc["reason"]), "context_over_budget")) {
		t.Fatalf("unexpected result %s", res.Data)
	}
	_ = fakes
}
