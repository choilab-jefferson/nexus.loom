package providers

import (
	"testing"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
)

// Agent Platform's Gemma MaaS omits finishReason (probed 2026-11-12); a text answer without one is a normal end,
// not an unknown stop that ends the run.
func TestGeminiStopReasonInfersEndTurnForTextWithoutFinishReason(t *testing.T) {
	tc := []compact.ToolCall{{ID: "1", Name: "read_file"}}
	cases := []struct {
		raw     string
		tcs     []compact.ToolCall
		content string
		want    StopReason
	}{
		{"", nil, "hello", StopEndTurn},
		{"", tc, "", StopToolUse},
		{"", nil, "   ", StopUnknown},
		{"STOP", nil, "x", StopEndTurn},
		{"MAX_TOKENS", nil, "x", StopMaxTokens},
	}
	for _, c := range cases {
		if got := geminiStopReason(c.raw, c.tcs, c.content); got != c.want {
			t.Errorf("geminiStopReason(%q, %d calls, %q) = %q, want %q", c.raw, len(c.tcs), c.content, got, c.want)
		}
	}
}
