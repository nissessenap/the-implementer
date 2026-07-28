# The implementer

A GitHub webhook app that triggers AI coding agents to implement an issue, based on a label or a mention of the GitHub App user.

It's built on the philosophy of Matt Pocock's [skills](https://github.com/mattpocock/skills), where the developer runs `/grill-with-docs` until the open questions are answered and issues have been created from there.

But instead of running `/implement` on your laptop, the implementer does it for you — in an isolated Kubernetes workload.

## How it works

1. A webhook event arrives (label applied, or the app user mentioned in a comment).
2. The orchestrator mints a short-lived, narrowly-scoped GitHub installation token and creates one isolated Kubernetes workload for the run.
3. The workload's entrypoint is a **run plan**: a script that invokes `claude -p` once per phase, with deterministic steps in between. Each phase is a fresh process, so each gets a clean context.
4. The agent commits and pushes its branch. It cannot open, comment on, approve, or merge a pull request.
5. The orchestrator watches the workload to completion, reads the run's structured result, and opens the pull request itself.

The orchestrator is really a **controller with a webhook front-end** — so run state lives in Kubernetes objects and GitHub, and there is no database. Restart it mid-run and it reconciles; webhook redelivery is idempotent because the workload name derives from the issue.

## Design decisions

Architecture is being worked out in the open. See **[the architecture map](https://github.com/nissessenap/the-implementer/issues/1)** for what's settled, what's still open, and the research behind each call. Decisions land as ADRs in `docs/adr/`.

Settled so far:

- **The agent is the `claude` CLI, run headlessly.** We don't build a harness. Matt's skills are Claude Code skills — `SKILL.md` discovery, `disable-model-invocation`, prose `/skill` cross-invocation, and subagent spawning are all harness features, not prompt content. Reimplementing them is a Claude Code clone, not a harness. Spotify reached the same conclusion the expensive way.
- **Phases are code, not prompt.** The run plan is a script, so phase boundaries have real exit codes and "the review never ran" is distinguishable from "the review passed."
- **No `kubectl exec` in the run path.** The pod's own `command` is the run plan. Results leave via the pod's termination message and its logs — both plain reads on objects the orchestrator already watches.
- **The orchestrator opens the PR, not the agent.** The sandbox gets `contents: write` and nothing more. A deterministic PR is also diagnosable when the run *failed*, which an agent-authored PR can never be.
- **Scion is not a dependency.** [Scion](https://github.com/GoogleCloudPlatform/scion) evaluated agent-sandbox and deliberately removed it, and webhook-driven agent creation is an explicit non-goal in its design docs. It stays useful as prior art.

Still open — don't build against these yet: the exact Kubernetes primitive (agent-sandbox `Sandbox` vs a plain `Job`), how sandbox environments are prepared, network egress policy, and trigger details.

## Container images

Your organization probably has multiple languages, and what's pre-installed in a sandbox is up to you. The strategy — a small set of per-language images, versus one multi-language base plus a repo-declared setup script and a snapshot — is [still being decided](https://github.com/nissessenap/the-implementer/issues/10). Whichever way it lands, the BYO-image contract will be documented and deliberately small.

One hard constraint is already known: **the sandbox must run as a non-root user.** `claude --dangerously-skip-permissions` refuses to start as root.

## Roadmap

- [ ] Trigger implementation based on a label
- [ ] Trigger implementation based on a mention + extra context in an issue comment
- [ ] Trigger feedback on a review using a comment in a PR — deliberately deferred

### Docs to write

- How to build an image ready for use
- How to install the implementer and its runtime dependencies in Kubernetes
- How to install gVisor for improved isolation
- How to set up the webhook with a GitHub App

### Long term

- A link from the issue to a live view of the run. Headless Claude Code emits a structured event stream, so this is a log pipe rather than a terminal proxy.
- State management: launching an agent to address code-review feedback.
