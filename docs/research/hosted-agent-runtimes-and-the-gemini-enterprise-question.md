# Hosting the worker on Google: what the Gemini Enterprise Agent Platform actually is, and what it buys

**Date:** 2026-08-28 · **Shape:** hosting survey, docs-first. The design being hosted is
[alert-driven-runs][flow]; nothing here redesigns it.

**Sources.** Google's HTML docs for this product render client-side and return nav-only to
a fetcher, so the load-bearing facts here come from three places that do not: the
**protos** in `googleapis/googleapis`, the **release-notes Atom feed**
(`/feeds/gemini-enterprise-agent-platform-release-notes.xml` — plain XML where the HTML
page is not), and a **local install of `google-cloud-aiplatform==2.0.1`** whose client
surface was read directly (`pip install` into a venv; no API enabled, no project touched,
no credentials used — §11 lists exactly what was run). Doc pages are cited where they did
render. Our side is the five [ADRs][adr], [`architecture.md`][arch] and [the flow
doc][flow], cited by file and line. Every Google claim carries a launch stage.

**Question asked:** replace the worker Job — and possibly the orchestrator — with a hosted
Google agent runtime, specifically "the Managed Agents API on the Gemini Enterprise Agent
Platform". Can it hold a narrowly-scoped GitHub credential, spin up per alert, run a
gradle/sbt build on a JVM, pull from Artifact Registry, and be watched live? What does it
cost, and what does the platform give that we would otherwise build?

**Answer.** The name is real and names two stacked products: **Managed Agents API** (Pre-GA,
"you may not use them for commercial or production purposes") and **Agent Runtime** (GA
2026-07-30, the renamed Vertex AI Agent Engine) on the renamed **Gemini Enterprise Agent
Platform**. On the two load-bearing questions the docs are conclusive and they split:
**Managed Agents cannot run a JVM** — its sandbox ships "Python 3.11, Node.js 20 … gcloud
CLI, git, curl, jq" and no JDK, with no BYO image ([sandbox-env][mgsandbox]) — while
**Agent Runtime can**, because Google's own page says Java deploys "using a custom
container" ([runtime][runtime]) and `ContainerSpec.image_uri` takes an Artifact Registry URI
([proto][proto1], verified locally). So the answer is not "it can't"; it is **"it can, and
it buys almost nothing"** — and once the PR is accepted as the completion signal, the
platform's remaining pitch shrinks to a debug console, because completion, state and
artefacts are already GitHub's job. Verdict: **keep the worker as a Job, keep the ADK event
stream in mind as the one genuinely live debug tap, and note that Google has independently
shipped [ADR 0005][adr5]'s credential proxy.**

Four findings carry the document:

1. **§2 — every code-execution door on this platform is an HTTP request.** Not one is a
   process. `ContainerSpec` is a single field, `image_uri`, with no `command` or `args`
   (verified locally at 2.0.1); the runtime contract is `POST /api/reasoning_engine` on port
   8080 ([contract][contract]); `sandboxes.send_command()` is an HTTP forwarder taking
   `http_method`, `path`, `headers` and returning an `HttpResponse`; and
   `ExecuteSandboxEnvironmentResponse` is `{outputs: list[Chunk]}` with **no exit code, no
   stdout, no stderr, no timeout**. A shape fact, not a maturity gap.
2. **§8 — Google shipped the flow doc's §5 table as a product, and called it `A2aTask`.**
   `A2aTaskState` is `SUBMITTED, WORKING, COMPLETED, CANCELLED, FAILED, REJECTED,
   INPUT_REQUIRED, AUTH_REQUIRED, PAUSED`; `A2aTask` carries `context_id`,
   `output.artifacts`, `ttl`; `TaskEvent` carries `event_sequence_number`. That is
   [flow:232-241][flow], implemented — and it is **poll-only**: no streaming RPC exists in
   the session or task protos.
3. **§4 — Google independently built our proxy.** "Agent Identity auth manager is a
   centralized credentials vault and authentication broker", and with Agent Gateway
   "end-user credentials … are encrypted by the auth manager and decrypted at the gateway,
   **ensuring that the agent can never access the raw credential**"
   ([auth-manager][authmgr]), GA 2026-08-22. Third independent convergence after Anthropic
   and Substrate. It does not cover a GitHub App JWT — so buy the *gateway*, not the runtime.
4. **§9.3 — ADK is already permanently ruled out, with measurements.**
   [`architecture.md:545-551`][arch]: ADK's `KnownFields(true)` makes
   `disable-model-invocation` a fatal parse error, "failing 24 of 41 skills including
   `implement`". The platform's managed value is reachable only from ADK or a Python
   framework; from a BYO container you get the autoscaler, `secret_env`, and one opaque span.

---

## 0. The product map

The name is real. It is also the third name this surface has had in eighteen months, which
is why 2025-era mental models are wrong. At Cloud Next '26 (2026-04-22) Google folded
Vertex AI into **Gemini Enterprise Agent Platform** — "It's the evolution of Vertex AI…
Moving forward, all Vertex AI services and roadmap evolutions will be delivered exclusively
through the Agent Platform" ([blog][blog0422]) — leaving the `reasoningEngines` resource and
the `aiplatform.googleapis.com` endpoint untouched. The API you call is still
`reasoningEngines`; the product you read about is called something else. `Vertex AI Search`
became `Agent Search` in docs only: "The user interface in the Google Cloud console is still
referred to as Vertex AI Search and AI Applications" ([app-builder release notes][gabrn]).

| Product | What it is | Runs your code | Status |
|---|---|---|---|
| **Gemini Enterprise** | End-user product — "an intranet search, AI assistant, and agentic platform" ([docs][ge]). Consumes agents, does not host them | no | GA |
| **Agent Runtime** (née Vertex AI Agent Engine; API `reasoningEngines` on `aiplatform.googleapis.com`) | "a fully-managed, opinionated runtime that you can use to deploy, operate, and scale agentic applications" ([runtime][runtime]) | **yes, BYO container** | **GA** 2026-07-30, worded "available for everyone, from Agent Runtime to Agent Identity" ([blog][blog0730]) |
| **Managed Agents API** (Gemini Developer API surface, `generativelanguage.googleapis.com/v1beta/interactions`) | "build managed, autonomous agents with a single API call… Powered by the Antigravity harness". Base agent `antigravity-preview-05-2026` | yes, in a fixed sandbox | **Pre-GA / Beta**: "solely for limited testing and evaluation, and you may not use them for commercial or production purposes" ([managed-agents][mgagents]) |
| **ADK** | "an open-source agent development framework", Python/TS/Go/Java; hosting targets are "Runtime, Cloud Run, or Google Kubernetes Engine" ([adk][adkdoc]) | n/a | GA, OSS |
| **Agent Gateway** | "the networking component… secures and governs connectivity for all agentic interactions" — ingress *and* egress, mTLS termination, IAM-gated, Model Armor inspection ([gateway][gateway]) | n/a | **GA** 2026-06-18 (feed); Model Armor on it GA 2026-06-24 |
| **Agent Registry** | "a unified catalog that lets you securely store, discover, and manage MCP servers, tools, and AI agents" ([registry][registry]) | n/a | **GA** 2026-06-18, v1 API + Terraform (feed) |
| **Agent Identity** / auth manager | SPIFFE identity per agent + a credentials vault and auth broker ([overview][aidoverview], [auth-manager][authmgr]) | n/a | **GA** 2026-08-22 ([IAM release notes][iamrn]); the Agent-Runtime integration page still shows Preview for sandboxes ([runtime-aid][runtimeaid]) — conflicting, flagged |
| Sessions / Memory Bank / Tasks / Sandboxes | §2, §8 | — | Sessions & Memory Bank GA endpoints 2026-06-17; the sandbox surface is **absent from the GA client** (§2.3) |

**Two API surfaces, and the distinction is load-bearing.** Agent Runtime is
`aiplatform.googleapis.com` / `reasoningEngines`, project-scoped and IAM-governed; the
Managed Agents / Interactions API is `generativelanguage.googleapis.com/v1beta/interactions`
on the Gemini Developer API. They share branding and little else — most sharply, the
Interactions API has a **webhook** and Agent Runtime does not (§2.4). A statement about one
is not a statement about the other; this survey's first pass conflated them.

**The phrase is also ambiguous across vendors.** Anthropic ships **Claude Managed Agents**
(beta, header `managed-agents-2026-04-01`): Ubuntu 24.04, up to 8 GB memory and 10 GB disk,
**Java OpenJDK 21 with Gradle and Maven preinstalled**, $0.08 per session-hour metered only
while running ([overview][clmg], [sandbox ref][clmgsb], [pricing][clmgprice]). On the JVM
question that product answers yes out of the box where Google's answers no. Recorded, not
evaluated — the question was about Google.

**Publishing into Gemini Enterprise is A2A, not deployment.** "you can add an A2A agent from
Agent Registry to a Gemini Enterprise app", and "You can only discover and import agents
from Agent Registry if the registry is associated with the Agent Gateway set up for your
Gemini Enterprise app" ([import][geimport]). A2A went to the Linux Foundation 2025-06-23
([announcement][a2alf]).

Adjacent, unasked, one line: Google now ships **CodeMender**, its own "AI code security
agent" that finds, verifies and fixes vulnerabilities with a human-in-the-loop workflow
(Public Preview 2026-07-21), which since 2026-08-12 "runs commands inside the process-level
sandbox by default" (feed). It is the closest thing Google ships to this project's worker —
and it is a CLI with a sandbox, not an Agent Runtime deployment.

## 1. The lens, with terminal state demoted

[The flow doc:196-201][flow] reduced "why a Job and not an agent platform" to four rows.
Three still gate. **Terminal state is demoted to a nice-to-have**, because the worker
already pushes its own branch and the PR is a good completion signal — §2.4 costs that
reframe honestly, including what it gives up.

| Criterion | Weight | Our Job | Agent Runtime | Managed Agents | Cloud Run job | GKE Agent Sandbox |
|---|---|---|---|---|---|---|
| **Per-run isolation** | **gate** | one pod per attempt ([ctx:125-129][ctx]) | ❌ a scaled deployment; `container_concurrency` default **9**, so nine alerts share one filesystem unless set to 1 | ✅ fresh fork per invocation | ✅ per execution | ✅ per pod |
| **Per-run resources** | **gate** | PodSpec | per **deployment**: `resource_limits` cpu ∈ {1,2,4,6,8}, mem 1–32 GiB, default 4/4Gi | **not published** | per execution, ≤8 vCPU / 32 GiB | PodSpec |
| **Documented isolation boundary** | **gate** | `runtimeClassName: gvisor`, "load-bearing rather than defence-in-depth" ([ctx:134-141][ctx]) | **no statement exists** (§9.1) | no statement exists | gen2 microVM, documented ([exec env][crexec]) | **gVisor** — literally ours ([GKE Sandbox][gkesandbox]) |
| **Concurrency control** | **gate** | scheduler is admission control | autoscaler, not a queue | 1 per environment; HTTP 400 on chaining | per-execution parallelism | scheduler + `SandboxWarmPool` |
| Terminal state to act on | nice-to-have | exit code + `/dev/termination-log` | `check_query_job` → `status`; `A2aTaskState` | interaction status enum **+ a webhook** | exit code | exit code |

The `container_concurrency: 9` default is the row that should worry a reader, because it is
[the kagent finding][kagentdoc] again: "one actor is spun from it per harness… chats are
multiplexed as ACP sessions inside the one actor", so "Two unrelated alerts would share one
micro-VM, one filesystem and one coursier cache" ([flow:210-213][flow]). Here the fix is a
knob — `container_concurrency: 1` gives one run per instance — which is genuinely better
than kagent's "shard by hand". But it is a knob you must know to set, and the default is
nine.

## 2. Per-run shape, the exec channel, and the completion signal

From protos, SDK source, and a local `google-cloud-aiplatform==2.0.1` install.

### 2.1 The deployment is a service, and `ContainerSpec` proves it

`ReasoningEngineSpec.ContainerSpec` is a **single-field message** ([proto][proto1]) — the
local read, verbatim:

```
ReasoningEngineSpec.ContainerSpec  →  ['image_uri']
ReasoningEngineSpec.DeploymentSpec →  ['env', 'secret_env', 'psc_interface_config',
                                       'min_instances', 'max_instances',
                                       'resource_limits', 'container_concurrency']
ReasoningEngineSpec.IdentityType   →  UNSPECIFIED=0, SERVICE_ACCOUNT=2, AGENT_IDENTITY=3
```

No `command`, no `args`, no `ports`, no `deployment_timeout`, no `health_route`. The image's
own `ENTRYPOINT` is the entire contract, and the contract is that it must "listen for HTTP
requests on `0.0.0.0` on port `8080`" and answer `POST /api/reasoning_engine` with
`{"class_method": "query", "input": {…}}` → `{"output": …}`, plus
`POST /api/stream_reasoning_engine` returning ndjson ([contract][contract]). **There is no
seam through which a batch command can be injected.** Cold start is ~4.7 s by default,
~1.4 s at `min_instances=10`, ~0.4 s warm ([optimize][optimize]); `create()` is "a few
minutes" for every path but a prebuilt image ([deploy][deploy]).

Two doc errors worth recording, both confirmed absent locally: **`buildOptions` and
`agentServerMode` do not exist** in the v1 proto or the 2.0.1 client. The real field is
`BuildSpec {worker_pool, service_account}` (on `master`, absent from 2.0.1), and
`service_account` sits on `ReasoningEngineSpec`, not `DeploymentSpec`.

### 2.2 The batch surface that does exist: `asyncQuery`

([reasoning_engine_execution_service.proto][proto2]; types present in the GA
`aiplatform_v1` package at 2.0.1)

```proto
rpc AsyncQueryReasoningEngine(AsyncQueryReasoningEngineRequest)
    returns (google.longrunning.Operation) { … }
message AsyncQueryReasoningEngineRequest  { string name=1; string input_gcs_uri=2;
                                            string output_gcs_uri=3; }
message AsyncQueryReasoningEngineResponse { string output_gcs_uri=1; }
message CancelAsyncQueryReasoningEngineRequest { string name=1; string operation_name=2; }
```

GCS in, GCS out, an Operation in between — a batch job with a result channel, wrapped in the
SDK as `run_query_job` / `check_query_job` / `cancel_query_job`, where `CheckQueryJobResult`
is `{operation_name, output_gcs_uri, status, result}`. So a status string and a cancel exist.
What does not: any push notification (§2.4), any documented max duration — **no timeout
field in the proto** — any per-run resource scope, and any statement of how an async query
reaches the container, since the runtime contract defines only the two `class_method`
endpoints.

### 2.3 `sandboxEnvironments` — capable, and still not a process

The most promising surface, and the one that disappoints most precisely. Locally, at 2.0.1:

```
vertexai._genai.sandboxes.Sandboxes → create, delete, execute_code, generate_access_token,
                                      generate_browser_ws_headers, get, list,
                                      send_command, snapshots, templates
```

**None of these is an exec.** `send_command(http_method, access_token, sandbox_environment,
port='8080', path, query_params, headers, request_dict) -> HttpResponse` says "Sends a
command to the sandbox" in its own docstring and is in fact an HTTP forwarder to a port
"specified during template creation". And:

```
ExecuteSandboxEnvironmentResponse → ['outputs']
Chunk                             → ['data', 'metadata', 'mime_type']
Metadata                          → ['attributes']    # a filename travels in here ad hoc
```

**No exit code. No stdout. No stderr. No timeout.** Verified locally and in the commit diff
that added the classes. With §2.1 this is the central shape finding: *every* code-execution
door on this platform is an HTTP request handled by a server you wrote, and none is a
process whose exit status the platform reports.

The rest is genuinely capable and worth knowing:

| | Local read at 2.0.1 |
|---|---|
| Environment kinds | `SandboxEnvironmentSpec → ['code_execution_environment', 'computer_use_environment']`. A `shell_environment` exists on `master` as an **empty marker message** — it selects a shell-capable template, it does not add an exec RPC |
| Code-execution config | `['code_language', 'machine_config']`. `Language` enum: **`LANGUAGE_PYTHON`, `LANGUAGE_JAVASCRIPT` — that is the whole enum.** `MachineConfig` enum: **`MACHINE_CONFIG_VCPU4_RAM4GIB` is the only value** — one shape, 4 vCPU / 4 GiB. Libraries are prebaked and "**You can't install your own libraries**" ([code-exec][codeexec]) |
| Custom container template | `SandboxEnvironmentTemplateCustomContainerEnvironment → ['custom_container_spec', 'ports', 'resources']`; `resources → ['limits','requests']`, K8s-style. Image must be "Linux-based (for example, Debian or Ubuntu)" and "**Must not require root privileges**" ([custom-containers][byoc], **Preview**) |
| Egress | `SandboxEnvironmentTemplateEgressControlConfig → ['internet_access']`. **One boolean.** Not a domain allowlist, not a proxy |
| Ingress | PSC-E service attachment; `SandboxEnvironmentConnectionInfo → ['load_balancer_hostname','load_balancer_ip','sandbox_internal_ip','routing_token']` |
| Lifecycle | `SandboxState → PROVISIONING, RUNNING, DEPROVISIONING, TERMINATED, DELETED` (+`PAUSED` on master). TTL at create (`ttl: "3600s"`) → `expire_time` |
| Suspend / snapshot | `pause` "Halts a running sandbox, releasing compute while preserving disk state", enterprise mode only. `SandboxEnvironmentSnapshot → ['ttl','size_bytes','source_sandbox_environment','parent_snapshot','post_snapshot_action' ∈ {RUNNING, PAUSE}]` |
| Disk | **NOT FOUND** anywhere. Snapshots are the persistence story instead |
| Maturity signal | the whole sandbox surface lives in **`vertexai._genai`** — an underscore-prefixed private module — and is **absent from both `aiplatform_v1` and `aiplatform_v1beta1`** generated clients |

Note what pause/snapshot is. [flow:242-248][flow] observes that "Substrate spends a whole
checkpoint/restore engine — golden snapshots, `DurableDir`, suspend-on-body-close — to make
an idle conversation cheap", and that not holding the conversation in a process gets the
property for free. Google now sells that engine. The argument is unchanged; it is now an
argument against a paid feature.

### 2.4 The completion signal, ranked — and what the reframe costs

Terminal state is not a gate. Ranked cheapest-first:

| | Mechanism | Needs from the platform | Verified status |
|---|---|---|---|
| **(a)** | **The agent opens its own PR; the orchestrator learns from the GitHub webhook it already receives** ([ADR 0004][adr4]) | **nothing** | works today, on any runtime |
| **(b1)** | **Managed Agents / Interactions API webhook.** `webhook_config` — "Optional. Webhook configuration for receiving notifications when the interaction completes" — with `uris` and `user_metadata`; events `interaction.completed`, `interaction.failed`, `interaction.requires_action`; HMAC `signing_secrets` with rotation ([webhooks][gwebhooks]). Backed by a real status enum: `queued, in_progress, requires_action, completed, failed, cancelled, incomplete, budget_exceeded` plus an `errors[]` of `{code, message}` ([interactions][interactions]) — note `requires_action` is `input-required` and `budget_exceeded` is `--max-budget-usd` as a first-class state | a Pre-GA/Beta product with no JVM | **exists, Beta** |
| **(b2)** | Agent Runtime completion event | — | **does not exist.** No `notificationConfig` / `pubsubTopic` field in `reasoning_engine_service.proto` or `reasoning_engine_execution_service.proto`; `aiplatform.googleapis.com` is **not** in Eventarc's Google-event-types list ([Eventarc][eventarc]); audit-log Eventarc triggers fire on the *initiating* call, not on `Operation.done` |
| **(b3)** | Cloud Logging sink → Pub/Sub, filtered on `resource.type="aiplatform.googleapis.com/ReasoningEngine"` ([logging][relog], [sinks][logsink]) | your agent must print a matchable "done" line | works, generically. Push-on-log, not push-on-completion |
| (c) | Poll a status API | `check_query_job` → `status`; `A2aTask.state`; interaction status | exists on all three; `operations.get`/`wait` only for the LRO |

**(a) is the right answer and it is free.** It also inverts the argument for a platform: if
completion needs nothing from the runtime, the runtime is left competing on isolation,
resources, cost and observability — and it loses on isolation (§9.1), ties on resources,
loses trivially on cost (§6), and wins only on observability (§8). Note the irony in (b1):
the *only* Google agent product with a real completion webhook is the one that cannot run a
JVM.

**The cost of (a), stated plainly.** The orchestrator gives up creating the PR, and with it
the failure branch [flow:99-105][flow] describes: *"failed → comment the failure, label
`needs-human`, no PR unless commits exist on the branch"*. Three consequences:

1. **PR attribution and body move into the sandbox.** [`architecture.md:185-199`][arch]
   assigns commit messages and the PR body — "overall status, branch, commit count,
   **summed** cost, elapsed, `pr_title`" — deliberately. Summed-across-phases cost the
   sandbox can compute; `Closes #N` it already handles.
2. **A failed-but-partial run either opens a bad PR or opens none.** "No PR unless commits
   exist" becomes a decision made *inside* the thing that just failed.
3. **A silent failure produces nothing at all** — no PR, no event, no comment. This is the
   real gap, and the answer is a **timeout on the orchestrator side**: record a deadline
   when the run is launched, and a sweep posts `needs-human` on anything labelled
   in-progress past it. A few lines and one timer, needing nothing from the platform, and
   strictly required the moment the informer goes away. Say it out loud: **the orchestrator
   stops being purely event-driven and acquires a clock**, and it puts a little state back
   — a deadline per open run — though that fits in the issue body like everything else
   ([flow:250-258][flow]).

## 3. Q6 and Q5a — the JVM and the BYO container, settled from docs

Both are load-bearing and both are answerable from documentation.

**(1) Managed Agents API: cannot run a JVM. Docs-conclusive.** The preinstalled list is
exhaustive and short — "Python 3.11, Node.js 20 (with TypeScript), gcloud CLI, git, curl,
jq, and standard UNIX utilities", with numpy / pandas / requests / beautifulsoup4 /
google-genai / pyyaml and `create-next-app` / `create-vite` / `typescript`
([sandbox-env][mgsandbox]). **No JDK, no Gradle, no Maven, no sbt, no Scala.** No BYO image
is documented for this sandbox; whether `apt install` works at runtime is not documented
either way; no vCPU, memory or disk figure is published anywhere. A Scala build would mean
the agent downloading a JDK and sbt over a domain-allowlisted egress path into a sandbox of
unpublished size, on every fork. Not a supported shape.

**(2) Agent Runtime: can run a JVM, and Google says so in one sentence** ([runtime][runtime]):

> "Python: Deploy agents to Agent Runtime using the `adk` CLI. Go: Deploy agents to Agent
> Runtime using the `adkgo` CLI… **Java: Deploy agents to Agent Runtime using a custom
> container.** TypeScript: Deploy agents to Agent Runtime using a custom container."

**(3) BYO container from Artifact Registry: yes, first-class, three ways.** `deploy-an-agent`
lists five deploy methods — from an agent object, from source files (≤8 MB tar), **from a
Dockerfile**, **from a prebuilt container image in Artifact Registry**, and from a Developer
Connect git repo ([deploy][deploy]) — of which only the first two are Python-shaped. The
prebuilt path is `ContainerSpec.image_uri`, "the Artifact Registry Docker image URI", e.g.
`us-central1-docker.pkg.dev/my-project/my-repo/my-image:tag` ([proto][proto1],
[Python types][pytypes]), needing `google-cloud-aiplatform>=1.144`. The Dockerfile path is
`SourceCodeSpec`, read locally:

```
ReasoningEngineSpec.SourceCodeSpec → ['inline_source', 'developer_connect_source',
                                      'python_spec', 'image_spec']
  .ImageSpec  → ['build_args']
  .PythonSpec → ['version', 'entrypoint_module', 'entrypoint_object', 'requirements_file']
```

`image_spec` and `python_spec` are alternatives — a source archive plus a Dockerfile plus
build args is a supported deploy, and nothing about it is Python.
(`PackageSpec.python_version` is 3.9–3.13, default 3.10, for the pickle path only.)

**(4) So the existing sandbox image is deployable, with one tax.** It needs an HTTP server
on :8080 answering the two `class_method` endpoints, wrapping the phase script. The smallest
change in this document — and also where the [ADR 0001][adr1] image contract gains a term it
does not have, which is the honest cost: today the contract says "an agent CLI on `PATH`",
not "serves HTTP".

**(5) What the docs do not say, and it is what a JVM build cares about most: disk.** Not
published for Agent Runtime, the custom container sandbox, or the Managed Agents sandbox.
A cold `~/.cache/coursier` or Gradle cache for a real Scala project is gigabytes. Nor is
filesystem writability stated for an Agent Runtime container. §11 ranks how to find out.

**(6) The other sandbox is out.** Code Execution's `Language` enum is `PYTHON` and
`JAVASCRIPT` only, its `MachineConfig` enum has exactly one value at 4 vCPU / 4 GiB (§2.3),
and "You can't install your own libraries."

| | vCPU | Memory | Disk | Wall clock |
|---|---|---|---|---|
| Agent Runtime | 1, 2, 4, 6, 8 (default 4) | 1–32 GiB (default 4) | **not published** | not published; the only figure found is a SNIPPET-ONLY 10-minute front-end timeout |
| Custom container sandbox (Preview) | template `resources` | template `resources` | **not published** | TTL only |
| Cloud Run job | ≤8 | ≤32 GiB | configurable | default 10 min, **max 168 h** ([timeout][crtimeout]) |
| Our Job | PodSpec | PodSpec | PodSpec | `activeDeadlineSeconds: 3600` ([arch:426][arch]) |

**One repo-side note.** [ADR 0003:159-161][adr3] normalises to `go, node, python, java,
rust`; there is no `scala` and no `jvm` key, and no `build.sbt` in the manifest table
(`:37-42`). Per `:100-125` a non-detection refuses and comments rather than guessing, so
today an sbt repository is refused before any of this matters. **The gradle/sbt toolchain
image and its detection entry are unbuilt regardless of hosting**, and a hosting decision
must not smuggle that work in.

## 4. Q1 + Q2 — credentials, and the surprise

The baseline in three lines: `GH_TOKEN` is the literal sentinel `proxy-injected…` and the
proxy substitutes a real installation token in flight ([ADR 0005:88-92][adr5]); the token is
minted per run and scoped to **the repository the run's annotations name**, never the one the
request URL names ([ctx:62-67][ctx]); the App's private key is a KMS key-version *reference*,
so there is no key holder ([ctx:76-81][ctx]). ADR 0005 rejected env-var-resident credentials
and Workload Identity in the sandbox, the latter because it is "**key**-termination, not
**credential**-termination" (`:443-449`).

### 4.1 The native answer is available, and it is the rejected baseline

Secret Manager integration is real, not plaintext env vars. Verified locally:

```
SecretEnvVar → ['name', 'secret_ref']     SecretRef → ['secret', 'version']
```

`DeploymentSpec.secret_env` is a repeated `SecretEnvVar` pointing at a Secret Manager secret
and version ([DeploymentSpec][deployspec], [env_var.proto][proto3]), resolved against
`roles/secretmanager.secretAccessor` on the runtime identity. **Changing `env` or
`secret_env` cuts a new agent revision** — rotation is a redeploy, not a re-read. (Cloud Run
is better: a secret mounted as a *volume* "always fetches the secret value from the Secret
Manager to use the value with the latest version", while an env var resolves once before
start and a failed retrieval means "the instance doesn't start" — [run/secrets][crsecrets].)
Identity is fine too: default is the Reasoning Engine Service Agent
`service-<PN>@gcp-sa-aiplatform-re.iam.gserviceaccount.com` ([access][reaccess]), a custom
`service_account` is supported, and `identity_type` selects `SERVICE_ACCOUNT` or
`AGENT_IDENTITY` — so `roles/artifactregistry.reader` is one binding away.

**And that is exactly what [ADR 0005][adr5] refuses.** A secret resolved into the agent's
environment is a live credential in the same process as untrusted repository content; a
runtime service account reachable via ADC is a mintable GCP identity in the same place. The
two published incidents ADR 0005 opens with (`:15-28`) attack precisely that. Secret Manager
improves the credential's *storage* and leaves its *blast radius* identical.

### 4.2 The surprise: Google shipped our proxy

Direct quotes ([auth-manager][authmgr], [overview][aidoverview]):

> "Agent Identity auth manager is a centralized credentials vault and authentication broker
> that simplifies outbound tool authentication for your agents. It lets agents authenticate
> using an API key or OAuth client ID and secret, or on behalf of a user through OAuth
> delegation using end-user access tokens."

> "When Agent Identity is used with Agent Gateway and Gemini Enterprise, end-user
> credentials … are encrypted by the auth manager and decrypted at the gateway, **ensuring
> that the agent can never access the raw credential**."

Agent Gateway is the enforcement point: "Agent Platform adopts a default-deny policy for all
outgoing traffic", "Agent Gateway blocks all outbound traffic to hosts not registered in
Agent Registry", it "Automatically handles mTLS handshakes and termination", and for content
it "intercepts the egress traffic and calls Model Armor" ([gateway][gateway]).

Agent Identity is *stronger* than ours on one axis: "Access tokens generated for Google Cloud
are cryptographically bound to the agent's unique X.509 certificates to prevent token theft",
and "Unlike service accounts, agent identities are not shared by multiple workloads by
default, can't be impersonated, and don't allow developers to generate long-lived service
account keys" ([overview][aidoverview]). A stolen token is unusable off-host. We have no
equivalent; a leaked installation token works from anywhere for its lifetime.

**What it does not do.** Nothing mentions a **GitHub App private key, JWT signing, a KMS
signer, or arbitrary `Authorization` injection into a third-party request**. The credential
types are API keys, OAuth client id/secret, and delegated end-user OAuth tokens — the
Gemini-Enterprise-connector shape (a human's Jira token), not
mint-an-installation-token-scoped-to-one-repo ([ctx:62-67][ctx]). And the "never access the
raw credential" sentence is scoped in its own wording to *end-user credentials*, *with Agent
Gateway*, *and Gemini Enterprise*.

### 4.3 The Managed Agents API has the sentinel swap, literally

([custom-agents][mgcustom], [create-manage][mgcreate])

```json
"network": { "allowlist": [ { "domain": "api.github.com",
    "transform": { "Authorization": "Basic YOUR_BASE64_TOKEN" } } ] }
```

with "network access in the environment is turned off. You must specify an `allowlist` to
enable access", the sandbox containing "no ambient credentials", and MCP headers "transmitted
only when accessing the specific MCP endpoints" ([sandbox-env][mgsandbox]). That is the
intercept list plus the credential switch, hosted, and the closest match in the survey. Four
things spoil it: the token is **static and pasted**, rotated by you overriding the config "to
refresh expired tokens or rotate API keys", so [ctx:62-67][ctx]'s mint-per-run has no
counterpart; granularity is a **domain string**, which cannot express
[ADR 0005:311-323][adr5]'s deliberately-wider-certificate trick that keeps `-docker.pkg.dev`
intercepted-and-tokenless *structurally*; the isolation claim is asserted without saying
**where** the transform is applied relative to the sandbox boundary; and it is Pre-GA, no
production use.

### 4.4 You can keep our proxy, and Google publishes the recipe

Agent Runtime egress can be forced through a customer-run forward proxy — in a VPC-SC
perimeter it is mandatory: "Vertex AI Agent Engine doesn't provide internet egress. Instead,
you're required to deploy a proxy VM with an RFC 1918 address for internet egress" plus Cloud
NAT ([vpcsc][vpcsc]; **UNVERIFIED-EXACT-WORDING**, page renders client-side). The mechanism
is a **Private Service Connect interface**, projecting a NIC from the Google-managed tenant
project into the customer VPC via a Network Attachment on a `/28`+ subnet ([PSC-I][pscif]) —
and `psc_interface_config` is right there on `DeploymentSpec` (verified locally). Google's own
codelab wires it through a Secure Web Proxy ([codelab][swp]):

> `PROXY_SERVER = 'http://swp.demo.com:8888'` — "Agent Engine traffic must be explicitly
> configured to use the SWP's internal IP address or Fully Qualified Domain Name and port as
> their forwarding proxy", with firewall rules that "allow traffic from the Network Attachment
> Subnet to the SWP while denying everything else."

So [ADR 0005][adr5]'s shape survives a move to Agent Runtime. What does **not** survive is
the second factor: the proxy resolves run identity from the connection's **source pod IP** to
a Pod's annotations ([ctx:112-116][ctx]), and PSC-I gives one Network Attachment subnet for
the whole deployment. Per-run identity would fall back to the run secret alone
([ctx:96-105][ctx]) — HMAC-authenticated, so probably fine, but it deletes a layer ADR 0005
calls a deployment precondition rather than a knob.

### 4.5 Logs are the other exfiltration path, and they are on by default

> "By default (without any additional set up), logs written to stdout and stderr will be
> routed to the log IDs `reasoning_engine_stdout` and `reasoning_engine_stderr`
> respectively." ([logging][relog])

No documented redaction. Traces are safer by default — base telemetry "enables the agent
traces, logs, and metrics … but doesn't include prompts and response data" until you set
`OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT`, which Google's own doc guards with
"Ensure you have any necessary end user consents, notices, and data handling policies in
place" ([tracing][retrace]). Compare [`architecture.md:352-354`][arch]: "Redaction is not
done in MVP. The transcript is readable only by someone with `pods/log` on our namespace,
which is already a trusted position." Hosted, the audience for an accidental `print(token)`
becomes everyone holding `roles/logging.viewer` on the project. Also: "Cloud Logging is not
supported for child resources of Agent Runtime" (Sessions, Memory Bank, Code Execution,
Example Store) — which matters for §8.

### 4.6 Scorecard, on the eight non-negotiables

Same rows as [kagent §5.5][kagentdoc], so the surveys compare.

| Non-negotiable | Agent Runtime + BYO container | Managed Agents API |
|---|---|---|
| Zero credentials in the sandbox ([0005][adr5]) | **Passes only if the proxy stays** (§4.4). Native answer puts it in-process | **Partially** — no ambient credential; transform boundary asserted, not described |
| Mint for the annotation's repo, never the URL's ([0005][adr5]) | preserved with the proxy; the source-IP factor is lost | **No counterpart** — one pasted static token per domain |
| Zero key holders (KMS-signed App JWT) ([0005][adr5]) | preserved (the proxy still signs) | **Fails** — no signer, no JWT, no KMS |
| gVisor load-bearing ([0001][adr1]) | **no isolation statement exists** (§9.1) | no isolation statement exists |
| Open BYO image contract ([0001][adr1]) | **Converges, with a tax** — BYO is first-class, but the image must serve HTTP on :8080 | **Fails** — no BYO image |
| No UID constant in the control plane ([0001][adr1]) | converges by omission | converges by omission; root forbidden |
| Proxy not a sidecar; reporter not in the pod ([0005][adr5], [0004][adr4]) | proxy external via PSC-I ✅; **no informer** — §2.4 | no informer, but a webhook (§2.4 b1) |
| `sender.type == "User"`, drop silently ([0004][adr4]) | N/A — our webhook front-end is unchanged | N/A |

## 5. Q5b — Maven and Gradle artifacts from GAR

From [GAR's Java auth page][garjava]:

| Path | Auth |
|---|---|
| `com.google.cloud.artifactregistry:artifactregistry-gradle-plugin:2.2.5`, repo url `artifactregistry://LOCATION-maven.pkg.dev/PROJECT/REPO` | **ADC**: `GOOGLE_APPLICATION_CREDENTIALS` → metadata server → `gcloud auth application-default login` cache |
| `artifactregistry-maven-wagon:2.2.5` | same ADC chain |
| No plugin: `<httpHeaders><property><name>Authorization</name><value>Bearer ${token}</value>`, `TOKEN=$(gcloud auth print-access-token)` | a plain bearer token over HTTPS to `LOCATION-maven.pkg.dev` |
| username `_json_key_base64` + base64 SA key | basic auth, bypassing ADC |

Read is `roles/artifactregistry.reader` ([access-control][garacl]); remote/virtual
repositories with a `MAVEN-CENTRAL` upstream preset are GA since 2023-11-01
([remote][garremote]).

Row 3 is load-bearing: because GAR Maven is a plain `Authorization: Bearer` over HTTPS to a
`*-maven.pkg.dev` host, **the credential proxy's per-host injection covers it unchanged** —
the same mechanism already proven for `{region}-python.pkg.dev` and `{region}-go.pkg.dev`
([ADR 0005:295-306][adr5]), where a real private wheel installed into a pod holding no GCP
credential at all. Adding `-maven.pkg.dev` to `credFor` is a table entry. (Google documents
the wire protocol, not the proxy-injects-it pattern; the pattern follows. Marked as
inference.)

Row 1 is the temptation: grant the hosted runtime `artifactregistry.reader` and let ADC do
it. That re-introduces exactly what ADR 0005 rejected — a mintable GCP credential reachable
from inside the sandbox via the metadata server. [flow:291][flow] already flags internal
dependency resolution as unbuilt; this is the same problem with a Google-shaped shortcut.

One pleasant side effect of hosting: a hosted runtime pulls its own image with its own
identity, so [ADR 0005:311-323][adr5]'s deliberate exclusion of `-docker.pkg.dev` from the
intercept list — "its challenge/response dance is unmeasured and blob fetches redirect" —
stops being our problem.

## 6. Q4 — cost. Compute is noise; say so with numbers.

Published SKUs, us-central1, fetched 2026-08-28. (`cloud.google.com` pricing pages exceed
10 MB and defeat a plain fetcher; these came via a reader proxy, cross-checked on repeats.)

| SKU | Price |
|---|---|
| **Agent Compute** (Agent Runtime + Sandbox, one SKU) | **$0.085 / vCPU-hour** ([vertex pricing][vxprice]) |
| **Agent Memory** | **$0.009 / GiB-hour** |
| Agent Storage | $0.000410959 / GiB-hour (≈$0.30/GiB-month) |
| Free tier | 50 vCPU-h + 100 GiB-h / month |
| Sessions | $0.30/GiB-month + ops. Billing-start date is **contested** in the sources seen (2026-09-01 vs an earlier date) — treat as "starts imminently", verify before budgeting |
| Memory Bank | storage $0.30/GiB-mo; 1 vCPU-h per 3M reads, per 1M writes; "model tokens used for memory generation and embeddings are billed separately under their respective model SKUs" |
| Session / Memory events | ~$0.25 per 1,000 events — **SNIPPET-ONLY**, not confirmed from the rendered page |
| Example Store | **NOT PUBLISHED** |
| Code Execution / Sandbox | **no separate SKU** — folded into Agent Compute/Memory |
| Cloud Trace | ~2.5M spans/month free, then ~$0.20/million spans; 30-day retention — **SNIPPET-ONLY**, the pricing page renders client-side |
| Cloud Run instance-based **and jobs** | $0.000018 / vCPU-s, $0.000002 / GiB-s; free 240k vCPU-s + 450k GiB-s ([run pricing][crprice]) |
| GKE Autopilot general-purpose | $0.0445 / vCPU-h, $0.0049225 / GiB-h; Spot $0.0133 / $0.0014767 ([gke pricing][gkeprice]) |
| GKE cluster management | $0.10 / cluster-hour, all clusters; $74.40/mo credit ≈ one free cluster |
| Claude Opus 5 / Sonnet 5 on Vertex | $5.00 / $25.00 · $2.00 / $10.00 per 1M tokens ([genai pricing][genaiprice]) |
| Gemini 3.1 Pro Preview / 3.7 Flash | $2.00 / $12.00 (≤200K) · $0.75 / $3.75 |

One run = 15 min at 2 vCPU / 8 GiB (0.25 h = 900 s):

| | Arithmetic | Per run | × 300/mo gross | net of free tier |
|---|---|---|---|---|
| Agent Runtime | `2×0.25×0.085 + 8×0.25×0.009` | **$0.0605** | $18.15 | **$13.00** |
| Cloud Run job | `2×900×0.000018 + 8×900×0.000002` | **$0.0468** | $14.04 | **$8.82** |
| GKE Autopilot pod | `2×0.25×0.0445 + 8×0.25×0.0049225` | **$0.0321** | $9.63 | **$9.63** + cluster fee |

**And the number that ends the conversation.** [`architecture.md:436-439`][arch] measured one
agent phase at 162 s / $1.01 on host runc and 202–206 s / $0.88–0.95 under gVisor, projecting
a trivial three-phase run to **~600 s and ~$2.70** — with `--max-budget-usd 10` *per phase*
and a stated worst case "~$30/run" (`:428`).

| | Per run | Share of a $2.70 run |
|---|---|---|
| Model tokens (measured, trivial issue) | ~$2.70 | **98 %** |
| Hosted compute, most expensive option | $0.0605 | **2.2 %** |
| gVisor's ~25 % wall-clock tax | $0.00 in dollars (`arch:439`) | 0 % |

Compute is 2 % at the cheap end of the token bill and 0.2 % at the worst case. The monthly
delta between the most and least expensive option at 300 runs/month is **$4.18** — less than
two runs' tokens. **Cost cannot decide this in either direction.** One note: Agent Compute's
"idle time between turns is not billed" beats an instance-based Cloud Run *service* and ties
a Cloud Run *job*, which does not idle.

## 7. Q7 — what the platform gives, and what this design already has

| Feature | Status | Does this design want it? |
|---|---|---|
| **Sessions / Tasks / Memory Bank** as a **debug tap** | Sessions & Memory Bank GA endpoints 2026-06-17; the Tasks surface lives in the private `_genai` module | **See §8.** Not as a state store — the issue remains that ([flow:250-258][flow]). As an opt-in observation layer it is the most interesting thing here |
| **Tracing → Cloud Trace** | "Default-On Tracing" GA 2026-06-18 (feed) | **Partly, and narrower than it looks.** [`arch:329-335`][arch] wants "a span per run, a span per phase" in OTel, portable to whatever backend the org runs. Agent Runtime exports to **Cloud Trace only** — no OTLP path found — so this is a backend decision dressed as a feature |
| **Agent Registry / Gallery / A2A publish** | Registry GA 2026-06-18 | No. [flow:228-241][flow] rejects A2A on properties, not availability |
| **IAM per agent (Agent Identity)** | GA 2026-08-22 | **The X.509-bound token is a property we lack** (§4.2) |
| **Agent Gateway + Model Armor** | GA 2026-06-18 / 06-24 | **Yes — the one thing worth buying.** Default-deny egress, mTLS, and prompt-injection inspection we have nothing for. It addresses something [ADR 0005][adr5] leaves *open* rather than something it closed |
| **Evaluation** (agent + model evals) | **GA 2026-07-31** ([blog][blog0731]); `trajectory_exact_match` / `_recall` / `_precision`; "standard rates for the model calls behind LLM-as-a-judge… Code-based and computation metrics add no additional cost" | **Yes, eventually.** [ADR 0001][adr1] pins skills precisely so evaluation *becomes possible*; this is the first genuinely new capability |
| **Example Store** | Preview, us-central1 only, no published price | No |
| Skills format | Managed Agents mounts `.agents/skills/<name>/SKILL.md` with YAML frontmatter, pointing at `agentskills.io` ([custom-agents][mgcustom]) | **Already ours** — same format as [ADR 0001][adr1]'s baked skills. Skills are the portable asset; harnesses are not |

## 8. Q8 + Sessions — watching it work, as a debug tap

Re-scoped: Sessions is not being considered as state. It is being considered as an
**optional, per-run, opt-in way to see what the agent is doing** — and judged on that.

### 8.1 What the platform offers

| | Detail | Live? |
|---|---|---|
| `streamQuery` | chunks arrive `{'actions':[{'tool': …}]}` → `{'steps': …}` → `{'output': …}` — **tool calls visible mid-run** ([streamQuery][streamq]) | **yes**, to whoever holds the connection |
| Bidi streaming | "only available on `EXPERIMENTAL` server mode", 10-minute max ([bidi][bidi]) | yes, **Preview**, and 10 min is thin against a 600 s run |
| **`A2aTask` + `TaskEvent`** | §8.2 | **poll-only** |
| `sessions.events.list` | `SessionEvent → ['content','actions','author','error_code','error_message','event_metadata','invocation_id','name','timestamp','raw_event']` — rich per-turn records including errors | **poll-only.** `ListEvents` in `session_service.proto` is a plain unary GET; no server-streaming RPC and no watch method exists |
| Console, per deployed agent | **Playground**, **Trace** ("Traces of your conversations with the agent", gated on having enabled OTel), **Event** ("A graph of invoked APIs and event details during your conversations"), **State**, plus a Sessions tab ([use-an-agent][useagent]) | refresh over list endpoints; no "live" claim in the docs |
| Cloud Trace | spans are exported at span end / flush, so an in-flight span is **not** visible — post-hoc by construction | no |
| Cloud Logging | `reasoning_engine_stdout` / `_stderr` / `_build` — **but "Cloud Logging is not supported for child resources of Agent Runtime"** ([logging][relog]) | tail-able |
| `adk web` | `--session_service_uri agentengine://<id>` attaches the **session store**, not the runtime; "not meant for use in production deployments" ([ADK CLI][adkcli]) | no |
| **ADK `Runner.run_async()`** | returns an **async generator yielding `Event` objects live** — tool-call requests, tool results, partial text, state/artifact updates, errors. `google.adk.events.Event` carries `invocation_id`, `author`, `actions`, `content`, `partial`, `turn_complete`, `is_final_response()` ([event.py][adkevent]) | **yes — the only genuinely live per-run tap** |
| **ADK `BasePlugin.on_event_callback`** | a plugin hook invoked per event by `PluginManager`, so events can be fanned out to **any sink** — e.g. posting each to Slack ([base_plugin.py][adkplugin]). Caveat: it fires *after* `append_event()` persists, so it cannot redact before persistence ([adk#3990][adk3990]) | **yes** |

### 8.2 The finding: Google shipped the flow doc's §5 table

The Agent Engine SDK has a first-class durable task resource. Read locally at 2.0.1:

```
A2aTask      → ['context_id','create_time','metadata','name','next_event_sequence_number',
                'output','state','status_details','update_time','expire_time','ttl']
A2aTaskState → SUBMITTED, WORKING, COMPLETED, CANCELLED, FAILED, REJECTED,
               INPUT_REQUIRED, AUTH_REQUIRED, PAUSED
TaskEvent    → ['create_time','event_data','event_sequence_number']
TaskEventData→ ['metadata_change','output_change','state_change','status_details_change']
TaskOutput   → ['artifacts'] ;  TaskArtifact → ['artifact_id','description','display_name',
                                                'metadata','parts']
clients: agent_engines.a2a_tasks → create, delete, events, get, list
         a2a_task_events         → append, list        # no subscribe
```

Set beside [flow:232-241][flow]:

| A2A concept | Issue equivalent (ours) | Agent Engine, as shipped |
|---|---|---|
| `contextId` | issue number | `A2aTask.context_id` |
| message | comment | `TaskEvent` (sequence-numbered) |
| task state | label | `A2aTaskState` |
| `input-required` | `needs-human` label; the run **exits** | `INPUT_REQUIRED` — and the task *persists* |
| artifact | the pull request | `TaskArtifact` |
| cost while idle | zero — no process held | `ttl` / `expire_time`, `PAUSED` |
| replay / audit | the thread, readable and editable by a human | `a2a_task_events.list` |

The flow doc predicted this table and argued the issue wins it. That argument holds
unchanged on the last row: **`a2a_task_events.list` is an API read; a GitHub thread is a
place an on-call engineer already is and can reply in.** But the middle rows are now a
product, and the honest reading is that Google agrees with the model and disagrees about the
substrate.

### 8.3 Judged as a debug tap, on the four questions that matter

| Question | Answer |
|---|---|
| **Live, or only after?** | **Only after, on the managed surfaces.** `ListEvents` is unary with no streaming RPC; `a2a_task_events` has `append`/`list` and no subscribe; Cloud Trace spans exist only once exported at span end. `A2aTask.next_event_sequence_number` makes incremental polling cheap and correct, so a live-*ish* view is a poll loop you write. The **only genuinely live per-run tap is the ADK event generator**, and that runs in *your* process — meaning the live view is available from a Job exactly as well as from Agent Runtime |
| **Per-run, or all-on?** | **Not expressible per-run for tracing.** Telemetry is a **deploy-time** env var (`GOOGLE_CLOUD_AGENT_ENGINE_ENABLE_TELEMETRY`, plus `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT` for content) or `AdkApp(enable_tracing=True)`, and `env` is a versioned field: changing it cuts a new agent revision (§4.1). "Debug mode for this one alert" means a redeploy. Tasks and Sessions *are* per-invocation resources you create or don't — that half is per-run |
| **Cost when nobody looks** | **Yes, it bills.** Storage- and ingestion-priced: Agent Storage at $0.30/GiB-month for stored sessions and memories including revisions, and Cloud Trace per span ingested. Default session TTL is **365 days** if neither `expire_time` nor `ttl` is set, so a debug tap left on accrues indefinitely. The fix is a short `ttl` per task/session, which the API supports. Viewing is not separately billed |
| **Usable at 3am by someone who did not write the agent** | **The weak row.** Cloud Console surfaces, gated on project IAM — `roles/aiplatform.sessionViewer` / `sessionUser` / `sessionEditor` exist (GA) — in a console an SRE may not have open, with prompts and responses stored outside the spans and stitched back in the UI. **No non-console or shareable-link view was found.** Compare what the design already has: an issue thread and a Slack thread the on-call is already reading |

### 8.4 The cheap alternative, weighed properly

[`architecture.md:314-327`][arch] is the prior: "No live view", scion built a full PTY bridge
whose shareable-URL mechanism is an unimplemented stub, and Kelos built a working live view
without a PTY at "roughly 4,500 lines across three components plus an auth story."

Streaming the agent's turns into the **Slack thread** ([flow:72-74][flow]) and checkpoints
into the **issue** ([flow:250-258][flow]) costs a `chat.postMessage` per checkpoint and
inherits threading, search, mobile and permissions for free. It needs no platform, works
from a Job, and puts the transcript where the human is. It is strictly less detailed than a
span DAG — no token counts, no per-tool latency, no replay.

**Conclusion, and it is the sharpest thing in this section.** The two are not exclusive —
Slack/issue streaming for the on-call human, spans for whoever is debugging the agent — but
**neither one requires the hosted runtime.** The live tap is the ADK/harness event stream in
your own process; the spans are OTel, and [`arch:329-335`][arch] notes Claude Code ships
built-in OTel "so it is configuration rather than instrumentation". What Agent Runtime adds
is a console you did not have to build, whose enable switch is a redeploy, whose backend is
Cloud Trace only, and whose audience needs a GCP role. That is a real convenience and a poor
reason to move a workload.

## 9. Three things not asked

### 9.1 Untrusted-code posture — the gap is that there is no statement

The worker runs a model over repository content and then executes it. Our answer is
`runtimeClassName: gvisor`, uid 1000, `RuntimeDefault`, `readOnlyRootFilesystem`, all
capabilities dropped, with gVisor "**load-bearing rather than defence-in-depth**"
([ctx:134-141][ctx]).

Google's answer, for the container that hosts your agent code:

- **Agent Runtime container: no documented isolation boundary.** No gVisor claim, no microVM
  claim, no per-tenant statement. Searched; not found.
- **Code Execution:** "a secure, isolated, and managed sandbox environment" with "limited
  file system and no network access" ([code-exec][codeexec]) — adjectives, no mechanism.
- **Managed Agents:** "standard, isolated Linux sandboxes" ([mgagents][mgagents]). Same.
- **Cloud Run jobs:** documented — "Cloud Run jobs always use the second generation execution
  environment", i.e. microVM, gen1 being gVisor ([exec env][crexec]).
- **GKE Agent Sandbox:** documented, and it is literally ours — `runtimeClassName: gvisor` on
  Standard, optional Kata, plus `SandboxWarmPool` / `SandboxClaim`, i.e. the productisation of
  `kubernetes-sigs/agent-sandbox`. "Agent Sandbox is offered at no extra charge in GKE"
  ([install][gkeagentsb]; GA claimed in a Google Cloud **blog** for May 2026 while the how-to
  still uses `gcloud beta` — contradictory, flagged).

So the trade is **a less-documented isolation boundary that also holds your GitHub credential
natively** (§4.1), in exchange for a console. Note the direction of travel: [ADR 0002][adr2]
rejected `agent-sandbox` the CRD on lifecycle grounds while keeping gVisor, and Google has
since productised that same CRD set — so the *hosted* option best aligned with our posture is
GKE Agent Sandbox, not the agent platform.

### 9.2 Residency and tenancy

Repository source entering a Google-managed runtime is a new question the Job model does not
raise, because today the code never leaves the cluster.

- **Training:** "Google won't use your data to train or fine-tune any AI/ML models without
  your prior permission or instruction" ([data-governance][datagov]). Same page lists the
  exceptions needing action for true zero retention: a 24-hour cache (disableable),
  abuse-monitoring prompt logging (exception request), Grounding with Google Search retaining
  30 days.
- **CMEK:** supported via `encryption_spec` on create — verified locally on the
  `ReasoningEngine` type — but "You can't use CMEK if your Memory Bank or Sessions is
  configured to use the global endpoint."
- **Residency for `reasoningEngines`:** **unresolved.** The security-controls matrix page
  would not render; one indexed snippet says residency is unsupported for some Agent Platform
  surface, contradicting the CMEK finding.
- **EU Data Boundary:** the table lists "Generative AI on Vertex AI"
  (`aiplatform.googleapis.com`, restrictions "None") and has **no Agent Engine row**
  ([EU boundary][eudb]). Inheritance is plausible — **an inference, not a fact.**

### 9.3 Lock-in, and the fact that closes it

[`architecture.md:545-551`][arch] rules out "Building our own agent harness (GenKit Go,
**Google ADK**, bespoke)" **permanently**, on measurements: ADK's `KnownFields(true)` makes
`disable-model-invocation` a fatal parse error, "failing 24 of 41 skills including
`implement`"; both candidate loaders "scan immediate subdirectories only, so the two-level
layout yields zero skills silently"; cost "3–6 months to production, then a permanent
spec-drift treadmill."

| Path | What you write | What you can leave with |
|---|---|---|
| BYO container on Agent Runtime | our image + ~50 lines of HTTP server answering `class_method` | everything; the server is a shim. **Low lock-in, low benefit** |
| ADK agent on Agent Runtime | a new harness, and a skill-loading story [ADR 0001][adr1]'s pinned skills do not survive | very little. **High lock-in, and it re-opens a permanently-closed decision** |
| Managed Agents API | agent config + `SKILL.md` files, already our format | skills port cleanly; harness, sandbox and network model do not. **Pre-GA lock-in** |

The §8.3 finding sharpens this: the live debug tap everyone actually wants is
`Runner.run_async()` / `BasePlugin.on_event_callback` — **an ADK feature, not an Agent
Runtime feature**. So the one capability that would justify adopting the platform is
available without it, and unavailable to us for an unrelated reason (the skill loader).

## 10. The verdict

| Leg | Hosted? | Why |
|---|---|---|
| **Orchestrator — code paths** (fingerprint, dedup, issue CRUD, `thread_ts`, service→repo, opening the PR, launching the run — [flow:121][flow]) | **No** | Stateless Go with a webhook front-end ([ADR 0004][adr4]). The agent platform addresses none of it. If it moves it moves to Cloud Run, which is a deployment question, not this one |
| **Orchestrator — the diagnose loop** ([flow:122][flow]: model + Grafana MCP, no filesystem, no toolchain, no code execution) | **The only arguable one** | Exactly the shape Agent Runtime is built for: read-only MCP tools, seconds of work, no untrusted execution — and what it lacks today is observability. Against: [flow:172-175][flow]'s "the orchestrator holds its credentials directly because it is trusted code we wrote" applies unchanged, so hosting splits the trusted component across two runtimes; and per §8.3 the spans are OTel either way |
| **Worker — code execution** ([flow:123][flow]) | **No** | Not for lack of a JVM — §3 shows a BYO container runs one. For four docs-grounded reasons: no documented isolation boundary while the native credential path is in-process (§9.1, §4.1); `resource_limits` and `container_concurrency` are per **deployment**, default 9 (§1); **disk is unpublished** and a Scala build is a disk question (§3.5); and ADK — the only door to the managed value — is permanently closed (§9.3). Plus the reframe's own logic: if the PR is the completion signal, the platform's lifecycle buys nothing, leaving it to compete on isolation and cost, where it loses |
| **The egress and policy layer** | **Watch this one** | Agent Gateway + Model Armor is GA, default-deny, mTLS-terminating, and does prompt-injection inspection we have nothing for. The one component here solving a problem [ADR 0005][adr5] leaves open |

If the worker must ever leave the cluster, the destination is a **Cloud Run job** — exit
code, ≤8 vCPU / 32 GiB, ≤168 h, `--vpc-egress=all-traffic`, refreshing secret volumes,
per-job service account, `jobs.run` on demand, documented gen2 microVM. Same batch primitive,
different scheduler. If the constraint is the cluster rather than Kubernetes, **GKE Agent
Sandbox** is the managed form of the posture already chosen: gVisor, warm pools, no extra
charge.

## 11. Docs-first: what is settled, what is not, and the cheapest way to find out

**Settled by documentation and public source. Nothing provisioned.**

| Question | Answer | Source |
|---|---|---|
| Does the product the user named exist? | Yes — two stacked products on a renamed platform, on **two different API surfaces** | §0 |
| **Can Managed Agents run a JVM / gradle / sbt?** | **No.** Preinstalled list is Python + Node; no JDK; no BYO image | [sandbox-env][mgsandbox] |
| **Can Agent Runtime run a BYO container from GAR?** | **Yes**, three ways: prebuilt `image_uri`, a Dockerfile via `SourceCodeSpec.image_spec`, or Developer Connect | [deploy][deploy], [proto][proto1], local read |
| **Can that container run a JVM?** | **Yes** — "Java: Deploy agents to Agent Runtime using a custom container" | [runtime][runtime] |
| Is there an exec channel with an exit code? | **No**, anywhere. `ContainerSpec` has no `command`; `send_command` is an HTTP forwarder; `execute_code` returns `Chunk[]` | local read, §2 |
| Resource ceilings? | 8 vCPU / 32 GiB, per deployment; `container_concurrency` default 9 | [deploy][deploy], local read |
| Secret Manager? Service account? | `secret_env` → `SecretRef{secret, version}`; custom `service_account`; `identity_type ∈ {SERVICE_ACCOUNT, AGENT_IDENTITY}` | local read, [DeploymentSpec][deployspec] |
| Can egress go through our proxy? | **Yes** — `psc_interface_config`, and Google publishes the Secure Web Proxy recipe | local read, [codelab][swp] |
| Does anything push on completion? | **Not for Agent Runtime** — no notification field in either proto, and `aiplatform.googleapis.com` is absent from Eventarc's event-type list. **Yes for the Interactions API**, via `webhook_config` | [Eventarc][eventarc], [proto2][proto2], [webhooks][gwebhooks] |
| Can you watch a session live? | **No** on the managed surfaces — `ListEvents` is unary, no watch method. **Yes** via the ADK event generator, in your own process | [session proto][protosess], [event.py][adkevent] |
| GAR Maven auth? | ADC, or a plain `Authorization: Bearer` — so the existing proxy covers it | [garjava][garjava] |
| Cost? | Published; compute is ~2 % of a run | §6 |

**Genuinely not answered by documentation, and the cheapest way to settle each. Cost includes
"how much GCP must I turn on" — every row here is zero except the last.**

| Open question | Why the docs don't settle it | Cheapest first |
|---|---|---|
| **Disk / `/tmp` size and filesystem writability for an Agent Runtime container** — the load-bearing unknown for a JVM build | Not published for any of the three sandboxes. The quotas page (`resources/agent-quotas`) renders client-side and yields nothing to a fetcher | 1. **Open that one page in a browser** — 2 minutes, zero GCP. 2. Read the official containerised-agent sample Dockerfiles for any documented cache path. 3. Ask on the public issue tracker or a support case. Deployment is the *last* resort and it needs Vertex AI + Cloud Build + Artifact Registry enabled — which is exactly what we are avoiding until step 1 |
| **Request timeout for `query` / `streamQuery`** | Only a SNIPPET-ONLY "10 minutes" front-end figure | Same page. And note this is a *design* answer reachable from a number: if it is 10 minutes, a 600 s run has no margin and `asyncQuery` becomes mandatory |
| **The `sandboxEnvironments` REST schemas and `asyncQuery`'s error surface** | REST reference pages render client-side | **`curl 'https://aiplatform.googleapis.com/$discovery/rest?version=v1' \| jq '.schemas'`** — a public, unauthenticated JSON document. No project, no API enabled, no credentials. It defeated a fetcher on size; `jq` locally settles every field |
| **Isolation boundary for the Agent Runtime container** | Google does not publish it | A support case or a TAM. **Not** discoverable by deploying — you cannot observe your way to a tenancy guarantee. Until it exists, §9.1's objection can be stated, not quantified |
| **Sessions / Cloud Trace pricing exactly, and the Sessions billing-start date** | Both pricing pages render client-side; the two sources seen disagree on the date | Open both pricing pages in a browser, or use the pricing calculator. Free |
| **Residency support matrix for `reasoningEngines`** | The matrix page renders client-side; an indexed snippet contradicts the CMEK finding | Open the page; it is one table |

**What was actually run, and it is the whole experiment budget.** A venv,
`pip install google-cloud-aiplatform` (resolved to **2.0.1**), and the client surface read
with `inspect` and Pydantic `model_fields`: `ReasoningEngineSpec` and its nested
`ContainerSpec` / `DeploymentSpec` / `PackageSpec` / `SourceCodeSpec`, `IdentityType`,
`SecretEnvVar` / `SecretRef`, the `vertexai._genai` sandbox, session and task types, the
`Sandboxes.send_command` / `execute_code` / `create` signatures and docstrings, the
`AgentEngines` and `A2aTasks` / `A2aTaskEvents` method lists, and the `Language` /
`MachineConfig` / `SandboxState` / `A2aTaskState` / `PostSnapshotAction` enums. **No API
enabled, no project touched, no credentials used, no network call to Google beyond PyPI.**
That is where §2's central shape finding, the `A2aTask` discovery in §8.2, and the
`internet_access`-is-one-boolean fact came from — none of which the HTML docs would yield.

**Recommendation.** Do not host the worker. Three things, all cheap and all independent of
any hosting decision:

1. **Add `-maven.pkg.dev` to the credential switch** ([ADR 0005:295-306][adr5]) — a table
   entry, and it closes half of [flow:291][flow].
2. **Spend ten minutes with a browser and `curl`** on the six rows above. Three free actions
   convert most of that table from "unknown" to "known", and if the disk figure comes back
   small the whole question closes without deploying anything.
3. **If live visibility is the actual pain, build it where it belongs** — stream the harness's
   turns into the Slack thread and issue (§8.4). §8.3 established that the only genuinely live
   tap runs in your own process on any runtime, so this is not a hosting decision at all.

Then, if the diagnose loop's observability turns out to be the real pain, revisit **§8** —
not §2. That is the half where a hosted runtime has a genuine argument.

## 12. What this does not solve

1. **Three Google doc pages would not render, and two matter:** `vertex-ai-name-changes`
   (§0's mapping is a reconstruction, not a transcript), `resources/agent-quotas` (disk and
   the request timeout — the two numbers the JVM question turns on), and the
   security-controls matrix (§9.2's contradiction). All render client-side; a browser closes
   all three.
2. **No isolation statement exists for the Agent Runtime container.** §9.1. Not a research
   failure — Google does not publish it.
3. **Disk, everywhere.** Agent Runtime, the custom container sandbox, and the Managed Agents
   sandbox all omit it. For a JVM build this is not a detail.
4. **The Managed Agents `transform` boundary.** "no ambient credentials" is asserted; *where*
   the header is attached relative to the sandbox is not described. If it is inside the same
   VM the agent's code runs in, §4.3's appeal evaporates.
5. **Two pricing facts are SNIPPET-ONLY** — Cloud Trace's span rate and retention, and the
   Sessions/Memory per-event charge and its billing-start date, on which two sources
   disagree. §6 flags them; do not budget from them.
6. **The scala/sbt toolchain does not exist on our side either.** [ADR 0003:159-161][adr3]
   has no `scala` key and the manifest table no `build.sbt`. Every option here is blocked on
   the same unbuilt image, and that work is independent of hosting.
7. **The clock §2.4 introduces is unbuilt and unbudgeted.** Losing the informer means a
   per-run deadline and a sweep. Small, but it is the first thing in this system that is not
   purely event-driven, and it puts a little state back.
8. **Whether the diagnose loop wants hosting at all.** §10 calls it arguable and leaves it.
   Deciding needs a measurement nobody has: how often the diagnose loop is the thing that
   went wrong. If the answer is "rarely", the console is worth nothing and the platform
   question closes entirely.
9. **`google-cloud-aiplatform==2.0.1` is not `master`.** `BuildSpec`, `shell_environment` and
   `STATE_PAUSED` exist in the proto and not in the release; conversely the release is what
   you would actually install. Where they differ this document says so, but a claim read from
   one is not a claim about the other.
10. **The ADK event-tap plan is unmeasured against our harness.** §8.3's "the live tap runs in
    your own process" is an ADK fact. We run `claude -p` with `--output-format stream-json`
    ([arch:306-312][arch]), which is a different event stream with different fields. That it
    is *equally* tappable is very likely and is not verified here.

---
[flow]: alert-driven-runs-and-the-slack-orchestrator.md
[kagentdoc]: kagent-as-the-control-plane.md
[adr]: ../adr/
[adr1]: ../adr/0001-sandbox-image-strategy-and-byo-contract.md
[adr2]: ../adr/0002-a-run-executes-as-a-kubernetes-job.md
[adr3]: ../adr/0003-toolchain-detection-and-image-selection.md
[adr4]: ../adr/0004-the-orchestrator-is-a-controller-with-a-webhook-front-end.md
[adr5]: ../adr/0005-credentials-terminate-at-the-credential-proxy.md
[arch]: ../architecture.md
[ctx]: ../../CONTEXT.md

[ge]: https://docs.cloud.google.com/gemini/enterprise/docs
[geimport]: https://docs.cloud.google.com/gemini/enterprise/docs/import-govern-agent-registry
[runtime]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/build/runtime
[contract]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/runtime-contract
[deploy]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/deploy-an-agent
[optimize]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/optimize-and-scale
[useagent]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/use-an-agent
[byoc]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/sandbox/custom-containers
[codeexec]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/sandbox/code-execution-overview
[mgagents]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/build/managed-agents
[mgsandbox]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/build/managed-agents/sandbox-environment
[mgcreate]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/build/managed-agents/create-manage
[mgcustom]: https://ai.google.dev/gemini-api/docs/custom-agents
[gwebhooks]: https://ai.google.dev/api/webhooks
[interactions]: https://ai.google.dev/api/interactions-api
[adkdoc]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/build/adk
[adkcli]: https://adk.dev/api-reference/cli
[adkevent]: https://github.com/google/adk-python/blob/main/src/google/adk/events/event.py
[adkplugin]: https://github.com/google/adk-python/blob/main/src/google/adk/plugins/base_plugin.py
[adk3990]: https://github.com/google/adk-python/issues/3990
[gateway]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/govern/gateways/agent-gateway-overview
[registry]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/govern/agent-registry
[aidoverview]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/govern/agent-identity-overview
[runtimeaid]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/agent-identity
[authmgr]: https://docs.cloud.google.com/iam/docs/auth-manager-overview
[iamrn]: https://docs.cloud.google.com/iam/docs/release-notes
[relog]: https://docs.cloud.google.com/agent-builder/agent-engine/manage/logging
[logsink]: https://docs.cloud.google.com/logging/docs/export/pubsub
[retrace]: https://docs.cloud.google.com/agent-builder/agent-engine/manage/tracing
[reaccess]: https://docs.cloud.google.com/vertex-ai/generative-ai/docs/agent-engine/manage/access
[deployspec]: https://docs.cloud.google.com/java/docs/reference/google-cloud-aiplatform/latest/com.google.cloud.aiplatform.v1.ReasoningEngineSpec.DeploymentSpec
[pytypes]: https://docs.cloud.google.com/python/docs/reference/vertexai/latest/vertexai._genai.types.ReasoningEngineSpecContainerSpec
[streamq]: https://docs.cloud.google.com/vertex-ai/generative-ai/docs/reference/rest/v1/projects.locations.reasoningEngines/streamQuery
[bidi]: https://docs.cloud.google.com/vertex-ai/generative-ai/docs/agent-engine/bidirectional-streaming
[eventarc]: https://docs.cloud.google.com/eventarc/docs/event-types
[vpcsc]: https://docs.cloud.google.com/gemini-enterprise-agent-platform/machine-learning/general/vpc-service-controls
[pscif]: https://docs.cloud.google.com/agent-builder/agent-engine/private-service-connect-interface
[swp]: https://codelabs.developers.google.com/agent-engine-psc-interface-swp
[datagov]: https://cloud.google.com/vertex-ai/generative-ai/docs/data-governance
[eudb]: https://docs.cloud.google.com/assured-workloads/docs/control-packages/eu-data-boundary-support
[vxprice]: https://cloud.google.com/vertex-ai/pricing
[genaiprice]: https://cloud.google.com/vertex-ai/generative-ai/pricing
[gabrn]: https://docs.cloud.google.com/generative-ai-app-builder/docs/release-notes

[proto1]: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/aiplatform/v1/reasoning_engine.proto
[proto2]: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/aiplatform/v1/reasoning_engine_execution_service.proto
[proto3]: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/aiplatform/v1/env_var.proto
[protosess]: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/aiplatform/v1beta1/session_service.proto

[crexec]: https://docs.cloud.google.com/run/docs/configuring/execution-environments
[crtimeout]: https://docs.cloud.google.com/run/docs/configuring/task-timeout
[crsecrets]: https://docs.cloud.google.com/run/docs/configuring/services/secrets
[crprice]: https://cloud.google.com/run/pricing
[gkesandbox]: https://docs.cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods
[gkeagentsb]: https://docs.cloud.google.com/kubernetes-engine/docs/how-to/how-install-agent-sandbox
[gkeprice]: https://cloud.google.com/kubernetes-engine/pricing
[garjava]: https://docs.cloud.google.com/artifact-registry/docs/java/authentication
[garacl]: https://docs.cloud.google.com/artifact-registry/docs/access-control
[garremote]: https://docs.cloud.google.com/artifact-registry/docs/repositories/remote-overview

[blog0422]: https://cloud.google.com/blog/products/ai-machine-learning/introducing-gemini-enterprise-agent-platform
[blog0730]: https://cloud.google.com/blog/products/ai-machine-learning/whats-new-in-gemini-enterprise-agent-platform
[blog0731]: https://developers.googleblog.com/agent-and-model-evaluations-in-gemini-enterprise-agent-platform-are-now-ga/
[a2alf]: https://developers.googleblog.com/en/google-cloud-donates-a2a-to-linux-foundation/

[clmg]: https://platform.claude.com/docs/en/managed-agents/overview
[clmgsb]: https://platform.claude.com/docs/en/managed-agents/cloud-sandboxes-reference
[clmgprice]: https://platform.claude.com/docs/en/about-claude/pricing
