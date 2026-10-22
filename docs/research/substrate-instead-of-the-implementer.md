# Agent Substrate instead of the-implementer?

**Date:** 2026-10-21 · **Follows:** [agentgateway-revisited-2026-10.md][agw] and
[#64][i64]

**Question asked:** Rather than replacing only the proxy, should we scrap most of this
project and run on [Agent Substrate][sub]? We would keep a small trigger to launch the
agent.

**Short answer:** **No, not now. Re-check if Substrate gains a run-to-completion mode,
which nobody has asked it for yet (§7). A workable one-shot path exists today if we build the exit signal ourselves (§6).**

Substrate has caught up on the credential side faster than #64 predicted. Transparent
egress interception with fail-closed credential injection is now merged and graduated
from experimental. But Substrate is built for a different workload.

**What we run:** a one-shot batch run of about 450 seconds. We need its exit, a result,
and a report afterwards.

**What Substrate runs:** long-lived, mostly idle actors that it suspends and resumes.
**Substrate has no completion signal at all.** If we moved onto it:
- We would delete the parts of this repo that are cheap: the Job builder and the proxy's
  data path.
- We would keep everything that is ours: the trigger, the run plan, reporting, PR
  creation, toolchain detection, and every credential.
- We would rebuild the part Substrate lacks, which is a way to learn that a run has ended
  and what it produced.
- We would add Postgres, an object store, Kubernetes 1.36+ with beta APIs, and a pre-1.0
  API that breaks weekly.

**How these sources were read:**
- **Substrate:** three agents read it at HEAD `e4943430` (release v0.5.0, 2026-10-14) in
  shallow clones. Nothing was built or run.
- **agentgateway:** read at `origin/main` `974e1a30`.
- **This repo:** inventoried at `ee0bd91`.
- **Citations:** `file:line` citations into Substrate are the agents'. Re-verify one before
  building on it.

---

## 1. What Substrate is

- **Pitch:** a *"zero-trust runtime for sandboxed agents"* and *"a low-opinion runtime, not
  an agent SDK"* (README:20-24). Many *actors* are multiplexed onto a pool of warm
  *workers*, and idle actors are snapshotted out of memory.
- **It runs its own control plane, not Kubernetes objects.**
  - `ateapi` is a gRPC server that stores Actors, ActorTemplates and Workers in
    **PostgreSQL**.
  - The only CRDs are `WorkerPool` and `SandboxConfig`.
  - The stated reason: the apiserver is *"not designed to handle millions of resources"*
    (architecture.md:153-171).
- **Other components:**
  - `atelet` is a per-node DaemonSet that moves snapshots through GCS or S3.
  - The gVisor and microVM workers drive checkpoint and restore.
  - `atenet` contains the Envoy ingress router, `atunnel`, and the egress gateway.
  - Further pieces: an OIDC issuer, a pod-cert polyfill, and the `kubectl-ate` CLI.
- **What the cluster must provide:**
  - The PodCertificateRequest and ClusterTrustBundle APIs. On GKE that means 1.36 with
    the beta APIs enabled at cluster creation, or 1.37+. *"Versions below 1.36 are not
    supported"* (tools/setup-gcp/README.md:72-88).
  - gVisor or microVM isolation. MicroVM needs `/dev/kvm`. *"Plain Linux containers are
    not supported."*
  - It is installed with kustomize and `ate-setup`. **There is no Helm chart.**
- **Where it runs:** GKE is first-class, and kind has a dev quickstart. **k3s is never
  mentioned**, and our local test cluster is k3s.
- **Who runs it:** 16 maintainers from Google (including Tim Hockin and Ben Elder) and 3
  from Solo. The repo was created 2026-05-13 and has 5.7k stars, about 587 commits in the
  last 30 days, and 859 open issues.
- **Stability:** *"pre-1.0. We are not making any guarantees about backward
  compatibility"* (README:75-77). The architecture doc says *"Much of this architecture is
  aspirational, and is not yet implemented!"* (architecture.md:3). v0.5.0 alone lists 10
  breaking changes.

## 2. Why it does not fit a run

| What a run needs (ADR 0002/0004) | Substrate |
| --- | --- |
| Start once, run to completion | Create leaves an actor SUSPENDED. It starts on an inbound request or an explicit `ResumeActor`. No issue or discussion asks for a batch mode (§7). §6 builds one on top. |
| A terminal state when the agent exits | **None.** When the app container exits, the actor *still shows RUNNING* (observability.md:207, Substrate issue #2211). CRASHED only means the sandbox died or was evicted. |
| A result channel (`/dev/termination-log` blob) | **None.** No exit code, no result, no watch or stream RPC. You poll `GetActor`. |
| Reporting endings the agent cannot report (OOM, deadline, eviction, pull failure) | Lifecycle events are emitted as OTLP log records, not as an API. |
| Logs after the run | `kubectl ate logs` reads the current worker pod only. History needs a central log backend. |
| GC and deadline that come with the platform | No TTL GC of actors yet (roadmap). No `activeDeadlineSeconds` equivalent was found. |
| No database (ADR 0004) | Postgres is required. |
| A BYO image with a baked run plan (ADR 0001) | **Fits.** ActorTemplate takes `image`, `command`, `args`, env and limits. |
| gVisor | **Fits.** It is the default. |

The informer (`orchestrator/informer.go`) exists *because* of the second, third and fourth
rows. Substrate does not take those rows off our hands. It deletes the signal we currently
build them on.

## 3. Credentials and egress: what #64 got wrong, now

#64 (2026-08) said Substrate had *"built none of it"*. That has been overtaken:

- **2026-09-16:** the credential injector merged (#1360).
- **2026-09-22:** the k8s-Secret provider merged (#1335).
- **2026-09-28:** egress policy "for GA" merged (#1751).
- **2026-09-30:** a GCP Secret Manager provider merged (#1991).
- **2026-10-02:** injection graduated from experimental (#2101).
- **2026-10-09:** actor JWT injection merged (#2141).

What it does now:

- **Interception is transparent to the sandbox.**
  - nftables redirects TCP inside the worker's network namespace. `atunnel`, *outside* the
    sandbox, recovers the original destination and opens an mTLS CONNECT to the gateway
    with a short-lived per-actor cert (`docs/network-egress.md:11-69`).
  - The gateway checks the actor's UID and that it is running. That is the three-way
    liveness check #64 wanted.
  - DNS resolves only names that are in the policy.
  - The sandbox needs only the CA (`SSL_CERT_FILE`, `GIT_SSL_CAINFO`) and a placeholder
    header. **It needs no proxy env, no `http.proxyAuthMethod`, and no client cert.**
  - That answers the deciding unknown in [agentgateway-revisited §5][agw]. The answer is
    only available on Substrate's own runtime, though. Our bare Jobs have no `atunnel`.
- **Policy and injection** behave as described in [agentgateway-revisited §4][agw]:
  - rules on hostname and port, with intercept or passthrough chosen per hostname, so
    ADR 0005's disqualifier is cleared;
  - `replace_headers` swaps a placeholder for `FetchSecret(uri, actor_spiffe_id)`.
- **The dataplane is Envoy by default.** agentgateway is optional and pinned to an alpha
  build (`operator-install.md:294`).
  - agentgateway's 300 s credential cache is keyed by actor *name*, not UID, so a
    recreated actor can receive its predecessor's credential (`egress.rs:20-21,280-287`).
  - Envoy fronts a single provider.

Our needs against that:

| Need | Substrate |
| --- | --- |
| Sandbox holds no credentials | present |
| Egress allowlist (our #16, not built) | present, default deny, with DNS filtering |
| gh `token` header, git Basic | present: prefix `token ` and `Basic ` respectively. For Basic, the provider returns `base64("x-access-token:"+tok)`. |
| Per-run App token for one repo, App JWT signed by KMS/OpenBao | **we write it**: a `FetchSecret` provider. Its scope has to come from the policy URI plus the provider's own ACL, because the request carries no annotations. The shipped providers only read static secrets. |
| Vertex token, GAR token | **we write it**: same provider, returning an ADC token. Nothing in Substrate routes to an LLM. |
| Vertex restricted to one model call, POST only | **absent**: rules match host and port only |
| Push only to `implementer/*` | **absent**. The only hook is Envoy `additionalExtprocService`, which is marked experimental. |

Their own Claude Code demo (`demos/claude-code-multiplex`) puts `ANTHROPIC_API_KEY` in the
sandbox env *"as a plain value"*, and loops `claude --print` with a `sleep`. *"Stateful
Coding Adapters"* is still a roadmap idea. **No coding-agent, GitHub or webhook workflow
exists in core or anywhere else in the org.**

## 4. What we would delete, keep and add

Line counts are non-test and come from the inventory at `ee0bd91`.

**Delete**
- The Job builder `run` (`job.go`, `jobname.go`, 397 lines) and the Job template in
  `charts/orchestrator`.
- The proxy's transport, MITM, auth and identity code (about 1,200 of 1,857 lines), and
  `charts/proxy`.
- e2e stages 10–40 and 70.
- The egress allowlist we had not built yet.

**Keep, unchanged or ported**
- The webhook `serve` (222 lines). Substrate has no triggers.
- `phase.sh`, the Dockerfile and the schemas (590 lines). This is the product.
- `mint.go` and ghait, rewrapped as a `FetchSecret` server, plus Vertex/GAR token
  providers.
- Every open product issue: #75 PR creation, #76 toolchain detection, #74 images,
  #66 model pins, #43, #32.
- Ref enforcement, which is unbuilt either way.

**Rewrite**
- The informer (561 lines). It would become a poller over `GetActor`, plus a new result
  channel:
  - `phase.sh` would have to POST its blob somewhere through egress;
  - and something has to tell "the agent finished" apart from "the actor is idle in
    RUNNING".
- That is harder than what it replaces, not easier.
- The ADRs: 0002 and 0004 are superseded. 0005's data-path half is superseded.

**Add**
- Operating Postgres, an object store, `atelet`, the workers, `atenet` and the OIDC issuer.
- A cluster floor of Kubernetes 1.36+ with beta APIs, and no k3s path.
- Tracking a weekly pre-1.0 release train whose last release carried 10 breaking changes.

So "a small trigger" is not what remains. What remains is the trigger plus the whole
product layer plus the providers, now resting on a runtime with no notion of "finished".

## 5. Against our own yardstick

The Kelos note ([kelos-and-kubefoundry.md][kelos]) set two rules for adopting someone
else's platform:

- *"Our differentiator is not the orchestration plumbing … it is **the security posture
  and the GCP integration**."* Substrate now shares the security posture, which is new and
  is the real argument for it. The GCP integration (mint, KMS, Vertex, GAR) would still be
  ours, as providers.
- *"Before then, adopting Kelos saves little and costs the ADRs; after then, we own the
  plumbing and adopting it saves nothing."* The plumbing is written. What Substrate would
  take over is the part that already works, while the part it lacks (run completion) is
  the part we would have to rebuild.

The Flue note ([flue-and-the-orchestrator-question.md][flue]) disqualified that project
for being *"Experimental … APIs that may change"*. Substrate says the same thing about
itself, with far more backing.

## 6. A feasible path today: run-to-completion built on top

Substrate has no batch mode, but nothing in its API stops an unattended one-shot run. We
would build the exit signal and the deadline ourselves. Everything below comes from
`docs/api-guide.md` and `docs/glossary.md` at `e4943430`. **None of it has been run.**

### 6.1 One run

1. **Template, with the golden snapshot off.**
   - The ActorTemplate takes our sandbox image with `phase.sh` as its command, and
     `snapshotConfig.goldenSnapshotConfig.mode: GOLDEN_SNAPSHOT_MODE_DISABLED`.
   - This is required, not optional. With golden snapshots on, Substrate boots the
     workload once when the template is created and snapshots it, so `phase.sh` would
     already be running. Every actor would then resume mid-run from that one snapshot.
   - With golden snapshots off, an actor *"cold-boots"* from the image
     (glossary.md:131-133, api-guide.md:572-598).
2. **Start, from the webhook:** `CreateActor` (named after the run), then
   `CreateEgressPolicy`, then `ResumeActor`.
   - The policy holds HTTPS rules for the GitHub hosts and Vertex. Their `replace_headers`
     point at our provider.
   - *"Suspend is always explicit"* (api-guide.md:611-613), so nothing pauses a run
     halfway and no human is needed.
3. **Per-run input (owner, repo, issue).** Template env is literal and fixed per template:
   no interpolation, no `valueFrom` (api-guide.md:168). There are two ways around it:
   - **A template per run.** Templates are immutable but cheap with the golden snapshot
     off. Delete the template with its actor.
   - **Derive the run from the actor.** `phase.sh` reads its own name and uid from a
     `systemInfo` volume (`actorMetadata`, api-guide.md:203-233) and looks the run up.

   The first is simpler. The second keeps one template per image.
4. **Result: `phase.sh` POSTs it.**
   - Its last step POSTs the result blob, the same JSON `phase_test.go` types, to the
     orchestrator.
   - An egress rule for the orchestrator's host injects Substrate's actor JWT (`actor_jwt`
     effect, `MintActorJwt`). The orchestrator verifies it against Substrate's OIDC issuer
     (`ate-idp-server`), so only that actor can report for that run.
   - The orchestrator then comments and calls `DeleteActor`.
   - `phase.sh` should **`sleep` after the POST rather than exit**. An exited process still
     shows RUNNING ([#2211][s2211]), and a sleeping one is at least honest about it.
5. **Endings with no result: a reaper, as the deadline.** OOM, a sandbox crash and a hang
   all look like RUNNING (#2211), so a deadline is the only thing that catches them.
   - A loop runs `ListActors` over the atespace.
   - It deletes anything older than the deadline, judged by creation time in the actor's
     metadata, and anything in `CRASHED`.
   - Each deletion posts the "ended without a result" comment.
   - It needs no database. The `<!-- implementer-run: <uid> -->` marker is still the
     exactly-once record (ADR 0004), and the actor's creation time *is* the deadline's
     start.
   - **The cost:** an OOM is reported at the deadline, not when it happens. The Job
     informer reports it immediately.

### 6.2 What we would write

| Piece | Replaces | Size guess |
| --- | --- | --- |
| Substrate adapter in the orchestrator: create/resume, result endpoint + JWT check, reaper | `job.go` + informer (~960 lines) | about the same |
| `FetchSecret` provider: GitHub mint (from `mint.go` + ghait), Vertex token, GAR token | the proxy's `creds`/`mint`/`vertex`/`gar` | smaller, since the transport is gone |
| End of `phase.sh`: POST the result instead of writing `/dev/termination-log` | the termination-log channel | a few lines |

Unchanged: the webhook front-end, the run plan, PR creation (#75), toolchain detection
(#76) and the images (#74).

### 6.3 What a prototype has to settle

- **Claude on Vertex.** Injection replaces only a header the client already sends. Claude
  Code with `CLAUDE_CODE_SKIP_VERTEX_AUTH` probably sends no `Authorization` at all. It
  may need `ANTHROPIC_CUSTOM_HEADERS="Authorization: Bearer placeholder"`. Untested.
- **git.** Basic credentials go out on the retry after a 401. That retry has to carry a
  placeholder for the swap to happen, and a public-repo clone never triggers one, which is
  fine. Push must be checked.
- **Cold boot.** Does a template with the golden snapshot off cold-boot on `ResumeActor`
  alone, with no inbound request? The docs say yes.
- **Logs.** `kubectl ate logs` reads only a live worker (observability.md:31-37). The
  transcript has to travel in the result or go to a log backend.
- **Cluster.** It needs Kubernetes 1.36+ with beta APIs, Postgres and an object store.
  Use their kind quickstart (`hack/create-kind-cluster.sh`), not k3s.
- **What is lost against today:**
  - push-branch enforcement, which is unbuilt either way;
  - the Vertex path restriction;
  - immediate reporting of OOM and eviction.

### 6.4 Where to ask

The CNCF Slack channels are `#substrate-users` and `#substrate-dev` (invite at
slack.cncf.io). There is also the `ate-dev` Google Group, and a weekly meeting on
Thursdays at 10:00–11:00 PST (README:86-99).

Two questions are worth asking before building:
- Are run-to-completion actors with a readable exit status wanted?
- Is golden-off plus a self-reported result the intended pattern for batch work?

## 7. What would change the answer

Revisit when **all** of these hold:

1. Substrate ships a **run-to-completion mode** with an exit status that you can read or
   watch through the API.
   - **As of 2026-10-21, nobody has asked for one.** Searching issues for batch, one-shot,
     run-to-completion, exit code, short-lived and completion, and searching
     Discussions, found no request.
   - The roadmap's *"Decide: What mode(s) of actor runs"* (roadmap.md:29-33) is **not**
     this. Its four options are all ways to start a recurring activation (clean OCI,
     golden, persisted rootfs, full resume).
   - The nearest work:
     - [#2211][s2211] *"Actor stays RUNNING after its sandbox dies"* (open). Its reporter
       killed the app and got *"still RUNNING, 502s, no exit code"*. ateom's child reaper
       discards the exit status (`internal/childreap/childreap.go:153`), and the
       maintainer reply frames the fix as dead-guest *detection*, not as completion.
     - [#2212][s2212] makes the same crashes visible in logs and metrics.
     - [#450][s450] adds `preCheckpoint`/`postRestore` hooks. It is about suspend and
       resume, with no exit hook.
     - [#185][s185] proposes an exec channel.
   - A run-to-completion mode would have to be **requested by us**, and #2211 is the
     natural place to ask. Exit-status capture is the piece both needs share.
2. It tags a **1.0**, or at least a release with no breaking changes, so that the version
   skew policy applies.
3. It **installs on our local test cluster** (k3s or kind) without hand-patching.

If 1 lands, the move becomes "delete the Job builder, the informer and the proxy data
path; write three providers". That *is* close to the small layer hoped for here.

**Until then, three things are cheap to take:**
- **Write `mint.go` as if it were already a `FetchSecret` provider:** a pure function from
  (run identity, URI) to bytes. It almost is one today.
- **Copy the transparent-interception idea into ADR 0005's notes:** nftables plus a
  tunnel outside the sandbox. ADR 0005 rejected a sidecar because it needs `NET_ADMIN` in
  the constrained container. Substrate avoids that by owning the worker's network
  namespace, which a bare Job cannot do. Write that down so it is not re-derived.
- **Update #64:** it is stale. Its first two "steals" are now upstream behaviour.

[sub]: https://github.com/agent-substrate/substrate
[i64]: https://github.com/nissessenap/the-implementer/issues/64
[agw]: agentgateway-revisited-2026-10.md
[kelos]: kelos-and-kubefoundry.md
[s2211]: https://github.com/agent-substrate/substrate/issues/2211
[s2212]: https://github.com/agent-substrate/substrate/issues/2212
[s450]: https://github.com/agent-substrate/substrate/issues/450
[s185]: https://github.com/agent-substrate/substrate/issues/185
[flue]: flue-and-the-orchestrator-question.md
