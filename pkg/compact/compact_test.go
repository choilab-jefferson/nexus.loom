package compact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrefixHashDeterministic(t *testing.T) {
	p1 := PrefixHeader{
		SystemPrompt:    "You are Loom.",
		ToolSchemasJSON: `[{"name":"read_file"}]`,
		RepoSummary:     "go module nexus.loom",
	}
	p2 := PrefixHeader{
		SystemPrompt:    "  You are Loom. \n",
		ToolSchemasJSON: `[{"name":"read_file"}]`,
		RepoSummary:     "go module nexus.loom",
	}
	if p1.PrefixHash() != p2.PrefixHash() {
		t.Fatalf("expected identical PrefixHash after whitespace trim, got %s vs %s", p1.PrefixHash(), p2.PrefixHash())
	}
	p3 := PrefixHeader{
		SystemPrompt:    "You are Loom v2.",
		ToolSchemasJSON: `[{"name":"read_file"}]`,
		RepoSummary:     "go module nexus.loom",
	}
	if p1.PrefixHash() == p3.PrefixHash() {
		t.Fatal("expected different PrefixHash when SystemPrompt changes")
	}
}

func TestCompactMessagesPreservesPrefixAndUserTask(t *testing.T) {
	prefix := PrefixHeader{
		SystemPrompt:    "You are Loom.",
		ToolSchemasJSON: `[{"name":"read_file"}]`,
	}
	hashBefore := prefix.PrefixHash()

	bigToolOutput := strings.Repeat("line of output\n", 200) + "\nFAIL: AssertionError in test_auth.py:42\n"
	msgs := []Message{
		{Role: "user", Content: "Fix the failing auth test."},
		{Role: "assistant", Content: "Reading file"},
		{Role: "tool", Name: "read_file", Content: bigToolOutput},
		{Role: "assistant", Content: "Running second tool"},
		{Role: "tool", Name: "read_file", Content: "recent tool output"},
	}

	compacted, stats := CompactMessages(prefix, msgs, 150, 2)
	if !stats.Compacted {
		t.Fatal("expected messages to be compacted")
	}
	if stats.FoldedCount != 1 {
		t.Fatalf("expected 1 folded message, got %d", stats.FoldedCount)
	}
	if stats.PrefixHash != hashBefore || prefix.PrefixHash() != hashBefore {
		t.Fatal("Invariant I2 violated: PrefixHash mutated during compaction")
	}
	if compacted[0].Content != "Fix the failing auth test." {
		t.Fatal("initial user task message was mutated")
	}
	if !strings.HasPrefix(compacted[2].Content, "[loom:folded turn=3 ") {
		t.Fatalf("expected folded marker in compacted[2], got %q", compacted[2].Content)
	}
	// Check §8.3 parser recall of planted error keyword
	if !strings.Contains(compacted[2].Content, "AssertionError") {
		t.Errorf("expected folded preview to retain AssertionError keyword, got %q", compacted[2].Content)
	}
	if compacted[4].Content != "recent tool output" {
		t.Errorf("expected recent tool message within keepRecent to stay untouched, got %q", compacted[4].Content)
	}
}

func TestWireHashTracksWireBytesAndSchemasSentOnce(t *testing.T) {
	p := PrefixHeader{SystemPrompt: "sys", ToolSchemasJSON: `[{"name":"a"}]`, RepoSummary: "repo"}
	if strings.Contains(p.RenderSystemBlock(), `"name":"a"`) {
		t.Fatal("tool schemas must not be embedded in the system block (sent once via tools field)")
	}
	if p.PrefixHash() != WireHash(p.RenderSystemBlock(), p.ToolSchemasJSON) {
		t.Fatal("PrefixHash must equal WireHash of system block + tools")
	}
	if WireHash("sys", `[{"name":"a"}]`) == WireHash("sys", `[{"name":"b"}]`) {
		t.Fatal("hash must change when wire tools bytes change")
	}
	if WireHash("sys", "[]") == WireHash("sys2", "[]") {
		t.Fatal("hash must change when wire system bytes change")
	}
}

func TestElideOldToolResultsKeepsLastK(t *testing.T) {
	big := "first line\nsecond line\n\nthird line\nfourth line\n" + strings.Repeat("x", 5000)
	msgs := []Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "t"}}},
		{Role: "tool", ToolCallID: "1", Content: big},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "2", Name: "t"}}},
		{Role: "tool", ToolCallID: "2", Content: big},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "3", Name: "t"}}},
		{Role: "tool", ToolCallID: "3", Content: big},
	}
	out, n := ElideOldToolResults(msgs, 1)
	if n != 2 {
		t.Fatalf("elided %d, want 2", n)
	}
	for _, i := range []int{2, 4} {
		c := out[i].Content
		if !strings.HasPrefix(c, "[loom:elided bytes=") || !strings.Contains(c, "first line\nsecond line\nthird line") || len(c) > 600 {
			t.Fatalf("msg %d not elided as expected: %q", i, c)
		}
		if out[i].ToolCallID != msgs[i].ToolCallID {
			t.Fatalf("tool call id lost on %d", i)
		}
	}
	if out[6].Content != big || out[0].Content != "task" {
		t.Fatal("last tool result and user prompt must stay verbatim")
	}
	if msgs[2].Content != big {
		t.Fatal("input slice must not be mutated")
	}
	again, n2 := ElideOldToolResults(out, 1)
	if n2 != 0 || again[2].Content != out[2].Content {
		t.Fatalf("second pass must be a no-op, elided %d", n2)
	}
	if same, n3 := ElideOldToolResults(msgs, 3); n3 != 0 || same[2].Content != big {
		t.Fatalf("keep >= tool count must be a no-op")
	}
	if _, n4 := ElideOldToolResults(msgs, 0); n4 != 0 {
		t.Fatal("keep=0 means keep all")
	}
}

func TestElideContentUTF8AndSingleLongLine(t *testing.T) {
	got := elideContent(strings.Repeat("한", 500))
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "...") || len(got) > 300 {
		t.Fatalf("bad elision: %q", got)
	}
	if elideContent("") != "[loom:elided bytes=0 lines=1]" {
		t.Fatalf("empty: %q", elideContent(""))
	}
}

func TestElideTracksFlagNotContentPrefix(t *testing.T) {
	lookalike := "[loom:elided bytes=1 lines=1]\n" + strings.Repeat("workspace file body\n", 50)
	msgs := []Message{
		{Role: "user", Content: "task"},
		{Role: "tool", ToolCallID: "1", Content: lookalike},
		{Role: "tool", ToolCallID: "2", Content: "latest"},
	}
	out, n := ElideOldToolResults(msgs, 1)
	if n != 1 || !out[1].Elided || out[2].Elided {
		t.Fatalf("lookalike result must be elided by flag: n=%d %+v", n, out)
	}
	if _, n2 := ElideOldToolResults(out, 1); n2 != 0 {
		t.Fatalf("flagged message must not be re-elided")
	}
}
