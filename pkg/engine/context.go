package engine

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
)

// ContextSchema is the value of `schema_version` in a conversation context document
// (docs/contract.md, "Conversation context"). It is a string so the format can evolve
// independently of the TaskResult schema_version.
const ContextSchema = "loom.context.v1"

// RunRecord summarises one invocation (one user turn) inside a Context.
type RunRecord struct {
	RunIndex     int          `json:"run_index"`
	RunID        string       `json:"run_id,omitempty"`
	Status       string       `json:"status"`
	ExitCode     int          `json:"exit_code"`
	Turns        int          `json:"turns"`
	Usage        Usage        `json:"usage"`
	FilesChanged FilesChanged `json:"files_changed"`
}

// Context is the provider-neutral, caller-editable conversation state passed between
// invocations: `--context-out` writes it, `--context-in` reads it.
type Context struct {
	SchemaVersion string `json:"schema_version"`
	// UserTurn counts user prompts answered so far (invocations that produced this context).
	UserTurn int `json:"user_turn"`
	// Messages is the history: user prompts, assistant messages (with tool_calls), tool
	// results (role "tool") and compaction summaries (tool results folded to "[loom:folded ...]").
	Messages []compact.Message `json:"messages"`
	// Runs has one record per user turn: status, per-run usage and the files_changed
	// delta of that turn (each turn's baseline is the workspace at the start of that turn).
	Runs            []RunRecord `json:"runs"`
	CumulativeUsage Usage       `json:"cumulative_usage"`
	Compactions     int         `json:"compactions"`
}

// ContextError is a rejected context (invalid JSON, unknown schema, unusable structure).
// The CLI maps it to exit 2.
type ContextError struct{ Msg string }

func (e *ContextError) Error() string { return e.Msg }

// ParseContext decodes and validates a context document. Valid structure is enforced
// strictly (JSON, schema_version, known roles); pairing problems are repaired by
// RepairMessages and reported in the returned notes.
func ParseContext(raw []byte) (*Context, []string, error) {
	var c Context
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, nil, &ContextError{fmt.Sprintf("invalid context JSON: %v", err)}
	}
	if c.SchemaVersion != ContextSchema {
		return nil, nil, &ContextError{fmt.Sprintf("unsupported context schema_version %q (want %q)", c.SchemaVersion, ContextSchema)}
	}
	for i, m := range c.Messages {
		switch m.Role {
		case "user", "assistant", "tool":
		default:
			return nil, nil, &ContextError{fmt.Sprintf("messages[%d]: unknown role %q (want user, assistant or tool)", i, m.Role)}
		}
	}
	msgs, notes := RepairMessages(c.Messages)
	c.Messages = msgs
	if c.Runs == nil {
		c.Runs = []RunRecord{}
	}
	return &c, notes, nil
}

// RepairMessages makes an edited history renderable on every provider. Rules (documented
// in docs/contract.md):
//  1. A tool_call with no tool_result immediately after its assistant message is dropped
//     from that message; an assistant message left with no text and no calls is dropped.
//  2. A tool result whose tool_call_id matches no call in the directly preceding assistant
//     message is dropped (orphan).
//  3. Adjacent plain messages of the same role (no tool calls/results) are merged with a
//     blank line, so roles alternate (Gemini) and no empty turns are sent.
//
// It returns the repaired history and one human-readable note per repair.
func RepairMessages(in []compact.Message) ([]compact.Message, []string) {
	var out []compact.Message
	var notes []string
	for i := 0; i < len(in); {
		m := in[i]
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			j := i + 1
			results := map[string]compact.Message{}
			var order []string
			for ; j < len(in) && in[j].Role == "tool"; j++ {
				if _, dup := results[in[j].ToolCallID]; dup {
					notes = append(notes, fmt.Sprintf("dropped duplicate tool_result for %q", in[j].ToolCallID))
					continue
				}
				results[in[j].ToolCallID] = in[j]
				order = append(order, in[j].ToolCallID)
			}
			var kept []compact.ToolCall
			used := map[string]bool{}
			for _, tc := range m.ToolCalls {
				if _, ok := results[tc.ID]; ok && tc.ID != "" && !used[tc.ID] {
					kept = append(kept, tc)
					used[tc.ID] = true
				} else {
					notes = append(notes, fmt.Sprintf("dropped dangling tool_call %q (%s): no tool_result", tc.ID, tc.Name))
				}
			}
			for _, id := range order {
				if !used[id] {
					notes = append(notes, fmt.Sprintf("dropped orphan tool_result %q", id))
				}
			}
			am := m
			am.ToolCalls = kept
			if len(kept) == 0 {
				if strings.TrimSpace(am.Content) != "" {
					out = appendPlain(out, am)
				} else {
					notes = append(notes, "dropped empty assistant message")
				}
			} else {
				out = append(out, am)
				for _, tc := range kept {
					out = append(out, results[tc.ID])
				}
			}
			i = j
		case m.Role == "tool":
			notes = append(notes, fmt.Sprintf("dropped orphan tool_result %q", m.ToolCallID))
			i++
		default:
			if strings.TrimSpace(m.Content) == "" && m.Role == "assistant" {
				notes = append(notes, "dropped empty assistant message")
			} else {
				out = appendPlain(out, m)
			}
			i++
		}
	}
	return out, notes
}

func appendPlain(out []compact.Message, m compact.Message) []compact.Message {
	if n := len(out); n > 0 && out[n-1].Role == m.Role && len(out[n-1].ToolCalls) == 0 && m.Role != "tool" {
		out[n-1].Content = strings.TrimRight(out[n-1].Content, "\n") + "\n\n" + m.Content
		return out
	}
	return append(out, m)
}

// BuildContext appends this invocation to the prior context (nil for a fresh one).
func BuildContext(prior *Context, msgs []compact.Message, res TaskResult) *Context {
	c := &Context{SchemaVersion: ContextSchema, Runs: []RunRecord{}}
	if prior != nil {
		c.UserTurn = prior.UserTurn
		c.Runs = append(c.Runs, prior.Runs...)
		c.Compactions = prior.Compactions
	}
	c.UserTurn++
	c.Compactions += res.Compactions
	c.Messages = msgs
	c.Runs = append(c.Runs, RunRecord{
		RunIndex: c.UserTurn, RunID: res.RunID, Status: res.Status, ExitCode: res.ExitCode,
		Turns: res.Turns, Usage: res.Usage, FilesChanged: res.FilesChanged,
	})
	c.CumulativeUsage = res.CumulativeUsage
	return c
}

// AddUsage returns a+b (total recomputed).
func AddUsage(a, b Usage) Usage {
	a.PromptTokens += b.PromptTokens
	a.CompletionTokens += b.CompletionTokens
	a.TotalTokens = a.PromptTokens + a.CompletionTokens
	a.CacheCreationTokens += b.CacheCreationTokens
	a.CacheReadTokens += b.CacheReadTokens
	return a
}
