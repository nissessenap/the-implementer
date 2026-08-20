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

Stage 60 mints as a **GitHub App** and `gh` has no token for that, so it stays
skipped unless pointed at a real one. What the App needs:

- **Contents: read & write** (the clone and the push probe) plus Metadata: read.
  No installation id anywhere — the proxy resolves one per run from the run's own
  repository.
- `E2E_GITHUB_APP_KEY` is a **path** to GitHub's downloaded PEM, byte-for-byte
  (PKCS#1, `BEGIN RSA PRIVATE KEY`). Not the key's contents: the harness mounts
  the file as a Secret, and an inline key is one `echo` away from a transcript.
- `E2E_GITHUB_OTHER_REPO` is optional and only earns its keep when it is
  **private *and* in the same installation**. Public and the clone succeeds
  anonymously — a false FAIL. Outside the installation and it fails for the wrong
  reason — a false pass that proves nothing about the scoping.

```sh
E2E_GITHUB_APP_ID=… E2E_GITHUB_APP_KEY=~/.secrets/app.pem \
  E2E_GITHUB_REPO=me/scratch E2E_GITHUB_OTHER_REPO=me/other-private ./e2e/60-github-mint.sh
```

Stage 70 (GAR) is skipped on kind and k3s **by construction**, not by choice: the
proxy's Google identity is Workload Identity and the chart offers nowhere to mount
a service account key, so a cluster with no metadata server cannot turn it on. Do
not add one — `proxy/gar_test.go` proves the same path (attach on
`-python.pkg.dev`, tokenless on `-docker.pkg.dev`, through a real interception)
against a fake token source, which is what runs locally and in CI.

Against GKE, where it can run — the whole harness, because stage 70 upgrades the
release the earlier stages installed rather than installing one itself:

`E2E_ALLOW_REMOTE=1` alone is refused: it also needs `E2E_EXPECT_CLUSTER`, a
substring of the API server URL you meant. `make e2e` installs a namespace, a CA
and a Deployment into whatever context is current, and on this laptop that is a
production cluster unless something names the one you intended.

```sh
E2E_ALLOW_REMOTE=1 E2E_EXPECT_CLUSTER=my-sandbox-cluster \
  E2E_GAR_INDEX=https://europe-west1-python.pkg.dev/proj/repo/simple/ \
  E2E_GAR_PACKAGE=my-private-package E2E_GAR_GSA=proxy@proj.iam.gserviceaccount.com \
  make e2e
```

Check the proxy log names the identity you meant: with no Workload Identity binding
the metadata server hands out the *node pool's* service account and everything
works, which is the one way to get this wrong invisibly.

That service account needs `roles/artifactregistry.reader` and nothing else new.
