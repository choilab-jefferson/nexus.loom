package main

import (
	"flag"
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
