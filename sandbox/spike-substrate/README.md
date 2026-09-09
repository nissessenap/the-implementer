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

The first template create builds the golden snapshot and takes minutes. Expect
~450s and ~$2 for the run itself. Transcript:

```sh
kubectl --context kind-kind ate logs actors issue-1 -a implementer-spike
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

## Verdict

*Unrun.* Record the answer here — the question it settled, and whether ADR 0002's
three-part reconsider test is any closer to met.

Kill criteria worth writing down as they happen:

- [ ] The golden snapshot builds at all for a ~1.5GB image.
- [ ] `bubblewrap` still works — the base image is `USER 1000` and gVisor is the
      sandbox class, the same pairing ADR 0001 depends on.
- [ ] The actor survives one full run without being suspended under it.
- [ ] A second `run.sh` for the same issue collides instead of starting a run.

[sub]: https://github.com/agent-substrate/substrate
[i185]: https://github.com/agent-substrate/substrate/issues/185
[i1526]: https://github.com/agent-substrate/substrate/issues/1526
[adr2]: ../../docs/adr/0002-a-run-executes-as-a-kubernetes-job.md
[adr4]: ../../docs/adr/0004-the-orchestrator-is-a-controller-with-a-webhook-front-end.md
[adr5]: ../../docs/adr/0005-credentials-terminate-at-the-credential-proxy.md
[ctx]: ../../CONTEXT.md
