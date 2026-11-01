# Loom v0.1 — Technical Specification for the Nexus Local & Direct-API Leaf Execution Harness

> Status: Implemented in Go 1.22+ (`nexus.loom`, zero external module dependencies).
> Scope: Single-binary in-process leaf execution harness (`cmd/loom`, `pkg/{policy,compact,tools,providers,stm,engine,trace}`). No resident `loomd` daemon.

---

## 1. Design Philosophy & Scope

Loom v0.1 is the Nexus in-house, zero-dependency Go leaf execution harness designed to run bounded coding tasks inside an isolated workspace or git worktree.

1. **Single Static Go Binary (`CGO_ENABLED=0`)**: Eliminates Python/Node CLI boot overhead and external MCP subprocess cold start by embedding first-order filesystem, search, and command execution tools directly in-process.
2. **No Resident Sidecar (`loomd` Omitted)**: All tools execute in-process with sub-millisecond startup. Model KV-cache residency is delegated to the serving engine (`keep_alive: -1` on Ollama; ephemeral prompt caching on Anthropic) paired with strict prefix immutability (Invariant I2).
3. **Dual Model Transport + Nexus Token Store Integration**: Supports local open-weight models via Ollama alongside direct HTTP API clients (`openai`, `anthropic`/`anthropic-api`, `gemini`/`gemini-api`, `google-agent-platform`/`vertex`, `typesafe`/`jev`) and a deterministic `mock` replay provider. Call-time API keys are resolved directly from `$NEXUS_CONFIG_HOME/tokens.env` (`~/.config/nexus/tokens.env`) and scrubbed from all child processes spawned via `run_command`. Under `PHIMode`, cloud and non-local endpoints are hard-blocked at the policy layer.
4. **Automatic `AGENTS.md` Prefix Freezing & Nexus STM Injection (`pkg/stm`)**: Automatically discovers `AGENTS.md` (walking up from worktrees/submodules) to populate `PrefixHeader.RepoSummary` (Invariant I2), and injects the live Nexus System STM brief (`~/.local/share/nexus/stm/stm-<pid>`) into the initial user turn inside a `<nexus_stm>` safety fence without invalidating the frozen prefix cache.

---

## 2. Core Engine Invariants (I1–I4)

| Invariant | Name | Rule & Enforcement |
|---|---|---|
| **I1** | Bounded Single-Agent Leaf Loop & `StopReason` Normalization | Loom is strictly a leaf executor (`max_turns` bounded, no recursive sub-agent dispatch tool). Provider stop strings are normalized to 5 canonical kinds (`end_turn`, `tool_use`, `max_tokens`, `error`, `unknown`). If `len(ToolCalls) > 0`, `StopToolUse` is forced regardless of raw vendor `finish_reason`. `StopUnknown` retries once, then exits `2` if persistent. |
| **I2** | Prefix Immutability | `PrefixHeader` (`SystemPrompt` + canonical sorted `ToolSchemasJSON` + `RepoSummary` from `AGENTS.md`) is frozen at run start. Its SHA-256 digest (`PrefixHash()`) is verified before every turn and logged in JSONL traces (`frozen_prefix_hash`, `rendered_prefix_hash`). Volatile state such as Nexus STM is placed in the initial user turn, never the system prefix. Any mid-run mutation exits `2`. |
| **I3** | Prefix-Preserving Pre-Send Compaction | Before every `Provider.Chat` call, `CompactMessages` estimates total context tokens (`max(1, len(s)/4)`). When tokens exceed `maxContextTokens`, older `tool` messages outside the `keepRecent` window are deterministically folded into `[loom:folded turn=N sha256=... bytes=... lines=... preview=...]` while preserving `PrefixHeader`, the initial user task prompt, and salient test/linter error lines (`FAIL`, `ERROR`, `AssertionError`, `panic`). |
| **I4** | Capability Intersection & Workspace Confinement | Every file operation resolves symlinks (`ResolveScopedPath` via `filepath.EvalSymlinks` walk) and blocks paths escaping `Workspace` or matching `DenyGlobs` (`.git/**`, `.env*`). Command execution (`run_command`) forbids shell wrappers (`sh`, `bash`, `zsh`, `eval`, `make`, `npm`, `npx`, `uvx`, `sudo`), passes only an allowlisted child environment (everything else dropped), resolves argv[0] via `exec.LookPath` and rejects workspace binaries, bounds timeout (300s) and output (1 MiB), kills the process group; `run_command` is NOT a sandbox (child keeps the user's filesystem/network access), and validates `argv` token prefixes against `IntersectAllowCommands(global, agent, task)` (global/agent layers from `LOOM_GLOBAL_ALLOW_COMMANDS` / `LOOM_AGENT_ALLOW_COMMANDS`, `;`-separated). Path escape, symlink writes and PHI cloud refusal return `*policy.ViolationError` (Kind `fatal`) and exit immediately with code `2`; command-policy violations (allowlist mismatch, shell wrapper, interpreter eval flag) are recoverable: returned to the model as a tool error (trace `policy_violation_recovered`, counted toward the tool-error strike limit). |

---

## 3. Package Layout

```text
loom/
├── go.mod                      # module github.com/choilab-jefferson/nexus.loom (Go 1.22+, zero external dependencies)
├── Makefile                    # build, test, clean targets
├── .gitlab-ci.yml              # GitLab CI lint (gofmt/go vet) and test/build pipeline
├── cmd/loom/main.go            # CLI entrypoint (run, -p, stm, capabilities)
└── pkg/
    ├── policy/                 # Invariant I4: symlink scope, allow_commands intersection, argv validation, tokens.env reader
    │   ├── policy.go
    │   └── policy_test.go
    ├── compact/                # Invariants I2 & I3: PrefixHeader SHA-256 + pre-send token folding
    │   ├── compact.go
    │   └── compact_test.go
    ├── tools/                  # §4 Built-in tools + canonical sorted JSON schemas + subprocess env scrubbing
    │   ├── tools.go
    │   └── tools_test.go
    ├── providers/              # §5 Ollama, OpenAI, Anthropic, Gemini, GoogleAgentPlatform, TypeSafe, and ScriptedMock providers
    │   ├── providers.go
    │   └── providers_test.go
    ├── stm/                    # §6 AGENTS.md discovery + Nexus System STM brief loader (<nexus_stm>)
    │   ├── stm.go
    │   └── stm_test.go
    ├── trace/                  # §8 JSONL event stream writer
    │   └── trace.go
    └── engine/                 # Bounded leaf execution loop + task.json parser
        ├── engine.go
        └── engine_test.go
```

---

## 4. Built-in Tools & Subprocess Secret Scrubbing (`pkg/tools`)

All tool schemas are deterministically sorted by name in `CanonicalToolSchemasJSON()` so `PrefixHeader.PrefixHash()` never varies across invocations.

| Tool | Parameters | Behavior & Policy Gate |
|---|---|---|
| `read_file` | `path`, `start_line`, `end_line` | Resolves path via `ResolveScopedPath(..., write=false)`, enforces `MaxReadLines` (default 400) and `MaxOutputBytes` (default 16 KiB). |
| `list_dir` | `path` | Lists directory entries inside workspace, omitting `.git` and `.env*`. |
| `search` | `pattern`, `path`, `max_matches` | Pure Go `regexp` walk confined to workspace, skipping `.git`, `.worktrees`, `node_modules`, denied globs, and binary files. |
| `edit_file` | `path`, `target`, `replacement` | Resolves path via `ResolveScopedPath(..., write=true)` and replaces exact `target` with `replacement`. |
| `write_file` | `path`, `content` | Resolves path via `ResolveScopedPath(..., write=true)`, creates parent directories inside workspace, and writes `content`. |
| `run_command` | `argv []string`, `timeout_s int` | Validates `argv` via `policy.ValidateCommandArgv` and executes `exec.CommandContext` with `Dir = Workspace` and no shell (`shell=false`). Scrubs call-time API keys (`NEXUS_ANTHROPIC_API_KEY`, `NEXUS_GEMINI_API_KEY`, `NEXUS_GOOGLE_AGENT_API_KEY`, `TYPESAFE_API_KEY`, etc.) from `cmd.Env`. When `filepath.Base(argv[0]) == "ikb"`, injects `IKB_MCP_TOKEN` / `IKB_API_TOKEN` from `~/.config/nexus/tokens.env`. |
| `escalate` | `reason`, `minimal_context`, `attempted []string` | Terminates the run with Exit Code `3` and emits a structured `escalation_required` JSON payload on stdout. |

---

## 5. Model Providers & Nexus Token Store (`pkg/providers`)

### 5.1 Token Store Resolution (`~/.config/nexus/tokens.env`)

`ReadNexusTokenStore()` reads `$NEXUS_CONFIG_HOME/tokens.env` (default `~/.config/nexus/tokens.env`). `ResolveAPIKey(keys...)` checks `os.Getenv` first (standard name, then the `NEXUS_*` alias) and falls back to the token store. All HTTP errors are passed through `RedactSecret(msg, secret)` to scrub raw keys and `AIza...` / `sk-...` tokens.

### 5.2 Provider Implementations

1. **`OllamaProvider` (`ollama`)**: Calls `POST /api/chat` with `stream: false`, `keep_alive: -1`, and fixed `options.num_ctx: 32768`. Under `PHIMode`, verifies the endpoint resolves to loopback or RFC1918 private IP (`IsLocalEndpoint`).
2. **`OpenAIProvider` (`openai`)**: Calls `POST /v1/chat/completions` using `ResolveAPIKey("NEXUS_OPENAI_API_KEY", "OPENAI_API_KEY", "LOOM_API_KEY")`. Refused under `PHIMode` unless pointed at a local loopback/private endpoint.
3. **`AnthropicProvider` (`anthropic`, `anthropic-api`)**: Resolves `NEXUS_ANTHROPIC_API_KEY` / `ANTHROPIC_API_KEY` (default model `"claude-sonnet-5-5"`). Calls `POST https://api.anthropic.com/v1/messages` with `cache_control: {"type": "ephemeral"}` on the frozen system block and tool schema list, and full multi-turn `tool_use` / `tool_result` content block serialization. Always refused under `PHIMode`.
4. **`GeminiProvider` (`gemini`, `gemini-api`)**: Resolves `NEXUS_GEMINI_API_KEY` / `GEMINI_API_KEY` and sends `"x-goog-api-key"` in the HTTP header to `POST /v1beta/models/{model}:generateContent` (default `"gemini-3.8-flash"`). Preserves `ThoughtSignature` on `functionCall` parts across multi-turn tool invocations, serializes tool results as `functionResponse`, and skips `thought: true` reasoning parts when extracting text output. Always refused under `PHIMode`.
5. **`GoogleAgentPlatformProvider` (`google-agent-platform`, `vertex`)**: Resolves `NEXUS_GOOGLE_AGENT_API_KEY` from `~/.config/nexus/tokens.env` and reads `project` and `location` (default `"global"`) from `$NEXUS_GAP_CREDIT_CONFIG` or `~/.config/nexus/google-agent-platform-credit.json` (or `NEXUS_GCP_PROJECT` / `NEXUS_GCP_LOCATION`). Calls `https://{host}/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:generateContent` with `"x-goog-api-key"` header (default model `"gemini-3.8-flash"`). Always refused under `PHIMode`.
6. **`TypeSafeProvider` (`typesafe`, `jev`)**: Resolves `TYPESAFE_API_KEY` and calls `POST https://api.typesafe.ai/v1/systemone` with `model: "jev-latest"`, `state: {"context": ...}`, and `questions: {"q1": {"type": "noul", "instructions": ...}}`. Always refused under `PHIMode`.
7. **`ScriptedMockProvider` (`mock`)**: Deterministic in-memory replay transport recording per-turn `PrefixHash` and message history for unit tests and unattended CI verification.

---

## 6. `AGENTS.md` & Nexus System STM Integration (`pkg/stm`)

1. **`DiscoverAgentsMD(workspace)`**: Walks up to 4 parent directories from `workspace` (covering `.worktrees/worker-*` and git submodules) to read `AGENTS.md` (capped at 12,000 bytes). Unless `--no-agents-md` or `NEXUS_NO_AGENTS_MD=1` is set, this content is placed in `PrefixHeader.RepoSummary` so project rules are part of the immutable cached prefix.
2. **`ResolveProjectID(workspace)`**: Resolves the 21-char `p<20>` Nexus project ID from `NEXUS_PROJECT_ID`, `.git/nexus/project-id` (including git worktree/submodule `gitdir:` indirection), or `$NEXUS_STM_DIR/stm-w*/node/w*.md`.
3. **`LoadBrief(workspace, phiClamp)`**: Reads `$NEXUS_STM_DIR/stm-<pid>/node/<pid>.md` and up to 8 active recent nodes (filtering out `invalid_at != null`, sorted by `seq` descending). Under `phiClamp` or when `sensitivity != "standard"`, node summaries and agent fields are redacted.
4. **User-Turn Fence (`FormatFenced()`, Opt-In Continuity)**: Loom runs as an independent agent by default with its own bounded task context. When continuity with Nexus STM is explicitly requested (`--stm`, `"include_stm": true` in `task.json`, or `NEXUS_STM_INJECT=1`), the fenced `<nexus_stm>` block is appended to the initial user instruction so the frozen system prefix hash (Invariant I2) remains unchanged when STM nodes update.

---

## 7. CLI Contract & Exit Codes (`cmd/loom`)

> The machine-readable contract (versioned JSON schemas, trace events, exit codes) is in [../contract.md](../contract.md).

```bash
# Task-file execution mode
loom run --agent <name> --task-file <task.json> --workspace <dir> \
  [--provider ollama|openai|anthropic|anthropic-api|gemini|gemini-api|google-agent-platform|typesafe|mock] \
  [--model <m>] [--trace <jsonl>] [--phi] [--no-agents-md] [--no-stm] [--non-interactive] [--no-daemon] [--json] \
  [--allow-cmd "pytest"] [--pass-env NAME[,PREFIX*]] [--max-output-tokens N]
# run: --allow-cmd is UNIONed with task.json constraints.allow_commands, then intersected with global/agent layers

# Single-shot prompt mode
loom -p "<instruction>" [--workspace <dir>] \
  [--provider ollama|openai|anthropic|anthropic-api|gemini|gemini-api|google-agent-platform|typesafe|mock] \
  [--model <m>] [--max-turns 8] [--max-context-tokens 28000] \
  [--allow-cmd "pytest"] [--pass-env NAME[,PREFIX*]] [--max-output-tokens N] \
  [--trace <jsonl>] [--phi] [--no-agents-md] [--no-stm] [--json]

# Inspect workspace Nexus STM brief
loom stm [--workspace <dir>] [--phi] [--json]

# Capability negotiation
loom capabilities --json
```

| Code | Constant | Meaning | Output Contract |
|---|---|---|---|
| `0` | `ExitOK` | Task completed within turn budget | Final assistant output on stdout (or `TaskResult` JSON with `--json`) |
| `1` | `ExitTaskFail` | Retryable task failure, provider error, or `max_turns` exhausted | Diagnostic error on stderr (or `TaskResult` JSON with `--json`) |
| `2` | `ExitHarnessErr` | Fatal harness invariant error or `*policy.ViolationError` | Violation reason on stderr |
| `3` | `ExitEscalate` | Model invoked `escalate` tool (`escalation_required`) | Structured `EscalationPayload` JSON on stdout |
