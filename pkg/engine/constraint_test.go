package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
	"github.com/choilab-jefferson/nexus.loom/pkg/providers"
	"github.com/choilab-jefferson/nexus.loom/pkg/trace"
)

func callResp(id, name string, args map[string]any) providers.Response {
	return providers.Response{RawStopReason: "stop", ToolCalls: []compact.ToolCall{{ID: id, Name: name, Arguments: args}}}
}

// interleaved: run_command, read_file (success), repeated; then a final answer.
func interleavedMock(repeats int) *providers.ScriptedMockProvider {
	var rs []providers.Response
	for i := 0; i < repeats; i++ {
		rs = append(rs,
			callResp("rc", "run_command", map[string]any{"argv": []any{"rm", "-rf", "x"}}),
			callResp("rf", "read_file", map[string]any{"path": "a.txt"}),
		)
	}
	rs = append(rs, providers.Response{Content: "final", RawStopReason: "stop"})
	return providers.NewScriptedMockProvider(rs...)
}

func intPtr(n int) *int { return &n }

func wsWithFile(t *testing.T) string {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestNotExposedToolReturnsObservationAndContinues(t *testing.T) {
	pol := policy.DefaultPolicyConfig(wsWithFile(t))
	mock := interleavedMock(1)
	res := RunTask(context.Background(), TaskSpec{Instruction: "x", Constraints: TaskConstraints{MaxTurns: 10}}, pol, mock, nil)
	if res.ExitCode != policy.ExitOK || res.Status != "success" {
		t.Fatalf("got %d %s: %s", res.ExitCode, res.Status, res.Output)
	}
	second := mock.RecordedMsgs[1]
	last := second[len(second)-1]
	if last.Role != "tool" || !strings.Contains(last.Content, `"status":"tool_not_available"`) || !strings.Contains(last.Content, `"tool":"run_command"`) {
		t.Fatalf("observation = %+v", last)
	}
}

func TestRepeatedNotExposedToolEndsWithConstraintFailure(t *testing.T) {
	pol := policy.DefaultPolicyConfig(wsWithFile(t))
	var buf bytes.Buffer
	spec := TaskSpec{Instruction: "x", Constraints: TaskConstraints{MaxTurns: 20, MaxDisallowedToolRequests: intPtr(2)}}
	res := RunTask(context.Background(), spec, pol, interleavedMock(5), trace.NewWriter(&buf))
	if res.Status != "constraint_failure" || res.ExitCode != policy.ExitTaskFail {
		t.Fatalf("got %d %s: %s", res.ExitCode, res.Status, res.Output)
	}
	if res.Turns != 5 { // 3rd run_command request is on turn 5
		t.Errorf("turns = %d, want 5", res.Turns)
	}
	if !strings.Contains(buf.String(), `"reason":"constraint_failure"`) {
		t.Errorf("done event lacks reason constraint_failure:\n%s", buf.String())
	}
}

func TestZeroLimitEndsOnFirstDisallowedRequest(t *testing.T) {
	pol := policy.DefaultPolicyConfig(wsWithFile(t))
	spec := TaskSpec{Instruction: "x", Constraints: TaskConstraints{MaxDisallowedToolRequests: intPtr(0)}}
	res := RunTask(context.Background(), spec, pol, interleavedMock(1), nil)
	if res.Status != "constraint_failure" || res.Turns != 1 {
		t.Fatalf("got %s after %d turns", res.Status, res.Turns)
	}
}

func TestNoLimitKeepsTodaysBehaviour(t *testing.T) {
	pol := policy.DefaultPolicyConfig(wsWithFile(t))
	res := RunTask(context.Background(), TaskSpec{Instruction: "x", Constraints: TaskConstraints{MaxTurns: 20}}, pol, interleavedMock(5), nil)
	if res.Status != "success" {
		t.Fatalf("without a limit the counter must not end the run, got %s: %s", res.Status, res.Output)
	}
}

func TestAllowlistRejectionCountsTowardLimit(t *testing.T) {
	pol := policy.DefaultPolicyConfig(wsWithFile(t))
	pol.AllowCommands = [][]string{{"go", "test"}} // run_command exposed, "rm" rejected
	spec := TaskSpec{Instruction: "x", Constraints: TaskConstraints{MaxTurns: 20, MaxDisallowedToolRequests: intPtr(2)}}
	res := RunTask(context.Background(), spec, pol, interleavedMock(5), nil)
	if res.Status != "constraint_failure" {
		t.Fatalf("got %s: %s", res.Status, res.Output)
	}
}
