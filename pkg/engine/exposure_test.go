package engine

import (
	"context"
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
	"github.com/choilab-jefferson/nexus.loom/pkg/providers"
	"github.com/choilab-jefferson/nexus.loom/pkg/tools"
)

func doneMock() *providers.ScriptedMockProvider {
	return providers.NewScriptedMockProvider(providers.Response{Content: "done", RawStopReason: "stop"})
}

// expectedHash is the prefix hash a provider sees when exactly names are exposed.
func expectedHash(t *testing.T, names []string) string {
	t.Helper()
	s, err := tools.FilterSchemas(names)
	if err != nil {
		t.Fatal(err)
	}
	return compact.PrefixHeader{SystemPrompt: defaultSystemPrompt, ToolSchemasJSON: tools.SchemasJSON(s)}.PrefixHash()
}

func TestExposureExplicitToolSet(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	mock := doneMock()
	spec := TaskSpec{Instruction: "x", Constraints: TaskConstraints{Tools: []string{"read_file", "write_file"}}}
	res := RunTask(context.Background(), spec, pol, mock, nil)
	if res.ExitCode != policy.ExitOK {
		t.Fatalf("exit %d: %s", res.ExitCode, res.Output)
	}
	if want := expectedHash(t, []string{"read_file", "write_file"}); mock.RecordedHash[0] != want {
		t.Fatal("provider prefix does not carry only read_file and write_file")
	}
}

// builtinsExcept is every built-in tool name except the given ones, in canonical order.
func builtinsExcept(drop ...string) []string {
	skip := map[string]bool{}
	for _, d := range drop {
		skip[d] = true
	}
	out := []string{}
	for _, s := range tools.BuiltinSchemas() {
		if !skip[s.Name] {
			out = append(out, s.Name)
		}
	}
	return out
}

var cmdToolNames = []string{"run_command", "job_start", "job_status", "job_log", "job_wait", "job_kill"}

func TestExposureDropsRunCommandWithoutAllowlist(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	mock := doneMock()
	RunTask(context.Background(), TaskSpec{Instruction: "x"}, pol, mock, nil)
	if mock.RecordedHash[0] != expectedHash(t, builtinsExcept(append(cmdToolNames, "run_readonly")...)) {
		t.Fatal("run_command and the job tools must not be exposed when allow_commands is empty")
	}

	pol.AllowCommands = [][]string{{"go", "test"}}
	mock = doneMock()
	RunTask(context.Background(), TaskSpec{Instruction: "x"}, pol, mock, nil)
	if mock.RecordedHash[0] != expectedHash(t, builtinsExcept("run_readonly")) {
		t.Fatal("run_command and the job tools expected when a command is allowlisted")
	}
}

func TestExposureRunReadonlyOnlyWhenEnabled(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	pol.ReadonlyCommands = []string{"git"}
	mock := doneMock()
	spec := TaskSpec{Instruction: "x", Constraints: TaskConstraints{ReadonlyCommands: []string{"git"}}}
	RunTask(context.Background(), spec, pol, mock, nil)
	if mock.RecordedHash[0] != expectedHash(t, builtinsExcept(cmdToolNames...)) {
		t.Fatal("run_readonly must be exposed when readonly_commands is set")
	}
}

func TestExposureUnknownToolIsHarnessError(t *testing.T) {
	pol := policy.DefaultPolicyConfig(t.TempDir())
	spec := TaskSpec{Instruction: "x", Constraints: TaskConstraints{Tools: []string{"bash"}}}
	res := RunTask(context.Background(), spec, pol, doneMock(), nil)
	if res.ExitCode != policy.ExitHarnessErr {
		t.Fatalf("exit %d, want %d", res.ExitCode, policy.ExitHarnessErr)
	}
}
