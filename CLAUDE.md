# The implementer

## Agent skills

### Issue tracker

Issues live as GitHub issues, managed with the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical triage roles, used as-is (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.

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
