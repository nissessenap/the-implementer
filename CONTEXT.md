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
Pods and acts on the terminal one. It holds no run state of its own: state lives
in Kubernetes objects and GitHub, so **v1 has no database**. See
[ADR 0004](docs/adr/0004-the-orchestrator-is-a-controller-with-a-webhook-front-end.md).

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

**GAR credential** — the proxy's own Google identity, attached to
`{region}-go.pkg.dev` and `{region}-python.pkg.dev` so the sandbox installs
private packages holding nothing — not a token, and not even a sentinel. Workload
Identity and no key file: a service account key in a Secret is the long-lived
credential Workload Identity exists to delete, so a cluster with no metadata
server cannot turn this on, and kind proves the mechanism against a fake token
source instead. The authorization is one grant,
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
identity check, so anything in the binary can shadow a provider. `file` for the
e2e, `gcp` for production, and production drops `file` outright.

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

**Sandbox** — the isolated container a run executes in. Holds the checkout, the
toolchain, and the agent CLI. Nothing in it survives the run.

**Sandbox posture** — the security context a run executes under:
`runtimeClassName: gvisor`, uid 1000, `seccompProfile: RuntimeDefault`,
`readOnlyRootFilesystem: true`, all capabilities dropped, not privileged. gVisor is
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
(`command -v` for the agent CLI, `git`, `gh`, the script itself) and failing to
`/dev/termination-log` with a readable message. Exists so a non-conforming image
fails loudly rather than mysteriously.

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
informer watches Pods rather than Jobs.

**Resume seam** — the one obligation trigger #3 (PR-review feedback) places on
v1: the workspace and `HOME` mounts stay *swappable* from `emptyDir` to a PVC, and
neither the phase script nor the orchestrator assumes nothing survives a run.
Resuming is `claude --resume` against a persisted session directory, so it is a
property of the volume, not of the workload primitive.
