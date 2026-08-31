# 4. The orchestrator is a controller with a webhook front-end, and v1 has no database

Date: 2026-08-02

## Status

Accepted

## Context

This decision was made while charting the [map][map] and has been load-bearing
ever since — [ADR 0002][adr2] cites it as a fixed premise, [ADR 0003][adr3]
depends on it for "detection adds no state", and the trigger, credential and
observability decisions were all made assuming it. It was never written down
anywhere but a bullet in an issue body. This ADR records it, and records the
consequences that only became visible once the decisions downstream of it had
all landed.

The question: **what runs the run?** A GitHub webhook arrives; minutes later a
pull request has to exist. Something has to survive the gap, know that a run is
in flight, notice when it ends, and act on the result. The obvious shapes are a
job queue with a database, a long-lived process that blocks on the run, or a
Kubernetes controller.

Two properties of the workload decide it:

- **The state is already in Kubernetes.** [ADR 0002][adr2] makes a run a Job.
  Whether a run exists, whether it is finished, when it started, what image it
  resolved to and what it wrote to `/dev/termination-log` are all fields on
  objects the apiserver already stores and already replays to a watcher.
- **The rest of the state is already in GitHub.** The issue, the labels, the
  branch, the PR and the comment thread are GitHub's, not ours.

A database would therefore hold a *copy* of state owned elsewhere, and the
copy's job would be to disagree with the original.

## Decision

**The orchestrator is a Kubernetes controller with a webhook front-end, and v1
has no database.**

```
GitHub ──webhook──► front-end ──create Job──► apiserver
                                                  │
                                             Job runs
                                                  │
                    informer ◄──watch Pods────────┘
                        │
                        ├─► POST pull request       (GitHub)
                        └─► POST issue comment      (GitHub)
```

### The two halves

**The front-end** handles `issues`/`labeled` webhooks ([`palantir/go-githubapp`][pkgs]),
performs [ADR 0003][adr3]'s toolchain detection with 1–2 REST calls, and creates
one Job. Then it is done — it does not wait, block, or hand anything to a
worker. The HTTP response is sent before the run finishes.

**The informer** watches **Pods**, not Jobs. This is not a stylistic choice: both
result channels are pod-level. The compact result is
`pod.status.containerStatuses[].state.terminated.message`, the resolved image
digest [ADR 0001][adr1] requires is `containerStatuses[].imageID`, and the
transcript is `pods/log` on the Pod. A Job-level watch would see completion and
then have to go and find the Pod anyway.

On a terminal Pod the informer reads the blob, and — per the [push and PR
decision][push] — opens a draft pull request if commits exist, or posts an issue
comment if they do not.

### State lives in Kubernetes objects and GitHub

| What | Where it lives |
| --- | --- |
| A run exists | The Job object |
| Which issue a run is for | **Annotations** (`owner`, `repo`, `issue`) on the Job **and** on `spec.template` |
| Run finished, and how | Pod phase + `state.terminated` |
| The run's result | `/dev/termination-log` → terminated `message` |
| The image actually used | `containerStatuses[].imageID` |
| Everything the human sees | GitHub: branch, PR, issue comment |

Identity goes in **annotations rather than labels** because a label *value* caps
at 63 characters and repository names run past it — the same cliff the Job name
hits below. A 100-character value is rejected as a label and accepted as an
annotation; measured with `kubectl create --dry-run=server`. One
`app=implementer` label exists for listing.

**Written twice, deliberately — and this was a real gap until 2026-08-10.**
`spec.template.metadata.annotations` is what a Pod inherits; a Job's *own* metadata
does **not** propagate to its pods. The earlier wording said only "Job annotations",
which would have left the credential proxy unable to see run identity at all — it
resolves a request's source pod IP to a **Pod** ([ADR 0005][adr5]), and mint-for-the-
annotation's-repository is that ADR's one non-negotiable. Both copies have a reader,
so neither is redundant: the Job's is what a human sees in `kubectl get job -o yaml`
and what the relist-on-restart reconciliation below reads; the pod template's is what
the proxy resolves and what the informer already reads off the Pod it watches. They
are written from one struct in the Job builder, so drift is not a live risk.

This is also the bound on the proxy's RBAC: `get`, `list`, `watch` on **pods**, in
its own namespace, and nothing else — no Secrets, no Jobs, no cluster scope. Reading
identity off the Job instead would have cost `jobs: get` plus an ownerReference hop.
(For contrast, Kelos's chart ships a ClusterRole with cluster-wide
`secrets: get,list,watch`.)

### Idempotency is the Job name

Webhook redelivery, double-labelling and a mid-run restart all resolve to the
same mechanism: the Job name is derived from the issue, and `AlreadyExists` is
swallowed. There is no dedupe table because there is nothing to keep a table in.

```
slug = lower(owner-repo-issue), every char outside [a-z0-9] -> '-', trim '-'
hash = sha256("owner/repo#issue", case-folded)[:8]

name = trim(slug[:54]) + "-" + hash
```

Two things about this are not obvious and were measured rather than assumed
(`proto/jobname.sh`, dry-run against k3s):

- **63 characters is the cap, and the rejection comes from
  `spec.template.labels`, not `metadata.name`.** The Job controller stamps the
  name as a *label value*; `metadata.name` alone would have allowed 253. The
  apiserver error names no cause an operator would recognise.
  `implementer-kubernetes-sigs-cluster-api-provider-openstack-12345` is 64
  characters and is refused — a real repository in the Kubernetes org, not a
  pathological name.
- **The hash is unconditional, and the condition it replaces could not be
  written correctly.** An earlier version hashed only when the derivation lost
  something — `len(slug) <= 63 AND owner+repo are [A-Za-z0-9-] only` — and that
  misses the delimiter. The components are joined with `-`, which is legal
  *inside* an owner and inside a repo, so every re-split of the join at a
  different `-` is a distinct identity with the same name: `acme-my/repo#5` and
  `acme/my-repo#5`, or `kubernetes/sigs-cluster-api#70` and
  `kubernetes-sigs/cluster-api#70` — all lossless, all under the cap, all
  colliding. The second run is then swallowed as redelivery and silently
  dropped, which turns "no database" into "silently drops runs".

  Normalisation being lossy is the same failure by another route:
  `acme/my_repo#5` and `acme/my-repo#5` both slugify to `acme-my-repo-5`, and it
  is reachable — `google-deepmind/open_spiel` keeps its underscore and
  `google-deepmind/open-spiel` 404s, so the widespread claim that GitHub
  normalises `_` to `-` is false.

  Nine characters of suffix ends the whole class, keeps one code path instead of
  two, and the name still reads:
  `nissessenap-the-implementer-70-1a2b3c4d`. Case-folding needs no separate
  treatment either way, because GitHub does forbid owners and repos differing
  only in case.

The hash is over the **raw identity**, never the slug — hashing the lossy
artifact would make both variants hash identically and defeat the point.

The name carries no `implementer-` prefix. The orchestrator runs in a dedicated
namespace, which is both the uniqueness scope for the name and the reason the
prefix would be redundant — so **the dedicated namespace is a load-bearing
configuration item**, not an ambient assumption.

### Restart is a relist

The controller holds nothing in memory that it cannot rebuild. On start it lists
Jobs in its namespace and reconciles: a finished run with no PR gets its PR, a
running one is watched. This is the normal controller loop, not recovery code.

### What the orchestrator holds, and what it does not

It authenticates as the GitHub App and mints installation tokens for its **own**
calls — detection ([ADR 0003][adr3]), the pull request, and the issue comment.
Permissions: `contents: read`, `pull_requests: write`, `issues: write`.

It does **not** mint the sandbox's token and does **not** create a per-run
Secret. [ADR 0005][adr5] moves that to the credential proxy, which deletes the
Secret→Job→`ownerRef` sequence, its orphan sweep and its RBAC from this
component entirely. **The orchestrator creates exactly one object per run.**

## Consequences

- **No database, no migrations, no queue, no worker pool, no leader-election
  story beyond what `controller-runtime` gives for free.**
- **The apiserver is the single point of failure for run state**, which it
  already was — [ADR 0002][adr2] put the run there.
- **A run cannot be resumed by the orchestrator**, only re-created. Consistent
  with `backoffLimit: 0` and with retry policy being deliberately open on the
  [map][map].
- **`ttlSecondsAfterFinished` is a data-retention decision, not a tidiness
  one.** Deleting the Pod object makes `pods/log` return `NotFound` regardless
  of any window, and per the [observability decision][observability] MVP retains
  the transcript nowhere else.
- **A cap on concurrent runs is a namespace `ResourceQuota`**, not a counter.
  The pod scheduler is the admission control: no room, the pod stays `Pending`;
  room appears, it runs; the informer fires either way. Note
  `activeDeadlineSeconds` counts from `Job.status.startTime`, so **Pending time
  burns the run deadline**.
- **Two components now authenticate as the App** — this one and the credential
  proxy. Neither holds key bytes; see [ADR 0005][adr5].

## Alternatives rejected

- **A database plus a job queue.** It would hold a copy of state owned by the
  apiserver and GitHub, and its only distinctive behaviour would be disagreeing
  with them. It also imports migrations, connection management and a backup
  story into a component whose entire job is to create one object and read one
  field.
- **A long-lived process that blocks on the run.** Makes the webhook handler's
  lifetime the run's lifetime, so a deploy mid-run loses the result, and it
  needs its own concurrency limiting.
- **Watching Jobs rather than Pods.** Both result channels and the image digest
  are pod-level, so a Job watch has to resolve the Pod anyway. A Job's
  `status.conditions` carries no exit code, no termination message and no pod
  name.
- **A dedupe table keyed by delivery id.** The Job name already collides
  correctly, at the apiserver, with no state of ours. Adding a table would
  reintroduce the database this ADR exists to avoid, to solve a problem
  `AlreadyExists` solves for free.
- **Identity in labels.** Rejected by measurement: repository names exceed the
  63-character label-value cap.
- **Not writing an orchestrator at all — adopting [Kelos][kelos].** Added
  2026-08-10 from [its research][kelosresearch]. Kelos is the best prior art this
  project has found and it independently reached **seven** of our decisions,
  including the two most contested — a Job per run, and the Job name plus a
  swallowed `AlreadyExists` as the whole of idempotency. That convergence is the
  valuable part, and it is also the argument against adopting it: the plumbing we
  would save is the part two independent designs agree on, i.e. the commodity part.
  What it costs is everything the project exists for. Every credential lands in the
  sandbox as an environment variable, including a full-installation `GITHUB_TOKEN`
  and, worst case, an entire `~/.codex/auth.json` with its durable refresh token
  ([ADR 0005][adr5] says none may). **gVisor is not expressible in its API at all** —
  `PodOverrides` has fifteen fields and no `runtimeClassName`, confirmed against the
  generated CRD — and `AgentUID = int64(61100)` is a Go constant in its orchestrator,
  the exact trap [ADR 0001][adr1] wrote a clause against. Its webhook authorization
  is exact-string matching on `sender.login`, with no `sender.type` check anywhere in
  the repository. And plugin delivery breaks silently: `spec.skills[]` collapses
  everything into one reserved directory, so `/mattpocock-skills:implement` and
  `/ponytail:ponytail-review` both become `skills-sh:<name>` and our invocation
  strings stop resolving.

  A middle option was costed and also declined — ship a Kelos-conformant sandbox
  image plus our credential proxy, keeping the differentiator and renting the
  plumbing. Its image contract is genuinely open (no `FROM kelos-base`), so the shim
  is cheap; the blockers above are not, and one cost the research did not price is
  decisive: **an in-pod PR builder cannot report its own pod's death.** Today the
  informer writes a PR or an issue comment for `OOMKilled`, `activeDeadlineSeconds`
  and a dead review phase alike. Moving that inside the sandbox moves the failure
  path into the thing that fails.

  Nothing is owed to keep this reversible. The phase script already takes its inputs
  from argv and env and returns results through a marker channel, and it is the pod's
  `command` ([ADR 0001][adr1]) — so whatever invokes it is *already* swappable, and
  the residual blockers are on Kelos's side of the line, not ours.

[map]: https://github.com/nissessenap/the-implementer/issues/1
[kelos]: https://github.com/kelos-dev/kelos
[kelosresearch]: ../research/kelos-and-kubefoundry.md
[adr1]: 0001-sandbox-image-strategy-and-byo-contract.md
[adr2]: 0002-a-run-executes-as-a-kubernetes-job.md
[adr3]: 0003-toolchain-detection-and-image-selection.md
[adr5]: 0005-credentials-terminate-at-the-credential-proxy.md
[push]: https://github.com/nissessenap/the-implementer/issues/13
[credentials]: https://github.com/nissessenap/the-implementer/issues/14
[trigger]: https://github.com/nissessenap/the-implementer/issues/15
[observability]: https://github.com/nissessenap/the-implementer/issues/17
[concurrency]: https://github.com/nissessenap/the-implementer/issues/23
[pkgs]: https://github.com/palantir/go-githubapp
