# The implementer

## Agent skills

### Issue tracker

Issues live as GitHub issues, managed with the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical triage roles, used as-is (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.

### The sandbox image and the run plan

`sandbox/` is ADR 0001's BYO contract as code: `Dockerfile` (the contract itself),
`phase.sh` (the run plan, and the pod's `command`), and the two output schemas. The
shape of the `/dev/termination-log` blob is typed in `phase_test.go`, its only
reader until the orchestrator's PR builder becomes the second one.

```sh
make sandbox-image                 # local build; publishing is the v* tag workflow
make sandbox-images                # the base plus go, node and python
go test ./sandbox                  # the whole run plan, offline, in ~2s
```

The test runs the shipped `phase.sh` with `claude` and `gh` stubbed and **`git`
real**, against a bare repository in a temp dir — the clone URL is redirected with
git's own `insteadOf`, so nothing in the script bends for the test. That covers the
blob's shape and escaping, the `is_error` rule, a dead review phase leaving the
branch pushed at `completed_unreviewed`, and the commits-and-clean assertion per
review phase.

What it cannot cover is a run that costs money. Two ways to do that one:

- **In the cluster**, which is the real thing: `make sandbox-image`, load the image,
  install `charts/proxy` and `charts/orchestrator` with `sandbox.image` pinned, then
  `orchestrator run owner/repo#N`. The blob lands in the pod's termination message
  and the transcript in `kubectl logs`.
- **Under plain `docker run`**, which is how the run plan was measured in the first
  place and needs no cluster. ⚠️ There is no proxy in this mode, so the sandbox
  really does hold both credentials — use a scratch repo:

```sh
docker run --rm --tmpfs /workspace --tmpfs /tmp --tmpfs /home/agent \
  -e REPO=me/scratch -e ISSUE=1 -e HOME=/home/agent -e WORKSPACE=/workspace \
  -e TERM_LOG=/tmp/result.json -e IS_SANDBOX=1 -e TOOLCHAIN=go \
  -e GIT_AUTHOR_NAME=the-implementer -e GIT_AUTHOR_EMAIL=t@example.com \
  -e GIT_COMMITTER_NAME=the-implementer -e GIT_COMMITTER_EMAIL=t@example.com \
  -e GH_TOKEN="$(gh auth token)" -e CLAUDE_CODE_OAUTH_TOKEN="$CLAUDE_CODE_OAUTH_TOKEN" \
  ghcr.io/nissessenap/implementer-base:dev
```

Both need a repository whose `implementer/` branch prefix you are happy to have
pushed to. Expect ~450s and ~$2 for three phases against a small repo.

### The orchestrator's webhook front-end

`orchestrator serve` is ADR 0004's other half and the trigger the system exists
for: a signed `issues` webhook arrives, one Job is created, and the handler is
**done** — it does not wait for the run, and the response is sent minutes before
the pull request exists. `charts/orchestrator` renders its Deployment and Service
whenever `image` is set; empty leaves the chart as the Job template, the
ServiceAccount and the Role, which is what `run` and `watch` need.

```sh
POD_NAMESPACE=… PROXY_HOST=… RUN_KEY_FILE=… JOB_TEMPLATE_FILE=… \
  GITHUB_WEBHOOK_SECRET=… orchestrator serve   # POST /webhook, GET /healthz
make orchestrator-image                        # the reference the chart's `image` takes
./e2e/95-webhook.sh                            # a signed POST at the Service, no tunnel
```

Five things here are load-bearing, and three of them look like things to harden.
[ADR 0004][adr4] argues all five; what matters when editing this code is that each
one is a decision and not an omission:

- **The label is a constant, not a value.** `ready-for-agent`, the one `/triage`
  produces. Configurability is deferred to a per-installation lookup, so an env var
  now would be a knob to keep alive while the real thing replaced it.
- **`palantir/go-githubapp` is deliberately not used**, against the ticket's
  wording: its dispatcher *is* `github.ValidatePayload`, its `ClientCreator` wants
  the App key as PEM bytes that ADR 0005 exists to keep out, and it pins go-github
  **v90** against this repo's v88.
- **The authorization is two clauses on the payload and no permission API call.**
  `sender.type != "User" || sender.login == "ghost"` → ignore. `make test` asserts
  the absence with a `grep`, because "there is no call to the
  collaborator-permission endpoint" is the criterion.
- **The refusal is silent, and that is the security property.** The front-end holds
  **no GitHub credential of any kind** — there is nothing there to write with, and
  stage 95 asserts the Deployment mounts none.
- **`label` is not in the payload's required set** — that is `[action, issue,
  repository, sender]`. A `labeled` delivery with no label object is a real shape
  and must be ignored rather than dereferenced; `GetLabel().GetName()` answering
  `""` is a coincidence and not a check.

The **issue** is guarded the same way and for the same reason: a `labeled` delivery
with `number` 0 would pass `ParseIssue`, and a closed issue is ordinary housekeeping
that would otherwise spend ~450s and ~$2 on work already done. The state is compared
against `"open"` and not against `"closed"`, so a state GitHub adds later ignores.

**GitHub does not redeliver on its own.** A failed delivery is marked failed and
stays redeliverable by hand for three days, so a 500 here is a lost run unless a
human looks — which is why the drain, the body cap and the detached create context
are not cosmetic, and why `exists` logs the existing Job's phase (a re-label of a
run the TTL still holds creates nothing, and the delivery page nobody reads is the
only other place that could say so).

Idempotency adds nothing: a redelivery, a second label and a restart all resolve to
the Job name plus a swallowed `AlreadyExists`, and nothing here may key on the
delivery id. The **edit-after-label window** is #32 and deliberately open.

[adr4]: docs/adr/0004-the-orchestrator-is-a-controller-with-a-webhook-front-end.md

### The per-language images

`sandbox/go`, `sandbox/node` and `sandbox/python` are ADR 0003's toolchain
vocabulary as three thin derivatives, each `FROM` the base and each carrying a
toolchain and **nothing else**. No registry configuration, deliberately: an
organization that needs a private npm, pip or module registry bakes it into its own
derived image, and putting it here would make our images the place everyone
patches. They publish as `ghcr.io/nissessenap/implementer-{go,node,python}`, from
the same workflow run and under the same tags as the base — one build graph, so the
four are a matched set and the very first tag has no chicken-and-egg.

`sandbox/contract.sh <image> [toolchain]` is ADR 0001's contract checked against a
*built image*, and `make sandbox-images` runs it on each. Its largest check is to
run the shipped `phase.sh` and require it to get past preflight — one check instead
of a second copy of the list, which is the copy that would drift. The rest is what
preflight cannot see: the image's default uid, the absence of registry
configuration, and the toolchain the derivative exists to carry.

**A toolchain version the image lacks is not an error.** `GOTOOLCHAIN` stays at
`auto`, so a `go.mod` asking for a version we do not ship downloads it mid-run and
completes. Setting it to `local` would convert that into a failure that looks like
the repository's fault. The cost is egress to the toolchain hosts as well as the
module hosts, which #64 and #16 have to price.

### Docker in the `go` image, and what it costs

The wrap is a **per-run flag, off by default**: `orchestrator run -docker` writes
`SANDBOX_DOCKER=1`, and the run plan re-execs itself under `rootlesskit
--net=slirp4netns` — the *whole* script, not just the daemon, which is what puts
the agent, dockerd and inner containers in one netns on host networking. Unset, the
pod is byte-for-byte the posture the chart renders.

Four things about it were measured against k3s with real gVisor and are easy to
"simplify" back out:

- **`newuidmap` is privileged by a file capability, not by setuid.** Under gVisor a
  setuid-root exec raises `euid` to 0 and grants **no capabilities at all**
  (`CapPrm` stays `0`; host `runc` hands over the whole bounding set), so Debian's
  setuid-root `newuidmap` is unprivileged exactly where we run it. The only symptom
  is `newuidmap: write to uid_map failed: Operation not permitted`.
- **So a Docker run costs two PodSpec fields, and exactly two.** `no_new_privs` and
  an emptied bounding set each independently neuter a file capability, so the
  builder patches `allowPrivilegeEscalation: true` and `capabilities: add: [SETUID,
  SETGID]` — and nothing else. Not the uid, not the read-only rootfs, not the
  seccomp profile, not the runtime class.
- **`iproute2` is in the image because `slirp4netns` shells out to `ip`.** Missing,
  the only symptom is `nsenter: failed to execute ip` from inside rootlesskit.
- **The readiness gate is `docker version`, never `docker info`** — `info` answers
  about six seconds before the daemon is usable.

⚠️ **A Docker run cannot run in a `restricted` namespace.** Those same two fields
are precisely what Pod Security Standards `restricted` forbids, and the chart's Job
template otherwise satisfies it. Under
`pod-security.kubernetes.io/enforce=restricted` the Job is created and every pod is
rejected at admission, so the run sits with no pods until `activeDeadlineSeconds`
and is reported as a deadline with no hint of the cause. Such a namespace needs
`baseline` before `-docker` is used. A default run is unaffected.

⚠️ **`bwrap` does not work on this runsc, in the base image, with the wrap off.**
`bwrap --ro-bind / / true` fails `Can't open source /: Function not implemented` at
uid 1000 — identically in all four images, so the wrap neither causes nor fixes it.
The spike's green result does not reproduce; that is #22's thread, not the language
images'.

`e2e/85-language-images.sh` is the half no unit test reaches: each image cloning a
real repository of its language through the proxy and building and testing it, plus
the `go` image running Docker under gVisor. It needs stages 10–30 (a working
cert-manager) and never skips.

### The orchestrator's informer

`orchestrator watch` is ADR 0004's informer half. It watches the Jobs in its
namespace, reads their pods on demand, and gives every ending one issue comment —
built from the run plan's blob when there is one, and from the Kubernetes-level
reason when there is not.

```sh
POD_NAMESPACE=… GITHUB_APP_ID=… GITHUB_APP_PROVIDER=gcp GITHUB_APP_KEY=… \
  orchestrator watch          # the informer, until SIGTERM
orchestrator watch -once      # relist, report what has ended, exit
```

Three things about it are load-bearing and easy to "simplify" back out:

- **The second shape is the reason it exists.** `OOMKilled`,
  `activeDeadlineSeconds`, eviction and `ImagePullBackOff` run no in-pod code at
  all, so nothing inside the sandbox can report them. Delete this component and a
  run that dies that way leaves the issue labelled `ready-for-agent` with nobody
  ever told it happened.
- **The Job is the trigger; the pod is the result.** The blob is the container's
  terminated `message` and the digest is its `imageID`, so the pod is read (by run
  annotation, not by job name) on every report — but nothing *watches* one. The Job's
  terminal condition is the only signal that covers every ending: the controller
  defers it until the pods are terminal (1.31), so the blob is already readable when
  it arrives, and when the deadline expires it **deletes** the pod, leaving the
  condition as the sole record of the one ending a human has no other way to learn
  about. A Pod informer as well would only ever fire too early to report or after
  the Job event already did.
- **The exactly-once record is the comment.** `<!-- implementer-run: <uid> -->` on
  its first line, scanned for before every write. There is no database (ADR 0004)
  and the RBAC is read-only on Pods and Jobs, so there is nowhere in Kubernetes to
  mark a run as reported — which is also why nothing here needs to survive a
  restart, and why the in-process `done` set is a cost optimisation and never the
  argument.

`GITHUB_API_URL` is the seam, handed to both clients — ghait's mint path and the
orchestrator's own calls — and it is what stage 90 points at a mock.

### The Kubernetes versions this targets

**1.35–1.37**, which is what upstream supports as of 2026-09-01, and it moves: a
version leaves this window roughly every four months. Scope to what those releases
have and nothing older. Concretely, that means no compatibility branch for a
behaviour a supported release does not exhibit — the pod carries
`batch.kubernetes.io/job-name` (`batchv1.JobNameLabel`, GA since 1.27), so the
informer reads that and not the unprefixed legacy label, and the Job controller
defers the terminal condition until its pods are terminal (1.31), which is what
makes stage 90's "the pod is gone" assertion reliable rather than a race.

A feature younger than 1.35 is fair game. A workaround for one older than it is
dead code that nobody will ever delete, because nobody can prove it is unreachable.

### Running the credentialed e2e stages

Stage 50 needs a real GitHub token. `gh auth token` prints the logged-in one, so
there is nothing to create and nothing to leak into a file:

```sh
E2E_GITHUB_TOKEN=$(gh auth token) E2E_GITHUB_REPO=nissessenap/scratch make e2e
```

It needs `repo` scope, which `gh auth login` grants by default — check with
`gh auth status`.

Stage 55 needs nothing and never skips: OpenBao in dev mode in the cluster, a key
it generates itself, and `e2e/transit-import.sh` to get that key into transit —
OpenBao has no `bao transit import`, so that script is the wrapping protocol by
hand. Stage 60 signs through the same OpenBao, which is why there is no App-key
Secret in the cluster any more: the PEM is read on this machine and imported.

Stage 60 mints as a **GitHub App** and `gh` has no token for that, so it stays
skipped unless pointed at a real one. What the App needs:

- **Contents: read & write** (the clone and the push probe) plus Metadata: read.
  No installation id anywhere — the proxy resolves one per run from the run's own
  repository.
- `E2E_GITHUB_APP_KEY` is a **path** to GitHub's downloaded PEM, byte-for-byte
  (PKCS#1, `BEGIN RSA PRIVATE KEY`). Not the key's contents: the harness converts
  the file and wraps it into transit, and an inline key is one `echo` away from a
  transcript.
- `E2E_GITHUB_OTHER_REPO` is optional and only earns its keep when it is
  **private *and* in the same installation**. Public and the clone succeeds
  anonymously — a false FAIL. Outside the installation and it fails for the wrong
  reason — a false pass that proves nothing about the scoping.

```sh
E2E_GITHUB_APP_ID=… E2E_GITHUB_APP_KEY=~/.secrets/app.pem \
  E2E_GITHUB_REPO=me/scratch E2E_GITHUB_OTHER_REPO=me/other-private ./e2e/60-github-mint.sh
```

Stage 80 is the orchestrator's, and its clone half never skips: the builder derives
the run credential, `charts/orchestrator` renders the PodSpec, and the pod clones a
public repository through the proxy holding nothing of its own. It installs the
orchestrator chart itself and needs no credential for that half.

Its **push** half is the one that needs one, and stage 80 deliberately does not
install it — it reads off the proxy Deployment whether stage 50 or 60 left a GitHub
credential there, because the static and minted ones are not interchangeable and the
chart refuses to render both. So the full stage is:

```sh
E2E_GITHUB_TOKEN=$(gh auth token) E2E_GITHUB_REPO=me/scratch ./e2e/50-github-swap.sh
E2E_GITHUB_REPO=me/scratch ./e2e/80-orchestrator.sh
```

`E2E_GITHUB_REPO` is the run's *identity* there, not just the push target: a minted
token is scoped to the annotations rather than to the URL, so pushing anywhere else
fails for a reason that has nothing to do with the builder. The anonymous clone uses
`E2E_CLONE_REPO` (default `nissessenap/the-implementer`) instead, because the push
target may be private.

The harness no longer derives a run credential with `openssl` — `run_cred` shells out
to `orchestrator cred`, so the HMAC has one implementation.

There is no GAR e2e stage, deliberately: the proxy's Google identity is Workload
Identity and the chart offers nowhere to mount a service account key, so a cluster
with no metadata server cannot turn it on. Do not add one — `proxy/gar_test.go`
proves the same path (attach on `-python.pkg.dev`, tokenless on `-docker.pkg.dev`,
through a real interception) against a fake token source, which is what runs
locally and in CI.

Against a real cluster, check the proxy log names the identity you meant: with no
Workload Identity binding the metadata server hands out the *node pool's* service
account and everything works, which is the one way to get this wrong invisibly.

That service account needs `roles/artifactregistry.reader` and `roles/aiplatform.user`,
and nothing else new — one identity for both Google-facing halves.

Stage 70, the model route, needs no credentials and never skips: it runs against a
**mock** Vertex behind the `vertex.upstream` seam, which also stubs the token. Do
not replace it with a stage that mounts a Google credential — the reason is GAR's
above, spelled out in ADR 0005, and what the mock cannot prove is pinned by
`proxy/vertex_test.go`.

Stage 95, the trigger, needs no credentials and never skips: the front-end makes
no API call at all, so the payload is the whole of the input and the stage signs its
own. The delivery is a POST at the **Service** through a port-forward rather than a
public endpoint, which is what makes it runnable unattended — the alternative is
ngrok, a real App, and a stage that only ever passes on a laptop. Each ignored case
uses its own issue number, because sharing one would make "still exactly one Job"
true whether the case was ignored or wrongly started a run for an issue that already
had one.

Stage 90, the informer, needs no credentials and never skips either: GitHub is a
**mock** behind the `GITHUB_API_URL` seam, and the App JWT is signed by ghait's
`file` provider against a key the stage generates and throws away. Production links
`ghait.no_file` and cannot sign that way at all, which is why `make test` builds
`./cmd/orchestrator` under the production tags.

What only a cluster can prove there is the ending with no result: the stage patches
`activeDeadlineSeconds` down on a running Job, the Job controller deletes the pod,
and the comment is built from the Job's condition alone. Do not replace that with a
unit test — `orchestrator/informer_test.go` already covers the decision, and what
this stage covers is that the pod really is gone.
