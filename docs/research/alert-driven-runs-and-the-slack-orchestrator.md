# Alert-driven runs: the Slack orchestrator, and why the issue is the state machine

**Date:** 2026-08-20 · **Shape:** architecture, not survey. The kagent facts here
were verified against [`kagent-dev/kagent`][kagent] at `e761b057` and
[`agent-substrate/substrate`][sub] at `817f5f4a` during this session; the
adoption argument lives in [kagent-as-the-control-plane][kagentdoc] and is not
repeated. Every kagent claim below cites a file and line.

**Question asked:** a Grafana alert lands in a Slack channel. Something should
diagnose it and, when the fix is small, open a pull request. What runs where,
what holds the state, and does any of it want to be an agent platform?

**Answer.** One orchestrator, two channels, one Job per attempt, no agent
platform and no A2A. Slack carries attention, the **GitHub issue carries state**,
and the orchestrator stays stateless — its database is the issue body.

Two claims carry the design:

1. **§2 — deterministic work belongs to the orchestrator.** An alert does not name
   a repository, and finding it is a lookup, not a judgement. The orchestrator
   fingerprints, dedups, resolves the repo, pulls the surrounding metrics with
   fixed queries, and writes all of it into an issue *before* any model runs. The
   agent starts from an enriched issue, never from a raw alert.
2. **§5 — an issue is a durable A2A session with better properties.** Idle costs
   nothing because no process is held, `input-required` is a label rather than a
   suspended VM, and a human can read and edit the transcript.

---

## 1. The flow

```
  Grafana ── alert ──▶ #alerts (Slack) ──┐
                                         │
  ┌──────────────────────────────────────┴──────────────────────────────────┐
  │  ORCHESTRATOR — stateless Go. No DB. No LLM. All of this is code.       │
  │                                                                         │
  │   ① FINGERPRINT      alertname + chosen labels → sha256                 │
  │                                                                         │
  │   ② DEDUP            gh issue list --label alert --search <fp>          │
  │                          │                                              │
  │                    ┌─────┴─────┐                                        │
  │                  hit         miss                                        │
  │                    │           │                                        │
  │                    ▼           │   ┌── seen again: comment on the issue,│
  │              ┌──────────┐      │   │   reply in its recorded Slack      │
  │              │  STOP    │◀─────┼───┘   thread, bump a count. No Job.    │
  │              └──────────┘      │                                        │
  │                                ▼                                        │
  │   ③ RESOLVE REPO     alert labels (service/namespace/job)               │
  │                      ──▶ static map ──▶ owner/repo                      │
  │                      no match ──▶ needs-human, stop                     │
  │                                                                         │
  │   ④ ENRICH           Grafana HTTP API, fixed queries, no model:         │
  │                        · the firing rule + its expression               │
  │                        · error rate / latency around the window         │
  │                        · pod restarts, OOM kills, recent deploys        │
  │                        · top log patterns for the service               │
  │                        · trace exemplar if one exists                   │
  │                                                                         │
  │   ⑤ OPEN THE ISSUE   in the resolved repo. Body carries:                │
  │                        fingerprint · slack thread_ts · alert payload    │
  │                        · everything ④ found · the time window           │
  │                      label: alert, triaged                              │
  │                                                                         │
  │   ⑥ POST TO SLACK    thread reply: what fired, where, issue link        │
  │                                                                         │
  │   ⑦ CREATE THE JOB   one Job, arg = issue number                        │
  └────────────────────────────────┬────────────────────────────────────────┘
                                   │
                                   ▼
  ┌─────────────────────────────────────────────────────────────────────────┐
  │  JOB — gVisor · uid 1000 · zero credentials · backoffLimit 0            │
  │                                                                         │
  │   phase A  FEASIBILITY   read the enriched issue, clone, read the code. │
  │                          Follow up in Grafana MCP if it needs to.       │
  │                          Emit structured_output:                        │
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

One Job, two phases inside it. The feasibility verdict happens after the clone,
so a "no" pays the pod and the checkout — which is the right trade while the
enrichment is already done and the model only needs a few turns to answer. Split
it into two Jobs the day the discarded clone cost becomes visible; nothing else
in the design changes.

## 2. The split: deterministic in the orchestrator, judgement in the Job

The governing rule. Anything with a correct answer that code can compute belongs
in the orchestrator, where it is cheap, testable, and identical every time.

| Step | Where | Why |
| --- | --- | --- |
| Fingerprint an alert | orchestrator | a hash |
| Is this already open? | orchestrator | an API query |
| Which repo is this? | orchestrator | label → static map. A model would *guess* |
| Pull the metric context | orchestrator | fixed PromQL/LogQL. Same alert, same queries |
| Open / label / comment | orchestrator | API calls |
| Reply into the right Slack thread | orchestrator | `thread_ts` off the issue body |
| Open the PR | orchestrator | it knows branch, issue and run result |
| Is this fixable, and how hard? | Job | reads code against symptoms. Judgement |
| Which line is wrong | Job | judgement |
| Write the test and the fix | Job | judgement |
| Follow-up metric questions mid-fix | Job | open-ended, so MCP not fixed queries |

Note the consequence for tooling: **the orchestrator uses the Grafana HTTP API,
not the Grafana MCP.** MCP exists so a model can choose its own queries. The
orchestrator's queries are chosen at compile time, so MCP would be a protocol tax
on a fixed request. The Job gets the MCP, for exactly the open-ended half.

The other consequence: the orchestrator now holds a Grafana read credential. It
still holds no model credential and never calls an LLM.

## 3. Who talks to what

```
  ┌─────────────────────────────────────────────────────────────────────────┐
  │  ORCHESTRATOR                                                           │
  │                                                                         │
  │   in ◀── Slack Events API      alert messages, thread replies           │
  │   in ◀── GitHub webhooks       issue comments, PR reviews                │
  │                                                                         │
  │  out ──▶ Grafana HTTP API      fixed enrichment queries  (read-only)    │
  │  out ──▶ Slack Web API         chat.postMessage — attention only        │
  │  out ──▶ GitHub API            issues, labels, comments, PRs — state    │
  │  out ──▶ Kubernetes API        create Job, watch Pod                    │
  │                                                                         │
  │  holds: a Grafana token, a GitHub App key, a Slack token. No model key. │
  │  keeps: nothing. State is in the issue.                                 │
  └─────────────────────────────────────────────────────────────────────────┘
                                    │ creates one Job per attempt
                                    ▼
  ┌─────────────────────────────────────────────────────────────────────────┐
  │  JOB POD   gVisor · uid 1000 · zero credentials · backoffLimit 0        │
  │                                                                         │
  │   claude -p  ──MCP──▶  Grafana MCP        open-ended follow-up          │
  │             ──MCP──▶  GitHub MCP (RO)     issue thread, code search     │
  │             ──git──▶  github.com          clone, push  (sentinel cred)  │
  │             ──▶ Maven Central / GAR / internal Nexus   dependencies     │
  │                                                                         │
  │   every leg exits through the CREDENTIAL PROXY, which attaches the      │
  │   real tokens. The pod itself carries none.  ADR 0005.                  │
  └─────────────────────────────────────────────────────────────────────────┘
```

**One agent, not a diagnose-then-hand-off pair.** A handoff pays a lossy
translation at the seam: the second agent receives prose about what the first one
saw and cannot go back and ask another question once it is reading code. One
process holding both the metrics and the working tree can re-query mid-fix. That
deletes the A2A leg, the second image, and the context transfer.

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
| --- | --- | --- |
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

## 5. The issue is a durable A2A session

The reframe that removes the last reason to want a protocol here.

| A2A concept | Issue equivalent |
| --- | --- |
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
`thread_ts`, and the enrichment from §1④, which makes every hard problem a lookup:

```
  dedup            gh issue list --label alert --search "<fingerprint>"
  reply routing    thread_ts read off the issue body
  status           labels: triaged / needs-human / implementing / pr-open
  agent input      the issue body itself — no separate payload to pass
```

The orchestrator therefore keeps the no-database property [ADR 0004][adr4] already
claims, and can be redeployed mid-run without losing anything.

## 6. Where a warm process would actually win

One case, and it is not context and not startup cost: **perceived latency in a
human conversation.** A Job is 30–90 s to first token — image pull, JVM start,
clone. Fine for "assess this". Wrong for someone typing *"@bot what about the
retry config?"* into the thread and expecting an answer.

So: **batch turns → Job. Conversational turns → warm process.** The whole
enrich → assess → fix flow is batch turns. If a chatty surface is ever wanted it
is a separate warm reader over the same issue — read-only, no toolchain, no push —
and not the thing that writes the fix.

## 7. What this does not solve

1. **The label → repo map.** §1③ is now the orchestrator's job and it is the
   weakest link: a map from Grafana `service`/`namespace`/`job` labels to
   `owner/repo`, maintained by hand, wrong whenever a service is renamed. No
   match must mean `needs-human`, never a guess — a guessed repo spends a whole
   run on the wrong codebase. Where the map should live is undecided.
2. **Fingerprint stability.** Two alerts a human calls "the same problem" can
   differ in labels, and a flapping alert can produce a new fingerprint each time.
   Dedup is only as good as the fingerprint and nothing here proposes a good one.
3. **Which enrichment queries.** §1④ lists a plausible set. Nothing has been
   measured, and an over-stuffed issue body costs input tokens on every run
   while an under-stuffed one sends the agent back to Grafana anyway.
4. **Internal dependency access.** Resolving from a private Nexus or Artifactory
   is a third credential shape beside git-basic and `token`/`Bearer`, and it must
   reach the resolver without reaching the sandbox. Unbuilt.
5. **Infrastructure "workarounds."** Bumping a pod's resources to mitigate an
   alert is a write to running infrastructure, not a patch. Route it through the
   same path as any other change — the agent edits the gitops repo and opens a PR
   — so it stays diffable and reviewable. An agent with live cluster write access
   to mitigate a problem it diagnosed itself has no reviewer.
6. **Cost of a run on noise.** Every new fingerprint spends a Job. Nothing here
   rate-limits, and a flapping alert with an unstable fingerprint is a spending
   loop. A per-hour budget on new runs is the obvious guard and is not designed.

---
[kagent]: https://github.com/kagent-dev/kagent
[sub]: https://github.com/agent-substrate/substrate
[kagentdoc]: kagent-as-the-control-plane.md
[adr4]: ../adr/0004-the-orchestrator-is-a-controller-with-a-webhook-front-end.md
