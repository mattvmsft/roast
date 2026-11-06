---
name: roast
description: Run an adversarial Roast review after non-trivial code or prose changes, before declaring work complete, or when the operator requests /roast.
---

# Roast

Roast reviews local Git changes with two independently configured models. The
finder looks aggressively for defects; the verifier re-reads the cited code and
confirms or refutes each candidate before it reaches the final report.

## Setup and trust boundary

Roast sends the selected diff, repository context, prompts, and tool results to
the inference endpoints in the user's Roast configuration. It may also upload a
complete run bundle when uploads are explicitly enabled. These destinations are
user-controlled and may be third-party services. Do not assume they are trusted
for private source code: inspect the configuration or run `roast doctor`, and
follow the operator's data-handling policy.

Configure profiles in the user config file shown by `roast doctor`, following
the profile example in the Configure section of the Roast README
(https://github.com/bluesky-social/roast#configure). Use
`roast config init --from <url>` only with a trusted configuration URL that the
operator supplies. A profile selects an endpoint, model, wire protocol
(`responses`, `chat`, `anthropic`, or `copilot`), and output limit. API profiles
also select a credential source. The finder and verifier may use different
profiles and protocols.

The `copilot` protocol uses the authenticated GitHub Copilot CLI without
provider API keys. An omitted model uses Copilot CLI's defaults; it does not
read the desktop app's model selection. Use `--copilot` to select this provider
for both roles, `--model <name>` for both models, or `--finder-model <name>` and
`--verifier-model <name>` independently. Do not request API keys when Copilot
is selected. No launcher script is needed.

`roast skill install` installs this skill into the shared Agent Skills location
and any detected Claude Code or Polytoken installation. `roast skill status`
reports whether each installed copy matches the binary. On the first interactive
review in a repository, Roast asks once whether to install its native pre-commit
gate and records the answer in local Git config. Non-interactive runs never
install it without recorded consent. The gate is managed explicitly with
`roast gate enable|status|disable`.

## When to review

Review coherent, non-trivial changes such as features, refactors, concurrency,
protocol changes, and important bug fixes. Skip formatting-only changes and
minor documentation corrections. Review before committing with `--staged`, or
use `--base <ref>` when the work is already committed.

## Invocations

Run the `roast` binary on PATH from the repository being reviewed. Start with
`roast review --task "what changed and why"` and respect the user's configured
providers and model defaults. Keep finder/verifier verification enabled unless
the operator explicitly requests a draft or quick review. Pass `--author-model`
when the author's model is known; do not guess it. Do not install a Git gate or
enable uploads unless the operator asks for them.

```sh
roast review --task "what changed and why"
roast review --copilot --task "what changed and why"
roast review --copilot --finder-model <model> --verifier-model <model> --task "what changed and why"
roast review --staged --task "what changed and why"
roast review --quick --task "draft review"
roast review --interactive --task "review with repository navigation"
roast review --scope repo --task "survey this repository"
roast runs
roast runs show <run-id>
roast doctor
```

`--quick` skips independent verification and is a draft result. Repository
survey mode is prioritized sampling, not exhaustive coverage. A `NO_DIFF`
result means no review ran; it is not a clean review.

Roast writes JSON to stdout and a Markdown report to stderr by default. Exit 0
means the command completed without a finding at the requested gate threshold;
exit 1 means a finding met that threshold; exit 2 is invalid usage or config;
exit 3 is authentication; exit 4 is an operational or resource failure; and
exit 5 is cancellation.

Every non-empty review records a durable local run. `roast runs status` reports
upload state and `roast runs sync` retries eligible pending uploads. Replay and
evaluation tooling are intentionally not part of the `roast` command.

## Handling findings

Check every finding against the cited code. State whether it is genuine,
refuted, or a deliberate tradeoff and explain what will be changed. A verifier
refutation is expected behavior, not a failure. Do not silently discard a
confirmed finding.
