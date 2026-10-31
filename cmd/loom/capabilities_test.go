package main

import (
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
