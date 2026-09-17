# Skill Up Observer Codex plugin

This plugin captures explicitly attributed Skill interactions through Codex hooks and stores normalized observations locally. A bundled review Skill guides the user through approval before `skill-up` creates a candidate regression case.

## Requirements

- A current Codex CLI or Codex desktop release with plugin and lifecycle-hook support. The IDE extension does not support plugins.
- `skill-up` built from this repository and available on `PATH`.
- Explicit trust for the plugin's hook definition in Codex.

The pinned Codex 0.80.0 binary used by skill-up's evaluation adapter is intentionally out of scope. It remains available for custom-model evaluation and does not load this plugin.

## Data and consent

Enabling the plugin and trusting its hooks opts in to local capture. Pending drafts and finalized observations are stored under `~/.skill-up/observations` by default, or under `SKILL_UP_OBSERVATION_DIR` when set. Directories use mode `0700` and observation files use `0600`.

Common API-key, bearer-token, JWT, and secret-assignment shapes are redacted before persistence. Nothing is uploaded. Unattributed turns are discarded at `Stop` or `Interrupt`, and inferred attribution is rejected by the observation contract.

## Review workflow

```bash
skill-up observe list
skill-up observe show <observation-id>
skill-up observe case <observation-id>
skill-up observe approve <observation-id>
skill-up observe case <observation-id> --write --skill-root /path/to/skill
```

The preview command is read-only. Writing requires an approved observation, never overwrites an existing case, updates `evals/eval.yaml`, and validates the complete suite before keeping either change.
