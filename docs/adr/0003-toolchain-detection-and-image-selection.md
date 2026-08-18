# 3. Toolchain detection and image selection

Date: 2026-07-29

## Status

Accepted

## Context

[ADR 0001][adr1] settled *what is inside* a sandbox image: one base image plus
thin per-language layers, with an organization free to bolt the agent layer onto
its own hardened base. It explicitly left this question open:

> **[Issue #11][selection] shrinks but survives.** With one base plus thin
> language layers there is still a language-to-image mapping to resolve, unlike
> the single-fat-base option where selection would have collapsed to "is there an
> override".

So: a webhook arrives naming a repository, and something must produce a container
image reference before a Job can be created.

The [field survey][survey] found that **only Codex auto-detects**, by lockfile
sniffing, and that **Copilot explicitly calls auto-detection "slow and
unreliable"**. But those systems are detecting in order to *install dependencies*
across every public repo on GitHub. We are detecting to pick one of a handful of
images an organization maintains for its own repos — a far smaller problem, and
one where being wrong is cheap to report and cheap to fix.

The survey also confirms **nobody uses a model for this**, which the ticket asked
us to verify before considering one. We do not.

### Measurements

Both candidate signals were run against real repositories before choosing.

| repo | `GET /languages` → top | root manifest |
|---|---|---|
| `nissessenap/the-implementer` | `{}` — nothing | none |
| `mattpocock/skills` | `Shell` (12,609 B) over `JavaScript` (3,732 B) | `package.json` |
| `github/gh-aw` | `Go` (25.2 MB), 67% | `go.mod` |
| `grafana/grafana` | `TypeScript` 45.3 MB vs `Go` 43.0 MB — 51/49 | `go.mod` **and** `package.json` |

Two findings drive the decision.

**`/languages` is not cheaper.** The ticket listed it as the cheapest option, but
`GET /repos/{owner}/{repo}/contents/` returns the *entire root listing in a single
call*, so manifest sniffing costs one API call too — not one per candidate file.
The cost argument for `/languages` does not survive contact with the API.

**Where the two disagree, the manifest is right.** `mattpocock/skills` is an npm
package that Linguist reports as Shell, because Linguist counts bytes and excludes
Markdown as prose. Byte counts measure what is *written*; a manifest measures what
*toolchain builds it*, which is the question actually being asked.

Root-manifest collisions are rare but real: of seven established polyglot Go
projects checked (`prometheus`, `argo-cd`, `vault`, `etcd`, `cilium`,
`opentelemetry-collector`, `grafana`), only Grafana has two root manifests — and
Grafana is unresolvable by *either* signal, since 51/49 is noise.

## Decision

### Detection is a two-signal cascade, orchestrator-side

Detection runs in the orchestrator before the Job is created, against the
installation token. One or two REST calls, no clone, no model, no persistence —
consistent with [v1 having no database][map].

```
1. GET /repos/{o}/{r}/contents/     → root manifests → candidate toolchains
2. survivors = candidates ∩ keys(sandbox.images)
     exactly 1 → run it
     >1        → REFUSE, comment naming the candidates
     0         → step 3
3. GET /repos/{o}/{r}/languages     → Linguist names → toolchains → ∩ images
     exactly 1 → run it
     >1        → REFUSE
     0         → step 4
4. sandbox.defaultToolchain set? → run it
   unset                         → REFUSE, comment naming the cause
```

The manifest wins outright when present because it is the higher-confidence
signal. `/languages` exists as the fallback for the nested-monorepo case — a repo
whose `go.mod` lives at `services/payments/go.mod` shows nothing at root but is
unambiguously Go to Linguist. Two signals cost about fifteen lines of Go and each
covers the other's known failure.

### Intersect candidates with configured images

The set of toolchains an organization has configured images for is information the
operator has *already given us*. An org that maintains only a Go image has already
answered "Go or node?" for every repository it owns; asking again per-repo is
asking a question whose answer is on file.

So the candidate set is intersected with `keys(sandbox.images)` before the count
is taken. This resolves Grafana-shaped repos for free at any org that maintains
one of the two toolchains, and refuses only when the org genuinely maintains both.

### Ambiguity and non-detection both refuse

A fixed priority order (`go` > `node` > …) would give Grafana Go every time —
wrong about half the time, and wrong *silently*: the run clones, the agent burns
tokens against a toolchain that is not installed, and it fails deep. That is the
precise failure class ADR 0001 built [a preflight that actually runs][adr1] to
eliminate, and re-introducing it one layer up would be incoherent.

Using `/languages` as a tie-break is worse than it looks: at 51/49 it resolves a
real ambiguity with a coin flip and presents the result as a decision.

A refusal costs one comment and fails in milliseconds. **`issues: write` is
required on the orchestrator token** to post it — not a cost this decision incurs,
since the trigger path needs it anyway to acknowledge a run with a reaction.

`sandbox.defaultToolchain` exists as an opt-in escape for organizations that are
overwhelmingly one language, and ships **unset**, so nothing is ever guessed unless
an operator deliberately opted in. Note the greenfield case this covers: this repo
returns `{}` and has no `go.mod`, so with no default set the implementer cannot
implement its own first issue.

Rejected: falling back to `implementer-base` when nothing is detected. It genuinely
works for README and scaffolding tasks — the base carries a shell, `git`, `gh` and
the agent CLI — but a Rust repo with no configured image would land there too and
fail deep, which is the same silent wrongness by another route.

### Facts are hardcoded; choices are configuration

Two tables, deliberately kept apart because they are different kinds of thing.

`go.mod → go` is a **fact**. No organization disagrees with it, and getting it
wrong is a bug we fix in a patch release. Hardcoded:

```go
manifest = map[string]string{
    "go.mod": "go", "package.json": "node", "pyproject.toml": "python", ...
}
linguist = map[string]string{
    "Go": "go", "JavaScript": "node", "TypeScript": "node", "Python": "python", ...
}
```

`go → ghcr.io/myorg/implementer-go@sha256:…` is a **choice**. ADR 0001 ships only
the images we dogfood and expects organizations to supply their own, so this table
can never be compiled in:

```yaml
sandbox:
  images:
    go:   ghcr.io/nissessenap/implementer-go:v1.2.3@sha256:...
    node: ghcr.io/myorg/implementer-node@sha256:...
  defaultToolchain: ""   # unset → refuse
```

Config is Helm values rendered to a ConfigMap, reloaded by pod restart via the
standard checksum annotation. Watching it would be code; restarting is an
annotation.

### The key space is a toolchain vocabulary, not Linguist names

Both detectors normalise into a small vocabulary of ours — `go`, `node`, `python`,
`java`, `rust` — rather than joining on Linguist's names.

Linguist names do not survive the join. `package.json` is the manifest for both
`JavaScript` and `TypeScript`: one manifest, two Linguist names, one image.
`pyproject.toml` maps to `Python`, but Linguist also emits `Jupyter Notebook` and
`Cython` for repositories that want the same image. And an image is not a language
in the first place — ADR 0001's example layer is `RUN apt-get install -y
golang-1.25`, which is a toolchain.

Keying on Linguist names would work, at the price of the operator writing
`JavaScript:` and `TypeScript:` pointing at one image and absorbing every Linguist
quirk we failed to warn them about. The translation table is ten lines and it is
ours to fix; that cost belongs on us, not on every operator's values file.

A Linguist name absent from the table maps to nothing and falls through to the
non-detection branch. Incompleteness against Linguist's 500+ names is expected and
produces a refusal, never a wrong guess.

### The version seam

Resolution is **longest-key-first** on a flat map: look up `<toolchain>-<version>`,
fall back to `<toolchain>`.

v1 never emits a version, so it always takes the second branch. The mechanism
costs nothing now and is what keeps versioned toolchains from becoming a redesign
later:

```yaml
sandbox:
  images:
    python:      ghcr.io/myorg/implementer-py@sha256:...
    python-3.14: ghcr.io/myorg/implementer-py314@sha256:...   # post-MVP
```

This is the same shape as the map's **Resume seam**: v1 owes the seam, not the
feature. It is what the two deferred overrides land on — a comment naming
`python 3.14`, or a later pass that reads `go 1.25` from `go.mod` or
`requires-python` from `pyproject.toml`.

### Digest honesty is free

ADR 0001 requires that the orchestrator record the digest it actually ran, because
tags can be re-pushed and eval results citing a mutable tag are not honest. The
orchestrator cannot resolve a tag to a digest itself without registry credentials
it does not have and should not want.

It does not need to. `pod.status.containerStatuses[].imageID` carries the resolved
digest, reported by the kubelet, on the pod the orchestrator's informer
[already watches for the result channel][adr2]. No registry access, no new
dependency.

The orchestrator records the detected toolchain, which signal produced it, and the
resolved digest — enough to explain any run after the fact.

## Consequences

- **No repository-side configuration exists in v1.** Both override paths —
  `.implementer.yaml` and an image named in the triggering comment — are
  post-MVP, and are recorded as out of scope on [the map][map]. v1's only
  obligation to them is the version seam above.
- **`issues: write` is required on the orchestrator token**, inherited by
  [#13][push] and [#15][trigger] rather than rediscovered there.
- **Grafana-shaped repositories do not work at an org that maintains both
  toolchains** until the comment override ships. This is accepted: a refusal
  naming both candidates is one comment away from a human resolving it.
- **ADR 0001's matched-pair rule narrows.** The digest-pinned matched pair is
  *our* base image and orchestrator. Once the table holds an organization's own
  image references, keeping those in step is theirs, and [the preflight][adr1] is
  what catches drift.
- **Detection adds no state.** One or two REST calls per run, nothing cached,
  nothing persisted.

## Alternatives rejected

- **`/languages` alone.** Wrong on any repo whose bulk is not its build toolchain
  — `mattpocock/skills` is the measured case — and wrong silently.
- **Root manifest alone.** Refuses the nested-monorepo repo that `/languages`
  identifies instantly.
- **A fixed priority order over colliding manifests.** Silent wrongness on the one
  case it exists to handle.
- **`/languages` as a tie-break for colliding manifests.** A coin flip presented
  as a decision.
- **An operator-maintained per-repository image table.** Considered first and
  discarded: it scales with repository count rather than toolchain count, and it
  makes an operator restate for every repo something the repo already declares in
  its own manifest.
- **An LLM classifier.** Spends a model call to determine whether a file named
  `go.mod` exists. No system in [the survey][survey] does this.
- **Falling back to `implementer-base` when nothing is detected.** Correct for
  scaffolding tasks, silently wrong for every unconfigured compiled language.
- **No detection at all: one image per *agent CLI*, with dependency setup pushed
  into a per-repository `setupCommand`.** Added 2026-08-10 from
  [the Kelos research][kelosresearch] — a fourth option this ADR never considered,
  and worth recording because it is neither of the two it framed the decision
  between. [Kelos][kelos] ships exactly this: five images, one per agent CLI, and
  **zero** toolchain detection (no hits for `go.mod`, `package.json`, `pyproject`,
  `GET /languages` or "toolchain" outside its tests). What varies per repository is
  a `setupCommand` on a `Workspace` object, base64-decoded and executed by the
  entrypoint before the agent runs.

  Not adopted: it does not remove the problem, it **relocates it onto whoever writes
  the `Workspace`** — one hand-maintained setup script per repository, which is the
  per-repository-table alternative above wearing different clothes, and it scales the
  same wrong way. It also moves dependency installation from an image layer into a
  cold shell command on every run. Recorded because it is a legitimate design that a
  real system runs in production, and because it is the shape to reach for if
  detection ever proves unreliable in a way configuration cannot patch.

[map]: https://github.com/nissessenap/the-implementer/issues/1
[kelos]: https://github.com/kelos-dev/kelos
[kelosresearch]: ../research/kelos-and-kubefoundry.md
[adr1]: 0001-sandbox-image-strategy-and-byo-contract.md
[adr2]: 0002-a-run-executes-as-a-kubernetes-job.md
[survey]: https://github.com/nissessenap/the-implementer/issues/7
[selection]: https://github.com/nissessenap/the-implementer/issues/11
[push]: https://github.com/nissessenap/the-implementer/issues/13
[trigger]: https://github.com/nissessenap/the-implementer/issues/15
