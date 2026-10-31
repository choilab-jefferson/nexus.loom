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
