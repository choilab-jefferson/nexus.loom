package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
	"github.com/choilab-jefferson/nexus.loom/pkg/providers"
	"github.com/choilab-jefferson/nexus.loom/pkg/trace"
)

func traceEvents(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad trace line %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// assertSingleDone checks the trace ends with exactly one done event carrying status and run_id.
func assertSingleDone(t *testing.T, buf *bytes.Buffer, status string) map[string]any {
	t.Helper()
	evs := traceEvents(t, buf)
	n := 0
	for _, ev := range evs {
		if ev["t"] == "done" {
			n++
		}
	}
	last := evs[len(evs)-1]
	if n != 1 || last["t"] != "done" {
		t.Fatalf("want exactly one trailing done event, got %d (last=%v)", n, last)
	}
	if last["status"] != status {
		t.Fatalf("done status = %v, want %s", last["status"], status)
	}
	if id, _ := last["run_id"].(string); id == "" {
		t.Fatalf("done missing run_id: %v", last)
	}
	return last
}

func toolTurn(n int) providers.Response {
	r := providers.Response{RawStopReason: "stop", PromptTokens: 10, OutputTokens: 5}
	for i := 0; i < n; i++ {
		r.ToolCalls = append(r.ToolCalls, compact.ToolCall{ID: "c", Name: "list_dir", Arguments: map[string]any{"path": "."}})
	}
	return r
}

func TestTokenBudgetExhausted(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	mock := providers.NewScriptedMockProvider(toolTurn(1), toolTurn(1), toolTurn(1))
	var buf bytes.Buffer
	spec := TaskSpec{TaskID: "b", Instruction: "x", Constraints: TaskConstraints{MaxTurns: 8, MaxTotalTokens: 40}}
	res := RunTask(context.Background(), spec, pol, mock, trace.NewWriter(&buf))
	if res.Status != "budget_exhausted" || res.ExitCode != policy.ExitBudget {
		t.Fatalf("got %s/%d", res.Status, res.ExitCode)
	}
	if res.Usage.TotalTokens != 45 || res.Usage.CompletionTokens != 15 {
		t.Fatalf("usage = %+v", res.Usage)
	}
	done := assertSingleDone(t, &buf, "budget_exhausted")
	if done["reason"] != "max_total_tokens" {
		t.Fatalf("reason = %v", done["reason"])
	}
	if done["usage"].(map[string]any)["total_tokens"].(float64) != 45 {
		t.Fatalf("usage not in trace: %v", done["usage"])
	}
}

func TestToolCallCapRejected(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	mock := providers.NewScriptedMockProvider(toolTurn(3))
	var buf bytes.Buffer
	spec := TaskSpec{TaskID: "b", Instruction: "x", Constraints: TaskConstraints{MaxToolCallsPerTurn: 2}}
	res := RunTask(context.Background(), spec, pol, mock, trace.NewWriter(&buf))
	if res.Status != "budget_exhausted" || res.ExitCode != policy.ExitBudget {
		t.Fatalf("got %s/%d", res.Status, res.ExitCode)
	}
	for _, ev := range traceEvents(t, &buf) {
		if ev["t"] == "tool_call" {
			t.Fatal("rejected turn must not execute tools")
		}
	}
	if d := assertSingleDone(t, &buf, "budget_exhausted"); d["reason"] != "max_tool_calls_per_turn" {
		t.Fatalf("reason = %v", d["reason"])
	}
}

type blockingProvider struct{}

func (blockingProvider) Chat(ctx context.Context, _ compact.PrefixHeader, _ []compact.Message, _ int) (providers.Response, error) {
	<-ctx.Done()
	return providers.Response{}, ctx.Err()
}

func TestDeadlineMidRun(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	var buf bytes.Buffer
	spec := TaskSpec{TaskID: "d", Instruction: "x", Constraints: TaskConstraints{DeadlineMs: 30}}
	res := RunTask(context.Background(), spec, pol, blockingProvider{}, trace.NewWriter(&buf))
	if res.Status != "budget_exhausted" || res.ExitCode != policy.ExitBudget {
		t.Fatalf("got %s/%d", res.Status, res.ExitCode)
	}
	if d := assertSingleDone(t, &buf, "budget_exhausted"); d["reason"] != "deadline" {
		t.Fatalf("reason = %v", d["reason"])
	}
}

func TestCancelEmitsInterruptedDone(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	var buf bytes.Buffer
	res := RunTask(ctx, TaskSpec{TaskID: "s", Instruction: "x"}, pol, blockingProvider{}, trace.NewWriter(&buf))
	if res.Status != "interrupted" || res.ExitCode != policy.ExitInterrupted {
		t.Fatalf("got %s/%d", res.Status, res.ExitCode)
	}
	assertSingleDone(t, &buf, "interrupted")
}

type errProvider struct{}

func (errProvider) Chat(context.Context, compact.PrefixHeader, []compact.Message, int) (providers.Response, error) {
	return providers.Response{}, errors.New("boom")
}

func TestDoneOnEveryExitPath(t *testing.T) {
	ws := t.TempDir()
	cases := []struct {
		name   string
		prov   providers.Provider
		spec   TaskSpec
		status string
	}{
		{"provider_error", errProvider{}, TaskSpec{Instruction: "x"}, "provider_error"},
		{"model_stop_error", providers.NewScriptedMockProvider(providers.Response{RawStopReason: "error"}), TaskSpec{Instruction: "x"}, "model_stop_error"},
		{"unknown_stop", providers.NewScriptedMockProvider(
			providers.Response{RawStopReason: "weird"}, providers.Response{RawStopReason: "weird"}), TaskSpec{Instruction: "x"}, "unknown_stop_reason"},
		{"escalation", providers.NewScriptedMockProvider(providers.Response{RawStopReason: "stop", ToolCalls: []compact.ToolCall{
			{ID: "e", Name: "escalate", Arguments: map[string]any{"reason": "r", "minimal_context": "c"}}}}), TaskSpec{Instruction: "x"}, "escalation_required"},
		{"max_turns", providers.NewScriptedMockProvider(toolTurn(1), toolTurn(1)), TaskSpec{Instruction: "x", Constraints: TaskConstraints{MaxTurns: 2}}, "max_turns_exhausted"},
		{"empty_instruction", providers.NewScriptedMockProvider(), TaskSpec{}, "harness_error"},
		{"success", providers.NewScriptedMockProvider(providers.Response{Content: "ok", RawStopReason: "stop"}), TaskSpec{Instruction: "x"}, "success"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			RunTask(context.Background(), c.spec, policy.DefaultPolicyConfig(ws), c.prov, trace.NewWriter(&buf))
			assertSingleDone(t, &buf, c.status)
		})
	}
}

func TestResumeContinuesFromCheckpoint(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	spec := TaskSpec{TaskID: "r", Instruction: "x", Constraints: TaskConstraints{MaxTurns: 6}}

	// First run: checkpoint every turn, then cancel after turn 2 (simulated kill).
	var cps []Checkpoint
	ctx, cancel := context.WithCancel(context.Background())
	first := providers.NewScriptedMockProvider(toolTurn(1), toolTurn(1), toolTurn(1))
	var buf1 bytes.Buffer
	res := RunTaskWithOptions(ctx, spec, pol, first, trace.NewWriter(&buf1), RunOptions{OnCheckpoint: func(cp Checkpoint) {
		cps = append(cps, cp)
		if cp.Turn == 2 {
			cancel()
		}
	}})
	if res.Status != "interrupted" || len(cps) != 2 {
		t.Fatalf("status=%s checkpoints=%d", res.Status, len(cps))
	}
	last := cps[len(cps)-1]

	// Resume from the last checkpoint: must continue at turn 3 with prior messages and usage.
	second := providers.NewScriptedMockProvider(providers.Response{Content: "finished", RawStopReason: "stop", PromptTokens: 7, OutputTokens: 1})
	var buf2 bytes.Buffer
	res2 := RunTaskWithOptions(context.Background(), last.Spec, pol, second, trace.NewWriter(&buf2), RunOptions{Resume: &last})
	if res2.ExitCode != policy.ExitOK || res2.Turns != 3 {
		t.Fatalf("resume result %+v", res2)
	}
	if res2.RunID != res.RunID {
		t.Fatalf("run_id changed across resume: %s vs %s", res2.RunID, res.RunID)
	}
	if got := len(second.RecordedMsgs[0]); got != len(last.Msgs) {
		t.Fatalf("resumed provider saw %d msgs, checkpoint had %d", got, len(last.Msgs))
	}
	if res2.Usage.TotalTokens != last.Usage.TotalTokens+8 {
		t.Fatalf("usage not carried over: %+v vs %+v", res2.Usage, last.Usage)
	}
}
