package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/choilab-jefferson/nexus.loom/pkg/engine"
	"github.com/choilab-jefferson/nexus.loom/pkg/policy"
	"github.com/choilab-jefferson/nexus.loom/pkg/providers"
	"github.com/choilab-jefferson/nexus.loom/pkg/stm"
	"github.com/choilab-jefferson/nexus.loom/pkg/tools"
	"github.com/choilab-jefferson/nexus.loom/pkg/trace"
)

type multiStringFlag []string

func (m *multiStringFlag) String() string {
	return strings.Join(*m, ",")
}

func (m *multiStringFlag) Set(val string) error {
	*m = append(*m, val)
	return nil
}

func main() {
	os.Exit(runMain(os.Args[1:]))
}

const usageText = `loom - bounded single-agent coding harness

Usage:
  loom -p "<instruction>" [flags]     one-shot task
  loom run --task-file task.json      task from a JSON file
  loom resume <trace.jsonl>           resume an interrupted run
  loom capabilities --json            print tools/providers as JSON
  loom version

Core flags:
  -p string            task instruction
  --provider string    auto|ollama|openai|anthropic|gemini|google-agent-platform|typesafe|mock
                       (default auto: first of ANTHROPIC/GEMINI/OPENAI key found, else a running Ollama)
  --model string       model identifier
  --workspace dir      workspace root (default ".")
  --max-turns N        maximum agent turns (default 8)
  --allow-cmd "cmd.."  allowlisted command prefix (repeatable)
  --max-total-tokens N, --deadline 90s, --max-tool-calls N   budgets (0 = unlimited)
  --trace file         JSONL trace path
  --json               emit result as JSON
  --no-agents-md       do not load AGENTS.md into the prompt

Keys: ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY/GOOGLE_API_KEY, TYPESAFE_API_KEY.

Nexus integration (optional; not needed standalone):
  --stm / --no-stm     inject / skip the Nexus STM brief; "loom stm" inspects it
  --phi                local-only (PHI) model confinement
  ~/.config/nexus/tokens.env is read as a key fallback only if it exists;
  IKB_MCP_TOKEN from it is injected only into "ikb" subprocesses.

Exit codes: 0 ok, 1 task failed, 2 harness/config error, 3 escalate, 4 budget, 130 interrupted.
`

func printUsage() { fmt.Fprint(os.Stderr, usageText) }

func runMain(args []string) int {
	if len(args) == 0 {
		printUsage()
		return policy.ExitHarnessErr
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "-help" || args[0] == "help" {
		printUsage()
		return policy.ExitOK
	}
	if len(args) > 0 {
		switch args[0] {
		case "capabilities":
			return runCapabilities(args[1:])
		case "stm":
			return runSTM(args[1:])
		case "run":
			return runSubcommand(args[1:])
		case "resume":
			return runResume(args[1:])
		case "version", "--version":
			return runVersionCmd()
		}
	}
	return runPromptMode(args)
}

// budgetFlags registers the budget flags shared by run, prompt mode and resume.
type budgetFlags struct {
	maxTotalTokens *int
	deadline       *time.Duration
	maxToolCalls   *int
}

func addBudgetFlags(fs *flag.FlagSet) budgetFlags {
	return budgetFlags{
		maxTotalTokens: fs.Int("max-total-tokens", 0, "cumulative prompt+completion token budget (0 = unlimited)"),
		deadline:       fs.Duration("deadline", 0, "wall-clock deadline for the run, e.g. 90s (0 = none)"),
		maxToolCalls:   fs.Int("max-tool-calls", 0, "reject a model turn returning more tool calls than this (0 = unlimited)"),
	}
}

// apply overlays explicitly-set (non-zero) budget flags onto the task constraints.
func (b budgetFlags) apply(spec *engine.TaskSpec) {
	if *b.maxTotalTokens > 0 {
		spec.Constraints.MaxTotalTokens = *b.maxTotalTokens
	}
	if *b.deadline > 0 {
		spec.Constraints.DeadlineMs = int(b.deadline.Milliseconds())
	}
	if *b.maxToolCalls > 0 {
		spec.Constraints.MaxToolCallsPerTurn = *b.maxToolCalls
	}
}

func runCapabilities(args []string) int {
	fs := flag.NewFlagSet("capabilities", flag.ContinueOnError)
	jsonOut := fs.Bool("json", true, "emit capabilities as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return policy.ExitOK
		}
		fmt.Fprintf(os.Stderr, "loom capabilities error: %v\n", err)
		return policy.ExitHarnessErr
	}
	_ = jsonOut

	var toolNames []string
	for _, s := range tools.BuiltinSchemas() {
		toolNames = append(toolNames, s.Name)
	}
	caps := map[string]any{
		"schema_version":     1,
		"harness":            "loom",
		"spec_version":       "0.1",
		"invariants":         []string{"I1", "I2", "I3", "I4"},
		"builtin_tools":      toolNames,
		"providers":          []string{"ollama", "openai", "anthropic", "anthropic-api", "gemini", "gemini-api", "google-agent-platform", "typesafe", "mock"},
		"stm_integration":    true,
		"agents_md_autoload": true,
		"exit_codes": map[string]int{
			"ok":                      policy.ExitOK,
			"task_failure":            policy.ExitTaskFail,
			"harness_or_policy_error": policy.ExitHarnessErr,
			"escalation_required":     policy.ExitEscalate,
			"budget_exhausted":        policy.ExitBudget,
			"interrupted":             policy.ExitInterrupted,
		},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(caps); err != nil {
		return policy.ExitHarnessErr
	}
	return policy.ExitOK
}

func runSTM(args []string) int {
	fs := flag.NewFlagSet("stm", flag.ContinueOnError)
	workspace := fs.String("workspace", ".", "workspace directory")
	jsonOut := fs.Bool("json", false, "emit STM brief as JSON")
	phiFlag := fs.Bool("phi", false, "redact STM node summaries under PHI policy")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return policy.ExitOK
		}
		fmt.Fprintf(os.Stderr, "loom stm flag error: %v\n", err)
		return policy.ExitHarnessErr
	}

	phiClamp := *phiFlag || strings.EqualFold(os.Getenv("NEXUS_SENSITIVITY"), "phi")
	brief, err := stm.LoadBrief(*workspace, phiClamp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loom stm error: %v\n", err)
		return policy.ExitHarnessErr
	}

	if *jsonOut {
		raw, err := json.MarshalIndent(brief, "", "  ")
		if err != nil {
			return policy.ExitHarnessErr
		}
		fmt.Println(string(raw))
		return policy.ExitOK
	}
	if fenced := brief.FormatFenced(); fenced != "" {
		fmt.Println(fenced)
	}
	return policy.ExitOK
}

func runSubcommand(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	agent := fs.String("agent", "coder", "agent profile name")
	taskFile := fs.String("task-file", "", "path to task.json")
	workspace := fs.String("workspace", ".", "workspace directory")
	providerName := fs.String("provider", "auto", "model provider (auto|ollama|openai|anthropic|gemini|google-agent-platform|typesafe|mock)")
	modelName := fs.String("model", "", "model identifier")
	tracePath := fs.String("trace", "", "JSONL trace file path")
	nonInteractive := fs.Bool("non-interactive", true, "unattended execution mode")
	noDaemon := fs.Bool("no-daemon", false, "force in-process tool broker")
	noAgentsMD := fs.Bool("no-agents-md", false, "disable automatic AGENTS.md loading into prefix")
	stmFlag := fs.Bool("stm", false, "opt in to injecting Nexus STM brief (<nexus_stm>) for session continuity")
	noSTM := fs.Bool("no-stm", false, "explicitly disable Nexus STM user-turn injection")
	phiFlag := fs.Bool("phi", false, "enforce PHI local-only model confinement")
	jsonFlag := fs.Bool("json", false, "emit TaskResult as JSON")
	bf := addBudgetFlags(fs)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return policy.ExitOK
		}
		fmt.Fprintf(os.Stderr, "loom run flag error: %v\n", err)
		return policy.ExitHarnessErr
	}
	_ = nonInteractive
	_ = noDaemon

	if strings.TrimSpace(*taskFile) == "" {
		fmt.Fprintln(os.Stderr, "loom run error: --task-file is required")
		return policy.ExitHarnessErr
	}

	spec, err := engine.LoadTaskFile(*taskFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loom run error loading task file: %v\n", err)
		return policy.ExitHarnessErr
	}
	if spec.Agent == "" {
		spec.Agent = *agent
	}
	bf.apply(&spec)

	return executeSpec(spec, *workspace, *providerName, *modelName, *tracePath, *phiFlag, *noAgentsMD, *stmFlag, *noSTM, *jsonFlag)
}

func runPromptMode(args []string) int {
	fs := flag.NewFlagSet("loom", flag.ContinueOnError)
	prompt := fs.String("p", "", "single-shot task instruction")
	workspace := fs.String("workspace", ".", "workspace directory")
	providerName := fs.String("provider", "auto", "model provider (auto|ollama|openai|anthropic|gemini|google-agent-platform|typesafe|mock)")
	modelName := fs.String("model", "", "model identifier")
	maxTurns := fs.Int("max-turns", 8, "maximum agent turns")
	maxContextTokens := fs.Int("max-context-tokens", 28000, "pre-send compaction token ceiling")
	tracePath := fs.String("trace", "", "JSONL trace file path")
	noAgentsMD := fs.Bool("no-agents-md", false, "disable automatic AGENTS.md loading into prefix")
	stmFlag := fs.Bool("stm", false, "opt in to injecting Nexus STM brief (<nexus_stm>) for session continuity")
	noSTM := fs.Bool("no-stm", false, "explicitly disable Nexus STM user-turn injection")
	phiFlag := fs.Bool("phi", false, "enforce PHI local-only model confinement")
	jsonFlag := fs.Bool("json", false, "emit TaskResult as JSON")
	bf := addBudgetFlags(fs)
	var allowCmds multiStringFlag
	fs.Var(&allowCmds, "allow-cmd", "allowlisted command prefix (repeatable)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return policy.ExitOK
		}
		fmt.Fprintf(os.Stderr, "loom flag error: %v\n", err)
		return policy.ExitHarnessErr
	}
	if strings.TrimSpace(*prompt) == "" {
		printUsage()
		return policy.ExitHarnessErr
	}

	var parsedAllow engine.AllowCommandList
	for _, raw := range allowCmds {
		fields := strings.Fields(raw)
		if len(fields) > 0 {
			parsedAllow = append(parsedAllow, fields)
		}
	}

	spec := engine.TaskSpec{
		SchemaVersion:    1,
		TaskID:           "cli-oneshot",
		Agent:            "worker",
		Instruction:      *prompt,
		MaxContextTokens: *maxContextTokens,
		Constraints: engine.TaskConstraints{
			MaxTurns:      *maxTurns,
			AllowCommands: parsedAllow,
		},
	}
	bf.apply(&spec)

	return executeSpec(spec, *workspace, *providerName, *modelName, *tracePath, *phiFlag, *noAgentsMD, *stmFlag, *noSTM, *jsonFlag)
}

func executeSpec(spec engine.TaskSpec, workspace, providerName, modelName, tracePath string, phiFlag, noAgentsMD, stmFlag, noSTM, jsonOut bool) int {
	if fi, err := os.Stat(workspace); err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "loom error: invalid --workspace %q: not an accessible directory\n", workspace)
		return policy.ExitHarnessErr
	}
	pol := policy.DefaultPolicyConfig(workspace)
	pol.PHIMode = phiFlag || strings.EqualFold(os.Getenv("NEXUS_SENSITIVITY"), "phi")

	if len(spec.Constraints.AllowCommands) > 0 {
		taskCmds := [][]string(spec.Constraints.AllowCommands)
		pol.AllowCommands = policy.EffectiveAllowCommands(taskCmds)
	}

	// Auto-load AGENTS.md into frozen PrefixHeader.RepoSummary (Invariant I2)
	if !noAgentsMD && os.Getenv("NEXUS_NO_AGENTS_MD") != "1" && strings.TrimSpace(spec.RepoSummary) == "" {
		if _, content := stm.DiscoverAgentsMD(workspace); content != "" {
			spec.RepoSummary = content
		}
	}

	// Optional Nexus STM injection: Loom runs independently by default; STM is injected only when
	// explicitly requested via --stm, task.json "include_stm": true, or NEXUS_STM_INJECT=1.
	wantSTM := (stmFlag || spec.IncludeSTM || os.Getenv("NEXUS_STM_INJECT") == "1") && !noSTM && os.Getenv("NEXUS_STM_INJECT") != "0"
	if wantSTM {
		if brief, err := stm.LoadBrief(workspace, pol.PHIMode); err == nil && brief != nil {
			if fenced := brief.FormatFenced(); fenced != "" {
				spec.Instruction = strings.TrimSpace(spec.Instruction) + "\n\n" + fenced
			}
		}
	}

	prov, err := buildProvider(providerName, modelName, pol.PHIMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loom provider error: %v\n", err)
		return policy.ExitHarnessErr
	}

	meta := runMeta{Workspace: workspace, Provider: providerName, Model: modelName, PHI: pol.PHIMode, Trace: tracePath}
	return runAndReport(spec, pol, prov, meta, nil, jsonOut)
}

// runMeta is stored beside the checkpoint so `loom resume` can rebuild the run.
type runMeta struct {
	Workspace string `json:"workspace"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	PHI       bool   `json:"phi"`
	Trace     string `json:"trace"`
}

type ckptFile struct {
	Meta       runMeta           `json:"meta"`
	Checkpoint engine.Checkpoint `json:"checkpoint"`
}

func ckptPath(tracePath string) string { return tracePath + ".ckpt" }

// writeCkpt atomically replaces the checkpoint file (0600).
func writeCkpt(meta runMeta, cp engine.Checkpoint) {
	raw, err := json.Marshal(ckptFile{Meta: meta, Checkpoint: cp})
	if err != nil {
		return
	}
	dst := ckptPath(meta.Trace)
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".ckpt-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Chmod(tmp.Name(), 0o600) != nil || os.Rename(tmp.Name(), dst) != nil {
		os.Remove(tmp.Name())
	}
}

func runAndReport(spec engine.TaskSpec, pol *policy.PolicyConfig, prov providers.Provider, meta runMeta, resume *engine.Checkpoint, jsonOut bool) int {
	tw, err := trace.NewFileWriter(meta.Trace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loom trace error: %v\n", err)
		return policy.ExitHarnessErr
	}
	defer tw.Close()

	// SIGINT/SIGTERM cancel the context; the engine then emits a done event before returning.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := engine.RunOptions{Resume: resume}
	if meta.Trace != "" {
		opts.OnCheckpoint = func(cp engine.Checkpoint) { writeCkpt(meta, cp) }
	}
	res := engine.RunTaskWithOptions(ctx, spec, pol, prov, tw, opts)
	if meta.Trace != "" && res.Status == "success" {
		os.Remove(ckptPath(meta.Trace))
	}

	if res.ExitCode == policy.ExitEscalate && res.Escalation != nil {
		raw, _ := json.Marshal(res.Escalation)
		fmt.Println(string(raw))
		return policy.ExitEscalate
	}

	if jsonOut {
		raw, _ := json.Marshal(res)
		fmt.Println(string(raw))
	} else if res.Output != "" {
		if res.ExitCode == policy.ExitOK {
			fmt.Println(res.Output)
		} else {
			fmt.Fprintln(os.Stderr, res.Output)
		}
	}

	return res.ExitCode
}

func runResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	jsonFlag := fs.Bool("json", false, "emit TaskResult as JSON")
	bf := addBudgetFlags(fs)
	// Go's flag package stops at the first positional; accept the trace path on either side.
	var pos []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			fmt.Fprintf(os.Stderr, "loom resume flag error: %v\n", err)
			return policy.ExitHarnessErr
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: loom resume <trace-file> [--max-total-tokens N] [--deadline D] [--max-tool-calls N]")
		return policy.ExitHarnessErr
	}
	raw, err := os.ReadFile(ckptPath(pos[0]))
	if err != nil {
		fmt.Fprintf(os.Stderr, "loom resume error: no checkpoint for %s: %v\n", pos[0], err)
		return policy.ExitHarnessErr
	}
	var cf ckptFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		fmt.Fprintf(os.Stderr, "loom resume error: bad checkpoint: %v\n", err)
		return policy.ExitHarnessErr
	}
	cf.Meta.Trace = pos[0]
	spec := cf.Checkpoint.Spec
	bf.apply(&spec)

	pol := policy.DefaultPolicyConfig(cf.Meta.Workspace)
	pol.PHIMode = cf.Meta.PHI
	if len(spec.Constraints.AllowCommands) > 0 {
		taskCmds := [][]string(spec.Constraints.AllowCommands)
		pol.AllowCommands = policy.EffectiveAllowCommands(taskCmds)
	}
	prov, err := buildProvider(cf.Meta.Provider, cf.Meta.Model, pol.PHIMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loom provider error: %v\n", err)
		return policy.ExitHarnessErr
	}
	return runAndReport(spec, pol, prov, cf.Meta, &cf.Checkpoint, *jsonFlag)
}

func buildProvider(name, model string, phiMode bool) (providers.Provider, error) {
	name, err := providers.ResolveProviderName(name, providers.OllamaReachable)
	if err != nil {
		return nil, err
	}
	switch name {
	case "ollama":
		return providers.NewOllamaProvider("", model, phiMode), nil
	case "openai":
		p := providers.NewOpenAIProvider("", "", model, phiMode)
		return needKey(p, p.APIKey, phiMode, "OPENAI_API_KEY")
	case "anthropic", "anthropic-api":
		p := providers.NewAnthropicProvider("", "", model, phiMode)
		return needKey(p, p.APIKey, phiMode, "ANTHROPIC_API_KEY")
	case "gemini", "gemini-api":
		p := providers.NewGeminiProvider("", "", model, phiMode)
		return needKey(p, p.APIKey, phiMode, "GEMINI_API_KEY or GOOGLE_API_KEY")
	case "google-agent-platform", "vertex":
		p := providers.NewGoogleAgentPlatformProvider("", "", model, phiMode)
		return needKey(p, p.APIKey, phiMode, "NEXUS_GOOGLE_AGENT_API_KEY")
	case "typesafe", "jev":
		p := providers.NewTypeSafeProvider("", "", model, phiMode)
		return needKey(p, p.APIKey, phiMode, "TYPESAFE_API_KEY")
	case "mock":
		return providers.NewScriptedMockProvider(providers.Response{
			Content:       "mock task complete",
			RawStopReason: "stop",
		}), nil
	default:
		return nil, &policy.ViolationError{Reason: fmt.Sprintf("unknown provider %q", name)}
	}
}

// needKey turns a missing API key into a config error (exit 2) at startup
// instead of a task failure (exit 1) on the first Chat call. PHI mode keeps
// its own violation path.
func needKey(p providers.Provider, key string, phiMode bool, envNames string) (providers.Provider, error) {
	if !phiMode && strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("missing API key: set %s in the environment (optionally the Nexus token file ~/.config/nexus/tokens.env), or choose another --provider (e.g. ollama, mock)", envNames)
	}
	return p, nil
}
