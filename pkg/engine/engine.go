package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/choilab-jefferson/nexus.loom/pkg/compact"
	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
	"github.com/choilab-jefferson/nexus.loom/pkg/providers"
	"github.com/choilab-jefferson/nexus.loom/pkg/tools"
	"github.com/choilab-jefferson/nexus.loom/pkg/trace"
)

// AllowCommandList unmarshals allow_commands whether specified as ["pytest", "go test"] (§5.2)
// or [["pytest"], ["go", "test"]].
type AllowCommandList [][]string

func (a *AllowCommandList) UnmarshalJSON(data []byte) error {
	var matrix [][]string
	if err := json.Unmarshal(data, &matrix); err == nil {
		*a = matrix
		return nil
	}
	var flat []string
	if err := json.Unmarshal(data, &flat); err != nil {
		return err
	}
	out := make([][]string, 0, len(flat))
	for _, entry := range flat {
		fields := strings.Fields(entry)
		if len(fields) > 0 {
			out = append(out, fields)
		}
	}
	*a = out
	return nil
}

// TaskConstraints mirrors `constraints` in task.json (§5.2).
type TaskConstraints struct {
	MaxTurns   int `json:"max_turns"`
	DeadlineMs int `json:"deadline_ms"`
	// MaxTotalTokens caps cumulative prompt+completion tokens (0 = unlimited).
	MaxTotalTokens int `json:"max_total_tokens,omitempty"`
	// MaxOutputTokens caps tokens per model response (0 = LOOM_MAX_OUTPUT_TOKENS or providers.DefaultMaxOutputTokens).
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	// MaxToolCallsPerTurn rejects a model turn returning more tool calls (0 = unlimited).
	MaxToolCallsPerTurn int              `json:"max_tool_calls_per_turn,omitempty"`
	AllowCommands       AllowCommandList `json:"allow_commands"`
	// PassEnv extends the run_command env allowlist (names or PREFIX* patterns).
	// Secret-looking names pass only when listed exactly.
	PassEnv []string `json:"pass_env,omitempty"`
	// MaxCmdTimeoutSec raises/lowers the run_command timeout_s clamp (0 = policy default 300).
	MaxCmdTimeoutSec int `json:"max_cmd_timeout_sec,omitempty"`
}

// TaskSpec represents a Loom v5.1 §5.2 task specification.
type TaskSpec struct {
	SchemaVersion    int             `json:"schema_version"`
	TaskID           string          `json:"task_id"`
	Agent            string          `json:"agent"`
	Instruction      string          `json:"instruction"`
	SeedSymbols      []string        `json:"seed_symbols"`
	SeedFiles        []string        `json:"seed_files"`
	ErrorTrace       string          `json:"error_trace"`
	Constraints      TaskConstraints `json:"constraints"`
	TierHint         string          `json:"tier_hint"`
	CloudEligible    bool            `json:"cloud_eligible"`
	MaxContextTokens int             `json:"max_context_tokens"`
	SystemPrompt     string          `json:"system_prompt,omitempty"`
	RepoSummary      string          `json:"repo_summary,omitempty"`
	// ContextIn / ContextOut are the conversation context paths ("-" = stdin / stdout);
	// the --context-in / --context-out flags override them (docs/contract.md).
	ContextIn  string `json:"context_in,omitempty"`
	ContextOut string `json:"context_out,omitempty"`
}

// LoadTaskFile parses a task.json file while ignoring unknown fields (§5.3).
func LoadTaskFile(path string) (TaskSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return TaskSpec{}, err
	}
	var spec TaskSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return TaskSpec{}, err
	}
	if spec.SchemaVersion == 0 {
		spec.SchemaVersion = 1
	}
	return spec, nil
}

// TaskSlice holds the handover context inside an EscalationPayload (§5.5).
type TaskSlice struct {
	SeedSymbols    []string `json:"seed_symbols"`
	ContextFiles   []string `json:"context_files"`
	MinimalContext string   `json:"minimal_context,omitempty"`
	Attempted      []string `json:"attempted,omitempty"`
	ArtifactPath   string   `json:"artifact_path,omitempty"`
}

// EscalationPayload is the structured Exit Code 3 handover object (§5.5).
type EscalationPayload struct {
	SchemaVersion int       `json:"schema_version"`
	T             string    `json:"t"`
	Reason        string    `json:"reason"`
	Attempts      int       `json:"attempts"`
	LastError     string    `json:"last_error,omitempty"`
	Patch         string    `json:"patch,omitempty"`
	Branch        string    `json:"branch,omitempty"`
	CloudEligible bool      `json:"cloud_eligible"`
	TaskSlice     TaskSlice `json:"task_slice"`
}

// SchemaVersion is the version of the TaskResult / EscalationPayload JSON contract
// (docs/contract.md). Additive field changes keep it; breaking changes bump it.
const SchemaVersion = 1

// TaskResult captures the terminal outcome of RunTask.
type TaskResult struct {
	SchemaVersion int                `json:"schema_version"`
	ExitCode      int                `json:"exit_code"`
	Status        string             `json:"status"`
	Output        string             `json:"output"`
	Turns         int                `json:"turns"`
	PrefixHash    string             `json:"prefix_hash"`
	Compactions   int                `json:"compactions"`
	Escalation    *EscalationPayload `json:"escalation,omitempty"`
	RunID         string             `json:"run_id,omitempty"`
	Usage         Usage              `json:"usage"`
	FilesChanged  FilesChanged       `json:"files_changed"`
	// UserTurn is the 1-based user-turn index of this invocation within its context
	// (1 for a fresh run). Usage above is THIS invocation only; CumulativeUsage adds all
	// prior user turns from --context-in. Budgets (max_total_tokens) apply to Usage.
	UserTurn        int   `json:"user_turn"`
	CumulativeUsage Usage `json:"cumulative_usage"`
	// ContextPath is set by the CLI when --context-out names a file.
	ContextPath string `json:"context_path,omitempty"`
	// Msgs is the final history of this invocation (not serialized; feeds BuildContext).
	Msgs []compact.Message `json:"-"`
}

// Usage is cumulative provider-reported token usage for a run.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// Cache tokens, cumulative; omitted when no provider reported them.
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
}

// Checkpoint is the resumable loop state written after every completed turn.
type Checkpoint struct {
	RunID       string            `json:"run_id"`
	Spec        TaskSpec          `json:"spec"`
	Turn        int               `json:"turn"`
	Msgs        []compact.Message `json:"msgs"`
	Usage       Usage             `json:"usage"`
	Compactions int               `json:"compactions"`
	// Baseline is the workspace snapshot taken at the first start; absent in old checkpoints.
	Baseline *Snapshot `json:"baseline,omitempty"`
}

// RunOptions carries optional resume/checkpoint hooks.
type RunOptions struct {
	Resume       *Checkpoint
	OnCheckpoint func(Checkpoint)
	// TracePath, when set, excludes loom's own trace/checkpoint files from files_changed.
	TracePath string
	// Context is the validated prior conversation (--context-in); its messages precede the
	// new user prompt and its cumulative usage feeds TaskResult.CumulativeUsage.
	Context *Context
	// PriorUsage / PriorUserTurn carry the prior-context totals when Context.Messages is
	// not available (a resumed run whose checkpoint already holds the messages).
	PriorUsage    Usage
	PriorUserTurn int
}

// runState is mutated by runTask so the RunTaskWithOptions wrapper can emit
// a single done event (with usage and run_id) on every exit path.
type runState struct {
	runID  string
	usage  Usage
	reason string
	opts   RunOptions
	// baseline is the workspace snapshot from the first start of the logical run.
	baseline *Snapshot
	msgs     []compact.Message
}

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// MaxToolStrikes is the number of consecutive model-caused tool errors that
// ends the run with ExitTaskFail. Any successful tool call resets the count.
const MaxToolStrikes = 3

const defaultSystemPrompt = `You are Loom, the Nexus bounded single-agent leaf execution harness.
Operate strictly within the workspace policy. Use built-in tools to inspect, edit, and verify code.
If a task exceeds local capability or permissions, call the escalate tool.`

// wireHash hashes the bytes providers actually send: the rendered system block plus the live
// built-in tool schemas (re-read each call, so schema drift mid-run is detected).
func wireHash(p compact.PrefixHeader) string {
	return compact.WireHash(p.RenderSystemBlock(), tools.CanonicalToolSchemasJSON())
}

// RunTask executes the bounded Loom M1 leaf loop enforcing Invariants I1, I2, I3, and I4.
func RunTask(ctx context.Context, spec TaskSpec, pol *policy.PolicyConfig, prov providers.Provider, traceWriter *trace.Writer) TaskResult {
	return RunTaskWithOptions(ctx, spec, pol, prov, traceWriter, RunOptions{})
}

// RunTaskWithOptions is RunTask plus resume/checkpoint hooks. It guarantees exactly one
// terminal "done" trace event on every exit path.
func RunTaskWithOptions(ctx context.Context, spec TaskSpec, pol *policy.PolicyConfig, prov providers.Provider, traceWriter *trace.Writer, opts RunOptions) TaskResult {
	startWall := time.Now()
	st := &runState{runID: newRunID(), opts: opts}
	if opts.Resume != nil {
		st.runID = opts.Resume.RunID
		st.usage = opts.Resume.Usage
		st.baseline = opts.Resume.Baseline
	}
	exclude := loomOwnedExcluder(opts.TracePath)
	if st.baseline == nil && pol != nil {
		// Fresh run, or resume from an old checkpoint without a baseline (snapshot at resume time).
		st.baseline = TakeSnapshot(pol.Workspace, exclude)
	}
	res := runTask(ctx, spec, pol, prov, traceWriter, st)
	res.SchemaVersion = SchemaVersion
	if res.Escalation != nil {
		res.Escalation.SchemaVersion = SchemaVersion
	}
	res.RunID = st.runID
	res.Usage = st.usage
	prior, priorTurn := opts.PriorUsage, opts.PriorUserTurn
	if opts.Context != nil {
		prior, priorTurn = opts.Context.CumulativeUsage, opts.Context.UserTurn
	}
	res.CumulativeUsage = AddUsage(prior, st.usage)
	res.UserTurn = priorTurn + 1
	res.Msgs = st.msgs
	res.FilesChanged = emptyFilesChanged()
	if pol != nil {
		res.FilesChanged = DiffSnapshots(st.baseline, TakeSnapshot(pol.Workspace, exclude))
	}
	done := map[string]any{
		"t":         "done",
		"status":    res.Status,
		"exit_code": res.ExitCode,
		"turns":     res.Turns,
		"wall_ms":   time.Since(startWall).Milliseconds(),
		"run_id":    st.runID,
		"usage":     st.usage,
	}
	if st.reason != "" {
		done["reason"] = st.reason
	}
	if res.ExitCode != policy.ExitOK && res.Output != "" {
		done["error"] = res.Output
	}
	_ = traceWriter.Emit(done)
	return res
}

func ctxResult(ctx context.Context, st *runState, turn int, hash string, compactions int) TaskResult {
	if ctx.Err() == context.DeadlineExceeded {
		st.reason = "deadline"
		return TaskResult{ExitCode: policy.ExitBudget, Status: "budget_exhausted", Output: "deadline exceeded", Turns: turn, PrefixHash: hash, Compactions: compactions}
	}
	st.reason = "signal"
	return TaskResult{ExitCode: policy.ExitInterrupted, Status: "interrupted", Output: "interrupted", Turns: turn, PrefixHash: hash, Compactions: compactions}
}

func runTask(ctx context.Context, spec TaskSpec, pol *policy.PolicyConfig, prov providers.Provider, traceWriter *trace.Writer, st *runState) TaskResult {
	if pol == nil {
		return TaskResult{
			ExitCode: policy.ExitHarnessErr,
			Status:   "harness_error",
			Output:   "nil PolicyConfig",
		}
	}
	if prov == nil {
		return TaskResult{
			ExitCode: policy.ExitHarnessErr,
			Status:   "harness_error",
			Output:   "nil Provider",
		}
	}
	if strings.TrimSpace(spec.Instruction) == "" {
		return TaskResult{
			ExitCode: policy.ExitHarnessErr,
			Status:   "harness_error",
			Output:   "empty task instruction",
		}
	}

	maxTurns := spec.Constraints.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 8
	}
	maxContextTokens := spec.MaxContextTokens
	if maxContextTokens <= 0 {
		maxContextTokens = 28000
	}
	if spec.Constraints.DeadlineMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(spec.Constraints.DeadlineMs)*time.Millisecond)
		defer cancel()
	}

	sysPrompt := spec.SystemPrompt
	if strings.TrimSpace(sysPrompt) == "" {
		sysPrompt = defaultSystemPrompt
	}

	// Invariant I2: Freeze prefix at run start (§6.1)
	prefix := compact.PrefixHeader{
		SystemPrompt:    sysPrompt,
		ToolSchemasJSON: tools.CanonicalToolSchemasJSON(),
		RepoSummary:     spec.RepoSummary,
	}
	frozenHash := wireHash(prefix)

	_ = traceWriter.Emit(map[string]any{
		"t":                  "run_start",
		"run_id":             st.runID,
		"task_id":            spec.TaskID,
		"agent":              spec.Agent,
		"daemon":             "inproc",
		"frozen_prefix_hash": frozenHash[:16],
		"cloud_eligible":     spec.CloudEligible,
	})

	for _, name := range spec.Constraints.PassEnv {
		if policy.IsSecretEnvName(name) {
			_ = traceWriter.Emit(map[string]any{"t": "env_secret_passed", "run_id": st.runID, "name": name})
		}
	}

	initialPrompt := buildInitialUserPrompt(spec)
	var msgs []compact.Message
	if st.opts.Context != nil {
		msgs = append(msgs, st.opts.Context.Messages...)
	}
	msgs = appendPlain(msgs, compact.Message{Role: "user", Content: initialPrompt})
	defer func() { st.msgs = msgs }()

	compactions := 0
	unknownStopRetries := 0
	var lastError string
	disabledTools := make(map[string]int)
	toolStrikes := 0

	startTurn := 1
	if cp := st.opts.Resume; cp != nil {
		msgs = cp.Msgs
		compactions = cp.Compactions
		startTurn = cp.Turn + 1
	}
	maxTotal := spec.Constraints.MaxTotalTokens
	maxOutput := resolveMaxOutputTokens(spec.Constraints.MaxOutputTokens)
	maxToolCalls := spec.Constraints.MaxToolCallsPerTurn

	for turn := startTurn; turn <= maxTurns; turn++ {
		if ctx.Err() != nil {
			return ctxResult(ctx, st, turn-1, frozenHash, compactions)
		}
		// Invariant I2: Verify prefix hash has not drifted
		currentHash := wireHash(prefix)
		if currentHash != frozenHash {
			return TaskResult{
				ExitCode:   policy.ExitHarnessErr,
				Status:     "prefix_violation",
				Output:     "frozen_prefix_hash mutated mid-run",
				Turns:      turn,
				PrefixHash: frozenHash,
			}
		}

		// Invariant I3: Pre-send token compaction preserving prefix and initial user prompt
		var cStats compact.CompactionStats
		msgs, cStats = compact.CompactMessages(prefix, msgs, maxContextTokens, 4)
		if cStats.Compacted {
			compactions += cStats.FoldedCount
		}

		// Invariant I3: still over budget after folding => escalate, never send oversized context.
		if maxContextTokens > 0 && cStats.TokensAfter > maxContextTokens {
			reason := fmt.Sprintf("context_over_budget: %d tokens after compaction exceeds max_context_tokens %d", cStats.TokensAfter, maxContextTokens)
			esc := &EscalationPayload{
				T:             "escalation_required",
				Reason:        reason,
				Attempts:      turn,
				LastError:     lastError,
				CloudEligible: spec.CloudEligible,
				TaskSlice:     TaskSlice{SeedSymbols: spec.SeedSymbols, ContextFiles: spec.SeedFiles},
			}
			_ = traceWriter.Emit(map[string]any{
				"t":              "escalation_required",
				"reason":         reason,
				"attempts":       turn,
				"cloud_eligible": spec.CloudEligible,
			})
			return TaskResult{
				ExitCode:    policy.ExitEscalate,
				Status:      "escalation_required",
				Output:      reason,
				Turns:       turn,
				PrefixHash:  frozenHash,
				Compactions: compactions,
				Escalation:  esc,
			}
		}

		_ = traceWriter.Emit(map[string]any{
			"t":                    "turn_start",
			"turn":                 turn,
			"prompt_tokens":        cStats.TokensAfter,
			"compacted_this_turn":  cStats.FoldedCount,
			"frozen_prefix_hash":   frozenHash[:16],
			"rendered_prefix_hash": currentHash[:16],
		})

		resp, err := prov.Chat(ctx, prefix, msgs, maxOutput)
		if err != nil && ctx.Err() != nil {
			return ctxResult(ctx, st, turn, frozenHash, compactions)
		}
		if err != nil {
			if policy.IsViolation(err) {
				return TaskResult{
					ExitCode:    policy.ExitHarnessErr,
					Status:      "policy_violation",
					Output:      err.Error(),
					Turns:       turn,
					PrefixHash:  frozenHash,
					Compactions: compactions,
				}
			}
			if providers.IsModelNotFound(err) {
				st.reason = "model_not_found"
				return TaskResult{
					ExitCode:    policy.ExitHarnessErr,
					Status:      "model_not_found",
					Output:      err.Error(),
					Turns:       turn,
					PrefixHash:  frozenHash,
					Compactions: compactions,
				}
			}
			if providers.IsAuthError(err) {
				return TaskResult{
					ExitCode:    policy.ExitHarnessErr,
					Status:      "auth_error",
					Output:      err.Error(),
					Turns:       turn,
					PrefixHash:  frozenHash,
					Compactions: compactions,
				}
			}
			return TaskResult{
				ExitCode:    policy.ExitTaskFail,
				Status:      "provider_error",
				Output:      err.Error(),
				Turns:       turn,
				PrefixHash:  frozenHash,
				Compactions: compactions,
			}
		}

		stopReason := providers.NormalizeStopReason(resp.RawStopReason, resp.ToolCalls)
		if resp.StopReason != "" && len(resp.ToolCalls) == 0 {
			stopReason = resp.StopReason
		}

		st.usage.PromptTokens += resp.PromptTokens
		st.usage.CompletionTokens += resp.OutputTokens
		st.usage.TotalTokens = st.usage.PromptTokens + st.usage.CompletionTokens
		st.usage.CacheCreationTokens += resp.CacheCreationTokens
		st.usage.CacheReadTokens += resp.CacheReadTokens

		stopEv := map[string]any{
			"t":                 "stop",
			"turn":              turn,
			"reason":            string(stopReason),
			"raw_reason":        resp.RawStopReason,
			"provider":          resp.Provider,
			"prompt_eval_count": resp.PromptEvalCount,
			"prompt_tokens":     resp.PromptTokens,
			"output_tokens":     resp.OutputTokens,
			"total_tokens":      resp.PromptTokens + resp.OutputTokens,
			"cumulative": map[string]any{
				"prompt_tokens": st.usage.PromptTokens,
				"output_tokens": st.usage.CompletionTokens,
				"total_tokens":  st.usage.TotalTokens,
			},
		}
		if resp.CacheCreationTokens > 0 {
			stopEv["cache_creation_tokens"] = resp.CacheCreationTokens
		}
		if resp.CacheReadTokens > 0 {
			stopEv["cache_read_tokens"] = resp.CacheReadTokens
		}
		_ = traceWriter.Emit(stopEv)
		if maxTotal > 0 && st.usage.TotalTokens > maxTotal {
			st.reason = "max_total_tokens"
			return TaskResult{
				ExitCode:    policy.ExitBudget,
				Status:      "budget_exhausted",
				Output:      fmt.Sprintf("token budget exceeded: %d > %d", st.usage.TotalTokens, maxTotal),
				Turns:       turn,
				PrefixHash:  frozenHash,
				Compactions: compactions,
			}
		}
		if maxToolCalls > 0 && len(resp.ToolCalls) > maxToolCalls {
			st.reason = "max_tool_calls_per_turn"
			return TaskResult{
				ExitCode:    policy.ExitBudget,
				Status:      "budget_exhausted",
				Output:      fmt.Sprintf("turn returned %d tool calls, cap %d; rejected", len(resp.ToolCalls), maxToolCalls),
				Turns:       turn,
				PrefixHash:  frozenHash,
				Compactions: compactions,
			}
		}

		// Invariant I1: StopUnknown retries once, then exits 2 if it persists (§6 Table I1)
		if stopReason == providers.StopUnknown {
			unknownStopRetries++
			if unknownStopRetries > 1 {
				return TaskResult{
					ExitCode:    policy.ExitHarnessErr,
					Status:      "unknown_stop_reason",
					Output:      fmt.Sprintf("persistent unknown stop reason %q", resp.RawStopReason),
					Turns:       turn,
					PrefixHash:  frozenHash,
					Compactions: compactions,
				}
			}
			turn-- // retry current turn once
			continue
		}
		unknownStopRetries = 0

		// Never act on tool calls from a truncated/aborted response.
		if len(resp.ToolCalls) > 0 && providers.IsTruncatedOrAborted(resp.RawStopReason) {
			stopReason = providers.RawStopKind(resp.RawStopReason)
			resp.ToolCalls = nil
		}

		switch stopReason {
		case providers.StopError:
			return TaskResult{
				ExitCode:    policy.ExitTaskFail,
				Status:      "model_stop_error",
				Output:      resp.Content,
				Turns:       turn,
				PrefixHash:  frozenHash,
				Compactions: compactions,
			}
		case providers.StopMaxTokens:
			return TaskResult{
				ExitCode:    policy.ExitTaskFail,
				Status:      "max_tokens",
				Output:      strings.TrimRight(resp.Content, "\n") + fmt.Sprintf(maxTokensHint, maxOutput),
				Turns:       turn,
				PrefixHash:  frozenHash,
				Compactions: compactions,
			}
		case providers.StopPaused:
			return TaskResult{
				ExitCode:    policy.ExitTaskFail,
				Status:      "paused",
				Output:      resp.Content,
				Turns:       turn,
				PrefixHash:  frozenHash,
				Compactions: compactions,
			}
		}

		msgs = append(msgs, compact.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		if len(resp.ToolCalls) == 0 {
			return TaskResult{
				ExitCode:    policy.ExitOK,
				Status:      "success",
				Output:      resp.Content,
				Turns:       turn,
				PrefixHash:  frozenHash,
				Compactions: compactions,
			}
		}

		for _, tc := range resp.ToolCalls {
			if ctx.Err() != nil {
				return ctxResult(ctx, st, turn, frozenHash, compactions)
			}
			callEv := map[string]any{
				"t":         "tool_call",
				"name":      tc.Name,
				"kind":      "builtin",
				"args_hash": trace.HashArgs(tc.Arguments),
			}
			if tc.Name == "run_command" {
				if argv, ok := trace.RedactArgv(tc.Arguments); ok {
					callEv["argv"] = argv
				}
			}
			_ = traceWriter.Emit(callEv)

			// §6.1: If a tool failed with tool_unavailable >= 2 times consecutively, short-circuit
			if disabledTools[tc.Name] >= 2 {
				unavailMsg := fmt.Sprintf(`{"status":"tool_unavailable","tool":%q,"disabled":true}`, tc.Name)
				_ = traceWriter.Emit(map[string]any{
					"t":           "tool_result",
					"name":        tc.Name,
					"status":      "tool_unavailable",
					"consecutive": disabledTools[tc.Name] + 1,
				})
				msgs = append(msgs, compact.Message{
					Role:       "tool",
					Name:       tc.Name,
					ToolName:   tc.Name,
					ToolCallID: tc.ID,
					Content:    unavailMsg,
				})
				continue
			}

			res, toolErr := tools.Execute(ctx, tc.Name, tc.Arguments, pol)
			if toolErr != nil {
				// Invariant I4: Fatal policy violation exits immediately with Exit Code 2
				if policy.IsViolation(toolErr) {
					return TaskResult{
						ExitCode:    policy.ExitHarnessErr,
						Status:      "policy_violation",
						Output:      toolErr.Error(),
						Turns:       turn,
						PrefixHash:  frozenHash,
						Compactions: compactions,
					}
				}
			}

			if res.Escalation != nil {
				esc := &EscalationPayload{
					T:             "escalation_required",
					Reason:        res.Escalation.Reason,
					Attempts:      turn,
					LastError:     lastError,
					CloudEligible: spec.CloudEligible,
					TaskSlice: TaskSlice{
						SeedSymbols:    spec.SeedSymbols,
						ContextFiles:   spec.SeedFiles,
						MinimalContext: res.Escalation.MinimalContext,
						Attempted:      res.Escalation.Attempted,
					},
				}
				_ = traceWriter.Emit(map[string]any{
					"t":              "escalation_required",
					"reason":         esc.Reason,
					"attempts":       esc.Attempts,
					"cloud_eligible": esc.CloudEligible,
				})
				return TaskResult{
					ExitCode:    policy.ExitEscalate,
					Status:      "escalation_required",
					Output:      res.Output,
					Turns:       turn,
					PrefixHash:  frozenHash,
					Compactions: compactions,
					Escalation:  esc,
				}
			}

			if strings.Contains(res.Output, `"status":"tool_unavailable"`) {
				disabledTools[tc.Name]++
				_ = traceWriter.Emit(map[string]any{
					"t":           "tool_result",
					"name":        tc.Name,
					"status":      "tool_unavailable",
					"consecutive": disabledTools[tc.Name],
				})
			} else {
				delete(disabledTools, tc.Name)
				ev := map[string]any{
					"t":            "tool_result",
					"name":         tc.Name,
					"bytes_raw":    res.BytesRaw,
					"bytes_passed": res.BytesPassed,
					"truncated":    res.Truncated,
				}
				if res.ExitCode != nil {
					ev["exit_code"] = *res.ExitCode
					ev["timed_out"] = res.TimedOut
				}
				_ = traceWriter.Emit(ev)
			}

			if res.ViolationKind != "" {
				_ = traceWriter.Emit(map[string]any{
					"t":    "policy_violation_recovered",
					"kind": res.ViolationKind,
					"tool": tc.Name,
				})
			}
			if res.IsError {
				lastError = res.Output
				toolStrikes++
			} else {
				toolStrikes = 0
			}

			msgs = append(msgs, compact.Message{
				Role:       "tool",
				Name:       tc.Name,
				ToolName:   tc.Name,
				ToolCallID: tc.ID,
				Content:    res.Output,
			})
			if toolStrikes >= MaxToolStrikes {
				return TaskResult{
					ExitCode:    policy.ExitTaskFail,
					Status:      "tool_error_strikes",
					Output:      fmt.Sprintf("%d consecutive tool errors; last_error=%s", toolStrikes, lastError),
					Turns:       turn,
					PrefixHash:  frozenHash,
					Compactions: compactions,
				}
			}
		}

		if st.opts.OnCheckpoint != nil {
			st.opts.OnCheckpoint(Checkpoint{
				RunID:       st.runID,
				Spec:        spec,
				Turn:        turn,
				Msgs:        append([]compact.Message(nil), msgs...),
				Usage:       st.usage,
				Compactions: compactions,
				Baseline:    st.baseline,
			})
			_ = traceWriter.Emit(map[string]any{"t": "checkpoint", "turn": turn, "run_id": st.runID})
		}
	}

	return TaskResult{
		ExitCode:    policy.ExitTaskFail,
		Status:      "max_turns_exhausted",
		Output:      fmt.Sprintf("max turns (%d) exhausted; last_error=%s", maxTurns, lastError),
		Turns:       maxTurns,
		PrefixHash:  frozenHash,
		Compactions: compactions,
	}
}

func buildInitialUserPrompt(spec TaskSpec) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(spec.Instruction))
	if len(spec.SeedFiles) > 0 {
		b.WriteString("\n\nSeed files: ")
		b.WriteString(strings.Join(spec.SeedFiles, ", "))
	}
	if len(spec.SeedSymbols) > 0 {
		b.WriteString("\nSeed symbols: ")
		b.WriteString(strings.Join(spec.SeedSymbols, ", "))
	}
	if strings.TrimSpace(spec.ErrorTrace) != "" {
		b.WriteString("\nError trace:\n")
		b.WriteString(strings.TrimSpace(spec.ErrorTrace))
	}
	return b.String()
}

const maxTokensHint = "\n[loom: response truncated at max_output_tokens=%d; raise --max-output-tokens or LOOM_MAX_OUTPUT_TOKENS, or prefer smaller edits]"

// resolveMaxOutputTokens picks the per-response output cap: task spec, then
// LOOM_MAX_OUTPUT_TOKENS, then providers.DefaultMaxOutputTokens. (The CLI flag
// is overlaid onto the spec before this runs, so flag > spec > env > default.)
func resolveMaxOutputTokens(fromSpec int) int {
	if fromSpec > 0 {
		return fromSpec
	}
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LOOM_MAX_OUTPUT_TOKENS"))); err == nil && v > 0 {
		return v
	}
	return providers.DefaultMaxOutputTokens
}
