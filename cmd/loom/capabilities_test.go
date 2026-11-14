package main

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
)

func TestCapabilitiesExitCodesParity(t *testing.T) {
	want := []int{policy.ExitOK, policy.ExitTaskFail, policy.ExitHarnessErr, policy.ExitEscalate, policy.ExitBudget, policy.ExitInterrupted}
	var got []int
	for _, v := range exitCodeNames {
		got = append(got, v)
	}
	sort.Ints(got)
	sort.Ints(want)
	if len(got) != len(want) {
		t.Fatalf("exit_codes %v != policy constants %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("exit_codes %v != policy constants %v", got, want)
		}
	}
}

func TestCapabilitiesVersionAndCommit(t *testing.T) {
	ov, oc := version, commit
	defer func() { version, commit = ov, oc }()
	version, commit = "v1.2.3", "abc1234"
	raw, err := json.Marshal(capabilitiesMap())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["version"] != "v1.2.3" || m["commit"] != "abc1234" {
		t.Fatalf("version/commit = %v/%v", m["version"], m["commit"])
	}
	if m["schema_version"] != float64(1) {
		t.Fatalf("schema_version = %v, want 1", m["schema_version"])
	}
	codes, ok := m["exit_codes"].(map[string]any)
	if !ok || len(codes) != len(exitCodeNames) {
		t.Fatalf("exit_codes = %v", m["exit_codes"])
	}
	for k, v := range exitCodeNames {
		if codes[k] != float64(v) {
			t.Fatalf("exit_codes[%s] = %v, want %d", k, codes[k], v)
		}
	}
}

func TestCapabilitiesListsModelsSubcommand(t *testing.T) {
	m := capabilitiesMap()
	found := false
	for _, s := range m["subcommands"].([]string) {
		found = found || s == "models"
	}
	if !found || m["default_models"].(map[string]string)["gemini"] != "gemini-3.8-flash" {
		t.Fatalf("%v", m)
	}
	if runModels(nil) != 2 || runModels([]string{"--provider", "typesafe"}) != 2 {
		t.Fatal("models errors must exit 2")
	}
}

func TestCapabilitiesReadonlyCommands(t *testing.T) {
	raw, err := json.Marshal(capabilitiesMap())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["schema_version"] != float64(1) || m["read_only_commands"] != true {
		t.Fatalf("schema_version/read_only_commands = %v/%v", m["schema_version"], m["read_only_commands"])
	}
	ro, ok := m["readonly_commands"].(map[string]any)
	if !ok || ro["tool"] != "run_readonly" || ro["default"] != "off" || ro["task_key"] != "constraints.readonly_commands" {
		t.Fatalf("readonly_commands = %v", m["readonly_commands"])
	}
	al, _ := ro["allowlist"].(map[string]any)
	git, _ := al["git"].([]any)
	glab, _ := al["glab"].([]any)
	if len(git) != 8 || len(glab) != 8 {
		t.Fatalf("allowlist git=%v glab=%v", git, glab)
	}
	tools, _ := m["builtin_tools"].([]any)
	found := false
	for _, n := range tools {
		if n == "run_readonly" {
			found = true
		}
	}
	if !found {
		t.Fatalf("builtin_tools lacks run_readonly: %v", tools)
	}
}

func TestCapabilitiesToolResultCap(t *testing.T) {
	raw, err := json.Marshal(capabilitiesMap())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	cap, _ := m["tool_result_cap"].(map[string]any)
	if cap["task_key"] != "constraints.max_tool_result_bytes" || cap["flag"] != "--max-tool-result-bytes" || cap["default"] != float64(16384) || cap["min"] != float64(policy.MinToolResultBytes) {
		t.Fatalf("tool_result_cap = %v", m["tool_result_cap"])
	}
	keep, _ := m["keep_tool_results"].(map[string]any)
	if keep["task_key"] != "constraints.keep_tool_results" || keep["default"] != float64(0) {
		t.Fatalf("keep_tool_results = %v", m["keep_tool_results"])
	}
}
