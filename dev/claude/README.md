# Opt-in Claude Code config for this repo

`settings-review-gate.json` is a `PreToolUse` hook that blocks `gh pr create` locally when neither `--body` nor the `--body-file` file contains an "Adversarial review" section. It mirrors the `review-gate` required CI check (see [CONTRIBUTING.md](../../CONTRIBUTING.md#adversarial-review)). Mirrored from go-steer/core-agent.

To opt in, merge it into your **personal, untracked** `.claude/settings.json` at the repo root, or into `~/.claude/settings.json` for all projects. It's deliberately not committed as live config: assistant harness settings are personal tooling. The repo-native enforcement is the required CI check.
