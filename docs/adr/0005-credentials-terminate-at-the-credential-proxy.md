# 5. Credentials terminate at the credential proxy; the sandbox holds none

Date: 2026-08-02

## Status

Accepted

## Context

A run combines three things that security guidance says never to combine:
**untrusted input** (issue text), **secrets**, and **the ability to act on the
outside world**. Two published incidents target exactly this design:

- Microsoft found Claude Code's `Read` tool could reach `/proc/self/environ` and
  exfiltrate `ANTHROPIC_API_KEY`, laundered through a prompt framed as a
  "compliance review" ([writeup][ms]). Fixed in 2.1.128 by rejecting `/proc/`
  reads — but the *class* is env-var-resident credentials, which only removal
  closes.
- flatt.tech bypassed `claude-code-action`'s trigger permission check via GitHub
  App events, reaching environment and OIDC exfiltration ([writeup][flatt]).

Anthropic's own hosted product answers this architecturally rather than
hygienically: *"sensitive credentials such as git credentials or signing keys
are **never inside the sandbox**… Authentication is handled through a secure
proxy using scoped credentials."* Inside the VM `GH_TOKEN` reads as the literal
string `proxy-injected`. Their general recommendation: *"run a proxy outside the
agent's security boundary that injects credentials into outgoing requests."*

The map spent four tickets and two prototypes reaching the same shape. The route
matters because two of its turns were reversals:

- The credential ticket [#14][credentials] initially held that *"the proxy
  answers GitHub, not Anthropic"*, on the reasoning that the agent CLI **is** the
  API client, so there is no request we can add a header to that the CLI was not
  already making. **That is wrong**, and the correction is first-party: Claude
  Code's [gateway mode][gateway] documents `CLAUDE_CODE_SKIP_VERTEX_AUTH=1` plus
  `ANTHROPIC_VERTEX_BASE_URL` — *"the skip-auth variables tell Claude Code not to
  sign requests with provider credentials, since the gateway holds those."* So
  point-the-base-URL-at-our-proxy and Vertex-with-workload-identity are not
  competing options; they **compose**.
- The [egress decision][egress] recorded that `HTTPS_PROXY` is *"routing, not
  enforcement (Node's `fetch()` ignores it)"*. The first clause stands; the
  parenthetical is wrong for Claude Code, which [respects the standard proxy
  variables][netcfg], using the first of
  `https_proxy`/`HTTPS_PROXY`/`http_proxy`/`HTTP_PROXY`. No SOCKS.

Both halves were then built and measured on branch `proto/credential-proxy-33`
([PR #35][proto], throwaway), under gVisor at uid 1000, against a real issue.

## Decision

**Every credential a run needs terminates at a credential proxy. The sandbox
holds none.**

The proxy is a separate Deployment beside the orchestrator — **not a sidecar**,
because a sidecar shares the pod's network namespace and can be bypassed unless
iptables rules are added *inside* the pod, which needs `NET_ADMIN` in the very
container being constrained.

```
┌─ Sandbox (gVisor Job) ─────────┐        ┌─ Credential proxy ───────────┐
│ GH_TOKEN=proxy-injected        │        │ KSA: aiplatform.user         │
│ CLAUDE_CODE_SKIP_VERTEX_AUTH=1 │        │      artifactregistry.reader │
│ https_proxy=http://proxy:…     │───────►│      cloudkms.signer         │
│ SSL_CERT_FILE=<bundle>         │        │ cert-manager CA + leaf       │
│ …no credential of any kind     │        └───────┬──────────────────────┘
└────────────────────────────────┘                │
                                      Vertex ◄────┼────► GitHub
                                         GAR ◄────┘
```

### Model access: Vertex, terminated at the proxy

The sandbox gets five plain-config environment variables and no credential:
`ANTHROPIC_VERTEX_BASE_URL` (pointing at the proxy), `ANTHROPIC_VERTEX_PROJECT_ID`,
`CLOUD_ML_REGION`, `CLAUDE_CODE_USE_VERTEX=1`, `CLAUDE_CODE_SKIP_VERTEX_AUTH=1`.
The proxy is `httputil.ReverseProxy` plus `golang.org/x/oauth2/google`; its
Kubernetes ServiceAccount carries `roles/aiplatform.user` through Workload
Identity Federation.

Measured: a real implement run completed with `CLAUDE_CODE_OAUTH_TOKEN`
**omitted from the Secret entirely** — not blanked, omitted, so the run passing
is proof rather than assertion. SSE streaming survives `ReverseProxy` natively,
no `FlushInterval` tuning; deltas ~20 ms apart on a turn logged
`ttfb=1.5s total=56s`.

### GitHub: a sentinel, and a TLS-terminating swap

The sandbox's `GH_TOKEN` is the literal string `proxy-injected`. The proxy
terminates TLS for `github.com` with a cert-manager leaf and substitutes a real
installation token in flight.

Verified with free preflight probes, no agent turn: the sandbox sent the
sentinel and got `rate_limit=5000` — GitHub gives 60/h anonymous and 5000/h
authenticated, so the number is only reachable if the swap happened inside a TLS
session the sandbox believed was GitHub's. Plus `clone=ok push-dry-run=ok`.

**Two client behaviours that do not generalise to each other**, and a proxy
built to either alone breaks the other silently:

- **`git` does not send credentials preemptively.** It makes an anonymous
  request, takes the `401`, and only then retries with HTTP basic. The swap must
  survive the challenge round-trip; a header-only proxy breaks `git push` while
  `gh` keeps working. Corollary: on a public repo `clone` never authenticates at
  all, so **push-branch enforcement hangs off `service=git-receive-pack`**, not
  off "requests carrying a token".
- **`go` inverts the rule** — it sends nothing, ever, and does not retry on
  `401`, so the proxy must attach unconditionally on the first request.

Both credential *shapes* are handled: `Basic base64(x-access-token:SENTINEL)`
(git's, from the clone URL's userinfo) and `token`/`Bearer SENTINEL` (`gh`'s).
Non-sentinel credentials pass through untouched **and logged** — swallowing a
smuggled one would only hide it.

### The proxy mints, and Cloud KMS signs the App JWT

The proxy never receives a token from the orchestrator. There is no per-run
Secret, no orchestrator→proxy channel and no handover lifecycle, and token
refresh across the 1 h installation-token boundary comes free.

The fork was not really orchestrator-mints vs proxy-mints; it was **shared proxy
vs per-run proxy**. A shared Deployment must resolve run identity on every
request anyway, just to know which repository a token is for — and once you are
paying for that lookup, orchestrator-mints buys nothing. A per-run sidecar would
make identity free but put the App key in *every* run's pod, which is strictly
worse. The [egress decision][egress] already chose a shared pod.

`cryptoKeyVersions.asymmetricSign` with `RSA_SIGN_PKCS1_2048_SHA256` — exactly
RS256 — signs the App JWT, bound to a KSA via `roles/cloudkms.signer`. What the
proxy holds is not a key but a **revocable, audit-logged capability**. That is
what makes one minting component defensible rather than a concentration of risk:
the entire objection to proxy-mints is "a second holder of the key that mints for
every installation", and with KMS there is no holder.

> ⚠️ **Non-negotiable: mint for the repository named in the run's Job
> annotation, never for the repository the request URL names.** [ADR 0004][adr4]
> already puts run identity in annotations, so source pod IP → Pod → annotation
> is nearly free. Get this wrong and a compromised sandbox pushes to every
> repository the App is installed on.

Practical notes for implementation:

- KMS here is **import**, not generate-in-place. GitHub generates the App key
  and hands out a PEM; there is no bring-your-own-public-key ([docs][ghkeys]).
  So: GitHub generates → wrapped [import job][kmsimport] → destroy every local
  copy. The honest claim is *"the key stops existing anywhere we control and can
  never be read back"*, not *"never existed as bytes"*.
- KMS wants imported asymmetric keys as **PKCS#8 DER**; GitHub emits **PKCS#1
  PEM**. One `openssl pkcs8 -topk8`, but it eats an afternoon if discovered
  mid-import.
- Send the **digest**, not the message. In Go, a custom `golang-jwt`
  `SigningMethod` is the hook: JWT assembly stays normal, only `Sign()` becomes
  an API call.
- GitHub caps the JWT `exp` at 10 minutes, so signing is per-mint. The
  installation token is cached for the run — a handful of KMS calls per run, not
  per request.

**Seam, not work:** KMS ties this to GCP. Signing stays behind one interface so
a plain-key signer for a non-GCP operator is a later addition rather than a
rewrite. The second implementation is deliberately **not** built now.

### Registries: the same GCP token

`credFor()` is a per-host switch on the proxy: GitHub → swap the sentinel;
`{region}-go.pkg.dev` and `{region}-python.pkg.dev` → attach the GCP bearer;
anything else on the certificate → nothing. It reuses the `oauth2.TokenSource`
the Vertex half already holds, so no new secret, mount or scope — only
`roles/artifactregistry.reader` alongside `roles/aiplatform.user`.

Proven end to end: a real private wheel installed by `pip` into a
`python:3.13-slim` pod holding no GCP credential of any kind, no `gcloud`, no
`.netrc`. `go mod download` is unproven **only** because no Go repository exists
in the project to point it at — identical mechanism, a coverage gap rather than
a confidence gap. `{region}-docker.pkg.dev` is deliberately excluded: its
challenge/response dance is unmeasured and blob fetches redirect.

**The certificate is deliberately wider than the credential rule, and that is
load-bearing.** The SAN is `*.pkg.dev` because `crypto/x509` only matches a
wildcard occupying the whole leftmost label — `*-go.pkg.dev` issues fine from
cert-manager and is then rejected by every client. So `credFor` is what keeps
`-docker.pkg.dev` from being handed a token. The inverse is structural
protection: the wheel download 307-redirects to `/artifacts-downloads/` **on the
same host**, so it stays intercepted and still gets its token, whereas an
artifact store redirecting *off*-host lands on a name the certificate does not
cover, falls through to a plain `CONNECT` tunnel, and receives nothing. "Must
not leak the token to pre-signed storage URLs" is enforced by the certificate
rather than by remembering to check.

The proxy reads its intercept list back off its own certificate's SANs, so the
two cannot drift. Keep that property.

### The trust seam: five variables, and four of them replace

The sandbox must trust the proxy's CA. Under `readOnlyRootFilesystem: true` that
means environment variables, never `update-ca-certificates`.

| tool | reads |
| --- | --- |
| `gh`, anything Go (`crypto/x509`) | `SSL_CERT_FILE` |
| `git` (libcurl) | `GIT_SSL_CAINFO` |
| `curl` | `CURL_CA_BUNDLE` |
| `pip` | `PIP_CERT` — carries its own bundle, ignores `SSL_CERT_FILE` entirely |
| agent CLI (Node) | `NODE_EXTRA_CA_CERTS` |

**Everything except `NODE_EXTRA_CA_CERTS` *replaces* the trust store rather than
adding to it.** Point them at `ca.crt` alone and the sandbox verifies the proxy
perfectly and nothing else on the internet — failing on the first unrelated
HTTPS call, far from its cause. The value must be the system bundle
concatenated with the proxy's CA. Assembled by the **phase script at run-plan
start**; see [ADR 0001][adr1], which this was folded into.

cert-manager needs **two** Issuers, because a self-signed *leaf* is not a CA and
nothing chains to it: `selfSigned` Issuer → CA `Certificate` → `ca` Issuer →
leaf.

### Routing is not enforcement

`https_proxy` in the sandbox catches `git`, `gh` and `go` cleanly —
`proxy.golang.org`, `sum.golang.org`, `github.com` and `api.github.com` all
arrive as `CONNECT`, and that log is the starting inventory for an egress
allowlist. But proxy environment variables are **best-effort routing**. What
*enforces* is a `NetworkPolicy` permitting only the proxy pod, which is the
[egress decision][egress]'s target state and is not in MVP. See
[the architecture document][arch] for what MVP actually ships.

## Consequences

- **No proxy, no runs.** The proxy is on the critical path for every model call.
  It is a Deployment beside the orchestrator, so no new failure *class*, but the
  dependency is real and unconditional.
- **Three cluster prerequisites**, all of which land on the deployment story:
  **Workload Identity Federation**, **cert-manager**, and **Cloud KMS** with the
  App key imported.
- ⚠️ **The sandbox's KSA must carry no Workload Identity binding, and this must
  fail loudly if violated.** k3s has no metadata server, so during prototyping
  the sandbox *could not* obtain a GCP token even by accident — an absence doing
  work no configuration of ours was doing. On GKE `169.254.169.254` is reachable
  from every pod, so "the sandbox holds no GCP credential" stops being a
  property of the cluster and becomes a property of configuration.
- **Two components authenticate as the App** — the orchestrator for detection,
  the PR and the issue comment ([ADR 0004][adr4]); the proxy for the sandbox's
  token. Both sign through KMS with their own KSA, so the number of key *holders*
  is still zero.
- **The delivery-as-a-file decision is retired.** [#14][credentials] specified
  the GitHub token as a mounted file at `/run/secrets/gh/token` rather than an
  environment variable, to close the `/proc/self/environ` channel. Once the
  value is a worthless sentinel there is nothing to protect: `GH_TOKEN` is a
  plain environment variable again, and `proto/phase.sh` builds
  `https://x-access-token:${GH_TOKEN}@github.com/…` unchanged. **The seam
  behaved exactly as predicted — the phase script's credential path never
  changed.**
- **Push-branch enforcement becomes available**, retiring a limit this project
  accepted from charting onward: GitHub cannot scope a token to a branch prefix,
  and the repo-owner branch ruleset that papers over it is advisory. A proxy can
  express it. This is the strongest single reason the expensive half was worth
  building.
- **Cost in latency: an upper bound of ~23 %** (202 s → 249 s), and that bundles
  the proxy hop with a *provider* and *model* change, so it overstates the hop.
  The GitHub interception is invisible — one extra termination against 150–350 ms
  round trips.
- ⚠️ **The model pin is the customer's project, not our release.** The
  prototype's GCP project could invoke only `claude-sonnet-4-6` and
  `claude-haiku-4-5`, with `claude-opus-5`, `claude-sonnet-5` and `claude-opus-4-8`
  all 404. So all four model pins are Helm values, and the `opus` **alias** must
  be remapped or `/code-review`'s subagents 404. "Zero credentials in the
  sandbox" is paid for in whatever models the operator's project has enabled.
- **`--max-budget-usd` still binds but stops being an accounting figure.**
  Claude Code computes cost locally at Anthropic list rates, so on Vertex it is a
  runaway guard that does not match the GCP bill. Say so in the docs.
- **A known prototype bug is a production requirement**: the proxy calls
  `tls.LoadX509KeyPair` once at startup and never again, so it serves the
  boot-time certificate past renewal and past expiry. The failure mode is a
  long-lived *proxy pod*, not a long-lived run — a Job caps at
  `activeDeadlineSeconds`, so no run can straddle a renewal. Fix with
  `tls.Config.GetCertificate` from a reloader.

## Alternatives rejected

- **Workload Identity in the sandbox.** It is available and Google recommends it
  for sandboxed pods, but it is **key**-termination, not **credential**-termination
  — an agent that can reach the metadata server can mint a GCP token and
  exfiltrate it. Decisively, it is mutually exclusive with GKE's own hardening
  advice on the same page: *"block cluster metadata access using Network Policy
  to block access to 169.254.169.254"*. WIF **serves** tokens from that address.
  You cannot both give the sandbox an identity and take that advice.
- **An API key or OAuth token in the pod, with `apiKeyHelper` for freshness.**
  The interim mechanism Anthropic point at, and strictly better than a static
  org key — but it keeps a live credential in the same context as untrusted
  issue text, which is the combination both published incidents exploit.
- **A sidecar proxy.** Shares the pod network namespace, so it is a cooperative
  boundary rather than a real one. `gh-aw` does this successfully in Docker via
  DNAT with `NET_ADMIN` dropped before user code runs; in Kubernetes a separate
  pod plus a `NetworkPolicy` is a genuine boundary.
- **Orchestrator mints and hands over.** Keeps the App key in one component —
  but with KMS there is no key to concentrate, and it costs a per-run Secret
  lifecycle, its RBAC, its orphan sweep, and a new orchestrator→proxy channel.
  For the record the channel is possible: a per-run Secret the proxy mounts
  (needs a restart, so no), an admin endpoint, or the proxy reading a per-run
  Secret through the API keyed by run identity. The third is the one to reach
  for if this is ever revisited.
- **Claude Code's own `sandbox.credentials` masking.** Cheap, orthogonal and
  worth turning on, but it protects the key from the tools the CLI runs — the
  Microsoft `/proc/self/environ` class — not from the CLI itself. It answers a
  different question.
- **Trusting only the proxy's `ca.crt`.** Rejected by measurement; see the
  replace-vs-add trap above.

[adr1]: 0001-sandbox-image-strategy-and-byo-contract.md
[adr4]: 0004-the-orchestrator-is-a-controller-with-a-webhook-front-end.md
[arch]: ../architecture.md
[map]: https://github.com/nissessenap/the-implementer/issues/1
[credentials]: https://github.com/nissessenap/the-implementer/issues/14
[egress]: https://github.com/nissessenap/the-implementer/issues/16
[registries]: https://github.com/nissessenap/the-implementer/issues/19
[proto33]: https://github.com/nissessenap/the-implementer/issues/33
[proto34]: https://github.com/nissessenap/the-implementer/issues/34
[mint]: https://github.com/nissessenap/the-implementer/issues/36
[build]: https://github.com/nissessenap/the-implementer/issues/37
[proto]: https://github.com/nissessenap/the-implementer/pull/35
[gateway]: https://code.claude.com/docs/en/llm-gateway-connect#route-to-a-cloud-provider-through-a-gateway
[netcfg]: https://code.claude.com/docs/en/network-config#proxy-configuration
[ghkeys]: https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/managing-private-keys-for-github-apps
[kmsimport]: https://docs.cloud.google.com/kms/docs/key-import
[ms]: https://www.microsoft.com/en-us/security/blog/2026/06/05/securing-ci-cd-in-agentic-world-claude-code-github-action-case/
[flatt]: https://flatt.tech/research/posts/poisoning-claude-code-one-github-issue-to-break-the-supply-chain/
