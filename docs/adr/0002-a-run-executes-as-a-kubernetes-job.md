# 2. A run executes as a Kubernetes Job

Date: 2026-07-29

## Status

Accepted

## Context

The [README][map] assumed [scion][scion-research] for distribution plus
[agent-sandbox][sandbox] for isolation. The scion research ruled scion out for
v1, which left the narrower question this ADR answers: what Kubernetes object
does the orchestrator create when a run starts?

The question is narrower than it first appears because the [map][map] had already
fixed the shape of a run:

- **Fire-and-forget batch.** The pod's `command` is the [phase script][adr1]; it
  runs to completion in minutes and exits. Nothing drives it from outside.
- **The result channels are pod-level.** A compact structured result via
  `/dev/termination-log` →
  `pod.status.containerStatuses[].state.terminated.message`; the full transcript
  via `pods/log`. No `kubectl exec` anywhere.
- **No database.** The orchestrator is a controller with a webhook front-end;
  state lives in Kubernetes objects and GitHub, so a workload name derived from
  the issue is what makes webhook redelivery idempotent.
- **Nothing survives the run.** The agent pushes its own branch with a
  `contents: write` token, so no commit is stranded in a volume and no retained
  PVC is required for the happy path.

For that shape a plain `batch/v1` Job natively provides `backoffLimit`,
`activeDeadlineSeconds`, `ttlSecondsAfterFinished`, completion tracking and
`runtimeClassName: gvisor`, with **no CRDs to install**. What agent-sandbox
appeared to add on top was `operatingMode: Suspended`, a headless Service, warm
pools, and `SandboxTemplate`-level NetworkPolicy.

So the real question was: **is agent-sandbox earning its place in v1, or is it a
bet on trigger #3 that we should not yet be paying for?**

Resolving it required reading the controller rather than the API surface. The
[CRD research][sandbox] established the API; this ADR rests on a second pass over
`kubernetes-sigs/agent-sandbox` at `56d6269` (upstream `main`, 604 commits past
`v0.1.0`, `v0.5.3` era) that traced the reconcile paths. Line references below
are to that tree.

## Decision

**A run executes as a single `batch/v1` Job**, one pod, gVisor via
`runtimeClassName` on the pod template. No CRDs, no additional controller, no
warm pool.

### The Job's shape

| Field | Value | Why |
| --- | --- | --- |
| `spec.parallelism` / `spec.completions` | unset (1) | One run, one pod, one branch. |
| `spec.backoffLimit` | `0` | A retry is a second *paid* agent run against an unchanged issue and an unchanged Agent Brief. Retry and repair policy is deliberately still open on the [map][map]; `0` is the choice that does not pre-empt it. |
| `spec.activeDeadlineSeconds` | set | A wall-clock ceiling so a wedged run cannot hold a gVisor pod indefinitely. Complements `--max-budget-usd` and `--max-turns` from the [capability audit][audit], which cap spend but not hangs. |
| `spec.template.spec.restartPolicy` | `Never` | Required for the terminal-state semantics the result channel depends on. |
| `spec.template.spec.runtimeClassName` | `gvisor` | The whole of our isolation story; see below. |
| `spec.ttlSecondsAfterFinished` | not decided here | A tuning knob, not architecture. It trades debuggability for tidiness and belongs to [observability][observability], which owns whether the transcript has left the pod by then. |
| `podFailurePolicy`, `suspend` | unused | No taxonomy of pod-level failure reasons to act on yet. |

The Job name is derived from the issue, so webhook redelivery collides on
`AlreadyExists` — the idempotency mechanism the map assumed.

### gVisor is orthogonal to the choice

`runtimeClassName` has **zero** occurrences in any non-test Go file across
agent-sandbox's `api/`, `extensions/`, `controllers/`, `internal/`, `cmd/` and
`sandbox-router/`. It exists there only as a field of the embedded
`corev1.PodSpec`, and the controller deep-copies the pod spec verbatim
(`controllers/sandbox_controller.go:1153`, `:1176`, touching only `.Volumes`).
Its own README says it *"delegates low-level container isolation to secure
Sandbox Runtimes (like gVisor or Kata Containers)... via RuntimeClass."*

So agent-sandbox contributes nothing to sandboxing. It is a *lifecycle*
orchestrator, and lifecycle is the axis on which to judge it.

### Why not a directly-created `Sandbox`

A `Sandbox` becomes exactly one bare Pod with the PodSpec copied verbatim and
nothing injected. Judged as a batch lifecycle manager it is **worse than a Job on
the one axis that matters**, and it carries a hazard a Job does not.

- **No terminal-state GC.** `Sandbox.spec` is exactly
  `{operatingMode, podTemplate, service, shutdownPolicy, shutdownTime,
  volumeClaimTemplates}`. There is no `ttlSecondsAfterFinished` equivalent —
  `shutdownTime` is an absolute `metav1.Time`, and with it unset
  `checkSandboxExpiry` returns immediately (`sandbox_controller.go:1518-1520`),
  so **nothing ever deletes the pod or the Sandbox**. A duration measured from
  completion exists only as `SandboxClaim.spec.lifecycle.ttlSecondsAfterFinished`
  (`extensions/api/v1beta1/sandboxclaim_types.go:73-77`), i.e. behind a warm
  pool. Terminal-state cleanup was the main thing a controller-managed object was
  supposed to buy us over a bare Pod, and this one does not have it.

- ⚠️ **Pod resurrection.** If the pod object disappears — eviction, node drain,
  terminated-pod GC, a manual delete — while `operatingMode: Running` and
  `shutdownTime` is unset, the get misses (`:966-979`) and the controller creates
  a fresh pod from `podTemplate` (`:1106-1194`). **The phase script re-runs from
  scratch**: another paid agent run, another push, no attempt counter anywhere. A
  Job counts that against `backoffLimit`. This is a silent duplicate-run hazard
  and it is the single strongest argument against the CRD.

- **The completion signal is not durable.** The `Finished` condition is derived
  from live pod phase (`:462-471`) and is *removed* when the pod object goes
  (`:310-312`). A Job's `status.conditions` and `completionTime` persist on the
  Job.

- **`status` tells a batch consumer nothing.** It is
  `{serviceFQDN, service, conditions, selector, podIPs, nodeName}`. No exit code,
  no termination message, no timestamps; the pod name is an annotation
  (`agents.x-k8s.io/pod-name`), not a status field. Nothing in the repo reads
  `ContainerStatuses` at all. We read the Pod object directly either way — so the
  CRD adds an indirection without removing one.

- **No guardrail on `restartPolicy`.** It is neither defaulted nor validated —
  there are no admission webhooks in the project, only conversion webhooks
  (`cmd/agent-sandbox-controller/main.go:428-434`). Omit it and the apiserver
  defaults the pod to `Always`, the kubelet restarts the exited container
  forever, the pod never reaches `Succeeded`, and `Finished` never fires. A Job's
  pod template rejects `Always` at admission.

- **Template NetworkPolicy is unreachable.** Not merely absent — *unopt-in-able*.
  The generated policy selects
  `agents.x-k8s.io/sandbox-template-ref-hash` (`extensions/controllers/sandboxtemplate_controller.go:109-114`),
  which is propagated only when the Sandbox's controllerRef is in the
  `extensions.agents.x-k8s.io` group (`sandbox_controller.go:595-622`) — a
  directly-created Sandbox has none. And it cannot be set by hand: `isSystemLabel`
  strips any `agents.x-k8s.io/` key from `podTemplate.metadata.labels` on create
  and update, and actively scrubs it (`:1111-1119`, `:1244-1254`). A bare Sandbox
  gets no NetworkPolicy, and no route to one.

- **Dependency cost.** A CRD plus a controller to install and upgrade, pre-1.0 at
  `v0.5.3`, with in-code TODOs on core invariants (*"find a better way to make
  sure one sandbox has at most one pod"*, `:944`) and a documented
  non-field-compatible `v1alpha1`→`v1beta1` migration.

### `operatingMode: Suspended` is not a resume primitive — retracted

The [map][map] recorded, in two places, that `operatingMode: Suspended` was the
only native resume primitive available and therefore the reason to keep
agent-sandbox in play for trigger #3. **That is retracted.** For our workload
Suspend is not neutral, it is destructive.

The suspend path's only guards are ownership and a zero `DeletionTimestamp`
(`sandbox_controller.go:986`). **Pod phase is never consulted**, so it deletes an
*already-terminated* pod — taking with it
`containerStatuses[].state.terminated.message`, the logs, the pod-name annotation
(`:1004-1007`), and the `Finished` condition (`:310-312`). Flipping back to
`Running` builds a brand-new pod from `podTemplate` (`:1106-1177`) and the
`command` re-runs from the top. There is no checkpoint/restore mechanism anywhere
in the project.

So Suspend is a **stop/recreate** primitive for long-running or interactive
sandboxes. Our pod exits on its own; there is nothing to suspend.

**Trigger #3 rides on the volume, not on the primitive.** Resuming a run means
`claude --resume <session-id>` against a persisted session directory — a PVC plus
a fresh workload, which a Job mounts exactly as well as a Sandbox does. The
non-foreclosure obligation therefore lands somewhere concrete and cheap: keep the
workspace and `HOME` mounts *swappable* from `emptyDir` to a PVC, and do not bake
"nothing survives the run" into the phase script or the orchestrator's Go.

### Why not a warm pool

Warm pools are the wrong tool for this workload rather than a broken tool, and it
is worth recording which is which, because two of the four blockers are **our own
decisions**, not agent-sandbox's limitations.

The pool's intended shape is legible in the code: the Go SDK's entry point is
`CreateSandbox(ctx, warmPoolName, ns)` — warm-pool-only — and that path requires
`sandbox-router` deployed plus the image serving a runtime HTTP API on `:8888`
behind a readiness probe. Its flagship examples are
`hermes-agents-as-a-service` and `vscode-sandbox`. That is **interactive
sandbox-as-a-service**: a generic pre-booted image, a human waiting on p99
latency, per-request work arriving *over HTTP after the pod is already warm*, and
platform-owned credentials. Upstream states the trade-off plainly — *"pod env
exists before the user does, so the same credentials reach every tenant
sandbox."*

Against that, our four blockers:

| Blocker | Whose constraint | Escapable? |
| --- | --- | --- |
| Per-run **secret** (the `contents: write` installation token) | **Ours** — the charted decision to split the token by permission | Only via [credential termination at the proxy][credentials], which the map puts out of scope. `SandboxClaim.spec.env` is plaintext `{name,value}` *and* forces a cold start; an annotation would put a live token in `kubectl describe`. |
| Per-run **image** | **Ours** — thin per-language images ([ADR 0001][adr1], [selection][selection]) | No. A pool is per-`SandboxTemplate`, so N languages means N pools of idle pods. |
| Cold-start bypass | agent-sandbox | `len(claim.Spec.Env) > 0 \|\| len(claim.Spec.VolumeClaimTemplates) > 0` (`sandboxclaim_controller.go:1596-1601`) — the *entire* predicate. |
| Per-run repo / issue / branch | plain need | **Yes.** `additionalPodMetadata` does *not* force a cold start; it is merged and patched onto the adopted pod (`:397-457`). Annotations plus a downward-API volume carry it fine, with a fixed script path as the `command`. |

And the value on offer is small for us regardless. A pool saves image pull,
scheduling and gVisor boot — seconds — against runs measured in minutes. [ADR
0001][adr1] already decided there is no pre-agent setup phase and the agent
installs its own dependencies, and dependency caches are ephemeral pending
[#20][caches], so a warm pod has no repo, no dependencies and cold caches. **The
pool would pre-warm nothing that costs us anything.**

Reconsider only when *all three* hold: credentials terminate at the proxy, caches
persist, and per-run identity survives a shared pool ServiceAccount — the last
being a real open problem, since per-run branch scoping is the reason the proxy is
attractive and a pool's projected tokens are indistinguishable between runs.

### DNS is the cluster's problem

We set no `dnsPolicy` and no `nameservers`. Egress-capable cluster DNS is a
cluster-admin concern.

Recording this because agent-sandbox's claim path would have made the choice for
us: `ApplySandboxSecureDefaults` forces `automountServiceAccountToken: false`
and, in secure-default mode, `dnsPolicy: None` with `8.8.8.8`/`1.1.1.1`
(`extensions/controllers/utils.go:24-48`). A directly-created Sandbox gets none
of that — which for batch is the better default, and is what a Job gives us
anyway. The [egress ADR][egress] owns the resulting posture.

## Consequences

- **No CRDs, no operator, no Helm dependency.** The install story reduces to our
  own Deployment plus RBAC on `jobs`, `pods` and `pods/log`. Feeds the
  deployment-and-install work the map leaves for last.
- **`agent-sandbox` leaves the dependency set** and joins scion as prior art. Its
  threat model and secure-defaults helper stay worth reading.
- **The orchestrator's informer watches Pods, not Jobs.** Both result channels
  are pod-level, so the Job is a lifecycle wrapper we create and then largely
  ignore. Pods are selected by our own labels rather than
  `batch.kubernetes.io/job-name`, which keeps the watch independent of the owner
  object.
- **We own NetworkPolicy outright.** True under either primitive, but now with no
  template-shaped alternative to weigh; [the egress ADR][egress] has a clean
  field.
- **`backoffLimit: 0` means a lost pod is a failed run**, surfaced to the human
  rather than silently retried. The right default while retry policy is
  undecided, and the opposite of the resurrection behaviour we rejected.
- **Terminal-state cleanup is now an explicit open question**, not something the
  primitive answers for us. It sits with [observability][observability] together
  with whether Claude Code's built-in OTel carries the transcript or only
  metrics and events — if only the latter, `--output-format stream-json` on
  stdout remains the transcript channel and needs a collector before finished
  pods can be reaped. Note the constraint that shapes it: deleting the Pod object
  makes `pods/log` return `NotFound` regardless of any TTL window, because the
  only API path to `/var/log/pods/<uid>` is keyed by the Pod object and kubelet's
  log GC reaps the directory once it is gone.
- **Trigger #3 is not foreclosed**, and the obligation is now concrete and
  testable: the workspace and `HOME` mounts stay swappable from `emptyDir` to a
  PVC.

## Alternatives rejected

- **Directly-created agent-sandbox `Sandbox`.** No terminal-state GC, a
  non-durable completion condition, a `status` with nothing a batch consumer
  needs, no `restartPolicy` guardrail, no reachable NetworkPolicy — and pod
  resurrection silently re-running a paid agent run. A CRD and controller bought
  for a strictly worse batch lifecycle.
- **Warm-pool `SandboxClaim`.** Defeated by our own per-run token and per-language
  images before agent-sandbox's cold-start predicate even applies; and it would
  pre-warm nothing expensive, since the agent installs its own dependencies into
  ephemeral caches.
- **A bare `Pod`.** Honest baseline — `activeDeadlineSeconds`, `restartPolicy`,
  `runtimeClassName` and a deterministic name are all already pod-level, so the
  Job's headline features are mostly inert here. Rejected because
  finished-workload GC and a bounded attempt count are logic a Job already has
  and we would otherwise write and maintain ourselves.
- **Building on scion.** Already rejected by [its research][scion-research]; noted
  here only because it was the README's original assumption.

[map]: https://github.com/nissessenap/the-implementer/issues/1
[adr1]: 0001-sandbox-image-strategy-and-byo-contract.md
[scion-research]: https://github.com/nissessenap/the-implementer/issues/2
[sandbox]: https://github.com/nissessenap/the-implementer/issues/3
[audit]: https://github.com/nissessenap/the-implementer/issues/6
[selection]: https://github.com/nissessenap/the-implementer/issues/11
[credentials]: https://github.com/nissessenap/the-implementer/issues/14
[egress]: https://github.com/nissessenap/the-implementer/issues/16
[observability]: https://github.com/nissessenap/the-implementer/issues/17
[caches]: https://github.com/nissessenap/the-implementer/issues/20
