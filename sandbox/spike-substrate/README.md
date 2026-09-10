# Spike: the run plan on Agent Substrate workers

⚠️ **Throwaway.** Nothing here is production code, nothing imports it, and it is
meant to be deleted once it has answered its question. It lives next to
`sandbox/` because the thing it adapts is `sandbox/phase.sh`.

## The question

> Can [Agent Substrate][sub] carry the run plan without changing ADR 0001's BYO
> contract, and what does that cost in ADR terms?

Behind it is the one line [ADR 0002][adr2] left open when it rejected a warm pool:

> Reconsider only when *all three* hold: credentials terminate at the proxy,
> caches persist, and per-run identity survives a shared pool ServiceAccount.

The first now holds — [ADR 0005][adr5] shipped. Substrate is the first thing to
offer a credible answer to the third (`ateapi.ActorIdentity`: `MintJWT`,
`MintCert`, SPIFFE mTLS per actor), and its mandatory golden snapshots are a
partial answer to the second. **That, not the density, is why this is worth a
day.**

## What substrate does not have

Established by reading the code, the docs, the roadmap and ~1500 issues:

| The Job primitive relied on | Substrate |
| --- | --- |
| one-shot batch workload | **none.** No batch/job/one-shot concept anywhere. Discussions empty. |
| exec / run-a-command | **none.** [#185][i185] is open and unbuilt. Work arrives as an ordinary HTTP request through `atenet-router`, header `ate-target-actor: <atespace>/<actor>`; the *workload* is whatever HTTP server the container runs. |
| terminal state, exit code | **none.** `ActorState` is `{RESUMING,RUNNING,SUSPENDING,SUSPENDED,PAUSING,PAUSED,CRASHED,DELETING}`. No `SUCCEEDED`, no exit-code field. `CRASHED` means *the worker pod vanished* and is unrecoverable ([#1526][i1526]). |
| `activeDeadlineSeconds` | **none.** No per-actor deadline. |
| `ttlSecondsAfterFinished` | none; reclaim is idle-Suspend or an explicit Delete. |
| `backoffLimit: 0` | n/a, and in our favour: nothing supervises the workload process, so there is no resurrection hazard like the one that sank the `Sandbox` CRD. |
| deterministic name → `AlreadyExists` | **kept.** Actors are `(atespace, name)`, DNS-1123. |

The roadmap's only coding-agent line asks for the opposite shape: *"Stateful
Coding Adapters … to enable AI coding tasks that **maintain persistent
filesystem state** across terminal sessions."*

## Why the contract needed no change anyway

`phase.sh` already takes its result channel from `$TERM_LOG` — a path in an env
var, not a hard-coded `/dev/termination-log`. So the shim points it at a file
and returns the bytes. [ADR 0004][adr4] called this in advance:

> Nothing is owed to keep this reversible. The phase script already takes its
> inputs from argv and env and returns results through a marker channel, and it
> is the pod's command — so whatever invokes it is already swappable.

## The shape

**Actor per run. ActorTemplate per toolchain. WorkerPool is the warm capacity.**

```
run.sh
  ├─ kubectl ate create actor issue-42 --template implementer-go   ← collides on a re-label
  └─ POST /run   ate-target-actor: implementer-spike/issue-42
        body {repo, issue, toolchain, gh_token, claude_token}
        ↓ router resumes the actor onto a warm gVisor worker over an mTLS tunnel
     shim  (the actor's command, ~110 lines)
        ├─ TERM_LOG=$HOME/result.json ; exec /usr/local/bin/phase.sh
        └─ 200 with whatever phase.sh wrote there
        ↓
  blob on stdout                    ← the answer the spike is for
```

An actor per *toolchain* handling runs serially was rejected: it reuses `HOME`
and the workspace across runs, loses per-run identity, and breaks "nothing
survives the run by default".

`replicas: 1` per pool is also the concurrency limit — a worker hosts at most
one actor at a time. Accepted waste.

## What this costs in ADR 0005 terms

**This spike puts both credentials inside the sandbox.** There is no proxy in
it. That is a deliberate, bounded regression and it is the single most
important thing not to copy into anything real.

The mitigation that survives review: an `ActorTemplate`'s env is **literal-only**
(no Secret `valueFrom`) and the template is immutable and shared by every run of
a toolchain — so a token there would be exactly upstream's stated failure mode,
*"pod env exists before the user does, so the same credentials reach every
tenant sandbox"*. Hence **credentials travel in the POST body, per run, over the
router's mTLS, and never in the template.** Worse than the proxy, better than
the template, and absent from `kubectl describe`. Upstream agrees it is a gap:
the roadmap lists *"Credential injection … to eliminate exposure of bearer
tokens to actors."*

Use a scratch repo and a token you are happy to burn.

## Install (kind only — never a real cluster)

`run.sh` refuses any `KUBECTL_CONTEXT` that does not start with `kind-`.

```sh
cd ~/projects/oss/substrate
hack/create-kind-cluster.sh                    # cluster + local registry on :5001
hack/install-ate-kind.sh --deploy-ate-system   # postgres, RustFS, ateapi, atelet, atecontroller, atenet, podcert
```

### The router's route timeout — required, and not a default

`atenet-router` gives a workload request **10 seconds** end to end
(`defaultRouteTimeout`, `cmd/atenet/internal/router/xds.go:143`). A run is
minutes, so the POST comes back `504` long before `phase.sh` finishes. Raise it
once per cluster:

```sh
kubectl -n ate-system patch deploy atenet-router --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--route-timeout=20m"}]'
kubectl -n ate-system rollout status deploy/atenet-router
```

This is a supported setting rather than a workaround — the flag's own help says
*"Raise it for actors whose turns legitimately run long — a harness relaying an
LLM completion holds the request open for the whole generation."*

⚠️ The manifest derives `--drain-timeout` from the route timeout and notes that
the sum "must fit within `terminationGracePeriodSeconds`, or the kubelet
SIGKILLs mid-drain" — which is 60s. So restarting the router during a run drops
the in-flight request. Accepted here; a real version would either raise the
grace period to match or stop holding the request open at all.

Snapshotting cannot be switched off — `snapshotsConfig` is a required
`ActorTemplate` field and `CreateActorTemplate` always builds a golden snapshot.
The kind overlay ships **RustFS**, an in-cluster S3, so nothing leaves the
laptop and no Google credential is involved. The spike never calls
Suspend or Resume itself.

## Run

```sh
make -C ../.. sandbox-image                    # ghcr.io/nissessenap/implementer-base:dev
CLAUDE_CODE_OAUTH_TOKEN=… ./run.sh me/scratch#1 go
```

`run.sh` builds and pushes the actor image to the local registry (an
`ActorTemplate` image must be digest-pinned, and `kind load docker-image`
produces no digest to pin), ensures the pool, atespace and template, creates one
actor, waits for `/readyz`, POSTs once, prints the blob and deletes the actor
with `--any-state` (a `RUNNING` actor is not otherwise deletable).

The first template create builds the golden snapshot and takes minutes.

**Cost.** `~450s and ~$2` is [CLAUDE.md][cmd]'s measured figure for three phases
against a small repo, not a budget. The ceiling is much higher: `phase.sh`
defaults to `--max-budget-usd 10` *per phase*, and the Job builder does not set
it, so a real run is allowed $30. `MAX_USD_PER_PHASE` (default `2` here) is what
keeps the spike cheap; the shim itself passes the value through undefaulted so
its behaviour matches a Job's. Transcript:

```sh
# `kubectl ate` needs the plugin installed; otherwise, from the substrate repo:
#   go run ./cmd/kubectl-ate logs actors issue-1 -a implementer-spike --context kind-kind
kubectl ate logs actors issue-1 -a implementer-spike --context kind-kind
```

Offline check of the only non-trivial logic — the result channel:

```sh
go test ./sandbox/spike-substrate/shim/
```

## What replaces what

- **Deadline** → `--max-time` on the caller. It does **not** stop the actor; the
  script's `EXIT` trap deletes it. `MAX_BUDGET_PER_PHASE_USD` remains the real
  spend cap.
- **The ending with no result** → the HTTP call errors, or `phase.sh` writes no
  blob and the shim synthesises a `failed` Result. [CONTEXT.md][ctx]'s sharper
  argument still stands — a worker evicted mid-run reports nothing and goes
  `CRASHED`, so a real version still needs an external watcher; only its shape
  changes, from a Pod informer to a `GetActor` poll.
- **Idempotency** → the actor name is derived from the issue and `CreateActor`
  collides. Same mechanism, different object.

## Not built, deliberately

No webhook front-end, no informer, no issue comment, no PR, no orchestrator
change of any kind. If the answer is yes, the create-and-POST moves into
`orchestrator run` and reuses the informer's comment writer. Until then this
directory is the whole of it.

## Verdict: yes, it runs — at the cost of two ADR properties

Run against [nissessenap/tmp-test-repo#3][issue3] on 2026-09-10, `kind` +
`ate-system`, one warm gVisor worker:

```
status      completed        pr_title  feat(calc): add Min function mirroring Max
commits     1                message   pushed implementer/issue-3
cost_usd    1.37             phases    implement / review / ponytail — all completed
elapsed_s   170
```

Branch `implementer/issue-3` really landed — commit `ae0f144`, `calc/calc.go` +
`calc/calc_test.go`, authored `the-implementer`. So the answer to the question
is: **the run plan needs no change, and `phase.sh` was never touched.** The whole
adaptation is the shim, and `$TERM_LOG` being an env var is what made it free.

### Kill criteria, settled

- ✅ **The golden snapshot builds** for the ~1.5GB actor image, and resume is
  seconds.
- ❌ **`bubblewrap` as uid 1000 — moot, and the reason is bad.** The run does not
  execute as 1000 at all (below). `phase.sh` only presence-checks `bwrap`
  (`sandbox/phase.sh:79`) and never invokes it, so nothing failed — but ADR 0001's
  non-root default is gone rather than satisfied.
- ✅ **The actor survives a full run** without being suspended under it.
- ✅ **A second run of the same issue collides** on `AlreadyExists` and is
  refused, leaving the in-flight actor untouched. ADR 0004's idempotency
  mechanism carries over exactly.

### The two regressions

1. **Credentials are inside the sandbox** (ADR 0005). Argued above: per-run in
   the request body, never in the shared immutable template.
2. **The run executes as root** (ADR 0001). Not a choice. `atelet` unpacks the
   image with every path owned by `0:0` — the image's `useradd -u 1000 -m` home
   arrives root-owned `0700`, which `GET /debug` prints — and an `ActorTemplate`
   has no `runAsUser`: its `SecurityContext` carries `capabilities` and nothing
   else. Dropping with `setpriv` was tried and fails, because uid 1000 owns
   nothing in the rootfs it needs. `IS_SANDBOX=1` is what makes the agent CLI
   accept `--dangerously-skip-permissions` as uid 0, and gVisor is the
   "deliberate sandbox" that setting refers to.

### Also learned, and not obvious from the docs

- **The router gives a request 10 seconds** by default. Raising it is a supported
  flag, but it is not optional here — see above.
- **`ActorTemplates` are immutable**, so a template named for the toolchain alone
  silently pins its first image forever. The digest is in the name for that
  reason, which incidentally restores ADR 0003's `imageID` digest honesty. It
  still goes stale on a template-YAML-only edit; delete it by hand.
- **`kubectl ate` is a plugin nobody installs.** `run.sh` shells out to
  `go run ./cmd/kubectl-ate`, as substrate's own installer does.
- **No exec RPC ([#185][i185]) means no debugging from outside.** The `ateom`
  container is distroless, so there is no shell on the worker either. `GET /debug`
  on the shim exists because it was the only way to see the unpacked rootfs.

### Known gap in the spike itself

Every toolchain builds from `implementer-base`, so **there is no language
toolchain in the sandbox** — the implement phase said so itself: *"Could not run
go build/test/gofmt to verify since no Go toolchain is present in this
sandbox."* ADR 0003's thin per-language image is named but not wired up. Fix
before drawing any conclusion about output quality.

### What it does not answer

Whether substrate is worth it. Nothing here tested the density it exists for —
one actor on one worker, never suspended mid-run, snapshots written and ignored.
ADR 0002's three-part reconsider test is still where it was, except that
`ActorIdentity` remains unexercised: **per-run identity, the one open condition,
is exactly what this spike did not touch.** That is the next question, not a
migration.

[issue3]: https://github.com/nissessenap/tmp-test-repo/issues/3
[sub]: https://github.com/agent-substrate/substrate
[i185]: https://github.com/agent-substrate/substrate/issues/185
[i1526]: https://github.com/agent-substrate/substrate/issues/1526
[adr2]: ../../docs/adr/0002-a-run-executes-as-a-kubernetes-job.md
[adr4]: ../../docs/adr/0004-the-orchestrator-is-a-controller-with-a-webhook-front-end.md
[adr5]: ../../docs/adr/0005-credentials-terminate-at-the-credential-proxy.md
[ctx]: ../../CONTEXT.md
[cmd]: ../../CLAUDE.md
