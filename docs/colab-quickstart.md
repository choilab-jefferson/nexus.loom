# Loom on Google Colab

Loom is a single static binary with no dependencies, no daemon and no TTY prompts, so it runs from a plain `!` cell. In Colab it reads nothing from `~/.config/nexus` because that directory does not exist there (loom reads `tokens.env` and `google-agent-platform-credit.json` from it only when present). It writes nothing outside the workspace and the optional `--trace` path. Run each block as its own cell. Prefer the terminal? See [colab-cli.md](colab-cli.md).

Release assets live under `https://github.com/choilab-jefferson/nexus.loom/releases/download/<tag>/`; replace `v0.1.0` below with the release you want.

## 1. Get the binary

Option A: prebuilt, checksum-verified

```
!curl -fsSL -o loom https://github.com/choilab-jefferson/nexus.loom/releases/download/v0.1.0/loom-linux-amd64
!curl -fsSL -o checksums.txt https://github.com/choilab-jefferson/nexus.loom/releases/download/v0.1.0/checksums.txt
!grep loom-linux-amd64 checksums.txt | sed 's/loom-linux-amd64/loom/' | sha256sum -c -
!chmod +x loom && ./loom version
```

Option B: build from source (Go 1.22+; Colab does not ship Go)

```
!apt-get -qq install -y golang-go >/dev/null   # or download a Go tarball
!git clone https://github.com/choilab-jefferson/nexus.loom loom-src && cd loom-src && make build && cp bin/loom /content/loom
!/content/loom version
```

## 2. Smoke test (no model, no network, no key)

```
!./loom capabilities --json
!./loom -p "Colab smoke test" --provider mock --json
```

## 3. Hosted model via Colab Secrets

Add secrets in the Colab key icon panel (enable notebook access), then copy them into environment variables. Use only the ones you need.

```
import os
from google.colab import userdata
for k in ("ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"):
    try:
        os.environ[k] = userdata.get(k)
    except Exception:
        pass   # secret not set
```

```
!mkdir -p work && echo "def add(a,b): return a-b" > work/m.py
!./loom -p "Fix the bug in m.py" --workspace work --provider anthropic --max-turns 6
# or: --provider openai | --provider gemini
```

`--provider auto` (the default) picks whichever of those keys is set.

## 4. Local model with Ollama in Colab (GPU runtime recommended)

```
!curl -fsSL https://ollama.com/install.sh | sh
import subprocess, time
subprocess.Popen(["ollama", "serve"]); time.sleep(5)
!ollama pull qwen2.5-coder:7b
```

```
!./loom -p "Add a docstring to m.py" --workspace work --provider ollama --model qwen2.5-coder:7b
```

## 5. Use with nexus.lite

[nexus.lite](https://github.com/choilab-jefferson/nexus.lite) can orchestrate loom as a plain subprocess. Install loom (step 1), then call it from Python and check the exit code (0 ok, 1 retryable, 2 config error, 3 escalate, 4 budget):

```
import subprocess
r = subprocess.run(["./loom", "-p", "Summarize README.md", "--provider", "auto", "--json"],
                   capture_output=True, text=True)
print(r.returncode, r.stdout[-2000:], r.stderr[-500:])
```

Exit codes and flags: see the [README](../README.md).
