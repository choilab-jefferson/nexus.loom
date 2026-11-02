package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
	"github.com/choilab-jefferson/nexus.loom/pkg/providers"
	"github.com/choilab-jefferson/nexus.loom/pkg/trace"
)

func call(id, name string) compact.ToolCall {
	return compact.ToolCall{ID: id, Name: name, Arguments: map[string]any{}}
}

func TestParseContextRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"invalid json":   `{not json`,
		"missing schema": `{"messages":[]}`,
		"unknown schema": `{"schema_version":"loom.context.v9","messages":[]}`,
		"unknown role":   `{"schema_version":"loom.context.v1","messages":[{"role":"system","content":"x"}]}`,
	} {
		if _, _, err := ParseContext([]byte(raw)); err == nil {
			t.Errorf("%s: want error", name)
		} else if _, ok := err.(*ContextError); !ok {
			t.Errorf("%s: want *ContextError, got %T", name, err)
		}
	}
}

func TestRepairMessages(t *testing.T) {
	in := []compact.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "", ToolCalls: []compact.ToolCall{call("a", "read_file"), call("b", "list_dir")}},
		{Role: "tool", ToolCallID: "a", Content: "A"}, // b dangling
		{Role: "tool", ToolCallID: "zzz", Content: "orphan in run"},
		{Role: "assistant", Content: "", ToolCalls: []compact.ToolCall{call("c", "x")}}, // fully dangling, no text -> dropped
		{Role: "tool", ToolCallID: "gone", Content: "orphan"},
		{Role: "user", Content: "note"},
		{Role: "user", Content: "again"},
		{Role: "assistant", Content: "ok"},
	}
	out, notes := RepairMessages(in)
	if len(notes) < 4 {
		t.Fatalf("notes=%v", notes)
	}
	var roles []string
	for _, m := range out {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "user,assistant,tool,user,assistant" {
		t.Fatalf("roles=%s out=%+v", got, out)
	}
	if len(out[1].ToolCalls) != 1 || out[1].ToolCalls[0].ID != "a" {
		t.Fatalf("kept calls = %+v", out[1].ToolCalls)
	}
	if out[3].Content != "note\n\nagain" {
		t.Fatalf("merge = %q", out[3].Content)
	}
}

func TestRepairKeepsTextOfDanglingAssistant(t *testing.T) {
	out, _ := RepairMessages([]compact.Message{
		{Role: "user", Content: "u"},
		{Role: "assistant", Content: "thinking", ToolCalls: []compact.ToolCall{call("a", "x")}},
	})
	if len(out) != 2 || len(out[1].ToolCalls) != 0 || out[1].Content != "thinking" {
		t.Fatalf("%+v", out)
	}
}

func TestContextRoundTripAndCumulativeUsage(t *testing.T) {
	ws := t.TempDir()
	pol := policy.DefaultPolicyConfig(ws)
	var buf bytes.Buffer
	p1 := providers.NewScriptedMockProvider(providers.Response{Content: "one", RawStopReason: "stop", PromptTokens: 10, OutputTokens: 2})
	r1 := RunTaskWithOptions(context.Background(), TaskSpec{Instruction: "first"}, pol, p1, trace.NewWriter(&buf), RunOptions{})
	c1 := BuildContext(nil, r1.Msgs, r1)
	if c1.UserTurn != 1 || r1.UserTurn != 1 || c1.CumulativeUsage.TotalTokens != 12 {
		t.Fatalf("c1=%+v", c1)
	}
	raw, _ := json.Marshal(c1)
	loaded, notes, err := ParseContext(raw)
	if err != nil || len(notes) != 0 {
		t.Fatalf("err=%v notes=%v", err, notes)
	}
	p2 := providers.NewScriptedMockProvider(providers.Response{Content: "two", RawStopReason: "stop", PromptTokens: 20, OutputTokens: 3})
	r2 := RunTaskWithOptions(context.Background(), TaskSpec{Instruction: "second"}, pol, p2, trace.NewWriter(&buf), RunOptions{Context: loaded})
	if got := len(p2.RecordedMsgs[0]); got != 3 { // first, one, second
		t.Fatalf("provider saw %d msgs", got)
	}
	if r2.Usage.TotalTokens != 23 || r2.CumulativeUsage.TotalTokens != 35 || r2.UserTurn != 2 {
		t.Fatalf("r2 usage=%+v cum=%+v turn=%d", r2.Usage, r2.CumulativeUsage, r2.UserTurn)
	}
	c2 := BuildContext(loaded, r2.Msgs, r2)
	if c2.UserTurn != 2 || len(c2.Runs) != 2 || c2.Runs[1].RunIndex != 2 || len(c2.Messages) != 4 {
		t.Fatalf("c2=%+v", c2)
	}
}

// A per-invocation budget applies to this call only, not to cumulative usage.
func TestBudgetIsPerInvocation(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	prior := &Context{SchemaVersion: ContextSchema, UserTurn: 1, CumulativeUsage: Usage{PromptTokens: 1000, TotalTokens: 1000},
		Messages: []compact.Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}}}
	var buf bytes.Buffer
	p := providers.NewScriptedMockProvider(providers.Response{Content: "ok", RawStopReason: "stop", PromptTokens: 5, OutputTokens: 1})
	r := RunTaskWithOptions(context.Background(), TaskSpec{Instruction: "c", Constraints: TaskConstraints{MaxTotalTokens: 100}}, pol, p, trace.NewWriter(&buf), RunOptions{Context: prior})
	if r.ExitCode != policy.ExitOK || r.CumulativeUsage.TotalTokens != 1006 {
		t.Fatalf("%+v", r)
	}
}

// Interrupted context-continued run: resume keeps the prior history and totals.
func TestResumeAfterContextIn(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	prior := &Context{SchemaVersion: ContextSchema, UserTurn: 1, CumulativeUsage: Usage{PromptTokens: 50, TotalTokens: 50},
		Messages: []compact.Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}}}
	var cps []Checkpoint
	ctx, cancel := context.WithCancel(context.Background())
	var buf bytes.Buffer
	first := providers.NewScriptedMockProvider(toolTurn(1), toolTurn(1))
	res := RunTaskWithOptions(ctx, TaskSpec{Instruction: "c", Constraints: TaskConstraints{MaxTurns: 6}}, pol, first, trace.NewWriter(&buf), RunOptions{
		Context: prior,
		OnCheckpoint: func(cp Checkpoint) {
			cps = append(cps, cp)
			cancel()
		}})
	if res.Status != "interrupted" || len(cps) != 1 {
		t.Fatalf("%s %d", res.Status, len(cps))
	}
	last := cps[0]
	second := providers.NewScriptedMockProvider(providers.Response{Content: "fin", RawStopReason: "stop", PromptTokens: 4, OutputTokens: 1})
	res2 := RunTaskWithOptions(context.Background(), last.Spec, pol, second, trace.NewWriter(&buf), RunOptions{
		Resume: &last, PriorUsage: prior.CumulativeUsage, PriorUserTurn: prior.UserTurn})
	if res2.ExitCode != policy.ExitOK || res2.UserTurn != 2 {
		t.Fatalf("%+v", res2)
	}
	if res2.CumulativeUsage.TotalTokens != 50+res2.Usage.TotalTokens {
		t.Fatalf("cum=%+v usage=%+v", res2.CumulativeUsage, res2.Usage)
	}
	if res2.Msgs[0].Content != "a" || len(res2.Msgs) < 4 {
		t.Fatalf("history lost: %+v", res2.Msgs)
	}
}
