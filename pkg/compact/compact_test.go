package compact

import (
	"strings"
	"testing"
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
