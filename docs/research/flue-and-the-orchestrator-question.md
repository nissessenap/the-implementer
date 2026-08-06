# Flue, and whether we should still write an orchestrator

**Date:** 2026-08-06 · **Question asked:** Flue looks like it solves most of what we
want except running in Kubernetes; why reimplement it in Go? Are there better-fitting
frameworks? Is a k8s agent sandbox hard with Flue? Does the credential proxy
([#37][i37]) still make sense?

**Sources:** five parallel research agents against primary sources — `withastro/flue`
docs and source, `cloudflare/workerd`, Cloudflare Workers/Containers/Sandbox docs,
Pi/OpenClaw repos, Anthropic's Agent SDK docs, and the candidate frameworks' own
repositories — plus direct reads of our own `proto/proxy/main.go`, `proto/phase.sh`
and `docs/architecture.md`. Every external claim below is quoted from the source that
owns it. Inference is labelled as such.

**Short answer:** don't adopt Flue as the orchestrator, and the reason is not Kubernetes
— Flue runs on Node and Docker fine. It is that **Flue's value is its harness, and its
harness is the one part we cannot use.** Flue drives Pi with a per-tool-call sandbox
seam; we drive `claude -p` with whole autonomous phases inside a pod. Take Flue's
harness and you drop Claude Code, Matt's subagents and the three-phase run plan; keep
`claude -p` and Flue is reduced to a webhook-plus-Postgres wrapper around the Go
controller we were going to write anyway. **But the research turned up three things that
matter more than the Flue question**, one of which is a live security hole in our own
prototype (§6).

---

## 1. Three corrections to the premise

**Flue is not Cloudflare's.** It is [`github.com/withastro/flue`][flue] — the **Astro
team's** framework, Apache-2.0, ~1028 of the commits by one maintainer
(`fredkschott`). Cloudflare's role is host and promoter: their own post calls it *"Flue,
our new open-source framework from the team behind Astro, is the first to build on
it"* ([agents-platform-flue-sdk][cfsdk]). The [astro-issue-triage][cfblog] post is a
customer story about *Astro's* bot. This matters for the dependency question: adopting
Flue is adopting a 6-month-old single-maintainer project whose own npm description reads
**"Experimental framework under active development with APIs that may change"**, and
which shipped a breaking 2.0 rewrite on 2026-07-31, five days before this was written.
It is not a Cloudflare platform commitment. No public roadmap or support statement
beyond the launch post was found.

**Kubernetes is not the limitation.** Flue documents Node.js, Docker, Cloudflare
Workers, GitHub Actions, GitLab CI, Daytona and Render as deploy targets, and the
Cloudflare-only code is confined to `packages/runtime/src/cloudflare/` with a
first-class sibling at `packages/runtime/src/node/` — the coordinator is already
runtime-abstracted *in the source tree*, not just in the docs. Durable Objects are
required for the Cloudflare target only.

**Nor is the webhook.** The Astro bot is label-driven exactly as we are — *"Every new
submission starts with the label `triage needed`"* — and Flue ships webhook signature
verification (*"signatures checked against exact raw bytes, replay windows
enforced"*). We already decided webhook-over-Actions in
[ADR 0004][adr4]; Flue would not be buying us that, and the GHA latency complaint that
motivated the question is already answered by our own design. Note also what Flue does
**not** ship: GitHub App auth is a plain PAT, `new Octokit({ auth: process.env.GITHUB_TOKEN })`.
The JWT→installation-token flow — the thing [ADR 0005][adr5]'s KMS signing exists for —
is ours to build under either plan.

## 2. What Flue actually is

An agent harness, not a workflow engine and not a GitHub router. The primitive is a
function returning instructions, rebuilt every turn — *"every time the model is about to
be called, Flue runs your function again and rebuilds its instructions from scratch"* —
composed from React-shaped hooks (`useModel`, `useSandbox`, `useSkill`, `useTool`,
`useSubagent`, `usePersistentState`, `useMcpConnection`). Its README example is close
enough to our problem to explain the appeal:

```ts
export function Triage() {
  useModel('anthropic/claude-sonnet-4-6');
  useSandbox(local());
  useSkill(triage); useSkill(verify);
  useTool(openIssue); useTool(searchCode);
  return `Triage a bug report end-to-end...`;
}
```

Underneath is **Pi** — `@earendil-works/pi-agent-core`, MIT, Mario Zechner's harness (ex
`badlogic/pi-mono`, [moved to Earendil Works][pihome]), also the harness under
[OpenClaw][openclaw]. `pi-agent-core` is an agent loop with four tools:
read/write/edit/bash. That is the whole comparison with Claude Code in one line.

## 3. Five collisions with what we already decided

Scored against our decided architecture, not against a blank page.

| Our decision | Flue | Verdict |
| --- | --- | --- |
| `claude -p` + Matt's skills, three phases ([architecture §3][arch]) | Pi harness, 4 tools, `useSubagent()` takes **TS functions**, not `.claude/agents/*.md` | **Collides.** Skills survive; subagents and the run plan do not |
| gVisor is load-bearing, not defence-in-depth ([ADR 0001][adr1]) | self-hosted sandbox is `local()`: *"binds the agent directly to the host… **There is no isolation, by design**"* | **Collides.** No isolation story off Cloudflare |
| No database ([ADR 0004][adr4]) | Node/Docker target: in-memory SQLite lost on restart; durability needs `@flue/postgres` + `DATABASE_URL` | **Collides.** Reintroduces Postgres |
| No `kubectl exec` as a control or data channel, anywhere ([architecture §11][arch]) | a remote sandbox is an RPC-shaped interface, not an exec channel — see §4 | **Does not collide.** This objection was wrong; withdrawn |
| **Claude on Vertex** ([ADR 0005][adr5]) | provider `google-vertex` exists but its **model catalogue is Gemini-only**; Claude appears under `amazon-bedrock`, not Vertex | **Collides, and this is the hard one** |

**The skill-loader objection: docs say yes, code says no.** Flue's *docs* claim skills
*"follow the open **Agent Skills** format"*, which suggested Matt's skills would port.
Reading the actual loader in `triagebot-action`'s bundled copy refutes that in practice
(`dist/index.mjs:230137-230161`): `discoverLocalSkills` globs exactly
`<cwd>/.agents/skills/<dir>/SKILL.md` — **one level, no recursion** — and the directory
name must equal a kebab-case `name` or it **throws, aborting init for every skill**.
`agentskills.io` is referenced nowhere in the code. `allowed-tools` is parsed and never
read; `disable-model-invocation` does not exist.

That is [architecture §11][arch]'s **second** failure mode reproduced exactly — *"both
scan immediate subdirectories only, so the two-level layout yields zero skills
silently"*. Matt's skills would need flattening to one level, per-skill directory
renaming, `metadata` values quoted and `allowed-tools` stripped. §11's judgement stands;
my earlier note that it "does not transfer" was based on docs rather than code and is
withdrawn.

**The seam is RPC-shaped and genuinely reimplementable.** `SessionEnv` is 11 members —
`exec(cmd, {cwd, env, timeout, signal}) → {stdout, stderr, exitCode}` plus `readFile`,
`readFileBuffer`, `writeFile`, `stat`, `readdir`, `exists`, `mkdir`, `rm`, `cwd`,
`resolvePath`. Remote adapters (E2B, Daytona, Modal, Vercel, Cloudflare Sandbox) satisfy
it over their providers' HTTP APIs, and Flue ships `createSandboxSessionEnv` for exactly
that. **No `kubectl exec` is implied**, so the §11 objection does not apply and is
withdrawn. Confirmed too that the loop runs in the orchestrator's own process: the Pi
`Agent` is built inside Flue's `Session`, and `bash`/`grep`/`glob` each make one
`env.exec` per tool call.

So there are **three** integration shapes, not two:

- **(a) Flue's harness drives a remote pod per tool call.** Implementable via
  `SessionEnv`, but the pod becomes a long-lived server the orchestrator dials, the
  orchestrator must stay alive for the whole run, and `exec` is buffered — a 20-minute
  build is one hanging RPC. Workspace affinity is mandatory with no `onLost` hook to
  signal a restarted Job.
- **(b) Flue as durable dispatch/wait/callback only** — uses none of its harness. Our Go
  controller with a tax.
- **(c) ⭐ The whole Flue program runs *inside* the Job with `local()`.** This is what
  `triagebot-action` actually does, with the Actions runner as the Job. `local()` needs
  no RPC, gVisor supplies the isolation `local()` lacks, the orchestrator stays a dumb
  Job-creator that may crash mid-run, and Postgres never enters. **An earlier draft of
  this document claimed no third shape existed. That was wrong.**

Shape (c) is architecturally sound and is the only serious version of the question.
What kills it is not the shape — it is (§3 table) Claude-on-Vertex being absent from
Flue's catalogue, the skill loader above, and trading Claude Code for Pi.

## 3a. The reference implementation, read directly

`triagebot-action` (read locally, `~/projects/oss/triagebot-action`, HEAD `7a0dedd`) is
the Astro bot from the blog post. It is the most useful artefact in this whole
investigation, because it is a working system solving our problem in 1925 lines.

**It is a GitHub Action and nothing else.** `runs.using: node24`,
`main: dist/index.mjs`; zero webhook or server code in `src/` (grep for
`createServer|listen(|Hono|express|webhook` returns nothing). Triggered on
`issues: [opened, reopened, closed]` and `issue_comment: [created]` — **not** `labeled`.
Bounded by `timeout-minutes: 30`, serialized by `concurrency: triage-<issue>` with
`cancel-in-progress: false`. So the design admired in the question is the GHA shape the
question wanted to avoid; our webhook trigger is already ahead of it.

**It has no database, and that is the interesting part.** Labels are the only persistent
state; each run re-fetches the issue plus 100 comments; phase is derived by
`currentTriageLabel()` (`src/labels.ts:76-79`) and dispatched by a pure-TS FSM
(`src/router.ts:26-72`). The 3-attempt failure cap is counted by scanning comment bodies
for a hidden `<!-- triagebot:triage-failed -->` marker
(`src/handlers/triage.ts:33-34,313-316`). This corroborates [ADR 0004][adr4]: a system
like ours does not need one.

**Its control flow is our control flow.** `runTriagePipeline`
(`src/handlers/triage.ts:108-235`) makes exactly four LLM calls in fixed order —
reproduce, diagnose, verify, fix — with hardcoded early exits, and commit/push/release/
label are straight-line TS (`:416-528`). Inside each stage the model is fully
autonomous with only general tools (`read`, `write`, `edit`, `bash`, `grep`, `glob`,
`task`; bash is described as *"Execute a bash command. Returns stdout and stderr."*),
looping to `MAX_FOLLOWUPS=32`. Install and build commands are prose in the skill files,
model-chosen. **Fixed outer pipeline, autonomous inner loop** is precisely
[architecture §3][arch]'s three phases. Independent convergence on our design.

**Models are Claude, not Kimi.** Defaults are `anthropic/claude-opus-4-6` (triage) and
`anthropic/claude-sonnet-4-6` (verification), `src/index.ts:67-68`. The Kimi/Workers-AI
strings in the blog are README examples only. Provider selection is a string prefix plus
the matching env var (`src/index.ts:73-98`) — 62 lines, no abstraction.

**Two credential patterns worth stealing.** The model's bash sees only a **14-key env
allowlist**; `ANTHROPIC_API_KEY`, `CLOUDFLARE_*` and the write token are stripped, and
all GitHub writes execute outside the sandbox (`src/github.ts:299-302`). The orchestrator
commits and pushes, not the agent. That reaches [ADR 0005][adr5]'s core goal — no
credential the agent can read — with an allowlist instead of cert-manager, TLS
interception and five CA variables.

**Where it is weaker than we should be**, recorded because it tempers the "just use
theirs" instinct:

- The write token is **interpolated into a shell string** for `exec`
  (`src/github.ts:310-312`), so it is visible via `ps`, and it is logged at
  `triage.ts:444`.
- Untrusted issue text reaches a model holding shell access and a read token, with **no
  egress restriction whatsoever** — `local()` is plain `child_process.exec` on the
  runner, no container. Our gVisor posture is strictly stronger.
- **No measurement of triage quality.** 16 binary `assert.equal` eval cases covering only
  two cheap classifiers, both **excluded from `pnpm test` and CI**. No golden set, no
  judge, no pass rates for reproduction, diagnosis or fix correctness. Integration tests
  assert label transitions, never output quality. No typecheck in CI either.
- Three dead inputs: `build-command` read and never used; `triage-skill` **required** and
  never consumed (`d74b72a` removed it, discovery is convention-only); `pr-skill`'s path
  ignored. And `src/flue.ts:14-23` builds an `InMemoryFs` sandbox that is unreachable,
  because every handler passes `local()`.
- It pins `@flue/runtime ^0.8.1` while Flue is at 2.0.3, and reaches into
  `@flue/runtime/internal` (`src/flue.ts:2-9`) to build its own session. The flagship
  consumer is a major version behind and depends on non-public API.

## 4. Cloudflare Workers as the orchestrator, specifically

Ruled out on primary sources, in case it comes back:

- **Cloudflare Workflows has no self-host path.** Hosted product only; nothing in
  `workerd`. (Inference from absence, but a thorough search found no OSS engine.)
- **`workerd` does not give us isolation.** Its own README: *"`workerd` is not a
  hardened sandbox… when using `workerd` to run possibly-malicious code, you must run it
  inside an appropriate secure sandbox, such as a virtual machine."* We would re-add
  gVisor underneath it.
- **Miniflare is a *"fully-local simulator"***, framed under Testing, not Deployment.
- **A Worker cannot watch a Job.** No long-lived watch. Options are Workflows
  sleep+poll (hosted-only), DO alarms (how Flue's own coordinator waits, ~30 s
  heartbeat), or the pod posting back. Worker CPU is 30 s default / 5 min paid per
  request; 15 min wall on non-HTTP triggers.
- **Cloudflare Containers cannot reach our API server.** *"Only ports 80, 443, and DNS
  are available"* — k3s on 6443 is unreachable without fronting it on 443. They also run
  only on Cloudflare's network; no BYO compute.

Worth knowing, though: Claude Code on Cloudflare Sandbox is real and documented
(official tutorial, repo cloned into a sandbox, Claude driven by an API key secret), and
Docker-in-Docker works there with `--iptables=false` — a striking echo of our own §8
findings.

## 5. Alternatives: nothing off the shelf does this

Full scoring in the raw agent output; the load-bearing results:

- **`claude -p` as a subprocess is the officially documented path for us.** Anthropic:
  *"The SDK is available as a library for Python and TypeScript only. To drive the same
  agent loop from another language, run the CLI as a subprocess with the `-p` flag."*
  **There is no Go SDK.** Verified locally that our flags are current, not drifted:
  `claude --help` lists `--json-schema <schema>`, and `proto/phase.sh:209` uses it with
  `--output-format stream-json`.
- **Managed Agents fails self-hosting.** Self-hosted sandboxes *"keep the orchestration
  on Anthropic's side… Tool inputs and outputs still flow to Anthropic's control plane."*
- **`github/gh-aw` is Actions-only** — *"The gh aw CLI converts these into standard
  GitHub Actions workflows."* Rejected on the premise of the question.
- **Durable-execution backbones all fail our no-DB constraint or their licence.**
  Temporal's Helm chart *"does not install any database sub-charts"* and needs
  MySQL/PostgreSQL/Cassandra; DBOS needs Postgres *"for production"*; Restate is
  **BSL 1.1**, not OSI. None of them solve a problem we have — one Job per run is not a
  distributed transaction.
- **Argo Events + Workflows can do webhook→Job**, at the cost of two controllers and a
  NATS EventBus for a single trigger type.
- **Two projects are already our architecture**: `kube-foundry/kube-foundry` (*"Add the
  `factory:do` label to any GitHub issue, and the webhook server automatically creates a
  `SoftwareTask`"* — Go/kubebuilder, no DB, operator-resolved `secretKeyRef` injection)
  and `kelos-dev/kelos` (webhook TaskSpawner, label `agent-ready`, Apache-2.0, Go). Kube
  Foundry is 15 stars / 11 commits — **read as design reference, do not depend on**.
- **`kubernetes-sigs/agent-sandbox` reached v0.2.1** and is a SIG Apps project offering
  *"a standardized Kubernetes API that fully decouples the execution layer from the
  underlying isolation technology."* [ADR 0002][adr2] rejected it at an earlier
  maturity; that rejection deserves a re-read, not a reversal.

**Conclusion:** the combination of webhook front-end + run tracking + k8s Job execution,
self-hosted, no DB, in Go, does not exist off the shelf. Our ~1–2k lines of controller
is the cheapest path, and two independent projects arriving at the same shape is
corroboration.

## 6. The credential proxy — and a hole in our prototype

**The proxy survives the Flue question untouched**, because Flue explicitly disclaims
the problem: *"Treat network egress, mounted data, credentials, and side effects as
application security decisions."* Zero credential properties gained or lost. What
decides the proxy is what runs the agent, not what orchestrates it.

Two things are worth acting on regardless of which path we take.

**⚠️ The prototype proxy authenticates no caller.** Verified directly:
`proto/proxy/main.go:151` mounts `/vertex/` on the credential-injecting reverse proxy,
and `main.go:155-164` handles `CONNECT` with no caller check at all. Combined with
[architecture §7][arch]'s open egress and no `NetworkPolicy` in MVP, **any pod in the
namespace that sets `https_proxy` and trusts the CA gets authenticated GitHub and
Vertex.** `probe.sh` demonstrates this accidentally: a throwaway pod with only
`https_proxy` and the CA reached both. This is a larger hole than the one the expensive
half of the design closes, and it is cheap to fix — the `NetworkPolicy` §7 already
designs, plus a shared secret header. It should be a tracked issue.

**Three of ADR 0005's claims are weaker than they read.** Recorded so nobody relies on
them:

1. *"TLS interception is unavoidable"* is circular — it is unavoidable only given the
   premise that the sandbox believes it is talking to `github.com`, which is a choice.
   `git insteadOf` plus `GH_HOST` achieves the same swap with no cert-manager, no five
   CA variables, no HTTP/1.1 downgrade and no rotation bug. There *is* a good reason to
   intercept anyway — agent-authored URLs escape static rewriting — but the ADR never
   states it, so the argument on the page is not the argument that holds.
2. The branch ruleset is called *"advisory"*. It is not: GitHub's own docs say *"only
   users with bypass permissions can push to branches or tags whose name matches the
   pattern."* The proxy's real prize is **per-run** scoping, which a prefix ruleset
   genuinely cannot express — a narrower and still-sufficient claim.
3. The alternatives list rejects an API key **in the pod** but never considers one **at
   the proxy**, which keeps full zero-credential-in-sandbox while deleting WIF, the four
   model pins, and the measured quality regression (the one Vertex run shipped a red
   branch where the baselines went green).

And the status ledger is thinner than §12 implies: measured is *no model credential in
the sandbox*, the sentinel swap (with a **PAT**, not a minted token), GAR injection, and
the CA seam. Rotation is unmeasured. **Designed only, zero code**: the KSA-confined
identity (the prototype mounted a *user* ADC key file — WIF has never run), branch
enforcement, KMS, and the egress allowlist itself.

**Verdict — keep, but simplify.** Decouple it from GCP, stop treating GitHub
interception as MVP-mandatory, and promote sandbox→proxy authentication above both. Note
for the record that **Cloudflare Sandbox ships ADR 0005's design, GA, more completely
than our MVP** — outbound handlers are *"programmable egress proxies that run on the
same machine as the sandbox"* that *"can hold secrets that the sandbox itself never
sees"*; *"Sandboxes intercept HTTPS traffic by default"* with *"a unique ephemeral
certificate authority (CA) and private key … created for each sandbox instance"* whose
key *"never leaves the container runtime sidecar process"*; `enableInternet=false` plus
`allowedHosts` is *"a deny-by-default allowlist"*. That is our proxy, our TLS
interception, our CA seam, per-run scoping *and* the NetworkPolicy enforcement §7
defers. It is strong evidence the design is right. It is not a reason to move, because
the compute has to be Cloudflare's.

## 7. Recommendation

**Stay on the Go controller and the k8s Job. Do not adopt Flue** — but the reason is
narrower than the first draft of this document claimed, and the shape is viable.

Shape (c) in §3 — the whole Flue program running inside our gVisor Job with `local()`,
exactly as `triagebot-action` runs it on an Actions runner — is architecturally sound.
It dissolves the Postgres, isolation and `kubectl exec` objections entirely. Three things
kill it, in order:

1. **Claude on Vertex is not in Flue's model catalogue.** `google-vertex` carries Gemini
   only; Claude lives under `amazon-bedrock`. Our entire GCP posture — WIF, KMS, the
   proxy's `roles/aiplatform.user` — assumes Claude on Vertex. Adopting Flue means adding
   that provider ourselves or moving to Bedrock. That is the hard blocker, and it is
   fixable-but-ours.
2. **The skill loader is one-level with a hard throw**, reproducing §11's exact
   silent-zero-skills failure. Matt's skills need flattening and renaming to load.
3. **We would trade Claude Code for Pi** — `pi-agent-core` is read/write/edit/bash — plus
   lose `.claude/agents/*.md` subagents, which our review phase uses.

Against that: 1925 lines is what Flue saves us, and our controller is the same order of
magnitude. The trade is not "reimplement everything vs. reuse"; it is "keep our agent" vs
"save ~2k lines". Keep the agent.

**What to steal, in priority order:**

1. **Fix the proxy's missing caller auth** and land the `NetworkPolicy` — a real hole,
   found in our own code, independent of everything else here.
2. **Read `kube-foundry` and `kelos` before writing the controller.** Same shape, in Go,
   already built. Cheapest possible design review.
3. **Re-read [ADR 0002][adr2] against `agent-sandbox` v0.2.1**, which is now a SIG Apps
   project. Probably still a no; worth being able to say why at current maturity.
4. **Steal Flue's per-request credential resolution idea** (`auth.apiKey.resolve()`
   running per request) as the model for the proxy's rotation story — the certificate
   read once at startup is [architecture §12][arch]'s own known gap.
5. ~~Move the GitHub writes into the orchestrator, as `triagebot-action` does.~~
   **Rejected on review, recorded so it is not re-proposed.** `triagebot-action` keeps the
   write token out of the agent's environment and pushes from the orchestrator (§3a)
   *because it has no proxy* — a 14-key env allowlist is its only boundary. [ADR
   0005][adr5]'s sentinel swap already achieves the same property: the pod holds no real
   GitHub credential when it pushes. So the pattern buys nothing here and costs a channel
   for getting the working tree out of the pod. It is also strictly worse than it looks:
   the sandbox needs **GAR** credentials for private packages regardless, so the proxy is
   deployed either way, and removing one credential path from a mandatory component is not
   a saving. Sharing the workspace instead of shipping a patch would mean NFS or a GCS
   filesystem, neither of which is viable for dependency installation and builds.
6. **Copy the hidden-marker retry counter.** A `<!-- implementer:failed -->` comment
   marker gives a bounded retry count with no database — directly useful given
   [architecture §11][arch] currently cuts retry policy entirely.
7. **Take their eval gap as a warning.** They ship no measurement of triage quality and
   exclude what evals exist from CI. [#31][measure] is our equivalent exposure, and two
   of our three phases have never run.

**What would change my mind:** if we ever want the agent loop *itself* to be
programmable — dynamic instructions per turn, subagents composed in code, state across
turns — Flue is a well-shaped answer and our phase script is the wrong tool. That is a
different product than "label an issue, get a pull request", and it is worth revisiting
the day we want it.

## Unknowns

- Exact LOC of Flue's `packages/` alone; the 2.6 MB TS figure is the whole monorepo
  including docs site and examples.
- Whether Flue's Node coordinator supports multi-replica operation on k8s or assumes a
  single process — the durability docs imply lease-based single ownership; the source
  file was not read in full.
- Whether Workers mTLS bindings can trust a *custom server* CA (our k3s API server), as
  opposed to presenting a client cert. Only matters if the Workers path is revived.
- A documented `google-vertex` + Claude model string in Flue's own docs — the provider is
  listed, the combination was not found in an example.
- Whether `kube-foundry`/`kelos` use gVisor specifically, and whether either does draft
  PRs; neither confirmed.
- Cloudflare Sandbox's "private service connectivity" mechanism and limits — mentioned in
  the Managed Agents post, not verified against a limits page.

[i37]: https://github.com/nissessenap/the-implementer/issues/37
[flue]: https://github.com/withastro/flue
[cfblog]: https://blog.cloudflare.com/astro-issue-triage/
[cfsdk]: https://blog.cloudflare.com/agents-platform-flue-sdk/
[pihome]: https://pi.dev/news/2026/5/7/pi-has-a-new-home
[openclaw]: https://github.com/openclaw/openclaw
[arch]: ../architecture.md
[adr1]: ../adr/0001-sandbox-image-strategy-and-byo-contract.md
[adr2]: ../adr/0002-a-run-executes-as-a-kubernetes-job.md
[adr4]: ../adr/0004-the-orchestrator-is-a-controller-with-a-webhook-front-end.md
[adr5]: ../adr/0005-credentials-terminate-at-the-credential-proxy.md
[measure]: https://github.com/nissessenap/the-implementer/issues/31
