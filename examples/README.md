# Examples

- `colab_run.py`: run one loom task on a fresh Colab VM.
- `provider_switch.py`: one agent, same task, run once per provider (anthropic, gemini) to show that
  switching providers is only the `--provider` flag. Loom is a single-agent CLI; multi-agent
  orchestration is out of scope here and belongs to nexus.

```bash
make build
python3 examples/provider_switch.py                  # anthropic + gemini; falls back to mock if a key is missing
python3 examples/provider_switch.py --mock           # no network, no keys
```

Runs use an empty temp workspace with `--no-agents-md --no-stm`, `--max-turns 2` and a small token budget.
The mock provider always replies `mock task complete`.
