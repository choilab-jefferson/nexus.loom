package main

import (
	"flag"
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/engine"
	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
)

func TestToolsFlagCommaAndRepeatable(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cap := addCapabilityFlags(fs)
	if err := fs.Parse([]string{"--tools", "read_file, write_file", "--tools", "list_dir", "--tools", "read_file"}); err != nil {
		t.Fatal(err)
	}
	spec := engine.TaskSpec{}
	cap.apply(&spec)
	if want := []string{"read_file", "write_file", "list_dir"}; !reflect.DeepEqual(spec.Constraints.Tools, want) {
		t.Errorf("tools = %v, want %v", spec.Constraints.Tools, want)
	}
}

func TestToolsFlagOverridesSpec(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cap := addCapabilityFlags(fs)
	_ = fs.Parse([]string{"--tools", "read_file"})
	spec := engine.TaskSpec{}
	spec.Constraints.Tools = []string{"write_file"}
	cap.apply(&spec)
	if want := []string{"read_file"}; !reflect.DeepEqual(spec.Constraints.Tools, want) {
		t.Errorf("tools = %v, want %v", spec.Constraints.Tools, want)
	}

	fs = flag.NewFlagSet("t", flag.ContinueOnError)
	cap = addCapabilityFlags(fs)
	_ = fs.Parse(nil)
	spec.Constraints.Tools = []string{"write_file"}
	cap.apply(&spec)
	if want := []string{"write_file"}; !reflect.DeepEqual(spec.Constraints.Tools, want) {
		t.Errorf("unset flag must keep spec tools, got %v", spec.Constraints.Tools)
	}
}

func TestTaskFileToolsField(t *testing.T) {
	p := t.TempDir() + "/task.json"
	writeFile(t, p, `{"instruction":"x","constraints":{"tools":["read_file"]}}`)
	spec, err := engine.LoadTaskFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(spec.Constraints.Tools, []string{"read_file"}) {
		t.Errorf("tools = %v", spec.Constraints.Tools)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMaxDisallowedToolRequestsFlag(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cap := addCapabilityFlags(fs)
	_ = fs.Parse(nil)
	spec := engine.TaskSpec{}
	cap.apply(&spec)
	if spec.Constraints.MaxDisallowedToolRequests != nil {
		t.Fatal("unset flag must leave the limit unset (no default)")
	}

	fs = flag.NewFlagSet("t", flag.ContinueOnError)
	cap = addCapabilityFlags(fs)
	if err := fs.Parse([]string{"--max-disallowed-tool-requests", "0"}); err != nil {
		t.Fatal(err)
	}
	cap.apply(&spec)
	if p := spec.Constraints.MaxDisallowedToolRequests; p == nil || *p != 0 {
		t.Fatalf("explicit 0 must be kept, got %v", p)
	}

	fs = flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addCapabilityFlags(fs)
	if err := fs.Parse([]string{"--max-disallowed-tool-requests", "-1"}); err == nil {
		t.Error("negative value should fail")
	}
}

func TestTaskFileMaxDisallowedField(t *testing.T) {
	p := t.TempDir() + "/task.json"
	writeFile(t, p, `{"instruction":"x","constraints":{"max_disallowed_tool_requests":3}}`)
	spec, err := engine.LoadTaskFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if v := spec.Constraints.MaxDisallowedToolRequests; v == nil || *v != 3 {
		t.Fatalf("got %v", v)
	}
}

func TestPathGlobFlagsAndPolicy(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cap := addCapabilityFlags(fs)
	if err := fs.Parse([]string{"--read-glob", "inputs/**", "--write-glob", "work/**", "--write-glob", "outputs/**"}); err != nil {
		t.Fatal(err)
	}
	spec := engine.TaskSpec{}
	spec.Constraints.ReadGlobs = []string{"other/**"}
	cap.apply(&spec)
	if want := []string{"inputs/**"}; !reflect.DeepEqual(spec.Constraints.ReadGlobs, want) {
		t.Errorf("read globs = %v", spec.Constraints.ReadGlobs)
	}
	if want := []string{"work/**", "outputs/**"}; !reflect.DeepEqual(spec.Constraints.WriteGlobs, want) {
		t.Errorf("write globs = %v", spec.Constraints.WriteGlobs)
	}

	pol := policy.DefaultPolicyConfig(t.TempDir())
	applyPolicyExtras(spec, pol)
	if want := []string{"work/**", "outputs/**"}; !reflect.DeepEqual(pol.WriteAllow, want) {
		t.Errorf("WriteAllow = %v", pol.WriteAllow)
	}
	// Written files stay readable for verification.
	if want := []string{"inputs/**", "work/**", "outputs/**"}; !reflect.DeepEqual(pol.ReadAllow, want) {
		t.Errorf("ReadAllow = %v", pol.ReadAllow)
	}

	pol = policy.DefaultPolicyConfig(t.TempDir())
	applyPolicyExtras(engine.TaskSpec{}, pol)
	if !reflect.DeepEqual(pol.ReadAllow, []string{"**"}) || !reflect.DeepEqual(pol.WriteAllow, []string{"**"}) {
		t.Errorf("defaults changed: read %v write %v", pol.ReadAllow, pol.WriteAllow)
	}
}

func TestTaskFileGlobFields(t *testing.T) {
	p := t.TempDir() + "/task.json"
	writeFile(t, p, `{"instruction":"x","constraints":{"read_globs":["inputs/**"],"write_globs":["work/**"]}}`)
	spec, err := engine.LoadTaskFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(spec.Constraints.ReadGlobs, []string{"inputs/**"}) || !reflect.DeepEqual(spec.Constraints.WriteGlobs, []string{"work/**"}) {
		t.Errorf("got %+v", spec.Constraints)
	}
}

func TestCapabilitiesListsCapabilityRestriction(t *testing.T) {
	cr, ok := capabilitiesMap()["capability_restriction"].(map[string]any)
	if !ok {
		t.Fatal("capabilities lacks capability_restriction")
	}
	wantFlags := []string{"--tools", "--read-glob", "--write-glob", "--max-disallowed-tool-requests"}
	if !reflect.DeepEqual(cr["flags"], wantFlags) {
		t.Errorf("flags = %v", cr["flags"])
	}
	wantKeys := []string{"tools", "read_globs", "write_globs", "max_disallowed_tool_requests"}
	if !reflect.DeepEqual(cr["task_keys"], wantKeys) {
		t.Errorf("task_keys = %v", cr["task_keys"])
	}
	if cr["terminal_status"] != "constraint_failure" {
		t.Errorf("terminal_status = %v", cr["terminal_status"])
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	addCapabilityFlags(fs)
	for _, f := range wantFlags {
		if fs.Lookup(f[2:]) == nil {
			t.Errorf("listed flag %s is not registered", f)
		}
	}
}
