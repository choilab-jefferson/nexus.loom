# Loom

Loom is a small, dependency-free (Go stdlib only) agent CLI. It runs one bounded coding task against a model of your choice (Ollama, Anthropic, OpenAI, Gemini, ...) with a fixed tool set (read, search, edit, write, allowlisted commands), a workspace sandbox, hard turn/token/time budgets, and a JSONL trace. It is built to run unattended as a leaf worker, and works standalone with zero configuration files.

Loom is a single-agent CLI: one agent, one bounded task per run. It has no sub-agent or orchestration features; multi-agent orchestration belongs to nexus (see [Use from nexus.lite](#use-from-nexuslite)). Switching providers is one flag: see [`examples/provider_switch.py`](examples/provider_switch.py).

**Running in Google Colab?** See the [Colab quickstart](docs/colab-quickstart.md) (notebook cells: prebuilt binary, Colab Secrets, Ollama, nexus.lite) or drive Colab from your terminal with the [Colab CLI guide](docs/colab-cli.md).

## Install

```bash
go install github.com/choilab-jefferson/nexus.loom/cmd/loom@latest   # needs Go 1.22+
make build            # from a clone; produces ./bin/loom
# or download a prebuilt binary: https://github.com/choilab-jefferson/nexus.loom/releases/download/v0.1.0/loom-<os>-<arch>
```

Maintainers: see [docs/release.md](docs/release.md) for the GitHub snapshot publish procedure.

## 60-second quickstart

```bash
# Smoke test, no model, no network
./bin/loom -p "hello" --provider mock

# Local model (Ollama must be running: `ollama serve`)
./bin/loom -p "add a unit test for foo()" --provider ollama

# Hosted model
export ANTHROPIC_API_KEY=sk-...
./bin/loom -p "fix the failing test" --provider anthropic --allow-cmd "go test"
```

The default `--provider auto` picks the first of `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`/`GOOGLE_API_KEY`, `OPENAI_API_KEY` that is set, else a running Ollama (`OLLAMA_HOST`, default `127.0.0.1:11434`). If none is available, loom exits 2 with `no model provider configured: ...`. With `--provider ollama` and no server, it reports `ollama not reachable at ... (start it with ollama serve, ...)`.

## Usage

Run `loom` with no arguments (exit 2) or `loom --help` (exit 0) for the full list.

| Flag | Meaning |
|---|---|
| `-p "<text>"` | one-shot instruction (or `loom run --task-file task.json`) |
| `--provider` | `auto`, `ollama`, `openai`, `anthropic`, `gemini`, `google-agent-platform`, `typesafe`, `mock` |
| `--model` | model id (provider default otherwise) |
| `--workspace` | workspace root; all file access is confined here |
| `--allow-cmd "git status"` | allow a command prefix for `run_command` (repeatable; none by default) |
| `--max-turns`, `--max-total-tokens`, `--deadline`, `--max-tool-calls` | budgets |
| `--trace file.jsonl`, `--json` | trace output, machine-readable result |
| `loom resume <trace>` | continue an interrupted run |
| `loom capabilities --json` | tools and providers as JSON |

Keys: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`/`GOOGLE_API_KEY`, `TYPESAFE_API_KEY`.

## Safety model

- File tools resolve symlinks and refuse paths outside `--workspace` or matching deny globs.
- `run_command` runs only allowlisted argv prefixes (global ∩ agent ∩ task), never through a shell; interpreter eval flags are rejected.
- API keys are scrubbed from child-process environments and redacted in errors.
- Budgets (turns, tokens, tool calls, deadline) end the run with a defined exit code and trace.
- Three consecutive tool errors stop the run.

## Exit codes

| Exit Code | Constant | Meaning |
|---|---|---|
| `0` | `policy.ExitOK` | Task completed within turn budget |
| `1` | `policy.ExitTaskFail` | Retryable task failure or `max_turns` exhausted |
| `2` | `policy.ExitHarnessErr` | Fatal harness error, missing key, or `PolicyViolationError` |
| `3` | `policy.ExitEscalate` | Structured `escalation_required` JSON payload emitted on stdout |
| `4` | `policy.ExitBudget` | Token, deadline or tool-call budget exhausted |
| `130` | `policy.ExitInterrupted` | SIGINT/SIGTERM; resumable via `loom resume <trace>` |

`loom capabilities --json` is the machine-readable contract (tools, providers, exit codes). Details: [`docs/spec/loom-v0.1.md`](docs/spec/loom-v0.1.md). Colab: [`docs/colab-quickstart.md`](docs/colab-quickstart.md).

## Nexus integration (optional)

Nothing here is needed to use loom. These features only activate when Nexus files exist or when you opt in.

- **AGENTS.md autoload**: `AGENTS.md` found in the workspace or up to 4 parents is loaded into the prompt prefix. Disable with `--no-agents-md` or `NEXUS_NO_AGENTS_MD=1`.
- **STM brief** (`--stm`, `"include_stm": true` in `task.json`, or `NEXUS_STM_INJECT=1`): injects a short `<nexus_stm>` brief read from `$NEXUS_STM_DIR/stm-<project-id>/node/` into the first user turn. Inspect it with `loom stm --workspace . [--json]`.
- **STM env**: `NEXUS_STM_DIR` unset uses `~/.local/share/nexus/stm`; set to an empty string it disables STM lookup (nothing is read from the host). Project-id resolution order is `NEXUS_PROJECT_ID`, then `.git/nexus/project-id`, then a fallback scan of `$NEXUS_STM_DIR/stm-w*/node/w*.md` (nexus workspace-entity nodes: `- id: <pid>` followed by `line: <slug>:` where slug is the workspace dir name; first match wins).
- **`--phi`**: local-only model confinement (non-local endpoints are refused; STM summaries redacted).
- **Token store**: key lookup order per provider is the standard env var, then the `NEXUS_*` alias (`NEXUS_ANTHROPIC_API_KEY`, `NEXUS_OPENAI_API_KEY`, `NEXUS_GEMINI_API_KEY`, `NEXUS_GOOGLE_AGENT_API_KEY`), then `$NEXUS_CONFIG_HOME/tokens.env` (default `~/.config/nexus/tokens.env`) only if that file exists. `IKB_MCP_TOKEN` from it is injected only into `ikb` subprocesses.

- **google-agent-platform**: the only provider with extra required config: a GCP project via `NEXUS_GCP_PROJECT` (optional `NEXUS_GCP_LOCATION`, default `global`) or a JSON file `{"project","location"}` at `NEXUS_GAP_CREDIT_CONFIG` / `$NEXUS_CONFIG_HOME/google-agent-platform-credit.json`. Without it the provider fails with a clear error; all other providers are independent of nexus.

### Use from nexus.lite

[nexus.lite](https://github.com/choilab-jefferson/nexus.lite) is the intended orchestrator. Loom is invoked as a plain subprocess; no nexus.lite code is needed on the loom side:

```bash
loom run --task-file task.json --workspace . --provider auto --json   # check the exit code
loom capabilities --json                                             # feature discovery
```

## License

MIT, see [LICENSE](LICENSE).

## Hermetic tests

Packages that can touch Nexus state carry a `hermetic_test.go` `TestMain` that points `HOME`, `NEXUS_CONFIG_HOME` and `NEXUS_STM_DIR` at a temp dir and unsets `NEXUS_PROJECT_ID`, `NEXUS_SENSITIVITY`, `NEXUS_STM_INJECT`, `NEXUS_NO_AGENTS_MD`. Today that is six packages: `pkg/compact`, `pkg/engine`, `pkg/policy`, `pkg/providers`, `pkg/stm`, `pkg/tools`. `cmd/loom` and `pkg/trace` have none because their tests touch no Nexus state; add one when a new package starts reading Nexus env or files.
