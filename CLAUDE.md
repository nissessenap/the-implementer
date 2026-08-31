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

### The orchestrator's informer

`orchestrator watch` is ADR 0004's informer half. It watches Pods **and** Jobs in
its namespace and gives every ending one issue comment — built from the run plan's
blob when there is one, and from the Kubernetes-level reason when there is not.

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
- **Jobs are watched as well as Pods, and not for symmetry.** The *result* is
  pod-level — the blob is the container's terminated `message`, the digest is its
  `imageID` — but when the deadline expires the Job controller **deletes** the pod,
  so the Job's condition is the only record of the one ending a human has no other
  way to learn about.
- **The exactly-once record is the comment.** `<!-- implementer-run: <uid> -->` on
  its first line, scanned for before every write. There is no database (ADR 0004)
  and the RBAC is read-only on Pods and Jobs, so there is nowhere in Kubernetes to
  mark a run as reported — which is also why nothing here needs to survive a
  restart.

`GITHUB_API_URL` is the seam, handed to both clients — ghait's mint path and the
orchestrator's own calls — and it is what stage 90 points at a mock.

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
