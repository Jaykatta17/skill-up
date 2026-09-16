# DeepSeek Harness Custom Engine example

This example runs a Skill evaluation through DeepSeek Harness (DSH) using
skill-up's local Custom Engine contract. It defaults to DashScope's
`qwen3.8-max` model and keeps the API key in the process environment.

## Prerequisites

- skill-up v0.11.0 or newer
- Python 3.10 or newer
- `dsh` available on `PATH`
- `DASHSCOPE_EVAL_API_KEY` available in the environment

Install the pinned DSH version used to verify this example:

```bash
npm install -g @deepseek-ai/dsh@0.1.5-rc.1
dsh --version
```

## Run

From this repository checkout:

```bash
export DSH_SKILL_UP_RUNNER="$PWD/examples/deepseek-harness/evals/fixtures/dsh_runner.py"
skill-up run ./examples/deepseek-harness/evals/eval.yaml
```

The example deliberately does not put the DashScope key in YAML. Inject it
from your secret manager or shell environment. To select another compatible
Qwen model, pass `--model <model-name>` to `skill-up run`. Override
`DASHSCOPE_BASE_URL` to use another compatible endpoint.

The runner creates a fresh `.skill-up-dsh/runs/<run-id>/` home for every case
variant, disables DSH telemetry, configures the DashScope OpenAI-compatible
route, exposes the installed Skill directory to DSH, and returns skill-up's
structured `SessionResult`, including tool calls, tool results, and token
counts. Raw DSH session JSONL and stderr are retained as run artifacts for
debugging.

This is an experimental local integration. MCP installation and native
turn-by-turn session resume are not provided by skill-up's Custom Engine.
