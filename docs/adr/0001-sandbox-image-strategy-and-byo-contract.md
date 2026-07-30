# 1. Sandbox image strategy and the BYO-image contract

Date: 2026-07-29

## Status

Accepted

## Context

A run executes as a container. Something has to decide what is inside it, who
maintains it, and what an organization must do to supply its own.

The original premise in the README was three example images — Go, Python, npm —
with other organizations building their own. [The field survey][survey] found
that almost nobody in the top tier does that. The dominant strategy is **one fat
multi-language base image, a repo-owned setup script, and a snapshot of the
post-setup state**; Copilot, Codex, Jules, Cursor, Devin and OpenHands all
converge on it. Their stated reasons: it is the only approach that scales to a
long tail of repos without the platform maintaining N images, the config lives in
the repo so it versions with the code, and snapshotting recovers the startup cost
the script would otherwise pay every run.

Two facts prevent us from simply copying that.

**We do not get the snapshot.** [Research into agent-sandbox][sandbox] found
snapshots are not in its core. So we could adopt the dominant strategy but not
its cost-recovery mechanism, paying the setup script in full on every run.

**Our long tail is much shorter.** Those platforms serve every public repo on
GitHub. A self-hosted orchestrator serves one organization's repos, and that
organization already knows its own toolchain.

Meanwhile [scion][scion-research] is a cautionary tale for the contract. Its
`harnesses/authoring-guide.md:520-525` states outright:

> "Every scion harness image must be built **on a scion base image** — the base
> provides the `scion` user (uid 1000, zsh), `sciontool` (the container entrypoint
> is `sciontool init --`), python3, node/npm with a shared global prefix, and the
> standard developer toolchain."

Its entrypoint owns PID 1, UID 1000 is hard-coded, the `user` config field is a
false affordance that silently does nothing, and the Kubernetes path additionally
requires `tar`, `chown` and `touch` in-image because it syncs via pod exec. Its
`required_image_tools` field has three occurrences repo-wide and zero readers, so
a non-conforming image gets no diagnostic at all. Its own design notes call the
coupling fatal: *"every new harness requires rebuilding sciontool and the
container image, which defeats the plugin model entirely."*

## Decision

### Strategy: thin per-language images on one base

We maintain **one base image**. Language images are thin derivatives:

```dockerfile
FROM ghcr.io/nissessenap/implementer-base:v1
RUN apt-get install -y --no-install-recommends golang-1.25
```

The deciding axis is **CVE surface**: a fat multi-language base carries five
runtimes' worth of vulnerabilities, of which any given run uses one. A fat base
also would not have covered the repo's actual dependencies anyway — every
non-trivial repo installs its own at run time.

We ship only the language images we actually dogfood, starting with **Go**.
Adding Python later is a four-line Dockerfile, which is the definition of not
needing it now.

### The contract is the deliverable, not the image set

**The base image's Dockerfile *is* the contract.** An organization either:

- does `FROM implementer-base:v1` and adds its runtime, or
- copies those handful of `RUN` lines into its own hardened base image.

The second path is why there is no separate installer artifact to maintain: it is
the same lines, published as documentation rather than as a second code path. It
also means an organization building in an air-gapped environment controls its own
sourcing.

The contract must be small enough to bolt onto an org's existing builder image.
An org whose base image carries its pinned, audited toolchain must never be
forced to abandon it and inherit from ours.

### Contract terms

| Requirement | Why |
|---|---|
| **glibc base** — no musl/Alpine | The `claude` binary is a dynamically-linked 263 MB ELF against `/lib64/ld-linux-x86-64.so.2` |
| **A shell** | The phase script is `sh` |
| **Non-root, `USER 1000` as the image default** | Under gVisor, `bubblewrap` fails outright as uid 0 ([spike][k3sspike]). The agent CLI's own root refusal is the weaker reason — it is bypassable |
| **No UID constant in the orchestrator** | scion's exact trap; the PodSpec owns the UID |
| **`git`** | clone, branch, commit, push |
| **`gh`** | GitHub CLI operations inside the run |
| **An agent CLI on `PATH`** | Deliberately engine-agnostic wording |
| **`bubblewrap`** | Required by Claude Code's subprocess env scrubbing. The contract requires the **binary**, not a working network namespace — `--unshare-net` is broken under gVisor ([spike][k3sspike]) |
| **Baked skills at a read-only path** | See below |
| **The phase script at a known path** | See below |
| Explicitly **not** required: `tar`, `chown`, `touch`, `rsync`, `zsh`, `sudo`, node/npm | We never use pod exec, so nothing in-image serves our control plane |
| Explicitly **nothing about the network** | Egress belongs to the pod's environment |
| Explicitly **no credentials** | Injected at run time, never baked |

**The agent CLI is named as a role, not a binary.** v1 ships `claude` as the only
implementation, but the contract says "an agent CLI on `PATH`" so an org's image
stays valid if the engine changes. This mirrors how gh-aw stays engine-agnostic.

Distroless is ruled out: no shell for the phase script, and no `git` or `gh`.

### Skills: baked and pinned, loaded via `--plugin-dir`

Skills are installed at **build time** and pinned to an explicit version.

The install path is `claude plugin marketplace add`, which is a plain `git clone`
into a marketplace directory. This avoids the `npx skills add` path entirely, so
**node and npm are not in the base image** — a direct CVE reduction.

Skills are baked to a read-only path and loaded with **`--plugin-dir`**, *not*
discovered from `$HOME`. This is load-bearing: `HOME` is an ephemeral volume at
run time (see below), which would otherwise mask anything baked into
`~/.claude/plugins/` and produce `Unknown command: /implement`.

`--bare` must never be passed — it silently skips skill discovery despite
`--help` claiming otherwise ([audit][audit]).

**Every image build asserts `init.plugin_errors` is empty.** A skill that stopped
loading then fails the build instead of surfacing as a baffling agent run.
Plugins install asynchronously unless `CLAUDE_CODE_SYNC_PLUGIN_INSTALL=1`, so the
build sets it.

Rejected: fetching skills at run time. It needs run-time egress to GitHub, makes
runs non-reproducible, and — decisively — **makes evaluation impossible**, since
a behaviour change could come from an upstream commit rather than from us.
Staleness is the cost, and it converts into rebuild cadence, which is a knob we
own rather than a variable we do not.

The target repo's own `.claude/` needs no mechanism — it arrives with the
checkout and is loaded from cwd upward.

### The phase script is baked into the image

The run plan is baked at a known path and invoked as the pod's `command`.

The image and the orchestrator are a **matched, digest-pinned pair**. Since a
version bump rebuilds both anyway, baking costs nothing in rebuild churn, and it
buys a standalone-runnable image for debugging plus a PodSpec that stays readable
instead of carrying a wall of inline shell.

This is *not* scion's trap. Scion's coupling was the entrypoint owning PID 1, a
hard-coded UID, and in-image binaries its control plane invoked over pod exec. A
script at a known path that our pod `command` invokes is far weaker: a BYO image
adds one `COPY` line, and the preflight names it if missing.

Rejected: `//go:embed` in the orchestrator, passed as `command`. Its only real
advantage was avoiding rebuilds, which the matched-pair versioning removes.

### Versioning: the image tag versions the whole triple

Nothing floats inside the image. The tag versions **agent CLI + skills +
toolchain** together, all pinned explicitly in the Dockerfile, with Renovate
opening the bump PRs.

- **Floor: `claude >= 2.1.214`.** Below it, a large piped response can truncate
  the final line and omit the `result` message ([audit][audit]). A correctness
  floor, not a preference.
- We publish a moving `:v1` and immutable `:v1.2.3`/digest tags. **Helm defaults
  to the immutable one.**
- **The orchestrator records the digest it actually ran.** Tags can be re-pushed;
  eval results that cite a mutable tag are not honest.

### No pre-agent setup phase

Dependency installation is **the agent's own work**, driven by the target repo's
existing `CLAUDE.md` and the build commands it must run anyway. There is no
`.implementer/setup.sh` convention and no server-side hook store.

The reason the field runs a pre-agent setup script is Codex's model: internet on
during setup, off during the agent phase, so untrusted issue text never coexists
with registry access. That property does not survive contact with our workload —
`/implement` and `/tdd` add a dependency and re-run the build mid-work, so
closing the registry afterwards breaks the job. Codex can close it because Codex
pre-installs everything; we cannot, across arbitrary repos.

Three of three comparables agree:

- **Anthropic's cloud environments** run setup scripts under the *same* network
  policy as the agent phase. No looser setup mode is documented.
- **gh-aw's** phase split is about *write permissions* (`safe-outputs`), not
  egress.
- **scion** has phases — pre-start hooks run as root before privilege drop — but
  network access is byte-identical across them: one container, one netns,
  container-wide capabilities. Its one pre-agent install hook is
  operator-configured and stored server-side in a `project_pre_start_hooks`
  table, which v1 cannot use because [v1 has no database][map].

The cost is that a private-registry credential is live while the model runs. That
becomes the central problem of [issue #19][registries], which has a real answer
available in `sandbox.credentials` masking, so this is a deferral to a solvable
ticket rather than an unacknowledged hole.

### Run-time posture

- **`readOnlyRootFilesystem: true`.**
- **`HOME`, `/tmp` and the workspace are `emptyDir` volumes with `fsGroup` set.**
  An `emptyDir` mounts `root:root 0755`, so `fsGroup` is what makes it writable
  by a non-root user — this is how the UID problem becomes the orchestrator's
  PodSpec field instead of something baked into the image.
- **`DISABLE_AUTOUPDATER=1`.** Versions are pinned and the rootfs is read-only.
- **CA trust without writing to `/etc`.** A proxy's CA is mounted as a file and
  pointed at via `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS` and `GIT_SSL_CAINFO`.
  `update-ca-certificates` is never run. mTLS to the proxy, when it arrives, is
  `CLAUDE_CODE_CLIENT_CERT` / `_KEY` — configuration, not a build change.
- **Subprocess env scrubbing stays on** (its default), which is why `bubblewrap`
  is a contract requirement. It strips Anthropic and cloud credentials from
  subprocess environments — the mitigation class for the Microsoft finding where
  the `Read` tool lifted `ANTHROPIC_API_KEY` from `/proc/self/environ`.
- **`seccompProfile: RuntimeDefault`,** which also satisfies Pod Security
  Standards `restricted`. On host `runc` this profile *breaks* bubblewrap by
  blocking `unshare(CLONE_NEWUSER)`; **under gVisor it is a no-op**, because the
  sentry serves the syscall itself. Measured both ways in [the k3s
  spike][k3sspike]. This is the reason gVisor is not merely defence-in-depth here:
  it is what lets the image contract and the strictest PSS profile coexist.
- **Capabilities.** Drop ALL. The spike ran the full path — agent, `git`, `gh`,
  and rootless Docker — at uid 1000 under gVisor with no added capability and
  without `privileged`, so the earlier worry that bubblewrap would need a
  capability did not survive measurement. bubblewrap creates its user namespace
  unprivileged; what it cannot do under gVisor is `--unshare-net`, and no
  capability fixes that.
- **gVisor (`runtimeClassName`) is load-bearing, not optional.** It is what makes
  the two bullets above true. It is also not free to the operator: k3s does not
  autodetect `runsc` and needs a containerd drop-in, and GKE needs a dedicated
  `--sandbox type=gvisor` node pool. That prerequisite belongs to the deployment
  story, but it is a prerequisite, not a nice-to-have.

Dependency caches live on ephemeral volumes and die with the pod. Reusing them
across runs is [issue #20][caches].

### A preflight that actually runs

The phase script's first act checks the contract: the agent CLI, `git`, `gh`, and
its own presence, via `command -v`. Failure writes a readable message to
`/dev/termination-log` and exits non-zero.

This exists specifically because scion's `required_image_tools` is declarative
config with zero readers, so a non-conforming BYO image there fails with no
diagnostic. A contract nobody verifies is a comment.

## Consequences

- **[Issue #11][selection] shrinks but survives.** With one base plus thin
  language layers there is still a language-to-image mapping to resolve, unlike
  the single-fat-base option where selection would have collapsed to "is there an
  override".
- **The image contract says nothing about the network**, which hands
  [issue #16][egress] a clean problem. scion's dormant `init-firewall.sh` —
  Anthropic's devcontainer script, baked into their Claude image with passwordless
  sudo, and invoked by nothing — is what putting egress in the image looks like.
- **No node/npm in the base image**, so the JavaScript ecosystem's CVE surface is
  absent entirely.
- **An org can bolt the agent layer onto its own base image**, which was the
  explicit requirement and scion's explicit failure.
- **Cross-run cache reuse is now ticketable** and graduated out of the map's fog
  as [issue #20][caches].

## Verification

### Verified: `--plugin-dir` exposes a skill in `-p` mode ✅

Tested against `claude 2.1.220`. A minimal plugin
(`.claude-plugin/plugin.json` + `skills/probe-implement/SKILL.md`, carrying
`disable-model-invocation: true` to mirror `/implement`):

```
claude -p --plugin-dir <path> "/probe-implement isolated789" --output-format json
→ subtype: success, is_error: False, terminal_reason: completed
  result: 'PLUGIN_DIR_SKILL_LOADED isolated789'
```

Critically, this was re-run with a **fresh `HOME` containing only
`.claude/.credentials.json`** — no `~/.claude/plugins/` at all — and it still
resolved. So the mechanism does not depend on `HOME`, which is exactly what the
ephemeral-`HOME` decision requires.

Two incidental findings from the same test:

- ~~**The skill resolved as bare `/probe-implement`.**~~ **Retracted — does not
  generalise.** That was a hand-rolled single-skill plugin. The real marketplace
  repo loads cleanly (`plugin_errors: null`, `mattpocock-skills@1.2.0`) but
  advertises only the **prefixed** forms in `slash_commands`:
  `mattpocock-skills:implement`, `:tdd`, `:code-review`. There is no bare
  `implement`. The phase script must pass the prefixed name, which is what both
  spikes do. Relevant to the run-plan ticket's prompt strings — this is the
  correction, not the original claim.
- **An empty `HOME` breaks authentication**, not skill loading: with no
  `~/.claude/.credentials.json` the run failed `terminal_reason: api_error`,
  `result: 'Not logged in · Please run /login'`. So an ephemeral `HOME` means the
  pod **must** supply credentials via the environment or `apiKeyHelper` — it
  cannot rely on anything resident in `~/.claude`. This is a constraint on the
  credential model, not an obstacle here.

### Verified against a cluster ✅

Both open items from [issue #22][verify] are now measured — first on kind, then on
k3s with real gVisor. Detail in [the k3s spike][k3sspike].

1. **`/dev/termination-log` is writable under `readOnlyRootFilesystem: true`** ✅ —
   the kubelet's bind mount survives the read-only rootfs, and the compact result
   round-trips to `.status.containerStatuses[].state.terminated.message`. Confirmed
   at uid 1000 *and* uid 0, with and without gVisor, in runs where `/` is
   separately confirmed read-only.
2. **`bubblewrap` initialises — partly.** Under gVisor at uid 1000, plain
   `bwrap --ro-bind / /` works and is **unaffected by `seccompProfile`**.
   `--unshare-net` fails (`loopback: Failed RTM_NEWADDR`), which is an open
   upstream bug ([bubblewrap#745][bw745], [gvisor#13438][gv13438]) still present in
   runsc `release-20260727.0`. At uid 0 bubblewrap fails entirely.

So the fallback this section anticipated — `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0` or
a looser security context — is **not needed for the seccomp reason**, which was the
likely one. What remains unavailable is bubblewrap's network namespace
specifically. Whether env scrubbing depends on it is not yet established; the
untried lead is `runsc bwrap`, a bubblewrap-CLI-compatible reimplementation that
ships in the same runsc release ([gvisor#13347][gv13347]), reachable via the
`CLAUDE_CODE_BUBBLEWRAP` variable the CLI already reads.

### Verified: the whole path runs under gVisor ✅

Three green end-to-end runs against a scratch repo — kind/host-`runc` at uid 1000,
then gVisor at uid 0 and uid 1000 — each ending in a pushed branch with CI green.
gVisor costs roughly **25% wall-clock** and nothing in dollars. Also measured:
**rootless Docker runs inside the plain gVisor sandbox at uid 1000, unprivileged**
(container, bind mount, `compose up`), which is a capability this ADR never
claimed and which the base image does not currently provide — see [issue
#28][dind].

## Alternatives rejected

- **One fat multi-language base** (the field's dominant strategy). Rejected on
  CVE surface: five runtimes' vulnerabilities for one runtime's use. Also, it
  would not have removed the run-time install step anyway.
- **A thin agent-only base with the toolchain installed by a setup script.**
  Trades a one-time, node-cached image pull for a recurring toolchain install —
  the wrong end of the trade without snapshots.
- **A separate install-script artifact** alongside the base image. A second code
  path to test and keep in sync, when copying the Dockerfile's `RUN` lines
  achieves the same thing at zero maintenance cost.
- **`devcontainer.json` consumption.** Nobody in the survey does it — a real gap,
  but also a signal, and it is a large surface for a v1.
- **Skills fetched at run time.** Non-reproducible, therefore un-evaluable.
- **The phase script embedded in the orchestrator via `go:embed`.** Its rationale
  evaporates once the image and orchestrator are versioned as a pair.

[map]: https://github.com/nissessenap/the-implementer/issues/1
[sandbox]: https://github.com/nissessenap/the-implementer/issues/3
[audit]: https://github.com/nissessenap/the-implementer/issues/6
[survey]: https://github.com/nissessenap/the-implementer/issues/7
[scion-research]: https://github.com/nissessenap/the-implementer/issues/2
[selection]: https://github.com/nissessenap/the-implementer/issues/11
[egress]: https://github.com/nissessenap/the-implementer/issues/16
[registries]: https://github.com/nissessenap/the-implementer/issues/19
[caches]: https://github.com/nissessenap/the-implementer/issues/20
[verify]: https://github.com/nissessenap/the-implementer/issues/22
[dind]: https://github.com/nissessenap/the-implementer/issues/28
[k3sspike]: https://github.com/nissessenap/the-implementer/pull/27
[bw745]: https://github.com/containers/bubblewrap/issues/745
[gv13438]: https://github.com/google/gvisor/issues/13438
[gv13347]: https://github.com/google/gvisor/pull/13347
