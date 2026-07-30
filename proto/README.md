# PROTOTYPE — throwaway

Not production code. Not the real base image. Exists to answer open questions in
[ADR 0001][adr1] / [ADR 0002][adr2] and [issue #22][22] against a real cluster,
before any Go is written.

```sh
export CLAUDE_CODE_OAUTH_TOKEN=$(claude setup-token)   # or set it however
./go.sh nissessenap/tmp-test-repo 1
```

Probe-only, costs nothing and needs no agent credentials:

```sh
PREFLIGHT_ONLY=1 ./go.sh nissessenap/tmp-test-repo 1
PREFLIGHT_ONLY=1 SECCOMP=RuntimeDefault JOB_SUFFIX=-rtdef ./go.sh nissessenap/tmp-test-repo 1
```

Four files: `Dockerfile` (ADR 0001 contract), `phase.sh` (the run plan, as the
pod's `command`), `job.yaml` (ADR 0002 primitive + ADR 0001 runtime posture),
`go.sh` (stands in for the Go orchestrator).

## Findings

### ⚠️ `seccompProfile: RuntimeDefault` breaks bubblewrap

Confirmed in kind, `runAsUser: 1000`:

| seccomp profile | `bwrap --ro-bind / /` |
|---|---|
| unset (kubelet default) | ok, including `--unshare-net` |
| `RuntimeDefault` | **fails** — `No permissions to create new namespace` |
| Docker default (outside k8s) | **fails** — same |

The default profile blocks `unshare(CLONE_NEWUSER)` without `CAP_SYS_ADMIN`.
`--cap-add SYS_ADMIN` gets further and then fails at `pivot_root`.

This collides head-on with **Pod Security Standards `restricted`, which
*requires* `seccompProfile: RuntimeDefault`**. ADR 0001 makes `bubblewrap` a
contract requirement and treats capabilities as the only tension; seccomp is the
sharper one, and it is the setting a hardened cluster is most likely to already
have. Options, none yet decided: ship a custom seccomp profile allowing
`unshare`, run `Unconfined` and lean on gVisor for isolation, or accept
`CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0` and lose the env-scrub mitigation.

### ✅ `/dev/termination-log` is writable under `readOnlyRootFilesystem: true`

[Issue #22][22] question 1. The kubelet's bind mount survives the read-only
rootfs, and the compact result channel round-trips to
`.status.containerStatuses[].state.terminated.message`. `/` is confirmed
read-only in the same run, so this is not a false positive.

### ⚠️ Matt's skills register **prefixed**, not bare

ADR 0001 records `--plugin-dir` resolving a skill as bare `/probe-implement`.
That was a hand-rolled single-skill plugin. The real marketplace repo loads
cleanly (`plugin_errors: null`, `mattpocock-skills@1.2.0`) but advertises
`mattpocock-skills:implement`, `mattpocock-skills:tdd`,
`mattpocock-skills:code-review` — no bare `implement` in `slash_commands`.
The phase script uses the prefixed form. Bare form untested with credentials.

### ✅ Skills load from a baked, read-only path with an ephemeral `HOME`

`--plugin-dir /opt/skills`, `HOME` an `emptyDir`, no `~/.claude/plugins`.
Cloned at a pinned SHA with `.git` removed.

### ✅ Agent CLI installs as a bare pinned binary

`install.sh` writes a launcher into `$HOME`, which is ephemeral here. Fetching
`downloads.claude.ai/claude-code-releases/$VERSION/linux-x64/claude` directly and
verifying the manifest checksum avoids that, and keeps node/npm out.

### ➖ gVisor not exercised

kind has no `runtimeClassName: gvisor`. Feasible via a custom `kindest/node` with
`runsc` + `containerdConfigPatches`, but the only PoC is from 2019. Cheaper path
for the question we actually care about: `docker run --runtime=runsc`.
Upstream reports bwrap's `--unshare-net` failing under gVisor at
`loopback_setup()` ([bubblewrap#745]) — so gVisor may reintroduce the failure
that seccomp-unset avoids. `go.sh` uses the RuntimeClass automatically if the
cluster has one.

### ✅ The whole path works end to end

One green run against [tmp-test-repo#1][t1]: preflight → clone → issue fetch →
`claude -p /mattpocock-skills:implement` → commit → push → CI green.

- **162s, $1.01** for a ~60-line change. Cost is per-run and visible in the
  `result` message, so `--max-budget-usd` has something real to bound.
- **`--json-schema` round-trips.** `structured_output` came back valid on the
  success path, carrying `status`, `summary`, `pr_title`, `files_changed` — the
  orchestrator can build a PR deterministically from it without parsing prose.
- **`/code-review` spawned parallel subagents** (Standards + Spec) inside `-p`,
  as the capability audit predicted.
- **The agent wrote a conventional-commit message and `Closes #1` unprompted.**
  Neither was in the brief. Worth deciding whether the run plan should own those
  rather than leave them to the model.

### ⚠️ A transient 529 burns the run

One earlier attempt died after **209s** on ten `api_retry` events, all
`529 overloaded`, before emitting any turn. The retry ladder is internal to the
agent CLI and caps at 10; there is no partial progress to resume from. So an
unattended run has a failure mode that costs $0 and three minutes and is fixed
entirely by trying again — which is an argument for orchestrator-level retry on
`is_error` with no turns, distinct from retrying a *failed implementation*.

Also: `structured_output` is **absent** on error paths, so status fell to
`unknown` and the phase script initially mistook an infra failure for "the agent
declined". A `result` carrying `is_error` now fails hard regardless of status.

### ➖ CI triggering is NOT settled

The push triggered the target repo's workflow and it went green — but this
prototype pushes with a **user PAT** (`gh auth token`), not a GitHub App
**installation token**. [Issue #13][13]'s open question is specifically about the
latter. This result does not answer it.

[t1]: https://github.com/nissessenap/tmp-test-repo/issues/1
[13]: https://github.com/nissessenap/the-implementer/issues/13

[adr1]: ../docs/adr/0001-sandbox-image-strategy-and-byo-contract.md
[adr2]: ../docs/adr/0002-a-run-executes-as-a-kubernetes-job.md
[22]: https://github.com/nissessenap/the-implementer/issues/22
[bubblewrap#745]: https://github.com/containers/bubblewrap/issues/745
