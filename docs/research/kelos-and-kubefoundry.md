# Kelos and KubeFoundry — could we depend on either instead of building?

**Date:** 2026-08-06 · **Sources read directly:** [`kelos-dev/kelos`][kelos] at `22d52a9`
(2026-08-05, shallow clone — history before 2026-07-02 was unavailable),
[`kube-foundry/kube-foundry`][kf] at `28c0e39`, and the live
[kubefoundry.io][kfsite]. Our side is [`docs/architecture.md`][arch] and the five
[ADRs][adr]. Every claim below cites a file and line in one of the three trees, or a
URL. Where a source could not settle a question, it says so.

**Question asked:** rather than re-implementing, how do Kelos and KubeFoundry fit into
the v1 architecture; what decisions have they made that are better or worse than ours;
and is any of it worth changing the architecture for.

**Short answer.** KubeFoundry is not a candidate — 11 commits, one author, abandoned
since 2026-04-01, and its documented install path provably cannot complete a single
task. Kelos is a serious candidate and the best prior art this project has found: it
independently converged on **seven** of our decisions, including the two most
contested ones (a Job per run; the Job name plus a swallowed `AlreadyExists` as the
whole of idempotency). But it makes the opposite choice on exactly the two decisions
that are this project's reason to exist — **every credential lands in the sandbox**
([ADR 0005][adr5] says none may) and **gVisor is not expressible in its API at all**
([ADR 0001][adr1] calls gVisor load-bearing). One of those is a code change we could
send upstream in an afternoon; the other is architectural.

Recommendation: **do not adopt Kelos as a dependency now. Steal six specific
mechanisms from it. Send it one small upstream patch. Re-open the question at the
moment we would otherwise write the webhook front-end** — which is the point where
adopting it stops being cheap and starts being expensive. §7 costs all three options.

---

## 1. KubeFoundry: dispatched

Same category as us, and eerily convergent on surface details — a label
(`factory:do`) triggers a sandboxed agent pod that opens a PR
(`webhook/handler.go:180`), and it independently landed on the same pod→controller
return channel we did, `TerminationMessageReadFile` on a termination log
(`internal/controller/softwaretask_controller.go:627-628`). Apache-2.0
(`gh api repos/kube-foundry/kube-foundry`). The site is its own MkDocs `docs/`
directory on GitHub Pages (`mkdocs.yml:4-5`); there is no SaaS, no pricing page
(`/pricing/` → 404).

It is not a candidate, on three independent grounds:

- **Dead.** 11 commits, all by one author, 2026-03-28 → 2026-04-01. One release,
  `v0.1.0`. 15 stars, 0 subscribers. The only external contribution — three issues and
  a PR fixing PSA-`restricted` failures, filed 2026-05-15 — has sat unanswered for
  ~3 months.
- **Never run end-to-end via its own documented path.** The Helm chart ships no `Skill`
  CRD (`grep -rni skill chart/` → no match) although the controller resolves one, and
  its ClusterRole omits `serviceaccounts` while `handlePending` does a `Get` on a
  ServiceAccount on every reconcile (`softwaretask_controller.go:166`, `:679-701`) —
  so the `helm install` path fails every task before a pod exists, while the kustomize
  path works. Its e2e patches the agent image to a **mock** shell script
  (`test/e2e/testdata/mock-agent/entrypoint.sh`).
- **Posture we would have to undo.** No gVisor, no RuntimeClass, no seccomp on the
  sandbox (one repo-wide hit, on the *operator* pod: `config/manager/manager.yaml:58`).
  `sender` is never parsed from the webhook payload, so no actor-type check exists, and
  HMAC verification is skipped when the secret is unset (`webhook/handler.go:165-172`) —
  an unauthenticated public task-creation endpoint. Names carry `time.Now()`
  (`handler.go:186`), so re-labelling runs twice. Every credential is a pod env var,
  and an inline `githubToken` is stored as **plaintext in the CR spec**
  (`api/v1alpha1/softwaretask_types.go:96-99`).

Two things are worth taking anyway:

1. **`callbackURL` on the task spec** — one optional field; the operator POSTs
   `{taskName, status, pullRequestUrl, errorMessage}` on terminal transition with a
   10 s timeout (`softwaretask_types.go:82-85`, `controller:272-317`). ~35 lines. This
   is precisely the seam [§5][arch] names as missing when it says a CI-green gate
   "needs something to *watch* the PR after the run ends, which nothing does."
2. **An anti-pattern to cite.** Its NetworkPolicy allowlists provider CIDRs by hand,
   including `104.18.0.0/16` labelled "Anthropic API" — a shared Cloudflare range, so
   the policy permits a large slice of the internet
   (`chart/kube-foundry/values.yaml:65-73`). Concrete ammunition for [§7][arch]'s
   position that allowlist questions belong inside the proxy.

Its docs also drift from its code in two checkable places (branch format at
`README.md:429` vs `controller:500`; callback payload at `README.md:164-170` vs
`controller:272-278`), which is the tell that nothing was validated. Not worth more
words.

---

## 2. Kelos: what it actually is

> Run and orchestrate coding agents on Kubernetes. — `README.md:3`

Much larger than us: **11 binaries** under `cmd/`, **11 CRDs** in `api/v1alpha2/`, five
agent CLIs (claude-code, codex, gemini, opencode, cursor), and **two** run primitives —
`Task` (one-shot Job) and `Session` (long-lived pod + PVC, resumable, with a web UI and
a TUI). Triggers from GitHub, Linear, Jira (poll-only), cron, generic webhooks and
Slack. Reports to issue comments, GitHub Check Runs and Slack.

The structural difference in one sentence: **almost every question we answered in Go,
Kelos answers in a prompt template.** An operator writes a `TaskSpawner` whose YAML
contains the entire prompt as a Go template. Its own dogfood configuration under
`self-development/*.yaml` is the honest documentation of how it runs, and
`self-development/kelos-workers.yaml:22-83` is a ~60-line numbered checklist covering
implement, push, `gh pr create`, self-review loop, PR-body update, CI wait, squash and
hand-off.

**Health.** v0.43.0 → v0.50.0 in the five visible weeks, ~1 minor per week, still 0.x,
no 1.0. Merge subjects run PR #1415 → #1591, so the project is ~1,400 PRs older than
the clone shows. `git shortlog -sne`: **one author at 96 %** (245 of 256 commits). At
least 58 of 125 merges came from `agent/` or `codex/` branches — it is substantially
self-developed by its own agents. CI discipline is genuinely strong and a real positive
signal: GitHub merge queue (`.github/workflows/ci.yaml:9`), `-race` unit and integration
tests, envtest, e2e, and `hack/verify.sh:18-136` gating that all 10 CRDs, RBAC, the
webhook and both generated client sets regenerate byte-identical.

**API stability, honestly.** The deprecation policy is written down and followed —
`CONTRIBUTING.md:86-97`: *"Do not change a field's kind… mark the old one `+deprecated`
and keep it functional rather than removing it."* But there are **17 `Deprecated:`
fields inside the current storage version** (`task_types.go:287,298,…`;
`taskspawner_types.go:403,886,…`), so a `worker` substruct migration already happened
*within* v1alpha2; five of the eleven kinds are v1alpha2-only and five weeks old; and
the v1alpha1↔v1alpha2 conversion webhook is **explicitly lossy**, stashing what it
cannot represent in annotations and giving up silently on malformed ones
(`internal/conversion/taskspawner.go:11-27`, `agentconfig.go:188-223`). Reading and
creating `Task` objects is safe to depend on. The surrounding surface is not.

⚠️ **Legal footnote.** `LICENSE` declares Apache-2.0 (`README.md:264`) with
`Copyright 2026 Gunju Kim`, an individual — but the text is not byte-identical to
canonical Apache 2.0. Three word-level edits sit **inside the license body**: `:51`
"submitted to **the** Licensor", `:63` "received by **the** Licensor", `:109`
"excluding **any** notices" where canonical says "those" — and `:103` still says
"those", so it is now internally inconsistent. Filling in the appendix is conventional;
editing words inside §1 and §4(d) is not. Reads LLM-regenerated. Not a blocker; worth
a note before anyone vendors or forks it.

---

## 3. Where Kelos independently confirms us

These are convergences, not influence in either direction, and they are the most
valuable output of this research: seven of our decisions were reached separately by a
system with ~1,400 PRs of production pressure behind it.

| Our decision | Kelos |
| --- | --- |
| **A run is a `batch/v1` Job, one pod** ([ADR 0002][adr2]) | `internal/controller/job_builder.go:927-955`, `RestartPolicy: Never` at `:942` |
| **Idempotency = Job name + swallowed `AlreadyExists`** ([ADR 0004][adr4]) | Job name **is** the Task name verbatim (`job_builder.go:929`); `AlreadyExists` on create becomes a requeue (`task_controller.go:447-450`) |
| **No database, no queue** ([ADR 0004][adr4]) | State in CRs only. No DB, no broker anywhere in `go.mod` |
| **Namespace as the uniqueness scope** ([ADR 0004][adr4]) | Task names are namespace-scoped; same property |
| **Deterministic results out of the pod, keyed** ([ADR 0004][adr4]) | A marker block `---KELOS_OUTPUTS_START---` / `…END---` with `key: value` lines (`internal/capture/capture.go:68-105`, `internal/controller/output_parser.go:6-7`) |
| **`is_error` must dominate any status field** ([§4][arch]) | `internal/claudecode/result.go:30-41` — and it adds two fields we had not listed, `stop_reason` and `terminal_reason`, with "empty accepted for older Claude Code versions" (`:28-29`). Only the last `type == "result"` line is kept (`internal/capture/usage.go:121-134`) — our "take the last" rule |
| **Attribution via `GIT_AUTHOR_NAME` / `GIT_COMMITTER_NAME` in the PodSpec** ([§3][arch]) | Identical, in `podOverrides.env` (`self-development/kelos-workers.yaml:109-116`) |

Two more convergences worth their own lines because they *vindicate* a decision rather
than merely match it:

**A warm worker pool is per-configuration, so per-run variation kills it.** We
concluded this about agent-sandbox in [ADR 0002][adr2]. Kelos shipped `WorkerPool`
anyway — a StatefulSet of warm pods with annotation-based dispatch and an optimistic
lock (`workerpool_controller.go:1025`, `:1195-1207`) — and then had to forbid
`workerPoolRef` from co-existing with eleven other fields via CEL, including `image`,
`credentials`, `workspaceRef`, `agentConfigRefs` and **`branch`**
(`task_types.go:258-270`). Our run requires `branch`. `WorkerPool` is dead weight for
us, demonstrated by its own constraints.

**Making an LLM own `Closes #N` costs more than writing it in Go.** [§3][arch] moved
the closing keyword into an orchestrator-built PR description so it is guaranteed by
construction. Kelos leaves it to the model, and here is the price: an all-caps MUST
with an explicit *"Do not embed the issue number in natural language"*
(`self-development/kelos-workers.yaml:54`, `:69`), duplicated verbatim across **seven**
spawner files, plus a **separate agent** whose job is partly to re-check the string
survived a squash (`kelos-squash-commits.yaml:115,118`). Nothing in Kelos creates a PR
in Go — the only Go-side PR interaction is `gh pr list --head <branch> --json url` to
*discover* the PR the agent made (`internal/capture/capture.go:112-146`).

---

## 4. Where Kelos is better than us — the steal list

Six things, all cheap, none requiring us to adopt Kelos.

1. **The metrics label set.** `kelos_task_cost_usd_total`,
   `kelos_task_input_tokens_total`, `kelos_task_output_tokens_total`, all labelled
   `{namespace, type, spawner, model}` (`internal/controller/metrics.go:46-71`). That
   `{spawner, model}` cut is exactly what we would want and [§6][arch] does not specify
   a label set. Copy it verbatim.
2. **`TaskRecord`: durable accounting without a database.** An immutable CR
   (`self == oldSelf` CEL) carrying type/model/phase/start/completion/usage, named from
   the Task UID, **deliberately without an ownerReference so GC cannot reap it**, with
   its own `ttlSecondsAfterCompletion` (`api/v1alpha2/taskrecord_types.go:18-89`), and
   a dedicated reconciler for expiry. This is a clean answer to "keep the cost
   accounting after the pod is gone, still no database", and it costs one CRD. It is
   also what makes their budget enforcement possible at all (§5).
3. **`nameTemplate` must not reference mutable external data.** Kelos forbids the
   Task-name template from touching `.Context` and enforces it by *walking the template
   parse tree* (`internal/taskbuilder/builder.go:229-236`, `:289-361`). The principle —
   run identity must not depend on anything that can change under you — is one we
   assert informally and do not enforce. Their API doc block also independently records
   three traps we hit: 63-char truncation collapsing distinct items, a bare issue
   number colliding across repositories, and a name collision with a *foreign* object
   being an error rather than dedupe (`taskspawner_types.go:975-997`).
4. **"Report the injection attempt" as an output contract.** Their reviewer prompt
   treats diffs, comments and other bots' reviews as untrusted **data**, names the
   concrete vectors (HTML comments, `<details>` blocks, "Prompt for AI agents"
   sections), and instructs the agent to append a `**Note on prompt injection**` line
   when it notices an adversarial directive (`self-development/kelos-reviewer.yaml:375-389`).
   Cheap, and we have no equivalent. Note this is prompt-level only — there is no
   sanitization in Kelos's code either.
5. **The caching layer inside `ghproxy`.** Ignore what it is for (§6) and take the
   mechanism: a 15 s freshness window plus ETag revalidation plus `singleflight`
   coalescing of concurrent identical GETs, with `Link`-header rewriting so pagination
   stays on the proxy (`cmd/ghproxy/main.go:126-156`, `:212-312`, `:352`). Directly
   applicable to [ADR 0003][adr3]'s orchestrator-side `GET /contents/` and
   `GET /languages` calls.
6. **Review routing by diff path.** Their worker picks which reviewer to summon from
   `git diff origin/main...HEAD --name-only` — `/kelos api-review` if `api/` is touched,
   else `/kelos review` (`self-development/kelos-workers.yaml:72`). A cheap addition to
   [§3][arch]'s language-reviewer injection that costs one `git diff` and no new
   mechanism.

Two more that are informative rather than adoptable:

- **A fourth option for image selection we never considered.** Not "detect the
  toolchain" ([ADR 0003][adr3]) versus "an operator table per repository" (rejected),
  but **one image per agent CLI, with dependency setup pushed into a workspace-scoped
  `setupCommand`** (`job_builder.go:221-237`, `:675-684`). Kelos has *no* toolchain
  detection — zero hits for `go.mod`, `package.json`, `pyproject`, `GET /languages` or
  "toolchain" outside tests. Worth recording in ADR 0003's alternatives, not worth
  switching to: it moves the problem onto whoever writes the `Workspace`.
- **The measured price of a live view.** [§6][arch] declined one on the evidence that
  scion's PTY bridge was expensive. Kelos built one *without* a PTY: a WebSocket
  bridged to `pods/exec` (`TTY: false`) running an in-pod runtime that speaks a line
  protocol over a unix socket with `subscribe`/`message`/`input`/`interrupt` verbs
  (`internal/sessionserver/server.go:740-767`, `internal/sessionruntime/server.go:965-977`),
  plus a web UI with bearer/cookie auth and a strict CSP, plus a ~1,000-line TUI. So
  the shape is achievable — at roughly 4,500 lines across three components plus an auth
  story. That is the number our deferral was implicitly betting on, and it is now
  measured rather than assumed. It is also a live counter-example to [§11][arch]'s "no
  `kubectl exec` as a control or data channel, anywhere" — their data plane *is* exec.

---

## 5. Where Kelos is worse, and how much it matters

### 5.1 The two that are load-bearing for us

**Every credential lands in the sandbox as an environment variable.**
`credentialEnvVars()` (`job_builder.go:286-323`) emits one `EnvVar{ValueFrom:
SecretKeyRef}` per credential: `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`,
`CODEX_API_KEY`, `GEMINI_API_KEY`, `CURSOR_API_KEY`, and — worst case —
**`CODEX_AUTH_JSON`, the entire `~/.codex/auth.json` including the long-lived
`refresh_token`** (`internal/codexauth/refresher.go:29`, `:136-150`), which a CronJob
rotates every 6 h while the refresh token itself stays durable in the pod's env. Plus a
**real GitHub installation token** as `GITHUB_TOKEN`/`GH_TOKEN` *and* as a mounted file
(`job_builder.go:431-457`, `:518-536`). `CredentialTypeNone` exists but only means
"supply your own via `podOverrides.env`" (`:315-318`) — still an env var in the
sandbox. Both exfiltration classes [ADR 0005][adr5] was written against are live here.

**The GitHub App's key is signed in-process, and tokens are minted unscoped.** The RSA
key comes from a Secret and is parsed into memory (`internal/githubapp/token.go:104-125`),
signed with `rsa.SignPKCS1v15` (`:182-202`). No KMS, no signer interface, no seam —
grep finds nothing. And the mint call is `POST /app/installations/{id}/access_tokens`
**with a nil body** (`:134-140`): no `repositories`, no `permissions` narrowing, so
every token carries the installation's full permission set across every repository it
covers. [ADR 0005][adr5]'s non-negotiable — *mint for the repository named in the run's
Job annotation, never for the repository the request URL names* — is structurally
impossible in this design.

**gVisor is not expressible.** `PodOverrides` has `labels`, `resources`,
`activeDeadlineSeconds`, `env`, `nodeSelector`, `tolerations`, `affinity`,
`imagePullSecrets`, `serviceAccountName`, `volumes`, `volumeMounts`,
`podSecurityContext`, `containerSecurityContext`, `extraContainers`,
`extraInitContainers` (`api/v1alpha2/task_types.go:80-207`) — and **no
`runtimeClassName`**, confirmed against the generated CRD (0 hits in
`task-crd.yaml`). An operator cannot request gVisor through the Kelos API at all. Nor
`priorityClassName` or `topologySpreadConstraints`.

And the posture inside the pod is thin in a way that compounds it: the **agent
container gets no `SecurityContext` whatsoever** (`job_builder.go:480-487`) while
Kelos's own control-plane pods get the full restricted set — `allowPrivilegeEscalation:
false`, `drop: [ALL]`, `runAsNonRoot`, `seccompProfile: RuntimeDefault`
(`session_controller.go:1490-1499`, chart `deployment.yaml:93-97`). uid 61100 comes
only from the image's `USER`, so a BYO image running as root runs as root and nothing
objects. The `skills-install` init container is the one with no `SecurityContext` at all
(`job_builder.go:760-768`), running as the `node:alpine` default. Both bundled agents
launch with sandboxing off: `--dangerously-skip-permissions`
(`claude-code/kelos_entrypoint.sh:72`) and `--dangerously-bypass-approvals-and-sandbox`
(`codex/kelos_entrypoint.sh:128`). Isolation is delegated by documentation — the
`ContainerSecurityContext` field's own doc comment tells the user to set the restricted
bits themselves (`task_types.go:158-164`). There is **no threat model**: zero
occurrences of "isolation", "untrusted", "prompt injection" or "threat model" across
`README.md`, `docs/` and `AGENTS.md`.

Also, no NetworkPolicy anywhere (the only two mentions are prose advising *ingress*
restriction for the webhook receiver), and no `HTTP_PROXY` plumbing except in test
fixtures asserting `podOverrides.env` passthrough works. Our MVP also runs open egress
([§7][arch]) — but ours pairs that with gVisor as the load-bearing isolation and a
sandbox holding zero credentials. Open egress from a pod holding a provider key and a
full-installation GitHub token is a materially different bet.

### 5.2 `cmd/ghproxy` is not what its name suggests — and is a confused deputy

This is the correction most worth recording, because "Kelos already has a GitHub proxy"
is the natural assumption and it is wrong.

`ghproxy` is a **read-through caching mirror of `api.github.com` for the control
plane**. Plain `http.Server` on `:8888`, `ListenAndServe`, **no TLS**
(`cmd/ghproxy/main.go:406`, `:442-447`, `:463`). It exists for rate-limit relief (§4
item 5). Its only caller is the TaskSpawner polling Deployment — the controller passes
`--gh-proxy-url` (`internal/controller/taskspawner_deployment_builder.go:83`) and
`kelos-spawner` uses it as its API base (`cmd/kelos-spawner/main.go:63`, `:716-723`).
**No agent pod, entrypoint or job_builder path references it.** It is not in the
sandbox's path. No MITM, no CONNECT, no cert generation, no CA — clients must be
reconfigured to point at it.

Two findings worth flagging as security issues in their tracker, not just as
comparisons:

- **No caller authentication and no enforcement.** `ServeHTTP` (`main.go:314-359`) has
  zero auth, zero allowlist, zero identity resolution, and `doNonGET` (`:160-207`)
  forwards **any** method at **any** path with `Authorization: token
  <installation-token>` attached (`:177-179`, `:242-244`).
- **`ghproxy.allowedUpstreams` is dead config.** It exists in `values.yaml:49`, in the
  CLI (`internal/cli/install.go:82,244,331-336`) and in `docs/reference.md:1178` — the
  chart never templates it and the binary has no such flag. It reads as an egress
  control and enforces nothing.

Combined: the workspace `ghproxy` Service is a ClusterIP **in the same namespace the
agent Jobs run in** (`workspace_ghproxy_builder.go:78-104`), with the App key mounted,
and no NetworkPolicy. Any pod in that namespace — including the agent sandbox — can
`POST http://ghproxy-<ws>.<ns>:8888/repos/<any-org>/<any-repo>/…` and act as the App at
full installation scope. That is the exact failure [ADR 0005][adr5] marks
non-negotiable, and Kelos does not resolve run identity at all.

Side by side:

| Axis | Our credential proxy ([ADR 0005][adr5]) | Kelos `ghproxy` |
| --- | --- | --- |
| In the sandbox's path | mandatory; no proxy → no runs | no — control plane only |
| Credentials terminated | Vertex (WIF), GitHub App (KMS-signed JWT), GAR | one GitHub token, injected outbound |
| Sandbox holds | nothing; a sentinel `GH_TOKEN` | real installation token (env + file) **and** a provider key |
| TLS interception | yes; cert-manager leaf, intercept list off cert SANs | none; plain HTTP |
| Caller identity | source pod IP → Pod → Job annotation | none |
| Key holder | zero — Cloud KMS `asymmetricSign` | raw RSA key in a Secret, parsed in-process |
| Token scope | narrowed to the run's repository | nil body: full installation |
| Enforcement seam | push-branch-prefix (`service=git-receive-pack`) | none |
| Also does | nothing | ETag cache + singleflight (which we lack) |

### 5.3 The rest, in order of how much it would cost us

- **`backoffLimit: 1`, hardcoded** (`job_builder.go:477`) — a failed *paid* agent run
  is retried once. [ADR 0002][adr2] chose 0. Not configurable; `podFailurePolicy` is
  the only lever. A catch-all `onExitCodes: {operator: NotIn, values: [0]}` → `FailJob`
  rule *may* reproduce `backoffLimit: 0`; **unverified against the API's validation
  rules** and worth a five-minute test before relying on it.
- **`activeDeadlineSeconds` defaults to unset** (`job_builder.go:786`, `:803`) — a
  wedged run holds a pod indefinitely. Ours defaults to 3600. Fixable in
  `taskTemplate.podOverrides`, but the default is the wrong way round.
- **No per-run cost or turn ceiling.** No `--max-turns` and no `--max-budget-usd`
  anywhere (`max_turns` appears only as a *terminal reason* the parser recognises,
  `internal/claudecode/result_test.go:30-44`). `TaskBudget` is **post-hoc admission
  control**: it sums usage from `TaskRecord`s of *completed* runs against a **Daily-only**
  period and blocks the *next* Task (`api/v1alpha2/taskbudget_types.go:11-60`,
  `internal/controller/task_budget_admission.go:40-126`, fails closed at `:60-86`). It
  can tell you tomorrow that yesterday cost too much; it cannot stop one run spending
  $300. Note these are **complementary, not substitutes** — a daily namespace-wide
  dollar cap is something we have no equivalent for, and our `ResourceQuota` answer
  caps concurrency, not money.
- **Results come from tailing 50 lines of `pods/log`** (`task_controller.go:1053-1080`)
  rather than the termination log — `grep TerminationMessage internal/controller/job_builder.go`
  → 0 hits. They built a 30 s window with a 5 s retry interval around it
  (`:37-42`, `:899-931`), which is itself the argument for our channel. A 50-line budget
  also has no room for our accumulated per-phase blob, and `status.results` is a flat
  `map[string]string` (`task_types.go:416`).
- **It watches Jobs, not Pods** (`task_controller.go:1404-1412`), finding the Pod by a
  label `List` on demand and taking the newest by creation time (`:1006-1022`) — the
  opposite of [ADR 0004][adr4].
- ⚠️ **TTL destroys idempotency, by design.** `task_types.go:358-362` says TTL deletion
  exists *"allowing TaskSpawner to create a new Task."* With
  `examples/03-taskspawner-github-issues/taskspawner.yaml:29` (`ttlSecondsAfterFinished:
  3600`) and a still-matching label, the same issue re-runs **every hour**. There is a
  second deliberate re-trigger path that deletes a completed Task when a newer trigger
  time arrives (`cmd/kelos-spawner/main.go:341-352`). Our AlreadyExists-forever
  semantics are strictly safer for a paid run, and this coupling is not something an
  operator would expect from a field named "TTL".
- **The default webhook Task name is `<spawner>-<event>-sha256(deliveryID)[:12]`**
  (`internal/webhook/handler.go:706-715`), i.e. **every delivery is a new Task** — the
  code says so at `:620-624`. Deduplication across deliveries about the same issue is
  opt-in via `nameTemplate`. Label an issue twice → two paid runs by default.
- **Label filtering is on the issue's current label set, not `label.name`.**
  `filters[].labels` requires all listed labels to be present
  (`internal/webhook/github_filter.go:481-504`); `payload.label` is never read. So a
  spawner matching `ready-for-agent` on `action: labeled` fires when *any* label is
  added to an issue already carrying it. [§1][arch]'s `label.name == "ready-for-agent"`
  assertion avoids that class entirely.
- **Operated surface.** N spawners = N Deployments (or CronJobs); a per-Task plugin
  ConfigMap; a per-Task GitHub token Secret the controller **re-mints on a 5-minute-margin
  loop while the Job runs** (`task_controller.go:48-65`, `:628-654`) — exactly the
  Secret→Job→ownerRef sequence [ADR 0005][adr5] deleted; an in-memory branch mutex with
  a status-based restart fallback (`internal/controller/branch_locker.go`,
  `task_controller.go:1202-1233`); mandatory cert-manager for the CRD **conversion**
  webhook (one Issuer, not our two — it mints a serving leaf, not a CA a proxy chains
  from); and a **ClusterRole with cluster-wide `secrets: get,list,watch`**
  (`templates/rbac.yaml:4-18`).
- **CRD size.** `PodOverrides` embeds full `corev1` types, so `task-crd.yaml` is
  **21,178 lines** and `taskspawner-crd.yaml` **23,460** — 68,222 across the set.
- **Generic webhook endpoint is unauthenticated by design** (`handler.go:133`,
  `:230-250`; `docs/integration.md:366`; their issue #1040). GitHub and Linear paths do
  verify HMAC-SHA256 with a constant-time compare and require the secret at startup
  (`signature.go:13-48`, `handler.go:134-141`, `:205-209`) — that part is fine.

### 5.4 Authorization: ours is strictly stronger, and theirs has the better half we rejected

The complete authorization gate on a Kelos **webhook**-triggered run is exact-string
matching of `sender.login` — `filters[].author` and `excludeAuthors`
(`github_filter.go:343-349`, `:401-413`). Point by point against [§1][arch]:

| Check | Kelos webhook path | Ours |
| --- | --- | --- |
| `sender.type == "User"` | **absent** — `sender.type` is never read anywhere in the repo | asserted |
| `[bot]` handling | no bypass exists, but no check either; bots are handled by *naming* them (`excludeAuthors: [dependabot[bot]]`) | covered structurally by the type assertion |
| `ghost` | **not handled**; zero occurrences repo-wide | rejected |
| `author_association` | never read | not used, deliberately |
| write-permission API call | not on the webhook path | not made, deliberately |

The `GitHubWebhook` spec has no authorization block at all — its full field set is
`events`, `repository`, `excludeAuthors`, `filters`, `reporting`
(`taskspawner_types.go:352-380`). In practice Kelos protects its own repository with a
single-login allowlist, `author: gjkim42` (`self-development/kelos-workers.yaml:19`),
plus a second filter whitelisting `author: kelos-bot[bot]` so a worker can trigger a
review by commenting on its own PR (`kelos-reviewer.yaml:50-57`). That is the
flatt.tech bot bypass, entered voluntarily and scoped to one login — safe for a
single-tenant App, and a good demonstration of why "just allowlist the bot" is
tempting. The flatt.tech disclosure has left no trace in the codebase: zero hits for
`checkWritePermissions` or `checkHumanActor`.

**But the polling path has a real authorizer, and it settles a question we could not.**
`internal/source/github_comment_policy.go` implements
`{TriggerComment, ExcludeComments, AllowedUsers, AllowedTeams, MinimumPermission}` with
a rank ladder `read:1 triage:2 write:3 maintain:4 admin:5` (`:14-20`), a real call to
`/repos/%s/%s/collaborators/%s/permission` preferring **`role_name` over `permission`
precisely because the legacy field cannot express `triage`** (`:177-201`), org team
membership requiring `state == "active"` (`:204-224`), per-login caching, and a
fail-open default when unconfigured (`:120-123`). [§1][arch] deleted our write check
partly because *"which fine-grained permission `GET /collaborators/{u}/permission`
requires could not be established"*. That question remains unanswered, but this is
proof that **`minimumPermission: triage` is expressible and someone shipped it** —
useful if we ever revisit. Note it is wired only into `internal/source/` and is
**unreachable from `internal/webhook/`**, and that logins are lowercased there
(`:388-390`) but compared with exact `==` on the webhook path, so `excludeAuthors` is
theoretically case-bypassable while the poll path is not.

### 5.5 The run plan: one invocation, and phases pushed into the prompt

One Task = one container run = **exactly one** agent CLI invocation.
`claude-code/kelos_entrypoint.sh:93` is a single unconditional statement; `spec.prompt`
is one scalar (`docs/reference.md:11`); there is no Pipeline or Workflow CRD, and every
"phase" in the docs means a Kubernetes lifecycle phase. Sequences are expressed three
ways:

1. **N Tasks + `dependsOn`** (`examples/07-task-pipeline/pipeline.yaml`) — three `Task`
   documents chained at `:38-39`, `:64-65`, sharing a branch so the branch mutex
   serialises them, with results threaded via
   `{{index .Deps "scaffold" "Results" "branch"}}` (`:41`). Real machinery behind it:
   DFS cycle detection and failure propagation (`task_controller.go:1091-1185`). Cost
   per phase: a fresh clone, a cold agent context, and state crossing only via the git
   branch plus a flat `map[string]string`. ⚠️ **Template resolution failure degrades
   silently to the raw prompt** (`:1279-1289`) — a phase would run with an unrendered
   `{{…}}` in it and nobody would know.
2. **One prompt containing a numbered plan** — what the maintainer actually ships.
   `grep -rn dependsOn self-development/` → **no matches.** The production setup does
   not use the pipeline pattern.
3. **GitHub as the message bus** — the worker posts `/kelos review` on its own PR and a
   *separate* reviewer TaskSpawner picks it up (`kelos-workers.yaml:72`,
   `kelos-reviewer.yaml:49-57`).

Their reviewer is a separate Task told *"You are a read-only agent: do NOT push code or
modify any files"* (`kelos-reviewer.yaml:20`) — a different answer to "how do you stop
the reviewer widening the diff" than our expand-then-contract pair, and a weaker one
(instruction versus a phase that only deletes). Their nearest analogue to our pair is
`review-all`, a skill invoking *"two independent review paths"*
(`kelos-reviewer.yaml:133-137`) — ⚠️ that skill lives in an external skills.sh package
(`gjkim42/kanon-repo`, `self-development/base-agent.yaml:6-7`) and its contents are
**undetermined**.

⚠️ Note the implement step in their flagship loop is a **`SessionSpawner` with a 10 Gi
PVC** (`kelos-workers.yaml:2`, `:84-89`) — not the Job-backed `Task` primitive.

---

## 6. Prompt assembly: Kelos pins the text, and then instructs the agent past it

This is the most directly useful finding for [#32][editwindow].

**Kelos renders the entire prompt in the webhook server, before the pod exists.**
Template vars are extracted from the parsed payload — `Title`, `Body`, `CommentBody`,
`Number`, `URL`, `Sender`, `Event`, `Action`, `Branch`, `Repository`, `ChangedFiles`,
and `Payload` (the whole event) — `github_filter.go:745-815`; rendered with
`text/template` and `missingkey=error` (`taskbuilder/builder.go:95-104`); stored in
`Task.spec.prompt`, which is **immutable after creation** (CEL, `task_types.go:467`);
and handed to the pod as container **`Args[0]`** (`job_builder.go:485`). So for the text
it pins, **Kelos does not have our edit-after-label window.**

**And the hole reopens immediately, because every Kelos prompt tells the agent to
re-fetch.** `kelos-workers.yaml:39-40`: *"0. Refresh the latest issue state: `gh issue
view {{.Number}} --comments`"*; likewise `kelos-reviewer.yaml:127-129`. The pinned text
is a starting context; the authoritative read happens in-pod anyway.

**The transferable lesson for #32: pinning is necessary but not sufficient.** Our fix
has to pin *and* remove the agent's reason to re-fetch — otherwise we pay the extra API
call and the multi-kilobyte-blob-into-the-pod problem [§11][arch] already prices, and
keep the hole.

Two mechanical notes on their approach we should not copy verbatim: the prompt is a
**pod argument**, so it is visible in `kubectl get job -o yaml` to anyone with pod-read;
and there is no size guard on the rendered prompt between the 10 MB payload cap upstream
and etcd's 1 MiB object limit downstream.

Also worth knowing: `internal/contextfetch/` is **not** issue hydration — it is generic
authenticated HTTP GET/POST with JSONPath extraction, fetched pre-Task-creation and
exposed as `{{.Context.<name>}}`, with a 10 s timeout, a 32 KiB response cap, https-only
unless `allowInsecure`, and `failurePolicy: Ignore` degrading to empty string
(`fetcher.go:57-182`). There is no `gh issue view` equivalent in Go anywhere; comment
threads reach the agent via `{{.Payload}}` or by the agent shelling out to `gh`. And a
nice bug class they hit and documented: `ChangedFiles` is a delivery-scoped fetch cache
shared across spawners, so it is passed explicitly rather than read off `eventData`, to
stop one spawner's file list leaking into another's prompt depending on map iteration
order (`github_filter.go:370-385`).

---

## 7. Could we build on it? Three options, costed

### The image contract is genuinely open

`docs/agent-image-interface.md` does **not** force `FROM kelos-base` — there is no
`kelos-base`; the five bundled images are independent `FROM ubuntu:24.04` builds. This
is the key fact that makes any adoption option viable at all, and it is a better
position than scion's (whose contract effectively required `FROM scion-base`,
[§11][arch]).

What it requires: an executable at **`/kelos_entrypoint.sh`** (`:16-19`), the prompt as
**`$1`** (`:30-32`), the repo at `/workspace/repo` (`:128-130`), decode and exec
`KELOS_SETUP_COMMAND` propagating the exit code (`:156-169`), pipe agent stdout through
`/kelos/kelos-capture` **or emit the marker block yourself** (`:201-260`) and **not
`exec`** the agent (`:244-253`), read `$KELOS_GITHUB_TOKEN_FILE` per call rather than
the env var (`:171-199`), and honour `CLAUDE_CONFIG_DIR` (`:53-65`).

Two problems with it:

- ⚠️ **`AgentUID = int64(61100)` is a Go constant in the orchestrator**, with the
  comment *"Custom agent images must run as this UID… This must be kept in sync with
  agent Dockerfiles"* (`job_builder.go:115-118`). This is precisely the trap
  [ADR 0001][adr1] wrote a clause against ("no UID constant in the orchestrator"), taken.
- **Nothing verifies the contract at run time.** No preflight; a non-conforming image
  fails as an opaque exec error. [ADR 0001][adr1]: *"a contract nobody verifies is a
  comment."* Also `image.tag: "latest"` is the chart default (`values.yaml:6`) — no
  digest pinning.
- Skills are fetched **at run time** by an init container running `npx -y skills add` in
  `node:22.14.0-alpine` (`job_builder.go:747-769`, `:1315-1369`) — the reproducibility
  hole [ADR 0001][adr1] rejected.

**Multiple `--plugin-dir` is native**: one flag per immediate subdirectory of
`/kelos/plugin` (`claude-code/kelos_entrypoint.sh:86-91`, `sessionruntime/claude.go:134-141`).
Weak positive evidence for [#31][measure]. But delivering *our* plugins is a problem:
`spec.plugins[]` requires pasting every SKILL.md body inline into the CRD as `content`
strings, capped at 900 KiB total (`job_builder.go:1166-1170`) — 41 Matt Pocock skills
would come close or blow past it, and need regenerating on every upstream change. And
`spec.skills[]` (skills.sh) **collapses everything into one reserved plugin dir named
`skills-sh`** (`job_builder.go:1366-1374`, `:79-82`), destroying plugin-level
namespacing: `/mattpocock-skills:implement` and `/ponytail:ponytail-review` would both
become `skills-sh:<name>` and our invocation strings would not resolve.

### Option A — adopt Kelos wholesale, operate it, write nothing

Delete the orchestrator, the webhook front-end, the informer, the Job builder, the PR
builder. Write `TaskSpawner`/`Workspace`/`AgentConfig` YAML.

**Costs:** every credential in the sandbox (ADR 0005 abandoned); no gVisor (ADR 0001
abandoned); no toolchain detection (ADR 0003 abandoned — image is per agent CLI); the
authorization posture of [§1][arch] abandoned for login string matching; PR body and
`Closes #N` become LLM instructions; `backoffLimit: 1`; no per-run cost cap; results
via a 50-line log tail; a `Workspace` CR per repository; plugin delivery broken as
above. **Verdict: no.** It abandons everything the project exists for.

### Option B — steal mechanisms, depend on nothing

§4's six items plus §5.4's `role_name` finding and §6's pinning lesson. Zero new
dependencies, zero operational surface, and every ADR intact.

**Cost:** we still write the orchestrator. Which, per [ADR 0004][adr4], is a webhook
front-end plus a Pod informer plus a Job builder — and Kelos's own equivalents confirm
that is the small part. **Verdict: this is the default.**

### Option C — be a Kelos-conformant sandbox image plus a credential proxy

The interesting one, and the reason this research was worth doing. Our differentiator
is not the orchestration plumbing — Kelos has 1,400 PRs of that and reached the same
conclusions we did (§3). Our differentiator is **the security posture and the GCP
integration**: a proxy where every credential terminates, gVisor as load-bearing
isolation, Vertex/WIF/KMS/GAR. Kelos has none of that and no seam for most of it.

So: ship (1) a Kelos-conformant agent image whose `/kelos_entrypoint.sh` **is** our
three-phase script, (2) the credential proxy, (3) `credentials.type: none` plus
`podOverrides.env` pointing the agent at the proxy — which is exactly the shape their
own Bedrock example uses (`examples/09-bedrock-credentials/task.yaml:12`,
`docs/reference.md:152`), so the seam exists and is documented.

What survives: the three-phase run plan (the phase script is ours), **`--max-turns` and
`--max-budget-usd` per phase** (the `claude` invocations are ours), attribution, and —
because the marker protocol explicitly permits emitting the block ourselves
(`agent-image-interface.md:238-242`) — **a deterministic PR built in Go inside the pod**,
by a small binary in our image, through the proxy, emitting `pr:`. The `Closes #N`
guarantee survives; it just moves location.

What does not survive, and would need work:
- **gVisor.** Needs `runtimeClassName` added to `PodOverrides` — or our own mutating
  webhook. See below.
- **Authorization.** Kelos's webhook path has none we would accept. Needs either an
  upstream contribution or our own front-end in front of theirs, which partly undoes
  the saving.
- **ADR 0003 toolchain detection.** Image is per-spawner; one spawner per toolchain, or
  a mutating webhook, or give it up.
- **The GitHub token.** It is injected from `Workspace.spec.secretRef`, not from the
  credential type — ⚠️ **whether a `Workspace` with no `secretRef` works, leaving git
  auth entirely to our proxy, is the single unverified fact this option turns on.**
  Worth an hour to test.
- `backoffLimit: 1`, the log-tail result channel, and the CRD/operational surface.

**Verdict: not now, but this is the shape to re-evaluate.** The trigger point is the day
we would otherwise start the webhook front-end. Before then, adopting Kelos saves
little and costs the ADRs; after then, we own the plumbing and adopting it saves
nothing.

### The one thing worth doing upstream either way

**Send Kelos a PR adding `runtimeClassName` to `PodOverrides`.** It is additive, which
their written policy explicitly favours (`CONTRIBUTING.md:86-97`); it is ~20 lines plus
CRD regeneration, which their CI verifies is byte-identical
(`hack/verify.sh:18-136`); it unblocks gVisor for everyone, not just us; and it turns
Option C from "architecturally blocked" into "a costed choice". Cheap, useful, and it
buys standing to raise the `ghproxy` findings in §5.2, which are real security issues a
maintainer would want to know about.

---

## 8. What this changes on our side

**Nothing in the five ADRs is invalidated.** Kelos contradicts ADR 0001 and ADR 0005
and confirms ADR 0002 and ADR 0004; ADR 0003 has no counterpart there. Concrete
follow-ups, each issue-sized:

1. **[§6][arch] metrics** — adopt the `{namespace, type, spawner, model}` label set for
   cost and token counters. (§4.1)
2. **[§6][arch] observability** — record the measured price of a live view (~4,500 LOC,
   WebSocket → `pods/exec` → unix socket, no PTY) as strengthened evidence for the
   deferral, and note it is a live counter-example to the blanket "no `kubectl exec` as
   a data channel". (§4)
3. **[#32][editwindow]** — sharpen it with the Kelos finding: pinning at webhook time is
   necessary but not sufficient, because a prompt that says "refresh the issue state"
   walks straight past the pin. (§6)
4. **[ADR 0003][adr3] alternatives** — record the fourth option (one image per agent CLI
   + a workspace-scoped `setupCommand`) and why we did not take it. (§4)
5. **[ADR 0003][adr3] / orchestrator** — lift the ETag + `singleflight` caching shape for
   the detection calls. (§4.5)
6. **[§4][arch] prompt assembly** — add the "treat third-party content as data, and
   report any injection attempt in your output" contract. (§4.4)
7. **[§3][arch]** — consider review routing by diff path for the language reviewer.
   (§4.6)
8. **[§6][arch] / accounting** — consider a `TaskRecord`-shaped immutable CR, no
   ownerReference, own TTL, as the way to keep cost accounting after the pod is GC'd
   without a database. This is a genuine gap on our side, not just a nice-to-have: today
   the cost of a run dies with the pod. (§4.2)
9. **[§1][arch]** — footnote that `minimumPermission: triage` is expressible via
   `role_name` and has been shipped by someone, in case the write check is ever
   revisited. (§5.4)
10. **Upstream** — the `runtimeClassName` PR, and the two `ghproxy` findings. (§7)

Evidence for [#31][measure]: multiple `--plugin-dir` is a supported and exercised shape
(weak positive — Kelos only ever exercises it with inline content, never two upstream
plugin repos). **No evidence either way** on `BASE_SHA` injection (Kelos passes a base
*branch* name, sometimes, and its review prompts hardcode `origin/main` —
`kelos-reviewer.yaml:133-140`; no commit SHA is ever captured) or on the `opus` alias
remap (they run `gpt-5.6-sol` on Codex for everything and pass `spec.model` raw to
`--model`).

One place where our decision is clearly better than the mature system's: **a Task whose
skill asks a human a question hangs**, because Tasks are one-shot `-p` with
`--dangerously-skip-permissions`, no `--max-turns`, and no `activeDeadlineSeconds`
default. Kelos handles this for Sessions (`--permission-prompt-tool stdio` plus
`input.requested` events, `sessionruntime/protocol.go:21`) and not at all for Tasks. Our
3600 s default is the guard, and [§3][arch]'s two unattended-hang traps are a real
concern Kelos has not addressed.

---

## 9. Could not determine

- Contents of the `review-all` skill (external skills.sh package `gjkim42/kanon-repo`) —
  the only place Kelos chains two reviews inside one run.
- Whether `npx skills add -a universal` preserves a two-level plugin layout or
  plugin-level namespacing. That is skills.sh behaviour, not visible in the Kelos tree.
- Whether a `Workspace` with no `secretRef` is valid — the fact Option C turns on.
- Whether `podFailurePolicy` with `onExitCodes: {operator: NotIn, values: [0]}` is
  accepted by the API and reproduces `backoffLimit: 0`.
- What GitHub App permissions Kelos expects an operator to grant. Nothing is sent when
  minting (`internal/githubapp/token.go:134-140`) and nothing is documented.
- Whether the webhook server's **in-memory** delivery cache
  (`internal/webhook/handler.go:71-130`) is safe under the multi-replica deployment its
  chart supports; `--leader-elect` exists (`cmd/kelos-webhook-server/main.go:55`) but
  HTTP serving is not obviously leader-gated.
- Whether a post-agent hook via `podOverrides.extraContainers` is possible — no
  completion signal from the agent container is documented.
- Any real numbers. **No cost or latency measurements anywhere in the Kelos repo or
  docs** — nothing comparable to our 162 s / $1.01 baseline. Also no eval results.
- Everything in Kelos before 2026-07-02: total commits, project age, tags before
  v0.43.0, and whether v1alpha2 has ever had a breaking change (only 20 `api/` commits
  visible in the shallow clone).
- Whether the GHCR images for kube-foundry were rebuilt after 2026-04-01 (needs
  `read:packages`).

[kelos]: https://github.com/kelos-dev/kelos
[kf]: https://github.com/kube-foundry/kube-foundry
[kfsite]: https://kubefoundry.io/
[arch]: ../architecture.md
[adr]: ../adr/
[adr1]: ../adr/0001-sandbox-image-strategy-and-byo-contract.md
[adr2]: ../adr/0002-a-run-executes-as-a-kubernetes-job.md
[adr3]: ../adr/0003-toolchain-detection-and-image-selection.md
[adr4]: ../adr/0004-the-orchestrator-is-a-controller-with-a-webhook-front-end.md
[adr5]: ../adr/0005-credentials-terminate-at-the-credential-proxy.md
[measure]: https://github.com/nissessenap/the-implementer/issues/31
[editwindow]: https://github.com/nissessenap/the-implementer/issues/32
