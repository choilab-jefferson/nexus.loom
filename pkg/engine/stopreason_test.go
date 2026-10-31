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

func runOnce(t *testing.T, spec TaskSpec, resp ...providers.Response) TaskResult {
	t.Helper()
	spec.Instruction = "x"
	var buf bytes.Buffer
	return RunTask(context.Background(), spec, policy.DefaultPolicyConfig(t.TempDir()),
		providers.NewScriptedMockProvider(resp...), trace.NewWriter(&buf))
}

func TestMaxTokensNoToolsIsNotSuccess(t *testing.T) {
	res := runOnce(t, TaskSpec{}, providers.Response{Content: "cut off", RawStopReason: "max_tokens"})
	if res.Status != "max_tokens" || res.ExitCode == policy.ExitOK {
		t.Fatalf("got %s/%d", res.Status, res.ExitCode)
	}
}

func TestTruncatedToolCallNotExecuted(t *testing.T) {
	tr := toolTurn(1)
	tr.RawStopReason = "max_tokens"
	// A second scripted turn would run if the tool call executed and the loop continued.
	res := runOnce(t, TaskSpec{}, tr, providers.Response{Content: "ok", RawStopReason: "stop"})
	if res.Status != "max_tokens" || res.ExitCode == policy.ExitOK || res.Turns != 1 {
		t.Fatalf("truncated tool call not stopped: %s/%d turns=%d", res.Status, res.ExitCode, res.Turns)
	}
}

func TestStopReasonTable(t *testing.T) {
	cases := map[string]string{
		"refusal": "model_stop_error", "malformed_function_call": "model_stop_error",
		"MALFORMED_FUNCTION_CALL": "model_stop_error", "RECITATION": "model_stop_error",
		"OTHER": "model_stop_error", "BLOCKLIST": "model_stop_error",
		"PROHIBITED_CONTENT": "model_stop_error", "SPII": "model_stop_error",
		"pause_turn": "paused", "model_context_window_exceeded": "max_tokens",
		"MAX_TOKENS": "max_tokens",
	}
	for raw, want := range cases {
		for _, withTool := range []bool{false, true} {
			r := providers.Response{Content: "c", RawStopReason: raw}
			if withTool {
				r.ToolCalls = []compact.ToolCall{{ID: "c", Name: "list_dir", Arguments: map[string]any{"path": "."}}}
			}
			res := runOnce(t, TaskSpec{}, r, providers.Response{Content: "ok", RawStopReason: "stop"})
			if res.Status != want || res.ExitCode == policy.ExitOK {
				t.Errorf("%s tool=%v: got %s/%d want %s", raw, withTool, res.Status, res.ExitCode, want)
			}
		}
	}
}

type capProvider struct{ got *int }

func (p capProvider) Chat(_ context.Context, _ compact.PrefixHeader, _ []compact.Message, n int) (providers.Response, error) {
	*p.got = n
	return providers.Response{Content: "ok", RawStopReason: "stop"}, nil
}

func TestMaxOutputTokensConfigurable(t *testing.T) {
	for _, c := range []struct {
		spec int
		env  string
		want int
	}{{0, "", 2048}, {0, "4096", 4096}, {1000, "4096", 1000}} {
		t.Setenv("LOOM_MAX_OUTPUT_TOKENS", c.env)
		got := 0
		var buf bytes.Buffer
		RunTask(context.Background(), TaskSpec{Instruction: "x", Constraints: TaskConstraints{MaxOutputTokens: c.spec}},
			policy.DefaultPolicyConfig(t.TempDir()), capProvider{&got}, trace.NewWriter(&buf))
		if got != c.want {
			t.Errorf("spec=%d env=%q: got %d want %d", c.spec, c.env, got, c.want)
		}
	}
}

// TestSchemaVersionOnResultAndTrace pins the docs/contract.md schema_version keys.
func TestSchemaVersionOnResultAndTrace(t *testing.T) {
	ws := t.TempDir()
	var buf bytes.Buffer
	prov := providers.NewScriptedMockProvider(providers.Response{RawStopReason: "stop", ToolCalls: []compact.ToolCall{
		{ID: "e", Name: "escalate", Arguments: map[string]any{"reason": "r", "minimal_context": "c"}}}})
	res := RunTask(context.Background(), TaskSpec{Instruction: "x"}, policy.DefaultPolicyConfig(ws), prov, trace.NewWriter(&buf))
	if res.SchemaVersion != 1 || res.Escalation == nil || res.Escalation.SchemaVersion != 1 {
		t.Fatalf("schema_version missing: %+v", res)
	}
	raw, _ := json.Marshal(res)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m["schema_version"] != float64(1) {
		t.Fatalf("TaskResult JSON schema_version = %v (%v)", m["schema_version"], err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected trace events, got %q", buf.String())
	}
	for _, l := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(l), &ev); err != nil || ev["schema_version"] != float64(1) {
			t.Fatalf("trace event lacks schema_version: %s", l)
		}
	}
}
