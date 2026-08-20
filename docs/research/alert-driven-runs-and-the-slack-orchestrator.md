# Alert-driven runs: the Slack orchestrator, and why the issue is the state machine

**Date:** 2026-08-20 · **Shape:** architecture, not survey. The kagent facts here
were verified against [`kagent-dev/kagent`][kagent] at `e761b057` and
[`agent-substrate/substrate`][sub] at `817f5f4a` during this session; the
adoption argument lives in [kagent-as-the-control-plane][kagentdoc] and is not
repeated. Every kagent claim below cites a file and line.

**Question asked:** a Grafana alert lands in a Slack channel. Something should
diagnose it and, when the fix is small, open a pull request. What runs where,
what holds the state, and does any of it want to be an agent platform?

**Answer.** One orchestrator, two channels, one Job per attempt, no agent platform
and no A2A. Slack carries attention, the **GitHub issue carries state**, and the
orchestrator stays stateless — its database is the issue body.

Two claims carry the design:

1. **§2 — the split is "does it need to execute code", not "is it AI".** The
   orchestrator diagnoses the alert itself, with a model and the Grafana MCP, in
   its own process: that work needs no filesystem, no toolchain and no sandbox, so
   it needs no pod. Only reading a codebase and running its tests needs a Job. The
   agent in the Job therefore starts from a diagnosed issue, never a raw alert.
2. **§5 — an issue is a durable A2A session with better properties.** Idle costs
   nothing because no process is held, `input-required` is a label rather than a
   suspended VM, and a human can read and edit the transcript.

---

## 1. The flow

```
  Grafana ── alert ──▶ #alerts (Slack) ──┐
                                         │
  ┌──────────────────────────────────────┴──────────────────────────────────┐
  │  ORCHESTRATOR — stateless Go. No DB. Holds credentials, keeps no state. │
  │                                                                         │
  │   ① FINGERPRINT      alertname + chosen labels → sha256      [code]     │
  │                                                                         │
  │   ② DEDUP            gh issue list --label alert --search <fp>  [code]  │
  │                          │                                              │
  │                    ┌─────┴─────┐                                        │
  │                  hit         miss                                        │
  │                    │           │                                        │
  │                    ▼           │   ┌── seen again: comment on the issue,│
  │              ┌──────────┐      │   │   reply in its recorded Slack      │
  │              │  STOP    │◀─────┼───┘   thread, bump a count. No Job.    │
  │              └──────────┘      │                                        │
  │                                ▼                                        │
  │   ③ DIAGNOSE                                          [model + MCP]    │
  │      ┌───────────────────────────────────────────────────────────┐      │
  │      │  in-process agent loop.  Grafana MCP only.                │      │
  │      │  No clone, no toolchain, no code execution → no pod.      │      │
  │      │                                                           │      │
  │      │  it decides what to ask: which rule fired and why, error  │      │
  │      │  rate and latency around the window, log patterns, pod    │      │
  │      │  restarts / OOM, recent deploys, a trace exemplar —       │      │
  │      │  chosen per alert, not from a fixed list.                 │      │
  │      │                                                           │      │
  │      │  structured_output: {summary, probable_cause, service,    │      │
  │      │                      signals[], confidence}               │      │
  │      └───────────────────────────────────────────────────────────┘      │
  │                                                                         │
  │   ④ RESOLVE REPO     service (from ③) → static map → owner/repo [code]  │
  │                      no match ──▶ needs-human, stop                     │
  │                                                                         │
  │   ⑤ OPEN THE ISSUE   in the resolved repo. Body carries:        [code]  │
  │                        fingerprint · slack thread_ts · alert payload    │
  │                        · the whole of ③ · the time window              │
  │                      label: alert, diagnosed                            │
  │                                                                         │
  │   ⑥ POST TO SLACK    thread reply: what fired, the diagnosis,   [code]  │
  │                      the issue link                                     │
  │                                                                         │
  │   ⑦ CREATE THE JOB   one Job, arg = issue number                [code]  │
  └────────────────────────────────┬────────────────────────────────────────┘
                                   │
                                   ▼
  ┌─────────────────────────────────────────────────────────────────────────┐
  │  JOB — gVisor · uid 1000 · zero credentials · backoffLimit 0            │
  │                                                                         │
  │   phase A  FEASIBILITY   read the diagnosed issue, clone, read the      │
  │                          code against the symptoms. Follow up in        │
  │                          Grafana MCP where the code raises a question.  │
  │                          structured_output:                             │
  │                            {feasible, difficulty, plan, confidence}     │
  │                              │                                          │
  │                    ┌─────────┴─────────┐                                │
  │                 no │                   │ yes                            │
  │                    ▼                   ▼                                │
  │          comment the analysis    phase B  IMPLEMENT                     │
  │          label needs-human                TDD: failing test first,      │
  │          exit 0                           fix, run the suite locally,   │
  │                                           push implementer/issue-<n>    │
  └────────────────────────────────┬────────────────────────────────────────┘
                                   │
                        informer on the terminal Pod
                                   │
                     ┌─────────────┴─────────────┐
                 succeeded                     failed
                     │                           │
                     ▼                           ▼
        orchestrator opens the PR,     comment the failure, label
        links it on the issue,         needs-human, no PR unless
        posts the link to Slack        commits exist on the branch
```

One Job with two phases, not two Jobs. The feasibility verdict lands after the
clone, so a "no" pays the pod and the checkout — acceptable while the diagnosis is
already done and the model needs few turns to answer. Split it the day the
discarded clone cost becomes visible; nothing else changes.

## 2. The split: does it need to execute code?

The axis is not "deterministic versus AI". Deciding what to ask Grafana about an
alert nobody has seen before is judgement, and no static query list covers it.
The axis is **what the work needs around it**:

| | Needs | Where | Examples |
|---|---|---|---|
| **Code** | nothing | orchestrator, inline | fingerprint, dedup query, issue/label/comment CRUD, `thread_ts` routing, service→repo map, opening the PR, creating the Job |
| **Judgement, no execution** | a model + Grafana | orchestrator, **in-process agent loop** | what is this alert, what to query, what is the probable cause, which service |
| **Judgement, needs execution** | a filesystem, a toolchain, network | **Job**, gVisor, one per attempt | read the codebase, is this fixable, write the test, write the fix, run the suite, push |

The middle row is the one that surprises. An agent loop that only calls
read-only MCP tools executes no untrusted code, touches no working tree, and
finishes in seconds. A pod would buy it nothing but an image pull. So it runs
inside the orchestrator process, and the orchestrator holds a model key alongside
the GitHub App key and the Slack token — it is the trusted component either way.

The bottom row is where a sandbox earns its keep, because that is where arbitrary
repository code gets read, built and run.

**Both agent legs get the Grafana MCP, for different reasons.** In the
orchestrator it is the whole toolset — signals are all there is. In the Job it is
the second half of a pair: the agent has the code *and* the signals, so when the
code raises a question the metrics can answer it without a handoff.

## 3. Who talks to what

```
  ┌─────────────────────────────────────────────────────────────────────────┐
  │  ORCHESTRATOR                                                           │
  │                                                                         │
  │   in ◀── Slack Events API      alert messages, thread replies           │
  │   in ◀── GitHub webhooks       issue comments, PR reviews                │
  │                                                                         │
  │  out ──▶ Grafana MCP           the diagnose loop's only toolset         │
  │  out ──▶ Anthropic API         the diagnose loop itself                 │
  │  out ──▶ Slack Web API         chat.postMessage — attention only        │
  │  out ──▶ GitHub API            issues, labels, comments, PRs — state    │
  │  out ──▶ Kubernetes API        create Job, watch Pod                    │
  │                                                                         │
  │  holds: model key, Grafana token, GitHub App key, Slack token.          │
  │  keeps: nothing. State is in the issue.                                 │
  └─────────────────────────────────────────────────────────────────────────┘
                                    │ creates one Job per attempt
                                    ▼
  ┌─────────────────────────────────────────────────────────────────────────┐
  │  JOB POD   gVisor · uid 1000 · zero credentials · backoffLimit 0        │
  │                                                                         │
  │   claude -p  ──MCP──▶  Grafana MCP        signals, beside the code      │
  │             ──MCP──▶  GitHub MCP (RO)     issue thread, code search     │
  │             ──git──▶  github.com          clone, push  (sentinel cred)  │
  │             ──▶ Maven Central / GAR / internal Nexus   dependencies     │
  │                                                                         │
  │   every leg exits through the CREDENTIAL PROXY, which attaches the      │
  │   real tokens. The pod itself carries none.  ADR 0005.                  │
  └─────────────────────────────────────────────────────────────────────────┘
```

Note the asymmetry, and that it is deliberate: the **orchestrator holds its
credentials directly** because it is trusted code we wrote, while the **Job holds
none** because it runs a model over repository content. The proxy exists for the
second case only.

**One agent per side, no diagnose-then-hand-off chain inside the Job.** The
handoff that does exist — orchestrator → issue → Job — is lossy on purpose and
survivable, because it is written down: the Job reads the diagnosis as text and
can re-query Grafana itself if the text is not enough. A live agent-to-agent
handoff would be lossy *and* ephemeral.

**GitHub MCP read-only for reading, git CLI for writing.** The hosted GitHub MCP
scopes by URL path — `…/x/repos/readonly` versus `…/x/repos` versus
`…/x/pull_requests` (`contrib/tools/github-mcp-server/tests/*_test.yaml:19-73`) —
so read-only is a URL, not a trust argument. The write half is deliberately *not*
MCP: the API path can create a branch and commit files, but it cannot run
`sbt test` first, which is the whole reason to have a Job with a toolchain.

## 4. Why a Job and not an agent platform

Verification is in [kagent-as-the-control-plane][kagentdoc]; the short form,
because it is the same handful of facts every time:

| Needed | Job | kagent actor |
|---|---|---|
| Terminal state to act on | exit code + `/dev/termination-log` | none. Suspends when the response body closes (`substrate_sandbox_transport.go:100-108`) |
| Per-run resources | pod requests/limits | **`ActorTemplateSpec` has none.** Resources live on the pooled worker only |
| Concurrency | scheduler is the admission control | fixed `WorkerPool.Replicas`, no HPA (`substrate/pkg/api/v1alpha1/workerpool_types.go:57-61`) |
| Full `securityContext` | yes | inexpressible; gVisor is the entire posture |

And the fact that kills the specific "just take a Hermes image" shortcut:
**`AgentHarness` is one actor per harness, not per session.**

> "one actor is spun from it per harness. Every chat is an ACP session inside that
> one actor's long-lived child process" … "chats are multiplexed as ACP sessions
> inside the one actor, so they all resolve to the same `ActorID(ah)`"
> — `go/core/pkg/sandboxbackend/substrate/agentharness_actor.go:14-20,39-42`

Two unrelated alerts would share one micro-VM, one `hermes acp` process, one
filesystem and one coursier cache — and *"because the actor is shared, suspending
affects every chat in the harness"* (`:97-98`). Concurrency means sharding into
`hermes-scala-0..N` by hand. `SandboxAgent` BYO does give one actor per session
(`agent_actor.go:38-52`), priced at an A2A server on port 80
(`go/api/v1alpha3/agent_types.go:387`) plus a `Command` copied verbatim into
`Process.Args` with no PATH or entrypoint fallback (`agent_lifecycle.go:192-197`).

Recorded so nobody tries it: **the kagent agent image is not standalone.**
`kagent-core/_config.py:17-22` raises when `KAGENT_URL` is unset — the ADK images
call the controller back for sessions. Taking the image means taking the
controller, and since v0.8 that means Postgres.

Nothing above argues against kagent for a *long-lived read agent* — that is what
it is good at. It argues that the write half is not an actor, and that once the
diagnose loop is 200 lines of in-process agent SDK with one MCP server attached,
there is no second consumer left to justify the control plane.

## 5. The issue is a durable A2A session

The reframe that removes the last reason to want a protocol here.

| A2A concept | Issue equivalent |
|---|---|
| `contextId` | issue number |
| message | comment |
| task state | label |
| `input-required` | `needs-human` label; the run **exits** |
| artifact | the pull request |
| cost while idle | zero — no process is held |
| replay / audit | the thread, readable and editable by a human |

Substrate spends a whole checkpoint/restore engine — golden snapshots,
`DurableDir`, suspend-on-body-close — to make an idle conversation cheap. Not
holding the conversation in a process gets the same property for free.

`input-required` comes out strictly better. A paused session holds a worker and
expires; here the run exits, a human comments three days later, and the next run
reads the whole thread as context. The pause is free and auditable.

**The issue body is the database.** It carries the fingerprint, the Slack
`thread_ts` and the §1③ diagnosis, which makes every hard problem a lookup:

```
  dedup            gh issue list --label alert --search "<fingerprint>"
  reply routing    thread_ts read off the issue body
  status           labels: alert / diagnosed / needs-human / pr-open
  agent input      the issue body itself — no separate payload to pass
```

The orchestrator therefore keeps the no-database property [ADR 0004][adr4] already
claims, and can be redeployed mid-run without losing anything.

## 6. Where a warm process would actually win

One case, and it is not context and not startup cost: **perceived latency in a
human conversation.** A Job is 30–90 s to first token — image pull, JVM start,
clone. Fine for "assess this". Wrong for someone typing *"@bot what about the
retry config?"* into the thread and expecting an answer.

Note that the §1③ diagnose loop already sidesteps this by being in-process: it
answers in seconds because it never waits for a pod. So the latency problem is
confined to the code-reading half, which is exactly the half nobody expects to be
conversational. **Batch turns → Job. Conversational turns → in-process.**

## 7. What this does not solve

1. **The service → repo map.** §1④ turns the diagnosis's `service` into a
   repository by hand-maintained lookup, and it is the weakest link: wrong
   whenever a service is renamed, and silent about services it has never heard of.
   No match must mean `needs-human`, never a guess — a guessed repo spends a whole
   run on the wrong codebase. Where the map lives is undecided; a Backstage-style
   catalog or a label on the k8s workload are both plausible and neither is chosen.
2. **Fingerprint stability.** Two alerts a human calls "the same problem" can
   differ in labels, and a flapping alert can produce a new fingerprint each time.
   Dedup is only as good as the fingerprint and nothing here proposes a good one.
3. **How much diagnosis to write down.** §1③ emits into the issue body, which
   every later run pays for in input tokens. Too little and the Job re-queries
   Grafana anyway; too much and every retry carries the weight. Unmeasured.
4. **Internal dependency access.** Resolving from a private Nexus or Artifactory
   is a third credential shape beside git-basic and `token`/`Bearer`, and it must
   reach the resolver without reaching the sandbox. Unbuilt.
5. **Infrastructure "workarounds."** Bumping a pod's resources to mitigate an
   alert is a write to running infrastructure, not a patch. Route it through the
   same path as any other change — the agent edits the gitops repo and opens a PR
   — so it stays diffable and reviewable. An agent with live cluster write access
   to mitigate a problem it diagnosed itself has no reviewer.
6. **Cost of a run on noise.** Every new fingerprint spends a diagnose loop and a
   Job. Nothing here rate-limits, and a flapping alert with an unstable
   fingerprint is a spending loop. A per-hour budget is the obvious guard and is
   not designed.
7. **The orchestrator now runs a model, so it can fail like one.** A diagnose loop
   that hallucinates a service name feeds §1④ garbage. `confidence` in the
   structured output is the intended guard; what threshold routes to `needs-human`
   is not set.

---
[kagent]: https://github.com/kagent-dev/kagent
[sub]: https://github.com/agent-substrate/substrate
[kagentdoc]: kagent-as-the-control-plane.md
[adr4]: ../adr/0004-the-orchestrator-is-a-controller-with-a-webhook-front-end.md
