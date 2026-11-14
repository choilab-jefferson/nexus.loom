package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
	"github.com/choilab-jefferson/nexus.loom/pkg/providers"
)

func inputsWorkspace(t *testing.T) (string, *policy.PolicyConfig) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "inputs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "inputs", "x.csv"), []byte("orig\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pol := policy.DefaultPolicyConfig(ws)
	pol.WriteAllow = []string{"work/**"}
	return ws, pol
}

func writeInputs() providers.Response {
	return callResp("w", "write_file", map[string]any{"path": "inputs/x.csv", "content": "changed\n"})
}

func TestWriteOutsideGlobIsObservation(t *testing.T) {
	ws, pol := inputsWorkspace(t)
	mock := providers.NewScriptedMockProvider(writeInputs(), providers.Response{Content: "final", RawStopReason: "stop"})
	res := RunTask(context.Background(), TaskSpec{Instruction: "x"}, pol, mock, nil)
	if res.ExitCode != policy.ExitOK {
		t.Fatalf("got %d %s: %s", res.ExitCode, res.Status, res.Output)
	}
	msgs := mock.RecordedMsgs[1]
	if obs := msgs[len(msgs)-1].Content; !strings.Contains(obs, "not permitted") {
		t.Errorf("observation = %q", obs)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "inputs", "x.csv")); string(b) != "orig\n" {
		t.Errorf("input changed: %q", b)
	}
}

func TestPathRefusalsCountTowardLimit(t *testing.T) {
	_, pol := inputsWorkspace(t)
	mock := providers.NewScriptedMockProvider(writeInputs(), writeInputs(), providers.Response{Content: "final", RawStopReason: "stop"})
	spec := TaskSpec{Instruction: "x", Constraints: TaskConstraints{MaxDisallowedToolRequests: intPtr(1)}}
	res := RunTask(context.Background(), spec, pol, mock, nil)
	if res.Status != "constraint_failure" || res.Turns != 2 {
		t.Fatalf("got %s after %d turns: %s", res.Status, res.Turns, res.Output)
	}
}
