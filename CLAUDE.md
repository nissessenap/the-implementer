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
