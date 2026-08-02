# The v1 architecture

**Status: decided, not built.** Every statement here is the output of a resolved
decision — see [the map][map] for the route and the evidence. Nothing in this
document is a proposal.

Decisions expensive to reverse live in [`docs/adr/`][adr] and are linked rather
than restated. Everything else lives here. Domain vocabulary is in
[`CONTEXT.md`][context].

| ADR | Decides |
| --- | --- |
| [0001][adr1] | Sandbox image strategy and the BYO-image contract |
| [0002][adr2] | A run executes as a Kubernetes Job |
| [0003][adr3] | Toolchain detection and image selection |
| [0004][adr4] | The orchestrator is a controller with a webhook front-end; no database |
| [0005][adr5] | Credentials terminate at the credential proxy; the sandbox holds none |

## The shape

Label an issue `ready-for-agent`; a draft pull request appears.

```
1. issues/labeled webhook          ──► orchestrator front-end
2. detect toolchain                     GET /contents/, GET /languages   (ADR 0003)
3. create one Job                       name derived from the issue      (ADR 0002, 0004)
4. pod runs the phase script            gVisor, uid 1000, zero credentials
     preflight · clone · brief
     implement · review · ponytail-review
     push · report
5. informer sees the terminal pod   ──► orchestrator
6. draft PR + issue comment                                              (GitHub)
```

Three components: the **orchestrator** (webhook front-end plus Pod informer),
the **credential proxy** (a Deployment; every credential terminates here), and
the **sandbox** (one Job per run). No database, no queue, no worker pool.

## 1. Trigger, authorization, idempotency

**Event:** `issues`, action `labeled`, `label.name == "ready-for-agent"` — the
label `/triage` already produces, so the skills' own readiness contract is
inherited rather than reinvented. Configurable per installation later; ships
with that default.

`label` is **not** in the payload's `required` set (verified against
[`octokit/webhooks`][webhooks], where `required` is
`[action, issue, repository, sender]`), so a missing `label` object means ignore
the event, not crash on it.

**Authorization:**

```
if sender.type != "User" || sender.login == "ghost"  -> ignore, log only
else                                                 -> run
```

**There is no permission API call**, and the reasoning matters because the
obvious design has it backwards in both directions.

The [flatt.tech post][flatt] is usually cited as proof a write-access check is
mandatory. It says the opposite: `claude-code-action` **had**
`checkWritePermissions` against `{write, admin}` and was bypassed anyway,
because it opened with `if (actor.endsWith("[bot]")) return true;`. The attack
needs no access to the target repository at all — create a GitHub App, install
it on your *own* repository, use its installation token against the target.
Anthropic's fix was `checkHumanActor`, asserting the actor's type is `User`. So
**the clause usually omitted is the one that was exploited, and the clause
usually insisted on is the one that failed to help.**

`ghost` is named beside it because GitHub substitutes that account for
unresolvable actors and **its type is `User`** (`{"login":"ghost","id":10137,"type":"User"}`),
so the type assertion alone would let it through.

Why no write check: [GitHub's roles table][roles] gives *Apply/dismiss labels*
to **Triage** and above, so the label event proves triage, not write. Triage is
trust enough here — the escalation ceiling is a branch plus a pull request that
**no component in this system can merge**. The residual cost is agent budget and
branch noise, spent by someone a maintainer deliberately granted triage. It also
deletes an unverifiable dependency: which fine-grained permission
`GET /repos/{o}/{r}/collaborators/{u}/permission` requires could not be
established, because GitHub's public OpenAPI encodes no fine-grained permissions
for it.

Consequence: **there is no authorization refusal to report.** The only refusals
in v1 are [ADR 0003][adr3]'s toolchain ones. The bot/ghost path is log-only — on
a public repository, commenting there would hand an unauthorized actor an
on-demand way to make the App write to issues, which is precisely the
`issues: write` plus untrusted-input combination flatt.tech flags.

**Idempotency** is the Job name plus a swallowed `AlreadyExists`. Format,
derivation and the two measured traps are in [ADR 0004][adr4].

## 2. Image selection

Entirely [ADR 0003][adr3]. Summary for orientation: root manifests first, `GET
/languages` as fallback, both normalised into a toolchain vocabulary of ours
(`go`, `node`, `python`) and intersected with the images the operator
configured. Ambiguity and non-detection both **refuse and comment** rather than
guess. Runs orchestrator-side before the Job exists.

## 3. The run plan

Three agent phases, not one, each a fresh `claude -p` process — which **is** the
context clear, since `/clear` no-ops in `-p`. The phase script is baked into the
image ([ADR 0001][adr1]) and is the pod's `command`.

```
preflight              det    image contract + posture probes
clone + branch         det    implementer/issue-<n>          -> BASE_SHA
brief                  det    gh issue view --json title,body,comments
implement              agent  /mattpocock-skills:implement
review + fix           agent  /mattpocock-skills:code-review @BASE_SHA
                              + a language subagent (toolchain injected via env)
                              + run tests if anything changed
ponytail-review + fix  agent  /ponytail:ponytail-review + run tests
push                   det    git push origin <branch>
report                 det    /dev/termination-log
PR                     orchestrator, in Go, outside the pod
```

### Why two review phases

`implement/SKILL.md` is five lines of instruction and ends *"Once done, use
/code-review to review the work. Commit your work to the current branch."* It
reviews, **then commits regardless** — it never says act on the findings. So a
review ran in every green prototype run and **nothing consumed its output**;
scope policing was in there too, via `/code-review`'s Spec axis reporting
*"behaviour in the diff that wasn't asked for"*, landing nowhere. Phase 2 is not
adding review. It is making the findings land.

Two phases because they are a **deliberate pair**: phase 2 widens the diff
(`/code-review` reports Standards and Spec, then the phase fixes what it found),
phase 3 narrows it (`/ponytail-review` hunts only over-engineering — it
deletes). Expansion followed by contraction. This is the answer to the
scope-creep trap that a review-which-improves would otherwise create. Write both
down together or someone later removes one and wonders why the diffs got worse.

### Nothing gates the pull request

The PR is always created. Implement-phase commits are on the branch already, and
discarding a ~$1 implement phase because a 529 hit the reviewer is the expensive
failure. A dead or failed review phase means: push anyway, `status:
completed_unreviewed`, and an **orchestrator-written PR comment** naming the
phase that died. No Stop hook, no verifier, no gate.

**No deterministic verification.** No content-activated verifiers, no regex
error extraction. The agent runs tests at its own discretion, which the skills
already instruct. **CI is the verifier** — it runs on the pushed branch, it knows
the repository's build system (which we do not, and that is exactly what made
"what counts as verified across arbitrary repos" hard), and it reports where a
human will read it. **A human is the judge.** The [field survey][survey] found
self-review as a merge gate is done by nobody, and the AIDev evidence is that
*CI*-green correlates with merging.

### Two unattended-hang traps

Both are places where a skill written for a human says "ask the user", which in
`-p` is a dead run rather than an error:

1. **`/code-review` step 1** — *"If they didn't specify [a fixed point], ask for
   it."* The prompt must pass **`BASE_SHA`**, captured at clone time.
2. **Step 2, spec resolution** — it fetches the originating issue via
   `docs/agents/issue-tracker.md`. Discharged by the pod token's `issues: read`
   rather than by pre-fetching, which means **target repositories are required
   to carry no Matt Pocock scaffolding at all**: no `docs/agents/issue-tracker.md`,
   no Agent Brief, no `/setup-matt-pocock-skills`.

Whether either trap actually fires is **unmeasured** — every green run so far
exercised the implement phase only. See [#31][measure].

### Ownership of commit messages and `Closes #N`

The run plan owns none of it. GitHub's closing keywords work from the **PR
description**, and the orchestrator builds the PR deterministically in Go — so
`Closes #N` goes there and is guaranteed by construction, with no
`git commit --amend` against commits three separate phases produced. A duplicate
`Closes #N` in a commit message is a no-op. Attribution is `GIT_AUTHOR_NAME` /
`GIT_COMMITTER_NAME` in the PodSpec.

`pr_title` comes from the **implement** phase only — the phase that knows the
change's intent. Review and ponytail schemas carry `status` and `summary` and
drop `pr_title`. The termination-log blob accumulates and is written once at the
end: overall status, branch, commit count, **summed** cost, elapsed, `pr_title`,
and a per-phase status/summary line. Comfortably inside 4 KB.

### The phase list is fixed; the language is injected

Not configurable per repository — configurability with one consumer is
speculative. The one thing that genuinely varies is the language reviewer's
brief, and [ADR 0003][adr3] already normalises a toolchain **before the pod
starts**. It arrives as an environment variable alongside `REPO` and `ISSUE`;
the review prompt interpolates it. Unset toolchain → the phase skips the
language subagent and runs `/code-review` alone. No `.implementer.yaml`, no
second detection mechanism.

## 4. Context and prompt assembly

**We push, we do not pull.** Title, body and the full comment thread go in up
front, from one `gh issue view --json title,body,comments` call. No
pre-hydration of linked issues, no resolving referenced PRs. This is Spotify's
posture and it is the right one for a system with no database: small,
deterministic, one API call.

```
/mattpocock-skills:implement

You are implementing GitHub issue #<n> in the repository <owner/name>.
The Agent Brief below is the authoritative specification. Work only from it.
You are running unattended: nobody can answer a question. If the brief is
genuinely unimplementable, stop and report status "blocked".

--- ISSUE: <title> ---
<body>
--- COMMENT by <login> ---
<body>   (× all comments)
```

- **The Agent Brief is preferred, not required.** When `/triage` produced one it
  is in the thread and the framing points at it; when it did not, the issue body
  carries the same role. No refusal path for a missing brief.
- **Our addendum is three lines**: work only from the brief, you are unattended,
  report `blocked` rather than guessing. That last line earns its place — the
  structured-output `status` field is what distinguishes "gave up politely" from
  "succeeded", which an exit code cannot.
- **No tool restriction.** No `--allowedTools` / `--disallowedTools` in v1. Tool
  minimalism is the field's advice and it is cheap to add later, but nothing has
  been observed going wrong, and guessing a restriction list ahead of evidence
  is how you break `/implement` in a way nobody notices.
- **The repository's own `CLAUDE.md` / `AGENTS.md` / `.claude/` are honoured.**
  They arrive free with the checkout and we do nothing about it. For v1 that is
  a **feature** — our own repositories, our own conventions. It becomes a hole
  only for repositories we do not control; see §11.

**Structured output is a supported feature, not a convention.** `--json-schema`
plus `--output-format json` yields a validated, auto-retried `structured_output`
field. ⚠️ It is **absent on error paths**, so a `result` carrying `is_error` must
fail hard regardless of any status field — otherwise infrastructure failure and a
politely-giving-up model are indistinguishable. Multiple `result` messages are
possible; take the last or filter on `origin.kind`.

## 5. Push, pull request, and attribution

- **Branch** `implementer/issue-<n>`, matching the field's prefix-as-agent-identity
  convention (`copilot/`, `claude/`, `cursor/`, `jules`).
- **The pod pushes its own branch.** The alternative — a retained workspace and
  a separate pusher — is dead: nothing is stranded, no callback service, no
  retained PVC, and the prototype proved it end to end with CI green.
- **The orchestrator opens the PR**, in Go, on the informer event. It knows the
  issue, the branch and the run result, and a deterministic PR is diagnosable
  even when the run failed.
- **Draft.** Field default, one boolean.
- **Body** assembled from the `/dev/termination-log` blob — status, branch,
  commit count, cost, elapsed, summary — plus the issue reference.
- **A failed or blocked run gets an issue comment and no PR**, unless commits
  exist.
- **Push is confined to the `implementer/` prefix by a repo-owner branch
  ruleset** in MVP, because GitHub cannot scope a token to a branch prefix.
  [ADR 0005][adr5] retires this: the proxy can express what the token cannot.
- **No CI-green gate and no dedupe check.** The AIDev evidence for both is
  strong — 17 % of rejections are CI failures and 23 % are duplicates, so the
  pair addresses ~40 % of rejection mass — and they remain the highest-value
  later additions. Neither is needed to have a working thing, and the CI half
  needs something to *watch* the PR after the run ends, which nothing does.

**Withdrawn rather than deferred:** whether pushes made with an installation
token trigger the target repository's Actions. It has no consumer — the run ends
when the PR is created, the pod is gone, and nothing is listening. For the record
the docs make the expected answer *yes*: the no-new-workflow-run rule is specific
to `GITHUB_TOKEN`, and an installation token is [GitHub's own documented
workaround][ghtoken]; our push does not originate in Actions at all.

## 6. Observability

**MVP: `pods/log` plus one issue comment.**

- **Full transcript** via `pods/log`, already carrying `--output-format
  stream-json`. No retention, no object storage, no log backend. When the pod is
  GC'd the transcript is gone — accepted, because a run we care about is a run
  whose PR we are reading, and that happens in minutes.
- **What the human sees**: one issue comment posted by the orchestrator when the
  informer fires, built from the termination-log blob.
- **No live view.** [The scion research][scion] found scion built a full PTY
  bridge for this and its shareable-URL mechanism is an unimplemented stub. That
  is the evidence: the interactive path is expensive and easy to get wrong.
- **No OTel spans and no metrics in the first cut.**

**Target: OTel.** A deliberate deferral of timing, not a rejection, and cheap
when it arrives — Claude Code ships built-in OTel, so it is configuration rather
than instrumentation, and `go.opentelemetry.io/otel` is already in the fixed
package set. Shape: a span per run, a span per phase, attributes for repo /
issue / image digest / model / tokens / cost / verdict. Per-tool-call spans are
feasible via `parent_tool_use_id` but are a later question. Metrics on the same
pass: runs started/succeeded/failed by reason, duration, tokens and cost.

**Redaction is not done in MVP.** The transcript is readable only by someone
with `pods/log` on our namespace, which is already a trusted position. It becomes
load-bearing the moment a transcript is retained or shipped anywhere.

## 7. Network egress

**MVP: open egress, no `NetworkPolicy`.** Three green end-to-end runs depended on
nothing about the network; gVisor is the isolation that is load-bearing
([ADR 0001][adr1]); there is no multi-tenant exposure on our own repositories.
What makes that acceptable and nothing more is exactly one condition — accepting
issue text we did not write.

**Target, decided though not built: all traffic through the proxy pod, with a
`NetworkPolicy` permitting only that pod.** The policy is then trivially small —
our pod label → the proxy, plus cluster DNS — and every allowlist question moves
inside the proxy where it belongs. Allowlist vocabulary: steal [gh-aw][ghaw]'s
**ecosystem bundles** (`go` → `proxy.golang.org` + `sum.golang.org`, `python` →
PyPI/pythonhosted, and so on) rather than hand-listing registries, plus Claude
Code's own required domains.

Closed for good, not deferred:

- **No setup/agent egress phase split.** [ADR 0001][adr1] removed the pre-agent
  setup phase and the agent installs dependencies mid-work, so closing the
  registry afterwards breaks the run. Only Codex in the field does this, and only
  because it pre-installs everything.
- **No DNS design.** Per [ADR 0002][adr2] we set no `dnsPolicy` and no
  nameservers; egress-capable cluster DNS is a cluster-admin concern.
- **bubblewrap is not an egress mechanism.** `bwrap --unshare-net` is broken
  under gVisor ([gvisor#13438][gvisorbug], still open at `release-20260727.0`),
  so network isolation cannot come from inside the pod even if we wanted it to.

## 8. Docker inside the sandbox

**The base image stays clean; the `go` image carries the rootless Docker stack**
and is the documented worked example of what a language image may add. That is
the more valuable output than the capability itself, since other organizations'
images derive from our base.

Measured (`proto/dind-net.sh`, plain `gvisor` RuntimeClass, uid 1000,
unprivileged, no extra runsc flags): wrapping the **whole phase script** — not
just dockerd — in `rootlesskit --net=slirp4netns` puts the agent, dockerd and
inner containers in one netns on `--network=host`. Sandbox→inner, inner→sandbox,
inner→inner, inner egress and a reachable compose service all pass. `docker
build` works with `--feature containerd-snapshotter=false`.

Runtime requirements, all measured: `dockerd --iptables=false --ip6tables=false
--bridge=none --feature containerd-snapshotter=false`; inner containers on
`--network=host`; readiness gates on `docker version`, **never** `docker info`
(which answers ~6 s before the daemon is usable); not `dockerd-entrypoint.sh`
(unguarded `iptables --version` under `set -e`, and iptables-nft cannot
initialise under gVisor). `rootlesskit --net=host` panics dockerd — a nil deref
in `getVersion`, moby 29.6.2.

The image needs `/etc/subuid` + `/etc/subgid` ranges for uid 1000 and
appropriately privileged `newuidmap`/`newgidmap` — the non-obvious half, and
what three grafting attempts died on.

⚠️ **Cost, still unmeasured:** inside rootlesskit the process reads its own uid
as 0, which trips the agent CLI's root gate (needing `IS_SANDBOX=1`, the exact
bypass the non-root posture exists to avoid) and puts bubblewrap — an
[ADR 0001][adr1] requirement — at risk. **Recommendation: make the wrap a
per-run flag defaulting off**, so the `go` image carries Docker, a normal run
keeps the clean uid-1000 posture, and only a run that asks for it pays.

**GKE does not transfer.** GKE Sandbox supports Docker v27/v28 only; our green
result is v29 and flag-free. Any yes needs re-measuring there.

## 9. Knobs

All Helm values, not code constants.

| Knob | Value | Why |
| --- | --- | --- |
| `activeDeadlineSeconds` | 3600 | ~6× headroom over a trivial three-phase run. Counts from `Job.status.startTime`, so it absorbs `Pending` time |
| `--max-turns` | 200 | Measured working, never came near it |
| `--max-budget-usd` | 10, **per phase** | The CLI can only bound its own process. Worst case ~$30/run — a number worth being told about before it is $300 |
| per-phase `timeout(1)` | none | `--max-turns` bounds the phase, `activeDeadlineSeconds` bounds the run. A third timer catching no observed failure is speculative |
| model pins | four | Not our choice; see [ADR 0005][adr5]. The `opus` alias must be remapped or subagents 404 |
| `sandbox.images` | per toolchain | [ADR 0003][adr3] |
| `sandbox.defaultToolchain` | **unset** | Opt-in escape hatch |
| `ttlSecondsAfterFinished` | operator's | A data-retention decision; see [ADR 0004][adr4] |

**Cost baseline:** one agent phase on a trivial issue measured 162 s / $1.01 on
host runc and 202–206 s / $0.88–0.95 under gVisor. Three phases projects a
trivial issue to **~600 s and ~$2.70**. A real issue is unknown. gVisor costs
~25 % wall-clock and nothing in dollars.

## 10. Operator prerequisites

Scattered across five decisions; collected here because an operator cannot
otherwise find them.

**gVisor is load-bearing, not defence-in-depth** ([ADR 0001][adr1]) — and from
the operator's side it is not merely `runtimeClassName`:

- **k3s** does not autodetect `runsc`; it needs a containerd drop-in plus a
  hand-written RuntimeClass.
- **GKE** needs a node pool with `--sandbox type=gvisor` and `cos_containerd`,
  **at least two node pools**, cannot mix sandboxed and unsandboxed pods in one
  pool, and cannot disable it once enabled. **GKE's runsc version is published
  nowhere**, so runsc-version-dependent behaviour cannot be relied on there.

**From [ADR 0005][adr5]:**

- **Workload Identity Federation** — the proxy's KSA carries the only GCP
  identity in the system.
- ⚠️ **The sandbox's KSA must carry no WI binding**, asserted somewhere that
  fails loudly. On k3s the absence of a metadata server hides this; on GKE
  `169.254.169.254` is reachable from every pod.
- **cert-manager**, with two Issuers (`selfSigned` → CA `Certificate` → `ca`
  Issuer → leaf).
- **Cloud KMS** with the GitHub App key **imported** (PKCS#8 DER; GitHub emits
  PKCS#1 PEM), and `roles/cloudkms.signer` on the signing KSAs.
- GCP roles on the proxy KSA: `roles/aiplatform.user`,
  `roles/artifactregistry.reader`.
- **The operator's GCP project decides which models are invocable.** Check
  before deploying; the prototype's project had no Opus of any version.

**From [ADR 0004][adr4]:** a **dedicated namespace**, load-bearing as the
uniqueness scope for Job names.

**From §5:** a **repo-owner branch ruleset** confining pushes to `implementer/`,
until push-branch enforcement lands in the proxy.

**From [ADR 0001][adr1]:** the base image must carry a system CA bundle at
`/etc/ssl/certs/ca-certificates.crt`; images are pinned by digest and are a
**matched pair** with the orchestrator.

**Not our problem, stated so nobody looks for it:** cluster DNS,
`imagePullSecrets` for the sandbox image itself.

## 11. Deliberately not in MVP

Each of these is a conscious cut with a recorded reason, not an oversight.

| Cut | Why it is safe *now* |
| --- | --- |
| Issue-text sanitization | Our repositories, our issues |
| Pinning issue text at webhook time | Same; see the edit-after-label window below |
| Open egress, no `NetworkPolicy` | gVisor is the isolation; no multi-tenant exposure |
| The target repo's `CLAUDE.md` reprogramming our agent | Our repositories — it is a feature |
| The pod token's `issues: read`, letting the agent read every issue in the repo | Same |
| Trigger #2 (mention + comment context) | Doubles the trust and authorization surface for a convenience the label covers |
| Any gate on PR creation | CI is the verifier, the human is the judge |
| Dependency cache persistence | Nothing has been measured. Upgrade path: nothing → a pull-through registry/module proxy outside the sandbox → per-repo PVC |
| Private package registries beyond GAR | An org bakes its `.npmrc` / `pip.conf` / `GOPROXY` into its own derived image — that is what the BYO contract is for |
| Concurrency and admission control | The pod scheduler *is* the admission control |
| Retry and repair policy | Nothing watches a run after the PR is created |
| Repo-side image selection (`.implementer.yaml`) | v1 owes only the **version seam** ([ADR 0003][adr3]) |
| Transcript redaction | Nothing retains a transcript |

### The multi-tenancy boundary

**Six of those cuts are acceptable only while the issues are ours.** Crossing
this boundary is a project, not a config change:

1. No issue-text sanitization.
2. The target repository's own `CLAUDE.md` reprogramming our agent.
3. Open egress.
4. The pod token's `issues: read` across the whole repository.
5. The agent's ability to read whatever credential the sandbox holds — **now
   discharged** by [ADR 0005][adr5]: the sandbox holds none.
6. **The edit-after-label window** ([#32][editwindow]). Authorization happens at
   webhook time; the pod fetches the issue at run time. Between those moments
   the text is mutable by someone we never authorized, so the agent works from
   text a human never approved. This is the fourth item in Anthropic's fix list
   for the flatt.tech disclosure — *"made workflows ignore issues/comments edited
   after triggering"* — and it is the one bypass in that post that survives both
   authorization checks in §1, because neither looks at the text.

Note the pairing: **1 and 6 are one piece of work.** Sanitizing text that can be
swapped afterwards, or pinning text that is never sanitized, each leave the same
door open. The fix is to capture title, body and comments in the orchestrator at
webhook time and pass them to the pod — which costs an extra API call plus a way
to get a multi-kilobyte blob into the pod, and partly undoes §4's simplicity.

Rejected as a cheap substitute: comparing `issue.updated_at` across the
boundary. It moves for labels, comments and assignment, so it would fire on
benign activity, and a check that cries wolf gets ignored or deleted.

### Also ruled out, permanently

- **Building our own agent harness** (GenKit Go, Google ADK, bespoke). Both
  candidate frameworks' skill loaders reject the majority of Matt's skills *by
  design* — ADK's `KnownFields(true)` makes `disable-model-invocation` a fatal
  parse error, failing 24 of 41 skills including `implement`; both scan
  immediate subdirectories only, so the two-level layout yields zero skills
  silently. 3–6 months to production, then a permanent spec-drift treadmill.
  Spotify tried the homegrown-loop route and abandoned it for Claude Code.
- **Building on scion.** It evaluated agent-sandbox and deliberately removed it;
  webhook-driven agent creation is an explicit non-goal in its own design docs;
  its image contract effectively requires `FROM scion-base`. Useful as prior art,
  not as a dependency.
- **agent-sandbox as the run primitive.** [ADR 0002][adr2].
- **An operator-maintained per-repository image table.** [ADR 0003][adr3].
- **`kubectl exec` as a control or data channel**, anywhere.

## 12. What is decided but unmeasured

Stated plainly, because confidence and decidedness are different things.

- **Two of the three agent phases have never run.** Every green run exercised
  `implement` only. [#31][measure] measures it: whether `/code-review` accepts an
  injected `BASE_SHA` without hanging, whether spec resolution degrades
  gracefully with `issues: read`, whether `/ponytail:ponytail-review` loads from
  a second `--plugin-dir`, whether nested subagents work three-deep, whether the
  review phases actually **commit** (`/code-review` is a reporting skill — a
  dirty tree would be silently dropped by the push step), and the real numbers
  behind the ~600 s / ~$2.70 projection.
- **`go mod download` through the proxy.** Identical mechanism to the Python
  path, which is proven end to end with a real private wheel; no Go repository
  exists in the project to point it at. A coverage gap, not a confidence gap.
- **bubblewrap inside rootlesskit** (§8), which is why the wrap should default
  off.
- **Workload Identity itself** — known-good on GKE, never exercised here,
  because k3s has no metadata server.
- **Whether the proxy's ~23 % latency cost is the hop or the provider/model
  change.** Isolating it needs one direct-API run pinned to the same model.
- **Docker/OCI registry injection** — the challenge/response dance is
  hypothesised, not measured, and blob fetches redirect.
- **HTTP/2 through the proxy** — it forces HTTP/1.1 upstream, which no client
  has minded yet.
- **Certificate and token rotation across a boundary.** Both refresh paths are
  implemented; the longest run was 249 s against a 1 h token and a 720 h
  certificate, so neither has crossed one. The real gap is the proxy reading its
  certificate once at startup ([ADR 0005][adr5]).

[map]: https://github.com/nissessenap/the-implementer/issues/1
[adr]: adr/
[adr1]: adr/0001-sandbox-image-strategy-and-byo-contract.md
[adr2]: adr/0002-a-run-executes-as-a-kubernetes-job.md
[adr3]: adr/0003-toolchain-detection-and-image-selection.md
[adr4]: adr/0004-the-orchestrator-is-a-controller-with-a-webhook-front-end.md
[adr5]: adr/0005-credentials-terminate-at-the-credential-proxy.md
[context]: ../CONTEXT.md
[survey]: https://github.com/nissessenap/the-implementer/issues/7
[scion]: https://github.com/nissessenap/the-implementer/issues/2
[measure]: https://github.com/nissessenap/the-implementer/issues/31
[editwindow]: https://github.com/nissessenap/the-implementer/issues/32
[webhooks]: https://github.com/octokit/webhooks/blob/main/payload-schemas/api.github.com/issues/labeled.schema.json
[flatt]: https://flatt.tech/research/posts/poisoning-claude-code-one-github-issue-to-break-the-supply-chain/
[roles]: https://docs.github.com/en/organizations/managing-user-access-to-your-organizations-repositories/managing-repository-roles/repository-roles-for-an-organization
[ghtoken]: https://docs.github.com/en/actions/concepts/security/github_token
[ghaw]: https://github.com/github/gh-aw
[gvisorbug]: https://github.com/google/gvisor/issues/13438
