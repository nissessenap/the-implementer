# Context

Domain glossary for `the-implementer`. Terms are added as architectural decisions
land; see `docs/adr/` for the decisions themselves.

`the-implementer` is a Go GitHub App and webhook receiver that launches isolated
Kubernetes workloads running Claude Code headlessly to implement a GitHub issue,
then produces a pull request.

## Sandbox and images

**Run** — one attempt at implementing one GitHub issue. Executes as a single
`batch/v1` **Job** — one pod, `restartPolicy: Never`, `backoffLimit: 0`, gVisor
via `runtimeClassName` — and produces at most one branch. The Job name derives
from the issue, which is what makes webhook redelivery idempotent. See
[ADR 0002](docs/adr/0002-a-run-executes-as-a-kubernetes-job.md).

**Sandbox** — the isolated container a run executes in. Holds the checkout, the
toolchain, and the agent CLI. Nothing in it survives the run.

**Agent CLI** — the binary that does the implementing, invoked by the phase
script. Named as a role rather than a product: v1 ships `claude` as the only
implementation, but the image contract says only "an agent CLI on `PATH`", so an
organization's image stays valid if the engine changes.

**Base image** (`implementer-base`) — the one image we maintain. Carries the agent
layer and nothing language-specific: a shell, `git`, `gh`, `bubblewrap`, the agent
CLI, baked skills, the phase script, and a non-root user. **Its Dockerfile is the
image contract.** See [ADR 0001](docs/adr/0001-sandbox-image-strategy-and-byo-contract.md).

**Toolchain layer** — a thin image derived from the base that adds exactly one
toolchain, typically four lines of Dockerfile. We ship only the ones we dogfood.

**Toolchain** — the key space image selection joins on: a small vocabulary of ours
(`go`, `node`, `python`, …), not a language name. An image installs a toolchain,
not a language, and Linguist's names do not survive the join — `package.json` is
the manifest for both `JavaScript` and `TypeScript`. See
[ADR 0003](docs/adr/0003-toolchain-detection-and-image-selection.md).

**Detection** — how a run picks its image: root manifests first, `GET /languages`
as the fallback, each normalised to a toolchain and intersected with the images
the operator configured. Runs orchestrator-side before the Job exists; adds no
state. Ambiguity and non-detection both **refuse and comment** rather than guess,
because a wrong image fails deep after the tokens are spent.

**Version seam** — image lookup is longest-key-first (`python-3.14` before
`python`), so a versioned toolchain is a later table entry rather than a redesign.
v1 never emits a version; it owes the seam, not the feature. The deferred
overrides — a repo-side `.implementer.yaml`, or an image named in the triggering
comment — land here.

**Image contract** — the minimum an image must satisfy to be usable as a sandbox.
Deliberately small enough to bolt onto an organization's existing builder image,
so a custom image never has to be `FROM implementer-base`. Enforced by the
preflight, not merely documented.

**BYO image** — an organization's own sandbox image, satisfying the image contract
either by deriving from the base image or by copying its `RUN` lines into their
own hardened base.

**Preflight** — the phase script's first act: verifying the image contract
(`command -v` for the agent CLI, `git`, `gh`, the script itself) and failing to
`/dev/termination-log` with a readable message. Exists so a non-conforming image
fails loudly rather than mysteriously.

**Matched pair** — the orchestrator and the sandbox image are versioned and
released together, and pinned by digest. The phase script lives in the image, so
the two cannot drift.

## Run execution

**Phase script** — the script baked into the sandbox image and invoked as the
pod's `command`. It is the run plan: deterministic steps in shell, with one agent
CLI invocation per agent phase. A fresh process per phase is how context is
cleared.

**Phase** — one agent CLI invocation within a run. Phases are code, not prompt:
control flow lives in the phase script so that "review passed" is distinguishable
from "review never ran".

**Baked skills** — [Matt Pocock's skills](https://github.com/mattpocock/skills)
installed at image build time and pinned, loaded via `--plugin-dir` from a
read-only path. Pinning is what makes evaluation possible; `--plugin-dir` is what
survives the ephemeral `HOME`.

**Result channel** — how data leaves the sandbox without `kubectl exec`. A compact
structured result via `/dev/termination-log`, surfaced on pod status; the full
transcript via `pods/log`. Both are pod-level, which is why the orchestrator's
informer watches Pods rather than Jobs.

**Resume seam** — the one obligation trigger #3 (PR-review feedback) places on
v1: the workspace and `HOME` mounts stay *swappable* from `emptyDir` to a PVC, and
neither the phase script nor the orchestrator assumes nothing survives a run.
Resuming is `claude --resume` against a persisted session directory, so it is a
property of the volume, not of the workload primitive.
