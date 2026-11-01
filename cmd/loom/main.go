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
	"runtime"
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
  loom models --provider P [--json]   list model ids served to your key (network; opt-in)
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
  --max-output-tokens N per-response output cap (flag > spec > LOOM_MAX_OUTPUT_TOKENS > 8192)
  --pass-env NAME[,NAME|PREFIX*]  extra env for run_command children (repeatable; secret names only by exact match)
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
		case "models":
			return runModels(args[1:])
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
	maxOutput      *int
	passEnv        *multiStringFlag
}

func addBudgetFlags(fs *flag.FlagSet) budgetFlags {
	var passEnv multiStringFlag
	fs.Var(&passEnv, "pass-env", "extra env var(s) for run_command children: NAME[,NAME|PREFIX*] (repeatable; secrets only by exact name)")
	return budgetFlags{
		passEnv:        &passEnv,
		maxOutput:      fs.Int("max-output-tokens", 0, "per-response output token cap (0 = spec, then LOOM_MAX_OUTPUT_TOKENS, then 8192)"),
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
	if *b.maxOutput > 0 {
		spec.Constraints.MaxOutputTokens = *b.maxOutput
	}
	for _, raw := range *b.passEnv {
		for _, name := range strings.Split(raw, ",") {
			name = strings.TrimSpace(name)
			if name != "" && !containsStr(spec.Constraints.PassEnv, name) {
				spec.Constraints.PassEnv = append(spec.Constraints.PassEnv, name)
			}
		}
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// parseAllowCmds turns repeated --allow-cmd values into command prefixes.
func parseAllowCmds(raw []string) engine.AllowCommandList {
	var out engine.AllowCommandList
	for _, r := range raw {
		if fields := strings.Fields(r); len(fields) > 0 {
			out = append(out, fields)
		}
	}
	return out
}

// applyPolicyExtras copies spec-level policy knobs onto the policy config and
// warns on stderr when a secret-looking env name is explicitly passed through.
func applyPolicyExtras(spec engine.TaskSpec, pol *policy.PolicyConfig) {
	if len(spec.Constraints.AllowCommands) > 0 {
		pol.AllowCommands = policy.EffectiveAllowCommands([][]string(spec.Constraints.AllowCommands))
	}
	pol.PassEnv = spec.Constraints.PassEnv
	for _, name := range pol.PassEnv {
		if policy.IsSecretEnvName(name) {
			fmt.Fprintf(os.Stderr, "loom warning: --pass-env exposes secret-looking variable %s to run_command children\n", name)
		}
	}
}

// exitCodeNames is the machine-readable exit-code contract emitted by
// `loom capabilities --json`; TestCapabilitiesExitCodesParity pins it to policy.Exit*.
var exitCodeNames = map[string]int{
	"ok":                      policy.ExitOK,
	"task_failure":            policy.ExitTaskFail,
	"harness_or_policy_error": policy.ExitHarnessErr,
	"escalation_required":     policy.ExitEscalate,
	"budget_exhausted":        policy.ExitBudget,
	"interrupted":             policy.ExitInterrupted,
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

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(capabilitiesMap()); err != nil {
		return policy.ExitHarnessErr
	}
	return policy.ExitOK
}

// capabilitiesMap is the `loom capabilities --json` document (docs/contract.md).
func capabilitiesMap() map[string]any {
	var toolNames []string
	for _, s := range tools.BuiltinSchemas() {
		toolNames = append(toolNames, s.Name)
	}
	return map[string]any{
		"schema_version":     1,
		"version":            version,
		"commit":             commit,
		"harness":            "loom",
		"spec_version":       "0.1",
		"invariants":         []string{"I1", "I2", "I3", "I4"},
		"builtin_tools":      toolNames,
		"providers":          []string{"ollama", "openai", "anthropic", "anthropic-api", "gemini", "gemini-api", "google-agent-platform", "typesafe", "mock"},
		"subcommands":        []string{"run", "resume", "capabilities", "models", "stm", "version"},
		"default_models":     defaultModels(),
		"stm_integration":    true,
		"agents_md_autoload": true,
		"exit_codes":         exitCodeNames,
	}
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
	var allowCmds multiStringFlag
	fs.Var(&allowCmds, "allow-cmd", "allowlisted command prefix, unioned with task.json constraints.allow_commands (repeatable)")

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
	spec.Constraints.AllowCommands = append(spec.Constraints.AllowCommands, parseAllowCmds(allowCmds)...)

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

	parsedAllow := parseAllowCmds(allowCmds)

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

	applyPolicyExtras(spec, pol)

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
	release, lerr := acquireRunLocks(meta.Workspace, meta.Trace)
	if lerr != nil {
		fmt.Fprintln(os.Stderr, lerr)
		return policy.ExitHarnessErr
	}
	defer release()
	tw, err := trace.NewFileWriter(meta.Trace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loom trace error: %v\n", err)
		return policy.ExitHarnessErr
	}
	defer tw.Close()

	// SIGINT/SIGTERM cancel the context; the engine then emits a done event before returning.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := engine.RunOptions{Resume: resume, TracePath: meta.Trace}
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
		fmt.Fprintln(os.Stderr, "usage: loom resume <trace-file> [--max-total-tokens N] [--deadline D] [--max-tool-calls N] [--max-output-tokens N] [--pass-env NAME]")
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
	applyPolicyExtras(spec, pol)
	prov, err := buildProvider(cf.Meta.Provider, cf.Meta.Model, pol.PHIMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loom provider error: %v\n", err)
		return policy.ExitHarnessErr
	}
	return runAndReport(spec, pol, prov, cf.Meta, &cf.Checkpoint, *jsonFlag)
}

func buildProvider(name, model string, phiMode bool) (providers.Provider, error) {
	name, err := providers.ResolveProviderNamePHI(name, providers.OllamaReachable, phiMode)
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

// defaultModels is the per-provider default model id (docs/contract.md); LOOM_MODEL overrides at run time.
func defaultModels() map[string]string {
	return map[string]string{
		"ollama":                providers.DefaultOllamaModel(runtime.GOOS),
		"openai":                "gpt-4o-mini",
		"anthropic":             "claude-sonnet-5-5",
		"gemini":                "gemini-3.8-flash",
		"google-agent-platform": "gemini-3.8-flash",
		"typesafe":              "jev-latest",
	}
}

// runModels implements `loom models --provider P [--json]`: the only path that lists models over the network.
func runModels(args []string) int {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	provider := fs.String("provider", "", "provider: ollama|openai|anthropic|gemini")
	jsonOut := fs.Bool("json", false, "emit {provider, models} as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return policy.ExitOK
		}
		fmt.Fprintf(os.Stderr, "loom models flag error: %v\n", err)
		return policy.ExitHarnessErr
	}
	if *provider == "" {
		fmt.Fprintln(os.Stderr, "loom models error: --provider is required (ollama|openai|anthropic|gemini)")
		return policy.ExitHarnessErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ids, err := providers.ListModels(ctx, *provider)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loom models error: %v\n", err)
		return policy.ExitHarnessErr
	}
	if *jsonOut {
		if ids == nil {
			ids = []string{}
		}
		raw, _ := json.MarshalIndent(map[string]any{"schema_version": 1, "provider": *provider, "models": ids}, "", "  ")
		fmt.Println(string(raw))
		return policy.ExitOK
	}
	for _, id := range ids {
		fmt.Println(id)
	}
	return policy.ExitOK
}
