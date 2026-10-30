package compact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// PrefixHeader holds the immutable run prefix (Invariant I2).
type PrefixHeader struct {
	SystemPrompt    string `json:"system_prompt"`
	ToolSchemasJSON string `json:"tool_schemas_json"`
	RepoSummary     string `json:"repo_summary"`
}

// RenderSystemBlock renders the canonical system string sent on the wire. Tool schemas are
// NOT embedded: providers send them once via their native tools field (see WireHash).
func (p PrefixHeader) RenderSystemBlock() string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(p.SystemPrompt))
	if strings.TrimSpace(p.RepoSummary) != "" {
		b.WriteString("\n\n--- REPO SUMMARY ---\n")
		b.WriteString(strings.TrimSpace(p.RepoSummary))
	}
	return b.String()
}

// WireHash is the SHA-256 hex digest of the exact bytes a provider puts on the wire for the
// frozen prefix: the rendered system block and the tools JSON, each sent once.
func WireHash(systemBlock, toolsJSON string) string {
	sum := sha256.Sum256([]byte("system:" + systemBlock + "\x00tools:" + strings.TrimSpace(toolsJSON)))
	return hex.EncodeToString(sum[:])
}

// PrefixHash computes the hash of the wire prefix (Invariant I2).
func (p PrefixHeader) PrefixHash() string {
	return WireHash(p.RenderSystemBlock(), p.ToolSchemasJSON)
}

// ToolCall represents a structured model tool invocation.
type ToolCall struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Arguments        map[string]any `json:"arguments"`
	ThoughtSignature string         `json:"thought_signature,omitempty"`
}

// Message represents a single turn entry in the conversation history.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// CompactionStats reports token usage before and after pre-send compaction (Invariant I3).
type CompactionStats struct {
	Compacted    bool   `json:"compacted"`
	FoldedCount  int    `json:"folded_count"`
	TokensBefore int    `json:"tokens_before"`
	TokensAfter  int    `json:"tokens_after"`
	PrefixHash   string `json:"prefix_hash"`
}

// EstimateTokens returns a deterministic token estimate: max(1, len(s)/4).
func EstimateTokens(s string) int {
	n := len(s) / 4
	if n < 1 {
		return 1
	}
	return n
}

// EstimateMessageTokens estimates tokens for a single Message including any tool calls.
func EstimateMessageTokens(m Message) int {
	total := EstimateTokens(m.Role) + EstimateTokens(m.Content)
	if len(m.ToolCalls) > 0 {
		raw, _ := json.Marshal(m.ToolCalls)
		total += EstimateTokens(string(raw))
	}
	return total
}

// EstimateTotalTokens computes total estimated tokens for prefix + message history.
func EstimateTotalTokens(prefix PrefixHeader, msgs []Message) int {
	total := EstimateTokens(prefix.RenderSystemBlock()) + EstimateTokens(prefix.ToolSchemasJSON)
	for _, m := range msgs {
		total += EstimateMessageTokens(m)
	}
	return total
}

// CompactMessages enforces Invariant I3 (Prefix-Preserving Pre-Send Compaction).
// When total estimated tokens exceed maxContextTokens, older `tool` messages outside the
// last keepRecent messages are deterministically folded into a compact summary stub while
// preserving PrefixHeader and the initial user task prompt untouched.
func CompactMessages(prefix PrefixHeader, msgs []Message, maxContextTokens int, keepRecent int) ([]Message, CompactionStats) {
	if keepRecent < 0 {
		keepRecent = 0
	}
	pHash := prefix.PrefixHash()
	tokensBefore := EstimateTotalTokens(prefix, msgs)

	out := make([]Message, len(msgs))
	for i := range msgs {
		out[i] = msgs[i]
		if len(msgs[i].ToolCalls) > 0 {
			tc := make([]ToolCall, len(msgs[i].ToolCalls))
			copy(tc, msgs[i].ToolCalls)
			out[i].ToolCalls = tc
		}
	}

	stats := CompactionStats{
		Compacted:    false,
		FoldedCount:  0,
		TokensBefore: tokensBefore,
		TokensAfter:  tokensBefore,
		PrefixHash:   pHash,
	}

	if maxContextTokens <= 0 || tokensBefore <= maxContextTokens {
		return out, stats
	}

	cutoff := len(out) - keepRecent
	if cutoff < 0 {
		cutoff = 0
	}

	for i := 0; i < cutoff; i++ {
		if EstimateTotalTokens(prefix, out) <= maxContextTokens {
			break
		}
		// Never mutate initial user prompt (i == 0 and role == "user") or non-tool messages.
		if out[i].Role != "tool" {
			continue
		}
		if strings.HasPrefix(out[i].Content, "[loom:folded ") {
			continue
		}
		out[i].Content = foldToolContent(i+1, out[i].Content)
		stats.Compacted = true
		stats.FoldedCount++
	}

	stats.TokensAfter = EstimateTotalTokens(prefix, out)
	return out, stats
}

func foldToolContent(turn int, content string) string {
	sum := sha256.Sum256([]byte(content))
	shortHash := hex.EncodeToString(sum[:8])
	lines := strings.Count(content, "\n") + 1
	preview := extractSalientPreview(content, 160)
	return fmt.Sprintf("[loom:folded turn=%d sha256=%s bytes=%d lines=%d preview=%q]",
		turn, shortHash, len(content), lines, preview)
}

// extractSalientPreview preserves error/failure lines from test/linter outputs
// before falling back to the head of the content.
func extractSalientPreview(content string, maxLen int) string {
	var salient []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		upper := strings.ToUpper(trimmed)
		if strings.Contains(upper, "FAIL") ||
			strings.Contains(upper, "ERROR") ||
			strings.Contains(upper, "PANIC") ||
			strings.Contains(upper, "ASSERTIONERROR") {
			salient = append(salient, trimmed)
		}
	}
	var candidate string
	if len(salient) > 0 {
		candidate = strings.Join(salient, " | ")
	} else {
		candidate = strings.Join(strings.Fields(content), " ")
	}
	if len(candidate) > maxLen {
		return candidate[:maxLen] + "..."
	}
	return candidate
}
