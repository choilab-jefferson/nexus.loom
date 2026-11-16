package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
	"github.com/choilab-jefferson/nexus.loom/pkg/engine"
	"github.com/choilab-jefferson/nexus.loom/pkg/filebus"
	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
	"github.com/choilab-jefferson/nexus.loom/pkg/providers"
	"github.com/choilab-jefferson/nexus.loom/pkg/tools"
	"github.com/choilab-jefferson/nexus.loom/pkg/trace"
)

// Loop mode (`loom serve`): a long-lived, non-interactive loom controlled only through the nexus file bus
// (nexus.lite docs/specs/file-bus.md section 13). One persistent loom.context.v1, one user turn per
// loom.task, control messages handled only at turn boundaries.

// Kinds handled / emitted in loop mode (advertised by `capabilities --json`).
var (
	loopKindsIn  = []string{"loom.task", "loom.prompt", "loom.inject", "loom.extract", "loom.stop", "loom.resume"}
	loopKindsOut = []string{"loom.result", "loom.extracted", "loom.state", "job"}
)

const (
	maxInjectFileBytes = 256 * 1024
	jobWatchEvery      = 250 * time.Millisecond
	summaryOutputRunes = 500
)

type serveConfig struct {
	BusDir, Name string
	Workspace    string
	ProviderName string
	ModelName    string
	PHI          bool
	NoAgentsMD   bool
	// Base carries the budget, capability and allow-cmd flags; per-turn constraints may only tighten budgets.
	Base             engine.TaskSpec
	MaxContextTokens int
	MaxUserTurns     int           // --max-turns (0 = unlimited)
	MaxWall          time.Duration // --max-wall (0 = unlimited)
	IdleTimeout      time.Duration // --idle-timeout (0 = never)
	OnJobExit        string        // "notify" (default) or "turn"
	ReportTo         string        // target for events outside a request (default: last requester)
	Poll             time.Duration // inbox poll interval (default 1s)
	JobPoll          time.Duration // job watcher interval (default 250ms)
	TraceDir         string
	NotifyTCP        string
	TTLSeconds       int
	Heartbeat        time.Duration // 0 = ttl/3
	Stderr           io.Writer
}

type jobEvent struct{ id, verdict string }

type server struct {
	cfg    serveConfig
	bus    *filebus.Bus
	sess   filebus.Session
	prov   providers.Provider
	pol    *policy.PolicyConfig
	start  time.Time
	wallAt time.Time // zero = no wall budget

	ctxPath string
	ctx     *engine.Context // persistent conversation, nil until the first turn or inject

	mu         sync.Mutex // guards peer, pendingJobs, jobSeen
	peer       string
	pendingJob []jobEvent
	jobSeen    map[string]string

	turnsUsed    int
	lastActivity time.Time
	dirty        bool // a turn ran since the last idle announcement
	done         map[string]bool
	jobWake      chan struct{}
}

func (s *server) logf(format string, args ...any) {
	fmt.Fprintf(s.cfg.Stderr, "loom serve: "+format+"\n", args...)
}

// relBus returns p relative to the bus dir (slash form) when it lies inside, else p unchanged.
func (s *server) relBus(p string) string {
	if rel, err := filepath.Rel(s.bus.Dir, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return p
}

func (s *server) absBus(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(s.bus.Dir, filepath.FromSlash(p))
}

func (s *server) setPeer(t string) {
	if t == "" || t == s.cfg.Name || filebus.ValidName(t) != nil {
		return
	}
	s.mu.Lock()
	changed := s.peer != t
	s.peer = t
	s.mu.Unlock()
	if changed {
		_ = s.bus.StateUpdate(s.cfg.Name, map[string]any{"peer": t})
	}
}

func (s *server) getPeer() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peer
}

// emit sends one loom -> host message; target "" means the current peer (dropped when none is known).
func (s *server) emit(target, kind, body string, data any, corr string, extraMeta map[string]any) string {
	if target == "" {
		target = s.getPeer()
	}
	if target == "" {
		return ""
	}
	meta := map[string]any{"origin": "loom"}
	if corr != "" {
		meta["corr"] = corr
	}
	for k, v := range extraMeta {
		meta[k] = v
	}
	m, err := s.bus.Send(filebus.Outgoing{Sender: s.cfg.Name, Target: target, Kind: kind, Body: body, Data: data, Meta: meta})
	if err != nil {
		s.logf("send %s failed: %v", kind, err)
		return ""
	}
	return m.ID
}

func (s *server) emitState(target, state, reason string) {
	d := map[string]any{"state": state, "turns_used": s.turnsUsed, "wall_ms": time.Since(s.start).Milliseconds()}
	if reason != "" {
		d["reason"] = reason
	}
	s.emit(target, "loom.state", "", d, "", nil)
}

// controlError reports a failed control message as loom.result harness_error with a reason. It never ends the loop:
// the caller acks the message and keeps serving (only startup/config errors exit 2).
func (s *server) controlError(m filebus.Message, reason string) {
	s.emit(replyTarget(m), "loom.result", reason, map[string]any{"request": m.ID, "status": "harness_error",
		"exit_code": policy.ExitHarnessErr, "turns": 0, "usage": engine.Usage{}, "reason": reason}, m.ID, taskMeta(m))
}

func replyTarget(m filebus.Message) string {
	if t := m.MetaString("reply_to"); t != "" {
		return t
	}
	return m.Sender
}

func taskMeta(m filebus.Message) map[string]any {
	if t := m.MetaString("task_id"); t != "" {
		return map[string]any{"task_id": t}
	}
	return nil
}

// serveLoop runs loop mode until stop, a budget, idle timeout, a signal (ctx cancel) or a fatal error.
// It returns the process exit code (docs/contract.md section 1).
func serveLoop(ctx context.Context, cfg serveConfig) int {
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	if cfg.Poll <= 0 {
		cfg.Poll = time.Second
	}
	if cfg.JobPoll <= 0 {
		cfg.JobPoll = jobWatchEvery
	}
	if cfg.OnJobExit == "" {
		cfg.OnJobExit = "notify"
	}
	s := &server{cfg: cfg, start: time.Now(), done: map[string]bool{}, jobSeen: map[string]string{}, jobWake: make(chan struct{}, 1)}
	s.lastActivity = s.start
	fail := func(format string, args ...any) int {
		fmt.Fprintf(cfg.Stderr, "loom serve error: "+format+"\n", args...)
		return policy.ExitHarnessErr
	}
	if err := filebus.ValidName(cfg.Name); err != nil {
		return fail("--name: %v", err)
	}
	if cfg.OnJobExit != "notify" && cfg.OnJobExit != "turn" {
		return fail("--on-job-exit must be notify or turn")
	}
	if cfg.ReportTo != "" {
		if err := filebus.ValidName(cfg.ReportTo); err != nil && cfg.ReportTo != filebus.Broadcast {
			return fail("--report-to: %v", err)
		}
	}
	if fi, err := os.Stat(cfg.Workspace); err != nil || !fi.IsDir() {
		return fail("invalid --workspace %q: not an accessible directory", cfg.Workspace)
	}
	bus, err := filebus.Open(cfg.BusDir)
	if err != nil {
		return fail("%v", err)
	}
	bus.Logf = func(f string, a ...any) { s.logf(f, a...) }
	s.bus = bus
	s.pol = policy.DefaultPolicyConfig(cfg.Workspace)
	s.pol.PHIMode = cfg.PHI || strings.EqualFold(os.Getenv("NEXUS_SENSITIVITY"), "phi")
	prov, err := buildProvider(cfg.ProviderName, cfg.ModelName, s.pol.PHIMode)
	if err != nil {
		return fail("provider: %v", err)
	}
	s.prov = prov
	if cfg.TraceDir == "" {
		cfg.TraceDir = filepath.Join(bus.Dir, "state", cfg.Name+".traces")
		s.cfg.TraceDir = cfg.TraceDir
	}
	if err := os.MkdirAll(cfg.TraceDir, 0o700); err != nil {
		return fail("trace dir: %v", err)
	}
	s.ctxPath = filepath.Join(bus.Dir, "state", cfg.Name+".context.json")
	if raw, err := os.ReadFile(s.ctxPath); err == nil {
		c, notes, perr := engine.ParseContext(raw)
		if perr != nil {
			return fail("persisted context: %v", perr)
		}
		for _, n := range notes {
			s.logf("context repair: %s", n)
		}
		s.ctx = c
	}
	st, err := bus.ReadState(cfg.Name)
	if err != nil {
		return fail("%v", err)
	}
	s.peer = cfg.ReportTo
	if s.peer == "" {
		s.peer, _ = st["peer"].(string)
	}

	notifier, err := bus.StartNotify(cfg.Name, cfg.NotifyTCP)
	if err != nil {
		return fail("notify: %v", err)
	}
	defer notifier.Close()
	ttl := cfg.TTLSeconds
	if ttl <= 0 {
		ttl = filebus.LiveTTLSeconds
	}
	sess, err := bus.Register(cfg.Name, notifier.Info, map[string]any{"kind": "loom", "loop_mode": 1}, ttl)
	if err != nil {
		return fail("%v", err)
	}
	s.sess = sess
	s.logf("registered %s on %s", cfg.Name, bus.Dir)

	if cfg.MaxWall > 0 {
		s.wallAt = s.start.Add(cfg.MaxWall)
	}
	hbCtx, stopHB := context.WithCancel(ctx)
	var bg sync.WaitGroup
	bg.Add(2)
	go func() { defer bg.Done(); s.heartbeat(hbCtx, ttl) }()
	go func() { defer bg.Done(); s.watchJobs(hbCtx) }()
	defer func() {
		stopHB()
		bg.Wait()
		_ = bus.Unregister(sess)
	}()

	s.emitState("", "started", "")
	s.recoverInflight(st)
	return s.run(ctx, notifier)
}

func (s *server) heartbeat(ctx context.Context, ttl int) {
	every := s.cfg.Heartbeat
	if every <= 0 {
		every = time.Duration(ttl) * time.Second / 3
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.bus.Heartbeat(s.sess); err != nil {
				s.logf("heartbeat failed: %v", err)
			}
		}
	}
}

// finish emits the terminal loom.state and returns code.
func (s *server) finish(state, reason string, code int) int {
	s.emitState("", state, reason)
	return code
}

func (s *server) budgetExhausted(reason string) int {
	return s.finish("exiting", reason, policy.ExitBudget)
}

// run is the boundary loop: drain the inbox, then block on a wake-up, poll tick or deadline.
func (s *server) run(ctx context.Context, n *filebus.Notifier) int {
	for {
		if code, exit := s.drain(ctx); exit {
			return code
		}
		if ctx.Err() != nil {
			return s.finish("exiting", "signal", policy.ExitInterrupted)
		}
		if code, exit := s.runJobTurns(ctx); exit {
			return code
		}
		if s.dirty {
			s.dirty = false
			s.emitState("", "idle", "")
		}
		now := time.Now()
		if !s.wallAt.IsZero() && !now.Before(s.wallAt) {
			return s.budgetExhausted("budget_exhausted")
		}
		if s.cfg.IdleTimeout > 0 && now.Sub(s.lastActivity) >= s.cfg.IdleTimeout {
			return s.finish("exiting", "idle_timeout", policy.ExitOK)
		}
		wait := s.pollInterval(now)
		if !s.wallAt.IsZero() {
			wait = minDur(wait, time.Until(s.wallAt))
		}
		if s.cfg.IdleTimeout > 0 {
			wait = minDur(wait, s.lastActivity.Add(s.cfg.IdleTimeout).Sub(now))
		}
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-n.Wake:
		case <-s.jobWake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func minDur(a, b time.Duration) time.Duration {
	if b < a {
		return b
	}
	return a
}

// pollInterval is the configured poll interval, backing off while idle (adaptive watch: +1x per idle minute, cap 10s).
func (s *server) pollInterval(now time.Time) time.Duration {
	idle := now.Sub(s.lastActivity)
	d := s.cfg.Poll * time.Duration(1+int(idle/time.Minute))
	if limit := 10 * time.Second; d > limit && s.cfg.Poll < limit {
		d = limit
	}
	return d
}

// drain handles every unacked message in id order; it returns (code, true) when the loop must end.
func (s *server) drain(ctx context.Context) (int, bool) {
	for ctx.Err() == nil {
		msgs, err := s.bus.Unacked(s.cfg.Name, filebus.DefaultConsumer, 64)
		if err != nil {
			s.logf("inbox read: %v", err)
			return 0, false
		}
		progressed := false
		for _, m := range msgs {
			if len(s.done) > 10000 {
				s.done = map[string]bool{}
			}
			if s.done[m.ID] {
				s.ack(m.ID)
				continue
			}
			progressed = true
			s.lastActivity = time.Now()
			if code, exit := s.handle(ctx, m); exit {
				return code, true
			}
			s.done[m.ID] = true
			if ctx.Err() != nil {
				return 0, false
			}
		}
		if !progressed {
			return 0, false
		}
	}
	return 0, false
}

func (s *server) ack(ids ...string) {
	if err := s.bus.Ack(s.cfg.Name, filebus.DefaultConsumer, ids...); err != nil {
		s.logf("ack failed: %v", err)
	}
}

func (s *server) handle(ctx context.Context, m filebus.Message) (int, bool) {
	if m.Sender == s.cfg.Name {
		s.ack(m.ID)
		return 0, false
	}
	switch m.Kind {
	case "loom.task", "loom.prompt":
		return s.handleTask(ctx, m)
	case "loom.inject":
		s.setPeer(replyTarget(m))
		s.handleInject(m)
		s.ack(m.ID)
	case "loom.extract":
		s.setPeer(replyTarget(m))
		s.handleExtract(m)
		s.ack(m.ID)
	case "loom.resume":
		return s.handleResume(ctx, m)
	case "loom.stop":
		s.setPeer(replyTarget(m))
		s.ack(m.ID)
		s.done[m.ID] = true
		return s.finish("stopped", stopReasonOf(m), policy.ExitOK), true
	default: // unknown kinds are ignored but acked: loom is their only consumer on its own inbox
		s.ack(m.ID)
	}
	return 0, false
}

func stopReasonOf(m filebus.Message) string {
	var d struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(m.Data, &d)
	return d.Reason
}

// ---- turns ----

type taskData struct {
	Prompt       string          `json:"prompt"`
	ContextFiles []string        `json:"context_files"`
	Constraints  json.RawMessage `json:"constraints"`
}

func (s *server) handleTask(ctx context.Context, m filebus.Message) (int, bool) {
	var d taskData
	_ = json.Unmarshal(m.Data, &d)
	if strings.TrimSpace(d.Prompt) == "" {
		body, err := s.bus.Body(m) // inline body, or the blob named by body_ref (> 256 KiB)
		if err != nil {
			s.setPeer(replyTarget(m))
			s.controlError(m, "task body: "+err.Error())
			s.ack(m.ID)
			return 0, false
		}
		d.Prompt = body
	}
	s.setPeer(replyTarget(m))
	if strings.TrimSpace(d.Prompt) == "" {
		s.controlError(m, "empty task prompt")
		s.ack(m.ID)
		return 0, false
	}
	if s.cfg.MaxUserTurns > 0 && s.turnsUsed >= s.cfg.MaxUserTurns {
		return s.budgetExhausted("budget_exhausted"), true // the task stays unacked for a later loom
	}
	return s.runAndReply(ctx, m.ID, replyTarget(m), taskMeta(m), false, func(tc *turnCall) {
		tc.instruction = d.Prompt
		tc.seedFiles = d.ContextFiles
		tc.constraints = d.Constraints
	})
}

type turnCall struct {
	instruction string
	seedFiles   []string
	constraints json.RawMessage
	resume      *ckptFile
	trace       string
}

// runAndReply brackets one turn with the inflight marker, emits loom.result and acks the request.
// synthetic turns (job exits) have no inbox message to ack.
func (s *server) runAndReply(ctx context.Context, reqID, target string, meta map[string]any, synthetic bool, prep func(*turnCall)) (int, bool) {
	tc := &turnCall{}
	prep(tc)
	tc.trace = tc.traceOr(s, reqID)
	_ = s.bus.StateUpdate(s.cfg.Name, map[string]any{"inflight": reqID, "started": float64(time.Now().UnixMilli()) / 1000,
		"reply_to": target, "trace": s.relBus(tc.trace), "synthetic": synthetic})
	s.emitState("", "busy", "")
	res := s.runTurn(ctx, reqID, tc)
	s.turnsUsed++
	s.dirty = true
	s.lastActivity = time.Now()
	data := map[string]any{"request": reqID, "status": res.Status, "exit_code": res.ExitCode, "turns": res.Turns,
		"user_turn": res.UserTurn, "usage": res.Usage, "trace": s.relBus(tc.trace), "run_id": res.RunID}
	if res.ContextPath != "" {
		data["context_path"] = s.relBus(res.ContextPath)
	}
	if res.Escalation != nil {
		data["escalation"] = res.Escalation
	}
	if s.emit(target, "loom.result", res.Output, data, reqID, meta) == "" && target == "" && s.getPeer() == "" {
		s.logf("turn %s finished with status %s (no peer to report to)", reqID, res.Status)
	}
	if !synthetic {
		s.ack(reqID)
	}
	_ = s.bus.StateUpdate(s.cfg.Name, map[string]any{"last_trace": s.relBus(tc.trace)}, "inflight", "started", "reply_to", "trace", "synthetic")
	s.done[reqID] = true
	switch {
	case ctx.Err() != nil:
		return s.finish("exiting", "signal", policy.ExitInterrupted), true
	case !s.wallAt.IsZero() && !time.Now().Before(s.wallAt) && res.ExitCode == policy.ExitBudget:
		return s.budgetExhausted("budget_exhausted"), true
	}
	return 0, false
}

func (tc *turnCall) traceOr(s *server, reqID string) string {
	if tc.trace != "" {
		return tc.trace
	}
	return filepath.Join(s.cfg.TraceDir, reqID+".jsonl")
}

// lowerInt returns the stricter of a base budget and a requested one (0 = unset).
func lowerInt(base, req int) int {
	switch {
	case req <= 0:
		return base
	case base <= 0 || req < base:
		return req
	}
	return base
}

// tighten applies per-task constraints. Only budgets can change, and only downward; every other key is ignored
// so a bus message cannot widen what the operator allowed on the command line.
func tighten(spec *engine.TaskSpec, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var c engine.TaskConstraints
	if json.Unmarshal(raw, &c) != nil {
		return
	}
	k := &spec.Constraints
	k.MaxTurns = lowerInt(k.MaxTurns, c.MaxTurns)
	k.DeadlineMs = lowerInt(k.DeadlineMs, c.DeadlineMs)
	k.MaxTotalTokens = lowerInt(k.MaxTotalTokens, c.MaxTotalTokens)
	k.MaxOutputTokens = lowerInt(k.MaxOutputTokens, c.MaxOutputTokens)
	k.MaxToolCallsPerTurn = lowerInt(k.MaxToolCallsPerTurn, c.MaxToolCallsPerTurn)
}

// runTurn runs one engine invocation (a fresh user turn, or a resumed one) on the persistent context.
func (s *server) runTurn(ctx context.Context, reqID string, tc *turnCall) engine.TaskResult {
	spec := s.cfg.Base
	spec.TaskID, spec.Agent = reqID, "worker"
	spec.Instruction, spec.SeedFiles = tc.instruction, tc.seedFiles
	spec.MaxContextTokens = s.cfg.MaxContextTokens
	var resume *engine.Checkpoint
	meta := runMeta{Workspace: s.cfg.Workspace, Provider: s.cfg.ProviderName, Model: s.cfg.ModelName, PHI: s.pol.PHIMode, Trace: tc.trace}
	if tc.resume != nil {
		spec = tc.resume.Checkpoint.Spec
		s.applyBase(&spec)
		resume = &tc.resume.Checkpoint
		meta.Prior = tc.resume.Meta.Prior
	} else {
		tighten(&spec, tc.constraints)
		if s.ctx != nil {
			p := *s.ctx
			p.Messages = nil
			meta.Prior = &p
		}
	}
	pol := policy.DefaultPolicyConfig(s.cfg.Workspace)
	pol.PHIMode = s.pol.PHIMode
	applyPolicyExtras(spec, pol)
	if !s.cfg.NoAgentsMD && os.Getenv("LOOM_NO_AGENTS_MD") != "1" && strings.TrimSpace(spec.RepoSummary) == "" {
		if _, content := engine.DiscoverAgentsMD(s.cfg.Workspace); content != "" {
			spec.RepoSummary = content
		}
	}
	tctx := ctx
	if !s.wallAt.IsZero() {
		var cancel context.CancelFunc
		tctx, cancel = context.WithDeadline(ctx, s.wallAt)
		defer cancel()
	}
	tw, err := trace.NewFileWriter(tc.trace)
	if err != nil {
		return engine.TaskResult{ExitCode: policy.ExitHarnessErr, Status: "harness_error", Output: fmt.Sprintf("trace: %v", err)}
	}
	defer tw.Close()

	opts := engine.RunOptions{Resume: resume, TracePath: tc.trace, OnCheckpoint: func(cp engine.Checkpoint) { writeCkpt(meta, cp) }}
	if resume == nil {
		opts.Context = s.ctx
	} else if meta.Prior != nil {
		opts.PriorUsage, opts.PriorUserTurn = meta.Prior.CumulativeUsage, meta.Prior.UserTurn
	}
	res := engine.RunTaskWithOptions(tctx, spec, pol, s.prov, tw, opts)
	if res.Status == "success" {
		os.Remove(ckptPath(tc.trace))
	}
	if len(res.Msgs) > 0 {
		base := s.ctx
		if resume != nil {
			base = meta.Prior
		}
		next := engine.BuildContext(base, res.Msgs, res)
		if err := writeContext(s.ctxPath, next); err != nil {
			s.logf("context write: %v", err)
		} else {
			s.ctx = next
			res.ContextPath = s.ctxPath
		}
	}
	return res
}

// applyBase re-applies the serve-level budgets to a resumed spec (they can only tighten it).
func (s *server) applyBase(spec *engine.TaskSpec) {
	b := s.cfg.Base.Constraints
	k := &spec.Constraints
	k.MaxTotalTokens = lowerInt(k.MaxTotalTokens, b.MaxTotalTokens)
	k.DeadlineMs = lowerInt(k.DeadlineMs, b.DeadlineMs)
	k.MaxToolCallsPerTurn = lowerInt(k.MaxToolCallsPerTurn, b.MaxToolCallsPerTurn)
	k.MaxOutputTokens = lowerInt(k.MaxOutputTokens, b.MaxOutputTokens)
}

// recoverInflight turns a turn that was running when loom died into an interrupted result (spec 13.1).
func (s *server) recoverInflight(st map[string]any) {
	id, _ := st["inflight"].(string)
	if id == "" {
		return
	}
	target, _ := st["reply_to"].(string)
	trace, _ := st["trace"].(string)
	synthetic, _ := st["synthetic"].(bool)
	data := map[string]any{"request": id, "status": "interrupted", "exit_code": policy.ExitInterrupted, "turns": 0,
		"usage": engine.Usage{}, "trace": trace}
	if s.ctx != nil {
		data["context_path"] = s.relBus(s.ctxPath)
	}
	s.emit(target, "loom.result", "turn interrupted: loom restarted while it was running; send loom.resume to continue from the checkpoint", data, id, nil)
	if !synthetic {
		s.ack(id)
	}
	s.done[id] = true
	_ = s.bus.StateUpdate(s.cfg.Name, map[string]any{"last_trace": trace}, "inflight", "started", "reply_to", "trace", "synthetic")
}

// handleResume continues the interrupted or paused turn from its checkpoint (`loom resume` semantics).
func (s *server) handleResume(ctx context.Context, m filebus.Message) (int, bool) {
	s.setPeer(replyTarget(m))
	var d struct {
		Checkpoint string `json:"checkpoint"`
	}
	_ = json.Unmarshal(m.Data, &d)
	fail := func(msg string) (int, bool) {
		s.controlError(m, msg)
		s.ack(m.ID)
		return 0, false
	}
	if s.cfg.MaxUserTurns > 0 && s.turnsUsed >= s.cfg.MaxUserTurns {
		return s.budgetExhausted("budget_exhausted"), true
	}
	path := d.Checkpoint
	if path == "" {
		st, _ := s.bus.ReadState(s.cfg.Name)
		path, _ = st["last_trace"].(string)
	}
	if path == "" {
		return fail("no checkpoint to resume")
	}
	trace := strings.TrimSuffix(s.absBus(path), ".ckpt")
	raw, err := os.ReadFile(ckptPath(trace))
	if err != nil {
		return fail("no checkpoint for the requested turn")
	}
	var cf ckptFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		return fail("bad checkpoint")
	}
	cf.Meta.Trace = trace
	return s.runAndReply(ctx, m.ID, replyTarget(m), taskMeta(m), false, func(tc *turnCall) {
		tc.resume, tc.trace = &cf, trace
	})
}

// ---- inject / extract ----

func (s *server) ensureContext() {
	if s.ctx == nil {
		s.ctx = &engine.Context{SchemaVersion: engine.ContextSchema, Runs: []engine.RunRecord{}}
	}
}

func (s *server) handleInject(m filebus.Message) {
	var d struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(m.Data, &d); err != nil {
		s.logf("inject %s: bad data", m.ID)
		return
	}
	var add []compact.Message
	for _, im := range d.Messages {
		if (im.Role == "user" || im.Role == "assistant") && strings.TrimSpace(im.Content) != "" {
			add = append(add, compact.Message{Role: im.Role, Content: im.Content})
		}
	}
	for _, rel := range d.Files {
		if filepath.IsAbs(rel) {
			s.logf("inject %s: refused absolute file path", m.ID)
			continue
		}
		p, err := policy.ResolveScopedPath(s.cfg.Workspace, rel, false, s.pol)
		if err != nil {
			s.logf("inject %s: file refused: %v", m.ID, err)
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			s.logf("inject %s: file unreadable", m.ID)
			continue
		}
		note := ""
		if len(raw) > maxInjectFileBytes {
			raw, note = raw[:maxInjectFileBytes], "\n[loom: truncated at 262144 bytes]"
		}
		add = append(add, compact.Message{Role: "user", Content: fmt.Sprintf("File %s:\n%s%s", rel, raw, note)})
	}
	if len(add) == 0 {
		return
	}
	s.ensureContext()
	msgs, _ := engine.RepairMessages(append(append([]compact.Message(nil), s.ctx.Messages...), add...))
	next := *s.ctx
	next.Messages = msgs
	if err := writeContext(s.ctxPath, &next); err != nil {
		s.logf("inject %s: %v", m.ID, err)
		return
	}
	s.ctx = &next
}

func (s *server) handleExtract(m filebus.Message) {
	var d struct {
		What string `json:"what"`
	}
	_ = json.Unmarshal(m.Data, &d)
	target := replyTarget(m)
	var payload []byte
	var schema string
	switch d.What {
	case "context", "":
		s.ensureContext()
		payload, _ = json.Marshal(s.ctx)
		schema = engine.ContextSchema
	case "summary":
		payload, _ = json.Marshal(map[string]any{"schema_version": "loom.summary.v1", "summary": s.summary()})
		schema = "loom.summary.v1"
	default:
		s.controlError(m, "unknown extract target "+d.What)
		return
	}
	path, _, err := s.bus.WriteBlob(target, "json", append(payload, '\n'))
	if err != nil {
		s.logf("extract %s: %v", m.ID, err)
		return
	}
	sum := sha256.Sum256(append(payload, '\n'))
	s.emit(target, "loom.extracted", "", map[string]any{"request": m.ID, "path": path,
		"sha256": hex.EncodeToString(sum[:]), "schema": schema}, m.ID, taskMeta(m))
}

// summary is a deterministic digest of the conversation (no model call): counters, per-turn status and the
// last assistant text.
func (s *server) summary() string {
	if s.ctx == nil || len(s.ctx.Runs) == 0 {
		return "no user turns yet"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "user_turns=%d messages=%d total_tokens=%d compactions=%d\n", s.ctx.UserTurn, len(s.ctx.Messages),
		s.ctx.CumulativeUsage.TotalTokens, s.ctx.Compactions)
	for _, r := range s.ctx.Runs {
		fmt.Fprintf(&b, "turn %d: %s exit=%d agent_turns=%d\n", r.RunIndex, r.Status, r.ExitCode, r.Turns)
	}
	for i := len(s.ctx.Messages) - 1; i >= 0; i-- {
		if m := s.ctx.Messages[i]; m.Role == "assistant" && strings.TrimSpace(m.Content) != "" {
			r := []rune(strings.TrimSpace(m.Content))
			if len(r) > summaryOutputRunes {
				r = append(r[:summaryOutputRunes], '…')
			}
			b.WriteString("last: " + string(r))
			break
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---- jobs ----

func jobVerdict(j tools.JobInfo) (verdict string, rc *int) {
	switch {
	case j.Status == "running":
		return "RUNNING", nil
	case j.Status == "killed":
		return "KILLED", nil
	case j.ExitCode == nil:
		return "GONE", nil
	case *j.ExitCode == -1: // killed by a signal
		return "KILLED", nil
	case *j.ExitCode == 0:
		return "OK", j.ExitCode
	}
	return "FAIL", j.ExitCode
}

// watchJobs reports .loom/jobs state changes as shared `job` messages (spec 13.6) and wakes the loop on exit.
func (s *server) watchJobs(ctx context.Context) {
	first := true
	t := time.NewTicker(s.cfg.JobPoll)
	defer t.Stop()
	for {
		jobs, _ := tools.ListJobs(s.pol)
		for _, j := range jobs {
			verdict, rc := jobVerdict(j)
			s.mu.Lock()
			prev, seen := s.jobSeen[j.JobID]
			s.jobSeen[j.JobID] = verdict
			if seen && (prev != "RUNNING" || verdict == "RUNNING") {
				s.jobSeen[j.JobID] = prev
				s.mu.Unlock()
				continue
			}
			s.mu.Unlock()
			if !seen && first {
				continue // present at startup: finished earlier, or still running from another process (tracked silently)
			}
			if !seen {
				s.emitJob(j.JobID, "RUNNING", nil)
				if verdict == "RUNNING" {
					continue
				}
			}
			s.emitJob(j.JobID, verdict, rc) // terminal transition
			if s.cfg.OnJobExit == "turn" {
				s.mu.Lock()
				s.pendingJob = append(s.pendingJob, jobEvent{j.JobID, verdict})
				s.mu.Unlock()
			}
			select {
			case s.jobWake <- struct{}{}:
			default:
			}
		}
		first = false
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *server) emitJob(id, verdict string, rc *int) {
	var rcv any
	rcText := "null"
	if rc != nil {
		rcv, rcText = *rc, fmt.Sprint(*rc)
	}
	data := map[string]any{"name": id, "verdict": verdict, "rc": rcv, "run_dir": tools.JobRecordPath(id),
		"ts": float64(time.Now().UnixMilli()) / 1000, "supervisor": "loom", "job_id": id}
	target := s.getPeer()
	if target == "" {
		return
	}
	s.bus.Send(filebus.Outgoing{Sender: s.cfg.Name, Target: target, Kind: "job", Summary: fmt.Sprintf("%s %s rc=%s", id, verdict, rcText),
		Data: data, Meta: map[string]any{"origin": "loom"}})
}

// runJobTurns starts one synthetic user turn per pending job exit (--on-job-exit turn), counted against --max-turns.
func (s *server) runJobTurns(ctx context.Context) (int, bool) {
	for ctx.Err() == nil {
		s.mu.Lock()
		if len(s.pendingJob) == 0 {
			s.mu.Unlock()
			return 0, false
		}
		ev := s.pendingJob[0]
		s.pendingJob = s.pendingJob[1:]
		s.mu.Unlock()
		if s.cfg.MaxUserTurns > 0 && s.turnsUsed >= s.cfg.MaxUserTurns {
			return s.budgetExhausted("budget_exhausted"), true
		}
		reqID := "job-" + ev.id + "-" + ev.verdict
		prompt := fmt.Sprintf("Background job %s finished with status %s. Inspect it with job_status and job_log and decide the next step.", ev.id, ev.verdict)
		code, exit := s.runAndReply(ctx, reqID, s.getPeer(), nil, true, func(tc *turnCall) { tc.instruction = prompt })
		if exit {
			return code, true
		}
	}
	return 0, false
}

// ---- command line ----

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	busDir := fs.String("bus-dir", os.Getenv("NEXUS_BUS_DIR"), "nexus file-bus directory (env NEXUS_BUS_DIR)")
	name := fs.String("name", "", "session name; hosts send to msg/<name>/ (required)")
	workspace := fs.String("workspace", ".", "workspace directory")
	providerName := fs.String("provider", "auto", "model provider (auto|ollama|openai|anthropic|gemini|google-agent-platform|typesafe|mock)")
	modelName := fs.String("model", "", "model identifier")
	maxAgentTurns := fs.Int("max-agent-turns", 8, "maximum agent turns inside one user turn")
	maxContextTokens := fs.Int("max-context-tokens", 28000, "pre-send compaction token ceiling")
	maxTurns := fs.Int("max-turns", 0, "maximum user turns for the whole serve session (0 = unlimited); exhaustion exits 4")
	maxWall := fs.Duration("max-wall", 0, "wall-clock budget for the serve session, e.g. 2h (0 = none); exhaustion exits 4")
	idle := fs.Duration("idle-timeout", 0, "exit 0 after this long without a message (0 = never)")
	onJobExit := fs.String("on-job-exit", "notify", "notify: emit a job message; turn: also start one synthetic user turn")
	reportTo := fs.String("report-to", "", "target for events outside a request (default: the last requester)")
	poll := fs.Duration("poll-interval", time.Second, "inbox poll interval (the wake-up layer makes it a fallback)")
	traceDir := fs.String("trace-dir", "", "directory for per-turn traces and checkpoints (default <bus>/state/<name>.traces)")
	notifyTCP := fs.String("notify-tcp", "", "advertise a loopback TCP wake-up endpoint host:port (loopback only)")
	noAgentsMD := fs.Bool("no-agents-md", false, "disable automatic AGENTS.md loading into prefix")
	phiFlag := fs.Bool("phi", false, "enforce PHI local-only model confinement")
	bf := addBudgetFlags(fs)
	capf := addCapabilityFlags(fs)
	var allowCmds multiStringFlag
	fs.Var(&allowCmds, "allow-cmd", "allowlisted command prefix (repeatable)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return policy.ExitOK
		}
		fmt.Fprintf(os.Stderr, "loom serve flag error: %v\n", err)
		return policy.ExitHarnessErr
	}
	if fs.NArg() > 0 || strings.TrimSpace(*busDir) == "" || strings.TrimSpace(*name) == "" {
		fmt.Fprintln(os.Stderr, "usage: loom serve --bus-dir DIR --name ID [run flags] [--max-turns N] [--max-wall D] [--idle-timeout D] [--on-job-exit notify|turn]")
		return policy.ExitHarnessErr
	}
	base := engine.TaskSpec{SchemaVersion: 1, Agent: "worker",
		Constraints: engine.TaskConstraints{MaxTurns: *maxAgentTurns, AllowCommands: parseAllowCmds(allowCmds)}}
	bf.apply(&base)
	capf.apply(&base)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveLoop(ctx, serveConfig{
		BusDir: *busDir, Name: *name, Workspace: *workspace, ProviderName: *providerName, ModelName: *modelName,
		PHI: *phiFlag, NoAgentsMD: *noAgentsMD, Base: base, MaxContextTokens: *maxContextTokens,
		MaxUserTurns: *maxTurns, MaxWall: *maxWall, IdleTimeout: *idle, OnJobExit: *onJobExit, ReportTo: *reportTo,
		Poll: *poll, TraceDir: *traceDir, NotifyTCP: *notifyTCP,
	})
}
