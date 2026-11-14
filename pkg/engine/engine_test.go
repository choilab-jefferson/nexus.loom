package engine

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
	"github.com/choilab-jefferson/nexus.loom/pkg/providers"
	"github.com/choilab-jefferson/nexus.loom/pkg/trace"
)

func TestRunTaskExit0AndPrefixStability(t *testing.T) {
	ws := t.TempDir()
	pol := policy.DefaultPolicyConfig(ws)

	mock := providers.NewScriptedMockProvider(
		providers.Response{
			RawStopReason: "stop", // Invariant I1: tool_calls present -> normalized to StopToolUse
			ToolCalls: []compact.ToolCall{
				{
					ID:   "call_1",
					Name: "write_file",
					Arguments: map[string]any{
						"path":    "hello.txt",
						"content": "hello loom\n",
					},
				},
			},
		},
		providers.Response{
			Content:       "Task completed.",
			RawStopReason: "stop",
		},
	)

	var traceBuf bytes.Buffer
	tw := trace.NewWriter(&traceBuf)

	spec := TaskSpec{
		TaskID:      "t-001",
		Agent:       "coder",
		Instruction: "Create hello.txt",
		Constraints: TaskConstraints{MaxTurns: 4},
	}

	res := RunTask(context.Background(), spec, pol, mock, tw)
	if res.ExitCode != policy.ExitOK {
		t.Fatalf("expected ExitOK (0), got %d (%s)", res.ExitCode, res.Output)
	}
	if res.Turns != 2 {
		t.Fatalf("expected 2 turns, got %d", res.Turns)
	}
	// Invariant I2: PrefixHash must be identical across all turns
	if len(mock.RecordedHash) != 2 || mock.RecordedHash[0] != mock.RecordedHash[1] {
		t.Fatalf("Invariant I2 violated: recorded hashes = %v", mock.RecordedHash)
	}
	if !strings.Contains(traceBuf.String(), `"t":"run_start"`) || !strings.Contains(traceBuf.String(), `"t":"done"`) {
		t.Errorf("expected run_start and done events in trace, got:\n%s", traceBuf.String())
	}
}

func TestRunTaskExit2OnPolicyViolation(t *testing.T) {
	ws := t.TempDir()
	pol := policy.DefaultPolicyConfig(ws)

	mock := providers.NewScriptedMockProvider(
		providers.Response{
			ToolCalls: []compact.ToolCall{
				{
					ID:   "call_escape",
					Name: "read_file",
					Arguments: map[string]any{
						"path": "../../etc/passwd",
					},
				},
			},
		},
	)

	res := RunTask(context.Background(), TaskSpec{
		TaskID:      "t-002",
		Instruction: "Attempt path escape",
	}, pol, mock, nil)

	if res.ExitCode != policy.ExitHarnessErr {
		t.Fatalf("expected ExitHarnessErr (2), got %d", res.ExitCode)
	}
}

func TestRunTaskExit3OnEscalate(t *testing.T) {
	ws := t.TempDir()
	pol := policy.DefaultPolicyConfig(ws)

	mock := providers.NewScriptedMockProvider(
		providers.Response{
			ToolCalls: []compact.ToolCall{
				{
					ID:   "call_esc",
					Name: "escalate",
					Arguments: map[string]any{
						"reason":          "local_tier_exhausted",
						"minimal_context": "needs architectural change",
						"attempted":       []string{"read_file"},
					},
				},
			},
		},
	)

	res := RunTask(context.Background(), TaskSpec{
		TaskID:        "t-003",
		Instruction:   "Complex cross-module refactor",
		SeedSymbols:   []string{"AuthValidator"},
		SeedFiles:     []string{"src/auth.go"},
		CloudEligible: true,
	}, pol, mock, nil)

	if res.ExitCode != policy.ExitEscalate {
		t.Fatalf("expected ExitEscalate (3), got %d", res.ExitCode)
	}
	if res.Escalation == nil || res.Escalation.Reason != "local_tier_exhausted" || !res.Escalation.CloudEligible {
		t.Fatalf("unexpected escalation payload: %+v", res.Escalation)
	}
}

func TestRunTaskExit1OnMaxTurnsExhausted(t *testing.T) {
	ws := t.TempDir()
	pol := policy.DefaultPolicyConfig(ws)

	mock := providers.NewScriptedMockProvider(
		providers.Response{
			ToolCalls: []compact.ToolCall{
				{ID: "1", Name: "list_dir", Arguments: map[string]any{"path": "."}},
			},
		},
		providers.Response{
			ToolCalls: []compact.ToolCall{
				{ID: "2", Name: "list_dir", Arguments: map[string]any{"path": "."}},
			},
		},
	)

	res := RunTask(context.Background(), TaskSpec{
		TaskID:      "t-004",
		Instruction: "Loop forever",
		Constraints: TaskConstraints{MaxTurns: 2},
	}, pol, mock, nil)

	if res.ExitCode != policy.ExitTaskFail {
		t.Fatalf("expected ExitTaskFail (1), got %d", res.ExitCode)
	}
}

func TestUnknownFieldCompatibilityInTaskJSON(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "task.json")
	raw := `{
		"schema_version": 1,
		"task_id": "task-503",
		"instruction": "Verify §5.3 forward compatibility",
		"future_unknown_field": {"nested": true},
		"constraints": {"max_turns": 6, "allow_commands": ["pytest", "go test"]}
	}`
	if err := os.WriteFile(tmp, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadTaskFile(tmp)
	if err != nil {
		t.Fatalf("LoadTaskFile failed with unknown fields: %v", err)
	}
	if spec.TaskID != "task-503" || len(spec.Constraints.AllowCommands) != 2 {
		t.Fatalf("unexpected parsed TaskSpec: %+v", spec)
	}
}

func TestRunTaskEscalatesWhenStillOverBudgetAfterCompaction(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	mock := providers.NewScriptedMockProvider()
	var buf bytes.Buffer
	spec := TaskSpec{
		TaskID:           "t-over",
		Instruction:      strings.Repeat("x", 4000), // initial prompt is never foldable
		MaxContextTokens: 200,
		Constraints:      TaskConstraints{MaxTurns: 2},
	}
	res := RunTask(context.Background(), spec, pol, mock, trace.NewWriter(&buf))
	if res.ExitCode != policy.ExitEscalate || res.Escalation == nil || !strings.Contains(res.Escalation.Reason, "context_over_budget") {
		t.Fatalf("expected escalation on over-budget context, got exit=%d status=%s out=%q", res.ExitCode, res.Status, res.Output)
	}
	if len(mock.RecordedHash) != 0 {
		t.Fatal("provider must not be called with over-budget context")
	}
}

func toolCallResp(id, name string, args map[string]any) providers.Response {
	return providers.Response{
		RawStopReason: "stop",
		ToolCalls:     []compact.ToolCall{{ID: id, Name: name, Arguments: args}},
	}
}

func TestRecoverableToolErrorContinuesRun(t *testing.T) {
	ws := t.TempDir()
	pol := policy.DefaultPolicyConfig(ws)
	mock := providers.NewScriptedMockProvider(
		toolCallResp("1", "read_file", map[string]any{"path": "missing.txt"}),
		toolCallResp("2", "read_file", map[string]any{}), // empty path arg
		providers.Response{Content: "done", RawStopReason: "stop"},
	)
	spec := TaskSpec{TaskID: "t", Instruction: "go", Constraints: TaskConstraints{MaxTurns: 6}}
	res := RunTask(context.Background(), spec, pol, mock, trace.NewWriter(&bytes.Buffer{}))
	if res.ExitCode != policy.ExitOK {
		t.Fatalf("expected run to continue to ExitOK, got %d %s (%s)", res.ExitCode, res.Status, res.Output)
	}
}

func TestToolErrorStrikeLimit(t *testing.T) {
	ws := t.TempDir()
	pol := policy.DefaultPolicyConfig(ws)
	var script []providers.Response
	for i := 0; i < 6; i++ {
		script = append(script, toolCallResp("c", "read_file", map[string]any{"path": "missing.txt"}))
	}
	mock := providers.NewScriptedMockProvider(script...)
	spec := TaskSpec{TaskID: "t", Instruction: "go", Constraints: TaskConstraints{MaxTurns: 10}}
	res := RunTask(context.Background(), spec, pol, mock, trace.NewWriter(&bytes.Buffer{}))
	if res.ExitCode != policy.ExitTaskFail || res.Status != "tool_error_strikes" {
		t.Fatalf("expected exit 1 tool_error_strikes, got %d %s", res.ExitCode, res.Status)
	}
	if res.Turns != MaxToolStrikes {
		t.Fatalf("expected stop at turn %d, got %d", MaxToolStrikes, res.Turns)
	}
}

func TestToolStrikesResetOnSuccess(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "ok.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.DefaultPolicyConfig(ws)
	bad := map[string]any{"path": "missing.txt"}
	good := map[string]any{"path": "ok.txt"}
	mock := providers.NewScriptedMockProvider(
		toolCallResp("1", "read_file", bad), toolCallResp("2", "read_file", bad),
		toolCallResp("3", "read_file", good),
		toolCallResp("4", "read_file", bad), toolCallResp("5", "read_file", bad),
		providers.Response{Content: "done", RawStopReason: "stop"},
	)
	spec := TaskSpec{TaskID: "t", Instruction: "go", Constraints: TaskConstraints{MaxTurns: 8}}
	res := RunTask(context.Background(), spec, pol, mock, trace.NewWriter(&bytes.Buffer{}))
	if res.ExitCode != policy.ExitOK {
		t.Fatalf("strikes must reset on success; got %d %s", res.ExitCode, res.Status)
	}
}

type authErrProvider struct{}

func (authErrProvider) Chat(context.Context, compact.PrefixHeader, []compact.Message, int) (providers.Response, error) {
	return providers.Response{}, &providers.AuthError{Status: 401, Msg: "HTTP 401 from x: unauthorized"}
}

func TestProviderAuthErrorExits2(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	spec := TaskSpec{TaskID: "t", Instruction: "go"}
	res := RunTask(context.Background(), spec, pol, authErrProvider{}, trace.NewWriter(&bytes.Buffer{}))
	if res.ExitCode != policy.ExitHarnessErr || res.Status != "auth_error" {
		t.Fatalf("expected exit 2 auth_error, got %d %s", res.ExitCode, res.Status)
	}
}

func TestRecoverablePolicyViolationContinuesAndTraces(t *testing.T) {
	ws := t.TempDir()
	pol := policy.DefaultPolicyConfig(ws)
	pol.AllowCommands = [][]string{{"go", "test"}} // run_command is exposed only with an allowlist
	mock := providers.NewScriptedMockProvider(
		toolCallResp("1", "run_command", map[string]any{"argv": []string{"rm", "-rf", "/"}}),
		toolCallResp("2", "run_command", map[string]any{"argv": []string{"bash", "-c", "ls"}}),
		providers.Response{Content: "done", RawStopReason: "stop"},
	)
	var buf bytes.Buffer
	spec := TaskSpec{TaskID: "t", Instruction: "go", Constraints: TaskConstraints{MaxTurns: 6}}
	res := RunTask(context.Background(), spec, pol, mock, trace.NewWriter(&buf))
	if res.ExitCode != policy.ExitOK {
		t.Fatalf("expected run to continue, got %d %s (%s)", res.ExitCode, res.Status, res.Output)
	}
	out := buf.String()
	if !strings.Contains(out, `"t":"policy_violation_recovered"`) || !strings.Contains(out, `"kind":"allowlist"`) || !strings.Contains(out, `"kind":"eval_flag"`) {
		t.Errorf("missing policy_violation_recovered events:\n%s", out)
	}
}

func TestRepeatedPolicyViolationHitsStrikeLimit(t *testing.T) {
	ws := t.TempDir()
	pol := policy.DefaultPolicyConfig(ws)
	var script []providers.Response
	for i := 0; i < 6; i++ {
		script = append(script, toolCallResp("c", "run_command", map[string]any{"argv": []string{"rm", "-rf", "/"}}))
	}
	spec := TaskSpec{TaskID: "t", Instruction: "go", Constraints: TaskConstraints{MaxTurns: 10}}
	res := RunTask(context.Background(), spec, pol, providers.NewScriptedMockProvider(script...), trace.NewWriter(&bytes.Buffer{}))
	if res.ExitCode != policy.ExitTaskFail || res.Status != "tool_error_strikes" {
		t.Fatalf("expected tool_error_strikes, got %d %s", res.ExitCode, res.Status)
	}
}

type modelErrProvider struct{ calls int }

func (m *modelErrProvider) Chat(context.Context, compact.PrefixHeader, []compact.Message, int) (providers.Response, error) {
	m.calls++
	return providers.Response{}, &providers.ModelNotFoundError{Provider: "gemini", Model: "nope", Status: 404}
}

func TestModelNotFoundExits2NoRetry(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	mp := &modelErrProvider{}
	var tr bytes.Buffer
	res := RunTask(context.Background(), TaskSpec{TaskID: "t", Instruction: "go"}, pol, mp, trace.NewWriter(&tr))
	if res.ExitCode != policy.ExitHarnessErr || res.Status != "model_not_found" || mp.calls != 1 {
		t.Fatalf("got %d %s calls=%d", res.ExitCode, res.Status, mp.calls)
	}
	if !strings.Contains(res.Output, `model "nope" not found`) || !strings.Contains(tr.String(), `"reason":"model_not_found"`) {
		t.Fatalf("output=%q trace=%s", res.Output, tr.String())
	}
}

func TestRunReadonlyEndToEndWithMockProvider(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ws := t.TempDir()
	gitIn := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitIn("init", "-q", ".")
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn("add", "a.txt")
	gitIn("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "seed commit")

	pol := policy.DefaultPolicyConfig(ws)
	pol.ReadonlyCommands = []string{"git"}
	mock := providers.NewScriptedMockProvider(
		toolCallResp("1", "run_readonly", map[string]any{"argv": []string{"git", "log", "--format=%s"}}),
		toolCallResp("2", "run_readonly", map[string]any{"argv": []string{"git", "push", "origin", "main"}}),
		toolCallResp("3", "run_readonly", map[string]any{"argv": []string{"glab", "mr", "list"}}), // not enabled
		providers.Response{Content: "answer", RawStopReason: "stop"},
	)
	var buf bytes.Buffer
	spec := TaskSpec{TaskID: "t", Instruction: "what is the last commit?", Constraints: TaskConstraints{MaxTurns: 8}}
	res := RunTask(context.Background(), spec, pol, mock, trace.NewWriter(&buf))
	if res.ExitCode != policy.ExitOK || res.Output != "answer" {
		t.Fatalf("got %d %s (%s)", res.ExitCode, res.Status, res.Output)
	}
	tr := buf.String()
	for _, want := range []string{
		`"name":"run_readonly"`,
		`"argv":["git","log","--format=%s"]`,
		`"argv":["git","push","origin","main"]`,
		`"exit_code":0`,
		`"t":"policy_violation_recovered"`,
		`"kind":"allowlist"`,
	} {
		if !strings.Contains(tr, want) {
			t.Errorf("trace missing %s:\n%s", want, tr)
		}
	}
	if n := strings.Count(tr, `"t":"tool_call"`); n != 3 {
		t.Errorf("want 3 traced tool_call events, got %d", n)
	}
	// The model saw the commit subject in the first tool result.
	seen := false
	for _, m := range mock.RecordedMsgs[1] {
		if m.Role == "tool" && strings.Contains(m.Content, "seed commit") {
			seen = true
		}
	}
	if !seen {
		t.Error("git log output did not reach the model")
	}
	if len(res.FilesChanged.Added)+len(res.FilesChanged.Modified)+len(res.FilesChanged.Deleted) != 0 {
		t.Errorf("read-only run changed files: %+v", res.FilesChanged)
	}
}

func TestRunReadonlyOffByDefaultInEngine(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	mock := providers.NewScriptedMockProvider(
		toolCallResp("1", "run_readonly", map[string]any{"argv": []string{"git", "log"}}),
		providers.Response{Content: "done", RawStopReason: "stop"},
	)
	var buf bytes.Buffer
	spec := TaskSpec{TaskID: "t", Instruction: "go", Constraints: TaskConstraints{MaxTurns: 4}}
	res := RunTask(context.Background(), spec, pol, mock, trace.NewWriter(&buf))
	if res.ExitCode != policy.ExitOK {
		t.Fatalf("got %d %s", res.ExitCode, res.Status)
	}
	// off by default, run_readonly is not exposed, so a call to it is refused as an unexposed tool
	if !strings.Contains(buf.String(), `"kind":"tool_not_available"`) || !strings.Contains(buf.String(), `"tool":"run_readonly"`) {
		t.Errorf("expected tool_not_available refusal in trace:\n%s", buf.String())
	}
}

func TestTaskJSONReadonlyCommands(t *testing.T) {
	p := filepath.Join(t.TempDir(), "task.json")
	if err := os.WriteFile(p, []byte(`{"instruction":"x","constraints":{"readonly_commands":["git","glab"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadTaskFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Constraints.ReadonlyCommands) != 2 || spec.Constraints.ReadonlyCommands[0] != "git" {
		t.Fatalf("readonly_commands not parsed: %+v", spec.Constraints)
	}
}

func TestReadonlyAndAllowCommandsMutuallyExclusive(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	mock := providers.NewScriptedMockProvider(providers.Response{Content: "done", RawStopReason: "stop"})
	spec := TaskSpec{TaskID: "t", Instruction: "go", Constraints: TaskConstraints{
		ReadonlyCommands: []string{"git"}, AllowCommands: AllowCommandList{{"go", "test"}},
	}}
	res := RunTask(context.Background(), spec, pol, mock, trace.NewWriter(&bytes.Buffer{}))
	if res.ExitCode != policy.ExitHarnessErr || !strings.Contains(res.Output, "mutually exclusive") {
		t.Fatalf("expected harness error, got %d %s (%s)", res.ExitCode, res.Status, res.Output)
	}
	if len(mock.RecordedHash) != 0 {
		t.Error("provider must not be called")
	}
}

func TestKeepToolResultsElidesOlderResults(t *testing.T) {
	ws := t.TempDir()
	body := "HEADLINE\n" + strings.Repeat("filler line of text\n", 200)
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.DefaultPolicyConfig(ws)
	mock := providers.NewScriptedMockProvider(
		toolCallResp("1", "read_file", map[string]any{"path": "a.txt"}),
		toolCallResp("2", "read_file", map[string]any{"path": "a.txt"}),
		toolCallResp("3", "read_file", map[string]any{"path": "a.txt"}),
		providers.Response{Content: "done", RawStopReason: "stop"},
	)
	var buf bytes.Buffer
	spec := TaskSpec{TaskID: "t", Instruction: "go", Constraints: TaskConstraints{MaxTurns: 6, KeepToolResults: 1}}
	res := RunTask(context.Background(), spec, pol, mock, trace.NewWriter(&buf))
	if res.ExitCode != policy.ExitOK {
		t.Fatalf("got %d %s (%s)", res.ExitCode, res.Status, res.Output)
	}
	last := mock.RecordedMsgs[len(mock.RecordedMsgs)-1]
	var tool []compact.Message
	for _, m := range last {
		if m.Role == "tool" {
			tool = append(tool, m)
		}
	}
	if len(tool) != 3 {
		t.Fatalf("want 3 tool messages, got %d", len(tool))
	}
	for _, m := range tool[:2] {
		if !strings.HasPrefix(m.Content, "[loom:elided bytes=") || !strings.Contains(m.Content, "HEADLINE") || len(m.Content) > 600 {
			t.Errorf("older result not elided: %.120q", m.Content)
		}
	}
	if strings.HasPrefix(tool[2].Content, "[loom:elided") || !strings.Contains(tool[2].Content, "filler line of text") {
		t.Errorf("last result must stay verbatim: %.120q", tool[2].Content)
	}
	if !strings.Contains(buf.String(), `"t":"tool_results_elided"`) {
		t.Errorf("trace lacks tool_results_elided:\n%s", buf.String())
	}
}

func TestKeepToolResultsOffByDefault(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := providers.NewScriptedMockProvider(
		toolCallResp("1", "read_file", map[string]any{"path": "a.txt"}),
		toolCallResp("2", "read_file", map[string]any{"path": "a.txt"}),
		providers.Response{Content: "done", RawStopReason: "stop"},
	)
	spec := TaskSpec{TaskID: "t", Instruction: "go", Constraints: TaskConstraints{MaxTurns: 6}}
	RunTask(context.Background(), spec, policy.DefaultPolicyConfig(ws), mock, trace.NewWriter(&bytes.Buffer{}))
	for _, m := range mock.RecordedMsgs[len(mock.RecordedMsgs)-1] {
		if strings.HasPrefix(m.Content, "[loom:elided") {
			t.Fatalf("elision must be off without keep_tool_results: %q", m.Content)
		}
	}
}
