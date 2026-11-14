package main

import (
	"flag"
	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
	"io"
	"reflect"
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/engine"
)

func TestBudgetFlagsOutputAndPassEnv(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	bf := addBudgetFlags(fs)
	if err := fs.Parse([]string{"--max-output-tokens", "4096", "--pass-env", "A,B*", "--pass-env", "A", "--pass-env", "C"}); err != nil {
		t.Fatal(err)
	}
	spec := engine.TaskSpec{}
	spec.Constraints.MaxOutputTokens = 100
	spec.Constraints.PassEnv = []string{"Z"}
	bf.apply(&spec)
	if spec.Constraints.MaxOutputTokens != 4096 {
		t.Errorf("flag should override spec, got %d", spec.Constraints.MaxOutputTokens)
	}
	if want := []string{"Z", "A", "B*", "C"}; !reflect.DeepEqual(spec.Constraints.PassEnv, want) {
		t.Errorf("pass env = %v, want %v", spec.Constraints.PassEnv, want)
	}
}

func TestParseAllowCmdsUnion(t *testing.T) {
	task := engine.AllowCommandList{{"pytest"}}
	got := append(task, parseAllowCmds([]string{"python", "go test", " "})...)
	want := engine.AllowCommandList{{"pytest"}, {"python"}, {"go", "test"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestMaxCmdTimeoutFlagAndEnv(t *testing.T) {
	for in, want := range map[string]int{"600": 600, "10m": 600, "90s": 90} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		bf := addBudgetFlags(fs)
		if err := fs.Parse([]string{"--max-cmd-timeout", in}); err != nil {
			t.Fatal(err)
		}
		spec := engine.TaskSpec{}
		bf.apply(&spec)
		if spec.Constraints.MaxCmdTimeoutSec != want {
			t.Errorf("%s -> %d, want %d", in, spec.Constraints.MaxCmdTimeoutSec, want)
		}
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addBudgetFlags(fs)
	if err := fs.Parse([]string{"--max-cmd-timeout", "bogus"}); err == nil {
		t.Error("bogus value should fail")
	}

	t.Setenv("LOOM_MAX_CMD_TIMEOUT", "900")
	pol := policy.DefaultPolicyConfig(t.TempDir())
	applyPolicyExtras(engine.TaskSpec{}, pol)
	if pol.MaxCmdTimeoutSec != 900 {
		t.Errorf("env fallback = %d, want 900", pol.MaxCmdTimeoutSec)
	}
	spec := engine.TaskSpec{}
	spec.Constraints.MaxCmdTimeoutSec = 120
	applyPolicyExtras(spec, pol)
	if pol.MaxCmdTimeoutSec != 120 {
		t.Errorf("spec should beat env, got %d", pol.MaxCmdTimeoutSec)
	}
	t.Setenv("LOOM_MAX_CMD_TIMEOUT", "")
	pol = policy.DefaultPolicyConfig(t.TempDir())
	applyPolicyExtras(engine.TaskSpec{}, pol)
	if pol.MaxCmdTimeoutSec != 300 {
		t.Errorf("default = %d, want 300", pol.MaxCmdTimeoutSec)
	}
}

func TestToolResultCapFlagsAndPolicy(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	bf := addBudgetFlags(fs)
	if err := fs.Parse([]string{"--max-tool-result-bytes", "4096", "--keep-tool-results", "2"}); err != nil {
		t.Fatal(err)
	}
	spec := engine.TaskSpec{}
	spec.Constraints.MaxToolResultBytes = 100000
	bf.apply(&spec)
	if spec.Constraints.MaxToolResultBytes != 4096 || spec.Constraints.KeepToolResults != 2 {
		t.Fatalf("flags not applied: %+v", spec.Constraints)
	}
	pol := policy.DefaultPolicyConfig(t.TempDir())
	def := pol.MaxOutputBytes
	applyPolicyExtras(engine.TaskSpec{}, pol)
	if pol.MaxOutputBytes != def {
		t.Errorf("unset constraint must keep default %d, got %d", def, pol.MaxOutputBytes)
	}
	applyPolicyExtras(spec, pol)
	if pol.MaxOutputBytes != 4096 {
		t.Errorf("cap = %d, want 4096", pol.MaxOutputBytes)
	}
	spec.Constraints.MaxToolResultBytes = 5
	applyPolicyExtras(spec, pol)
	if pol.MaxOutputBytes != policy.MinToolResultBytes {
		t.Errorf("tiny cap must clamp to %d, got %d", policy.MinToolResultBytes, pol.MaxOutputBytes)
	}
}
