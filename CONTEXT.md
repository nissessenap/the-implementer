# Context

Domain glossary for `the-implementer`. Terms are added as architectural decisions
land; see `docs/adr/` for the decisions themselves and
[`docs/architecture.md`](docs/architecture.md) for everything that did not earn
an ADR.

`the-implementer` is a Go GitHub App and webhook receiver that launches isolated
Kubernetes workloads running Claude Code headlessly to implement a GitHub issue,
then produces a pull request.

## Components

**Orchestrator** — the Go process that turns a labelled issue into a pull
request. A webhook front-end that creates one Job, plus an informer that watches
Jobs and reads their Pods on demand. It holds no run state of its own: state lives
in Kubernetes objects and GitHub, so **v1 has no database**. See
[ADR 0004](docs/adr/0004-the-orchestrator-is-a-controller-with-a-webhook-front-end.md).

**Trigger** — a signed `issues`/`labeled` webhook carrying the `ready-for-agent`
label, which is the label `/triage` already produces, so the readiness contract is
inherited rather than reinvented. The front-end creates one Job and is **done** — it
does not wait for the run. Two properties of it are counter-intuitive enough to be
worth stating here rather than only in the ADR:

**Payload authorization** — the whole of it: the sender's `type` must be `User` and
its `login` must not be `ghost`. **No permission API call**, because the obvious
design has it backwards in both directions. The flatt.tech disclosure is read as
proof a write check is mandatory; `claude-code-action` **had** one and was bypassed
anyway, through a login ending in `[bot]`, by an attacker with no access to the
target repository at all — so the clause usually omitted is the one that was
exploited and the clause usually insisted on is the one that failed to help. `ghost`
is beside the type check because GitHub substitutes that account for unresolvable
actors and *its type is `User`*. Labelling needs Triage, so the event proves triage
and nothing more; the escalation ceiling is a branch and a pull request nothing here
can merge.

**Silent refusal** — an unauthorized sender is logged and nothing else. There is no
"sorry, you're not allowed" comment, because on a public repository that is an
on-demand way to make the App write to issues with untrusted input in hand — so the
front-end holds no GitHub credential at all and there is nothing there to write
with. The only refusals a human sees in v1 are the toolchain ones. The
**edit-after-label window** stays open on purpose: authorization is at webhook time,
the issue text is fetched at run time, and neither clause looks at the text.

**Silent death** — a run that ends with **no in-pod code having executed**:
`OOMKilled`, `activeDeadlineSeconds`, eviction, `ImagePullBackOff`. No trap fires,
no phase script runs, nothing reaches `/dev/termination-log`. It is the deciding
reason the informer exists, and it is routine rather than pathological — a
transient 529 burned a run in the prototype. A pod cannot report its own death,
which is the same argument ADR 0004 uses against an in-pod PR builder: move the
reporting inside the sandbox and it moves the failure path into the thing that
fails. What it costs when nothing is watching is not a lost result but a *silent*
one — the issue sits there labelled `ready-for-agent` and nobody is ever told the
run happened.

**Run marker** — the `<!-- implementer-run: <run-uid> -->` line the informer puts
first in every comment it posts, and scans the thread for before posting. It is the
whole of exactly-once: there is no database to keep a dedupe table in, and the
orchestrator's RBAC is read-only on Pods and Jobs so it cannot mark the run as
reported in Kubernetes either. So the record lives in GitHub, where the other half
of this system's state already does — which is what makes redelivery, a second
label and a restart mid-run all cost one comment and no more. Keyed on the *run*
uid rather than the issue, for the same reason the credential is: a re-run is a new
run and gets its own comment.

**Credential proxy** — the Deployment every credential terminates at. The
sandbox holds none: it sends unsigned model requests and a **sentinel** GitHub
credential, and the proxy attaches the real ones. It is the only component with
a cloud identity, and it is a separate pod rather than a sidecar because a
sidecar shares the sandbox's network namespace and can be bypassed. See
[ADR 0005](docs/adr/0005-credentials-terminate-at-the-credential-proxy.md).

**Intercept list** — the hosts the credential proxy terminates TLS for, rather
than tunnelling opaquely. Not configuration: it is read back off the SANs of the
proxy's own serving certificate, so a host it cannot present a certificate for is
a host it cannot intercept and the two can never drift. `*.pkg.dev` is a wildcard
occupying the whole leftmost label, because that is the only shape `crypto/x509`
matches — so the list is deliberately **wider** than the credential rule layered
on it, and the per-host credential switch is what keeps `-docker.pkg.dev`
tokenless.

**Sentinel** — a worthless string standing where a credential would be
(`GH_TOKEN=proxy-injected…`). The proxy swaps it for the real token in flight.
The point of the sentinel is that the sandbox's code path is *unchanged*: the
phase script still builds an authenticated URL, the value is just no longer
worth stealing. Padded to the real token's 40 bytes — the swap only touches a
header today, so the equal length costs nothing and is what stops a future
credential-in-a-body swap from shifting `Content-Length`. Matched on its
**prefix** rather than whole:
this component's one unacceptable failure is a silent no-op, and a sandbox
holding the unpadded string must swap rather than push anonymously.

**Credential switch** (`credFor`, spelled `Creds.For` in the proxy) — the
per-host answer to "what is this host due". Each credential names its own host set, validated at load to be a subset of
the intercept list, and a host nobody names gets nothing. That is what keeps the
deliberately-wider certificate safe: `-docker.pkg.dev` and
`objects.githubusercontent.com` are both intercepted and both tokenless, the
latter because pre-signed blob URLs carry their own authorization already. Host
matching is case-insensitive at both ends, because x509's is: a host the
certificate intercepts under one casing and the switch misses under another is
an anonymous request with no error. Both normalisations, because x509 does both:
it folds case and trims the root label, so `CONNECT github.com.:443` is
intercepted too.

A host set is exact names, or one `*` standing for part of the leftmost
label — `*-go.pkg.dev`, because Artifact Registry's endpoints are regional and
pinning a region would be a second place to configure one. Narrower than x509's
wildcard rule on purpose, and validated through one sample host, which is sound
because every name such a pattern matches differs from that sample in the
leftmost label alone.

**Attached credential** — the other of the switch's two shapes, and the opposite
of the sentinel swap: the request leaves with a bearer whether or not it arrived
with one, and anything it did arrive with is overwritten. Per-credential, because
each shape is wrong on the other's hosts — `pip` and `go mod download` send
Artifact Registry nothing at all and do not retry on a 401, so there is no
sentinel to match; while on `api.github.com` an unconditional Basic is *ignored*
(measured: 200, limit 60, no error), which is a silent anonymous request.

**Model route** — the one request the credential proxy answers that is not a
CONNECT: `POST /vertex/…`, which is what `ANTHROPIC_VERTEX_BASE_URL` points at.
The sandbox sends the model call unsigned — it holds no model credential at all,
not blanked, absent — and the proxy reverse-proxies it to Vertex with the Google
identity below attached. No interception, because there is nothing to intercept:
the sandbox is *configured* to come here, so there is no name to impersonate, and
a plaintext in-cluster hop leaks nothing when neither direction carries a
credential. The upstream host comes from the **location in the path** rather than
from configuration here, so the proxy reads back whatever `CLOUD_ML_REGION` the
operator gave the sandbox — which makes the location sandbox-controlled input that
decides where a credential goes, and it is constrained to one DNS label's alphabet.
What the route forwards is **one inference call on one publisher model** —
`:rawPredict`, `:streamRawPredict`, `:countTokens`, and a POST — and not "a Vertex
path": `roles/aiplatform.user` also carries `customJobs.create`,
`pipelineJobs.create` and `endpoints.deploy`, which are POSTs to the same host
under the same prefix, so the host confines the credential to Vertex and only this
shape confines it to talking to a model. Streaming is the property to keep: `httputil.ReverseProxy`
forwards each SSE delta as it arrives with no `FlushInterval` tuning, and a
buffered turn is invisible in the content and obvious in the timing, which is what
both tests assert on.

**Model identity** — how the model route knows who is calling, and it is weaker
than the run secret on purpose: Claude Code reaches a base URL rather than a
proxy, so there is no `Proxy-Authorization` it could send and the **source pod is
the whole of the answer**. Bounded by what the route hands out — the credential is
the proxy's own Google identity, scoped to nothing a run says, so a run that
impersonated another would gain what it already has. What it still buys is that
the caller must be a run pod in this namespace, so nothing else in the cluster can
spend the operator's Vertex quota. The same weakening on the GitHub credential
would be a hole, which is why it is not there.

**Model pins** — the four models a run may name (`ANTHROPIC_MODEL`,
`ANTHROPIC_DEFAULT_OPUS_MODEL`, `ANTHROPIC_DEFAULT_SONNET_MODEL`,
`ANTHROPIC_DEFAULT_HAIKU_MODEL`), and they are the **customer's project's**, not
our release's: Vertex 404s a model the project has not enabled, and Claude Code
resolves `opus`/`sonnet` to its own defaults, so the opus alias must be remapped
or `/code-review`'s subagents 404. Sandbox environment and not proxy
configuration — the proxy never reads a model name — so they belong to whoever
writes the sandbox's environment.

**Google identity** — the proxy's own service account, reached through Workload
Identity and so the metadata server, and the credential behind *both* Google-facing
halves: the model route above and the GAR credential below. One token source for
the two, because it is one identity: no second secret, no second mount, and one
hourly refresh rather than two caches that could disagree. Two grants,
`roles/aiplatform.user` and `roles/artifactregistry.reader`, and no key anywhere —
the chart offers nowhere to mount one, which is why a cluster with no metadata
server cannot turn either half on.

**GAR credential** — the proxy's own Google identity, attached to
`{region}-go.pkg.dev` and `{region}-python.pkg.dev` so the sandbox installs
private packages holding nothing — not a token, and not even a sentinel. Workload
Identity and no key file: a service account key in a Secret is the long-lived
credential Workload Identity exists to delete, so a cluster with no metadata
server cannot turn this on, and `proxy/gar_test.go` proves the mechanism against a
fake token source instead. The authorization is one grant,
`roles/artifactregistry.reader` — and unlike the GitHub credential it is **not
scoped to the calling run**: it is the proxy's own identity, so every run reaches
everything the grant covers, which is why it belongs on a repository rather than
a project. `{region}-docker.pkg.dev` is on the same
certificate and stays **tokenless**: its `/v2/` challenge fires only on
unauthenticated requests, so attaching a bearer may suppress a dance the proxy
cannot yet answer, and blob fetches redirect to storage URLs that must not carry
our token either.

**Minted installation token** — what the GitHub credential actually is: an
installation token the proxy mints per run, scoped by
`InstallationTokenOptions.Repositories` to **the repository the run's annotations
name**, in the installation that repository resolves to. Two lookups off the same
`Run`, and never off the request: the URL, the `Host` header and the CONNECT
authority are all things the sandbox controls, and a token scoped to any of them
reaches every repository the App is installed on. Cached per run — keyed by the
whole identity, `run-uid` included, so a re-run does not inherit the previous
run's credential — and re-minted five minutes before expiry, so a clone starting
at minute 59 does not die mid-transfer. Signing is per-mint and not cached:
GitHub caps a GitHub App JWT's `exp` at ten minutes.

**Static token** (`StaticGitHub`) — the other half of the same credential slot: a
token read from a mounted Secret, scoped to whatever the operator put there. Not
a fallback and not a default — it is the seam that keeps the swap itself testable
with no App, no signer and no KMS. Never both at once: the two are not
interchangeable, so the chart refuses to render and the proxy refuses to boot
rather than pick a winner.

**Signing seam** — [`isometry/ghait`](https://github.com/isometry/ghait) behind
`ghinstallation.Signer`: the App's private key is a *reference* (a KMS crypto key
version) rather than bytes the proxy holds. The key is an **import** — GitHub
generates the App key pair and has no bring-your-own-public-key — and the proxy
always asks for ghait's boot-time `Check()`, which is what pins the key to ENABLED
and `RSA_SIGN_PKCS1_2048_SHA256` rather than discovering it on the first run.
Providers are linked in by **build tag** (`make image GO_TAGS=…`), never by a
blanket underscore import: ghait's provider registry is a global map with no
identity check, so anything in the binary can shadow a provider. `gcp` for
production, `vault` — Vault or OpenBao transit, wire-identical — for the e2e, and
both drop `file` outright: the e2e signs over the network too, so the App key
never enters the cluster in either.

Signing over the network is the property the e2e is buying there, and OpenBao is
what makes it affordable: transit needs no cloud credential, so the stage runs on
every pull request and the self-hosted operator story is demonstrated rather than
claimed. Two known edges. Transit must **import**, like KMS, and OpenBao ships no
`bao transit import` — `e2e/transit-import.sh` is that helper, against the raw API.
And ghait v0.14.0 strips a hardcoded `vault:v1:` from the signature, so a *rotated*
transit key returns `vault:v2:…` and the prefix lands inside the JWT's signature
segment: every mint fails closed, an outage rather than a bypass, and the fix
belongs upstream rather than here. Nothing rotates in dev mode, and stage 55
asserts the prefix it gets is the prefix ghait strips.

**Run identity** — `owner`, `repo`, `issue` and `run-uid` in **annotations**
(prefixed `implementer.dev/`), not labels,
because repository names exceed the 63-character label-value cap. Written in **two
places**: the Job's own metadata and `spec.template.metadata.annotations`, because a
Pod inherits only the latter. The informer reads them to get back to the issue, and
the proxy resolves them from a request's source pod IP — to a **Pod**, which is why
the second copy exists and why the proxy needs no RBAC beyond pods. Non-negotiable:
mint for the annotation's repository, never for the one the request URL names.

**Run secret** — a per-run value the orchestrator derives as
`HMAC-SHA256(shared-key, owner,repo,issue,run-uid)` and injects into the sandbox as
userinfo in the `https_proxy` URL, so every client sends it without being configured
to. The HMAC covers exactly the string that travels as the userinfo *username*, so
there is one encoding rather than two that can drift, and it uses commas because
`/`, `#` and `@` would need percent-encoding every client would have to agree on.
The proxy recomputes it rather than being told it, so there is no per-run Secret
and no orchestrator→proxy channel. It authenticates the *run*, which is why leaking
it to the sandbox costs nothing; what it actually guards is **run identity** against
informer-cache staleness when a pod IP is reused.

**Run UID** — the per-run half of that message: an annotation the orchestrator
picks, so a re-run of the same issue does not inherit the previous run's
credential. Not the **Job's** UID, which cannot be written into a sandbox at all —
[ADR 0005](docs/adr/0005-credentials-terminate-at-the-credential-proxy.md) has why.

**Source address** — the second factor resolves the *connection's* source IP, so
nothing between the sandbox and the proxy may SNAT. Cluster-internal ClusterIP
traffic preserves the pod IP; ipvs `masqueradeAll`, or a CNI masquerading
pod-to-pod, collapses every caller to a node IP and the proxy then refuses every
run rather than some. A deployment precondition, not a knob.

**Trust bundle** — the system CA bundle concatenated with the proxy's CA,
assembled by the phase script at run-plan start. A bundle rather than a bare
`ca.crt` because six of the seven trust variables *replace* the trust store
rather than adding to it.

## Sandbox and images

**Run** — one attempt at implementing one GitHub issue. Executes as a single
`batch/v1` **Job** — one pod, `restartPolicy: Never`, `backoffLimit: 0`, gVisor
via `runtimeClassName` — and produces at most one branch. The Job name derives
from the issue, which is what makes webhook redelivery idempotent. See
[ADR 0002](docs/adr/0002-a-run-executes-as-a-kubernetes-job.md).

**Job template** — the run's Job as a manifest the orchestrator's chart renders,
patched rather than generated: the builder sets the name, writes run identity in
both places, and appends the sandbox environment. Everything else — the sandbox
posture, the image, the volumes, `activeDeadlineSeconds` — is a field in that
manifest, which is what keeps uid 1000 and `runtimeClassName` out of Go.

**Sandbox** — the isolated container a run executes in. Holds the checkout, the
toolchain, and the agent CLI. Nothing in it survives the run.

**Sandbox posture** — the security context a run executes under:
`runtimeClassName: gvisor`, uid 1000, `seccompProfile: RuntimeDefault`,
`readOnlyRootFilesystem: true`, all capabilities dropped, not privileged, and no
ambient Kubernetes: `automountServiceAccountToken: false`, because a projected token
is a bearer for the apiserver and a run talks to it never, and
`enableServiceLinks: false`, because the alternative hands the sandbox an inventory
of every Service in the namespace. gVisor is
**load-bearing rather than defence-in-depth**: it is what makes `RuntimeDefault`
compatible with `bubblewrap`, so the strictest Pod Security Standard and the image
contract can coexist. Root is not a cheaper alternative — under gVisor,
`bubblewrap` fails outright as uid 0. See
[ADR 0001](docs/adr/0001-sandbox-image-strategy-and-byo-contract.md).

**Agent CLI** — the binary that does the implementing, invoked by the phase
script. Named as a role rather than a product: v1 ships `claude` as the only
implementation, but the image contract says only "an agent CLI on `PATH`", so an
organization's image stays valid if the engine changes.

**Base image** (`implementer-base`) — the one image we maintain. Carries the agent
layer and nothing language-specific: a shell, `git`, `gh`, `bubblewrap`, the agent
CLI, baked skills, the phase script, and a non-root user. **Its Dockerfile is the
image contract.** See [ADR 0001](docs/adr/0001-sandbox-image-strategy-and-byo-contract.md).

**Toolchain layer** — a thin image derived from the base that adds exactly one
toolchain, typically four lines of Dockerfile. We ship only the ones we dogfood.

**Toolchain** — the key space image selection joins on: a small vocabulary of ours
(`go`, `node`, `python`, …), not a language name. An image installs a toolchain,
not a language, and Linguist's names do not survive the join — `package.json` is
the manifest for both `JavaScript` and `TypeScript`. See
[ADR 0003](docs/adr/0003-toolchain-detection-and-image-selection.md).

**Detection** — how a run picks its image: root manifests first, `GET /languages`
as the fallback, each normalised to a toolchain and intersected with the images
the operator configured. Runs orchestrator-side before the Job exists; adds no
state. Ambiguity and non-detection both **refuse and comment** rather than guess,
because a wrong image fails deep after the tokens are spent.

**Version seam** — image lookup is longest-key-first (`python-3.14` before
`python`), so a versioned toolchain is a later table entry rather than a redesign.
v1 never emits a version; it owes the seam, not the feature. The deferred
overrides — a repo-side `.implementer.yaml`, or an image named in the triggering
comment — land here.

**Image contract** — the minimum an image must satisfy to be usable as a sandbox.
Deliberately small enough to bolt onto an organization's existing builder image,
so a custom image never has to be `FROM implementer-base`. Enforced by the
preflight, not merely documented.

**BYO image** — an organization's own sandbox image, satisfying the image contract
either by deriving from the base image or by copying its `RUN` lines into their
own hardened base.

**Preflight** — the phase script's first act: verifying the image contract
(`command -v` for the agent CLI, `git`, `gh`, `jq`, `bubblewrap`; the skills the run
plan invokes and the schemas it hands them; a writable result channel) and failing
to `/dev/termination-log` with a readable message. Exists so a non-conforming image
fails loudly rather than mysteriously, before a run spends anything.

**Matched pair** — the orchestrator and the sandbox image are versioned and
released together, and pinned by digest. The phase script lives in the image, so
the two cannot drift.

## Run execution

**Phase script** — the script baked into the sandbox image and invoked as the
pod's `command`. It is the run plan: deterministic steps in shell, with one agent
CLI invocation per agent phase. A fresh process per phase is how context is
cleared.

**Phase** — one agent CLI invocation within a run. Phases are code, not prompt:
control flow lives in the phase script so that "review passed" is distinguishable
from "review never ran".

**Baked skills** — [Matt Pocock's skills](https://github.com/mattpocock/skills)
installed at image build time and pinned, loaded via `--plugin-dir` from a
read-only path. Pinning is what makes evaluation possible; `--plugin-dir` is what
survives the ephemeral `HOME`.

**Result channel** — how data leaves the sandbox without `kubectl exec`. A compact
structured result via `/dev/termination-log`, surfaced on pod status; the full
transcript via `pods/log`. Both are pod-level, which is why the orchestrator's
informer reads the run's pod on every report — but it *watches* Jobs, whose terminal
condition is the one signal that also covers the ending that leaves no pod behind
(ADR 0004, amended 2026-08-31 and 2026-09-01).

**Result blob** — the JSON the phase script accumulates and writes once onto
`/dev/termination-log`: overall status, branch, commit count, summed cost, elapsed,
`pr_title` and a status/summary line per phase. An interface rather than a
convenience, and it has two ends now: written by `sandbox/phase.sh`, decoded by the
orchestrator's informer, and typed once in `sandbox/result.go` so the two cannot
disagree about the shape. Bounded field by field so the kubelet's blind 4096-byte
truncation cannot corrupt it. **A pod killed by a signal writes it not at all**,
which is the case the informer's second shape exists for.

**`completed_unreviewed`** — the run status when the implement phase landed and a
review phase died. Nothing gates the pull request: the branch is pushed anyway and
the dead phase is named in the blob, because discarding a ~$1 implement phase
because a 529 hit the reviewer is the expensive failure.

**Resume seam** — the one obligation trigger #3 (PR-review feedback) places on
v1: the workspace and `HOME` mounts stay *swappable* from `emptyDir` to a PVC, and
neither the phase script nor the orchestrator assumes nothing survives a run.
Resuming is `claude --resume` against a persisted session directory, so it is a
property of the volume, not of the workload primitive.
