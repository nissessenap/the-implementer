# The implementer

A GitHub webhook app that triggers AI coding agents to implement an issue, based on a label or a mention of the GitHub App user.

It's built on the philosophy of Matt Pocock's [skills](https://github.com/mattpocock/skills), where the developer runs `/grill-with-docs` until the open questions are answered and issues have been created from there.

But instead of running `/implement` on your laptop, the implementer does it for you — in an isolated Kubernetes workload.

## How it works

1. A webhook event arrives (label applied, or the app user mentioned in a comment).
2. One isolated Kubernetes workload is created. It carries no credentials — a short-lived, narrowly-scoped GitHub installation token is minted by the proxy, on demand, and never enters the sandbox.
3. The workload's entrypoint is a **run plan**: a script that invokes `claude -p` once per phase, with deterministic steps in between. Each phase is a fresh process, so each gets a clean context.
4. The agent commits and pushes its branch. It cannot open, comment on, approve, or merge a pull request.
5. The orchestrator watches the workload to completion, reads the run's structured result, and opens the pull request itself.

All model and registry traffic leaves through a **credential proxy**, so the sandbox itself holds no credentials — see below.

The orchestrator is really a **controller with a webhook front-end** — so run state lives in Kubernetes objects and GitHub, and there is no database. Restart it mid-run and it reconciles; webhook redelivery is idempotent because the workload name derives from the issue.

## Design decisions

**[`docs/architecture.md`](docs/architecture.md) is the v1 architecture** — every decision, the operator prerequisites, what MVP deliberately leaves out, and what is decided but unmeasured. Decisions expensive to reverse are ADRs in [`docs/adr/`](docs/adr/); everything else lives in that document. [The architecture map](https://github.com/nissessenap/the-implementer/issues/1) is the route that produced them, with the research behind each call.

Settled so far:

- **The agent is the `claude` CLI, run headlessly.** We don't build a harness. Matt's skills are Claude Code skills — `SKILL.md` discovery, `disable-model-invocation`, prose `/skill` cross-invocation, and subagent spawning are all harness features, not prompt content. Reimplementing them is a Claude Code clone, not a harness. Spotify reached the same conclusion the expensive way.
- **Phases are code, not prompt.** The run plan is a script, so phase boundaries have real exit codes and "the review never ran" is distinguishable from "the review passed."
- **No `kubectl exec` in the run path.** The pod's own `command` is the run plan. Results leave via the pod's termination message and its logs — both plain reads on objects the orchestrator already watches.
- **The orchestrator opens the PR, not the agent.** The sandbox gets `contents: write` and nothing more. A deterministic PR is also diagnosable when the run *failed*, which an agent-authored PR can never be.
- **The sandbox holds no credentials.** Model calls, GitHub, and private package registries all leave through a proxy that terminates credentials on the way out. The sandbox's `GH_TOKEN` is a worthless sentinel string the proxy swaps for a real token mid-flight; it never holds a model or cloud credential at all. Demonstrated end to end — [#33](https://github.com/nissessenap/the-implementer/issues/33), [#34](https://github.com/nissessenap/the-implementer/issues/34).
- **The proxy mints the GitHub token, and Cloud KMS signs the App JWT.** So the App private key exists only inside KMS, and what the proxy holds is a revocable, audit-logged signing *capability* rather than key bytes. The signer is [`isometry/ghait`](https://github.com/isometry/ghait), which implements `ghinstallation.Signer` for GCP KMS, AWS KMS, Azure Key Vault, Vault and a local PEM — selected by build tag, so the non-GCP operator story is a build flag rather than a rewrite. Every token is minted **for the repository the run's annotations name**, never for the one the request URL names. [#36](https://github.com/nissessenap/the-implementer/issues/36).
- **Scion is not a dependency.** [Scion](https://github.com/GoogleCloudPlatform/scion) evaluated agent-sandbox and deliberately removed it, and webhook-driven agent creation is an explicit non-goal in its design docs. It stays useful as prior art.

Nothing architectural is still open. Egress allowlisting and `NetworkPolicy` enforcement are *decided but not built* — MVP ships open egress, and the target shape is in [the architecture document](docs/architecture.md#7-network-egress).

## Container images

Your organization probably has multiple languages, and what's pre-installed in a sandbox is up to you. The strategy is settled in [ADR 0001](docs/adr/0001-sandbox-image-strategy-and-byo-contract.md): one small base image, with per-language images as short derivatives, rather than the field's usual multi-language base plus a setup script. The BYO-image contract is the base Dockerfile itself, and it is deliberately small.

The base image lives in [`sandbox/`](sandbox/): the `Dockerfile` that *is* the contract, the run plan the pod executes as its `command`, and the schemas its phases report through. `make sandbox-image` builds it; a `v*` tag publishes `ghcr.io/nissessenap/implementer-base` and prints the digest, because nothing inside the image floats and Helm defaults to the immutable reference.

Two hard constraints are already known. **The sandbox must run as a non-root user** — bubblewrap fails outright as uid 0 under gVisor. And it must **trust a private CA**, because the proxy terminates TLS for GitHub and the package registries; that turns out to need seven environment variables rather than one, since six of them *replace* the system trust store rather than adding to it.

## Running the e2e

```sh
make kind-up && make e2e            # a throwaway kind cluster
export KUBECONFIG=…; make e2e       # or any cluster you already have
RUNTIME_CLASS=gvisor make e2e       # gVisor, where a RuntimeClass exists
```

It needs `kubectl`, `helm`, `docker`, `openssl` and Go on `PATH` — `openssl` for
the certificate and key-wrapping stages, and Go both because the proxy image is
built with [ko](https://ko.build) (`make image`), which needs nothing installed
itself, and because the harness now derives each fixture's run credential by
running `orchestrator cred` rather than reimplementing the HMAC in shell. The harness is
`KUBECONFIG`-driven and assumes nothing else about the cluster's flavour but one
thing: from the proxy stage on it builds an image, which has to reach the
cluster's nodes somehow. That is a single variable — kind by default, anything
else via `E2E_IMAGE_LOAD` (a command taking the image name).

Stages run in filename order, and all but the two GitHub ones need no real
credential of any kind, so the whole thing runs on fork pull requests too — the
run secret the proxy authenticates against is derived from a key the harness
invents. The exceptions skip themselves unless they are given one:

- the **sentinel swap** wants `E2E_GITHUB_TOKEN` and `E2E_GITHUB_REPO`, a scratch
  repository it may push a dry run against;
- the **minted token** wants `E2E_GITHUB_APP_ID`, `E2E_GITHUB_APP_KEY` (the PEM
  GitHub hands out) and `E2E_GITHUB_REPO` the App is installed on, plus an
  optional `E2E_GITHUB_OTHER_REPO` for the negative clone.

The **signer** is the stage in between, and it needs nothing: OpenBao runs in the
cluster in dev mode, the App JWT is signed through its transit engine, and so the
signature crosses the network exactly as it does from KMS in production — the one
link a PEM on disk cannot exercise. It imports a key it generates and throws away,
verifies the signature against it, and then boots the proxy on `provider: vault`
so ghait's own check runs from inside the cluster. When the minted-token stage
above does have an App, it signs the same way, through the same OpenBao: the App
key is read on the harness's machine and imported, so it never reaches the cluster
at all.

There is deliberately no GAR stage: the proxy's Google identity is Workload
Identity, so a cluster with no metadata server cannot turn it on, and
`proxy/gar_test.go` proves the attach and the `-docker.pkg.dev` exclusion offline.

The **model route** stage has the same problem and answers it differently, because
the part worth watching in a cluster is not the credential: it runs unconditionally
against a mock Vertex, behind the proxy's `vertex.upstream` seam, and asserts the
wiring — base URL, path rewrite, a credential arriving on a request that carried
none, and SSE deltas arriving spread out rather than in one buffered lump. ADR
0005 says why it is a mock, and the refusals are `go test ./proxy`'s.

The **orchestrator** stage is the only one whose Job no
fixture describes: the binary builds it, `charts/orchestrator` renders its PodSpec,
and the pod reaches the proxy with a credential the builder derived and nothing of
its own. It asks the apiserver the two questions no amount of reasoning about
DNS-1123 settles — that a name past 63 characters is truncated-and-hashed into
something it accepts, and that two repository names differing only in a
normalisation the slug flattens get *different* Jobs. Its clone half needs no
credential; its push half runs when stage 50 or 60 has left one in the proxy.

The **trigger** stage is the last one, and the only one that plays GitHub rather
than talking to it: it signs an `issues`/`labeled` payload with the secret it put in
the cluster and POSTs it at the orchestrator's **Service** through a port-forward, so
there is no public endpoint and no tunnel and it runs unattended. One delivery
becomes a run; a redelivery, a second label, some other label, a `labeled` event with
no label object at all, a bot sender, `ghost`, and a wrong signature become nothing —
each asserted by the absence of a Job, and each on its own issue number so a shared
one cannot hide a miss.

What they prove — both credential shapes, git's 401 round-trip, the mint's scope,
cache and refresh, the GAR attach and the model route's rewrite and streaming — is
covered offline by `go test ./proxy`.
Adding a stage is adding a file to [`e2e/`](e2e/).

## Roadmap

- [x] Trigger implementation based on a label
- [ ] Trigger implementation based on a mention + extra context in an issue comment
- [ ] Trigger feedback on a review using a comment in a PR — deliberately deferred

### Docs to write

- How to build an image ready for use
- How to install the implementer and its runtime dependencies in Kubernetes
- How to install gVisor for improved isolation
- How to set up the webhook with a GitHub App

### Long term

- A link from the issue to a live view of the run. Headless Claude Code emits a structured event stream, so this is a log pipe rather than a terminal proxy.
- State management: launching an agent to address code-review feedback.
