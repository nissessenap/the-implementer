# agentgateway revisited: should the proxy be replaced by it?

**Date:** 2026-10-21 · **Follows:** [credential-brokers-and-whether-to-buy-the-proxy.md][prior]
(2026-08-06, which read Part 1 of the blog series and audited agentgateway at `db62d60`).

**Question asked:** Posta's series ([Part 1][p1], [Part 2][p2], [Part 2.5][p25]) reads as
though agentgateway already solves credential injection. Should we drop `proxy/` and run
agentgateway instead?

**Short answer: not the blogs. Maybe the Substrate egress path, pending one prototype.**

The blog series describes something different from what we need, and the parts that
are not OSS are the parts that would matter.

The **source code**, however, has moved since August:
- The new Substrate egress path has the same shape as ADR 0005. A caller identity selects
  a per-caller policy, and the policy names credentials that an external service resolves
  (§4).
- The one unproven link is how a sandbox presents that identity, which is an mTLS client
  certificate on the proxy connection.
- Two of the four blockers from the prior note are gone or softened. The other two are
  exactly the parts of our proxy that are hard to write:

| Prior blocker | 2026-08-06 | 2026-10-21 (`974e1a30`) |
| --- | --- | --- |
| Selective MITM by hostname ([#2416][ag2416]) | open | **closed, no code change**: maintainer says SNI listeners on an internal bind already do it. Unverified by us. |
| App private key would have to live in the proxy | yes | **avoidable** via the Substrate `CredentialProvider` gRPC seam, but we would still write the minter |
| Per-run identity from pod annotations | absent | **still absent**. Only Substrate's SPIFFE actor certs. |
| `git-receive-pack` ref enforcement | absent | **absent** (ours is not built yet either) |

ADR 0005's own trigger was *"Revisit agentgateway only if #2416 lands and a KMS signer
appears"* ([ADR 0005][adr5] l.527-539). The first half is arguably met. The second is not.

**How these sources were read:**
- **The blog posts:** fetched with curl and read in full.
- **agentgateway:** a research agent audited `origin/main` at `974e1a30` in a scratch
  worktree, covering the 728 commits since `db62d60`.
- **Issues and PRs:** taken from `gh`.
- **How far to trust the citations:** `file:line` citations into agentgateway are the
  agent's. Re-verify one before building anything on it.

---

## 1. The blog series: Solo Enterprise, MCP, human OAuth

All three posts are by Christian Posta (Solo.io), and they describe the same product.

- **[Part 1][p1] (07-27):** *"the pattern we implement in Solo Enterprise for
  agentgateway"*.
  - The OSS part is `backendAuth.oauthTokenExchange`, which covers RFC 8693, RFC 7523 and
    Entra OBO.
  - The flow is an MCP client logging in through an IdP, followed by elicitation of a
    *human's* third-party OAuth consent.
  - The resulting tokens are stored *"keyed by user identity and resource"*.
  - On GitHub: *"there's no exchange path from your corporate IdP to a GitHub token… You
    can't mint it."*
- **[Part 2][p2] (08-03, updated Oct):** envelope-encrypts the token vault. The "modes" are
  ways of guarding the key that wraps the vault (the KEK), not ways of injecting a
  credential.
  - **Mode 1:** the KEK lives in a k8s Secret.
  - **Mode 2:** the KEK lives in AWS KMS, and the controller holds `kms:Decrypt`.
  - **Mode 3:** a separate broker gates KMS grants per owner.
  - Identity is per *user* (the IdP `sub`), not per workload.
- **[Part 2.5][p25] (10-20):** KMS grants stop scaling at 50 rps and 50k grants per key,
  so the broker does the Decrypt instead. A fourth component, the "refresher", is added.
  The post narrows its own claim to: *"The rows that are in use leak as fast as they get
  used"*. No config, no code, no PRs. Part 3 (Vault/OpenBao) is unpublished.

None of the three mentions CONNECT, MITM, git, basic auth, GitHub App installation tokens,
or KMS *signing*. Their problem is a vault of long-lived user tokens. **We have no vault:**
our credentials are minted per run and expire in an hour, which is the thing Part 2.5 ends
up wishing for (*"Long-lived PATs, API keys, etc have no short-lived half"*).

The one idea that carries over is to put trust domains in separate processes. We already
do that: the proxy holds credentials, and the orchestrator holds the run key.

## 2. What agentgateway OSS can do at `974e1a30`

The code is Apache-2.0, and no enterprise gating was found in it.

| Our need | agentgateway | Evidence |
| --- | --- | --- |
| CONNECT + MITM with generated CA | yes | [#1846][ag1846], [#2134][ag2134] `dynamicCa` |
| Selective MITM, tunnel the rest | yes, via SNI listeners on an internal bind (not via the CONNECT authority) | #2416 close comment; `examples/traffic-egress-proxy/config-conditional.yaml:9-46`; re-entry is still by port, `proxy/gateway.rs:749-781` |
| Replace client `Authorization` (Bearer) | yes, `headers_mut().insert` | `http/auth/mod.rs:520-530` |
| git basic auth `x-access-token:<tok>` | only from a static or CEL value (`basic_header()` + `base64.encode`). No sentinel swap. | `auth/mod.rs:462-467`, `celx/src/general.rs:42-45` |
| Substrate placeholder swap | replaces only headers the client already sent; refuses `proxy-authorization`/`host` | [#3411][ag3411], `http/substrate/egress.rs:196-216,371-384` |
| Mint GitHub App token, KMS-signed JWT | **no**. `jwtSign` loads a PEM, and there is no kms/transit/vault/installation code. | `http/auth/jwt_sign.rs:98,131` |
| External credential seam | **new**: `CredentialProvider.FetchSecret(uri, actor_spiffe_id)`, cached for 300s | `protos/proto/credprovider.proto:7-19` |
| Per-run identity → per-run credential | Substrate only: mTLS SPIFFE SAN of the CONNECT client. The generic path has only name/namespace/SA, with no annotations. | `substrate/egress_actor_resolution.rs:31-60`, `cel/types.rs:356-368` |
| `git-receive-pack` ref restriction | no (the ext_proc seam is the place to add it) | grep: none |
| Egress host allowlist | yes, `tcpRoutes[].hostnames` (Substrate: per actor, deny by default) | `examples/traffic-egress-proxy/config.yaml`, `egress.rs:523-560` |
| Vertex / Anthropic-on-Vertex with GCP ADC | yes | `llm/mod.rs:1204`, `crates/llm/src/vertex.rs` |
| GAR token on `-python`/`-go.pkg.dev` only | `backendAuth.gcp` on any backend; host-conditional via per-host listeners | `http/auth/gcp.rs:27-48` |

The Substrate egress path is the closest match to our shape: [#3318][ag3318],
[#3411][ag3411], [#3524][ag3524] and [#3990][ag3990]. But it is **4–7 weeks old, still
churning** (22 commits, the latest on 10-15), and wired to agent-substrate's `ateapi`
control plane and its SPIFFE worker certs.

## 3. Our proxy, for comparison

`proxy/` is **1654 lines** of non-test Go, with 2113 lines of tests. The proxy-inventory
agent cited it `file:line` against this repo.

| File | Lines | What it does |
| --- | --- | --- |
| `proxy.go` | 340 | CONNECT, the pin, the HTTP/1.1 loop |
| `creds.go` | 313 | the sentinel swap, both Basic and Bearer shapes |
| `vertex.go` | 238 | the Vertex route |
| `mint.go` | 160 | mints per run from the annotation's repo |
| `auth.go` | 156 | HMAC run credential + pod-IP agreement + rate limit |
| `pods.go` | 129 | pod-IP lookup |
| `cert.go` | 96 | intercept list read off the cert SANs |
| `identity.go` | 88 | the run claim and its HMAC |
| `gar.go` | 74 | GAR host rules |
| `google.go` | 60 | ADC, metadata server only |

If we adopted agentgateway, only the transport and the Google halves would be deleted:
`proxy.go`, `cert.go`, `vertex.go`, `gar.go` and `google.go`, about 800 lines. **What
we would still own** is the part that is actually load-bearing:

1. **The minter.** `mint.go` plus ghait, rewritten as a `CredentialProvider` gRPC service.
2. **Run identity.** The HMAC and pod-IP agreement (`auth.go`, `pods.go`, `identity.go`)
   has no home in generic agentgateway. Going through Substrate means issuing SPIFFE
   certs to sandboxes, and that is new infrastructure, not a deletion.
3. **The sentinel semantics.** The swap only happens if the sentinel is present, and a
   non-sentinel credential passes through and is logged. Substrate's "replace headers
   already present" is close but not the same. git's basic-auth userinfo case would need
   a test against it.
4. **Ref enforcement**, which neither side has built.

In return we would take on a Rust data plane of more than 200k lines, its CRD and config
vocabulary, and a dependency on a Substrate API that is still moving.

The arithmetic is unchanged from August: **delete about 800 lines we understand, keep
about 850, and write a gRPC service and SPIFFE issuance to glue them on.**

## 4. The Substrate egress path, in detail

This section was read first-hand at `origin/main`. The sources were the files under
`crates/agentgateway/src/http/substrate/` (`README.md`, `egress.rs`,
`egress_actor_resolution.rs`), the two protos `crates/protos/proto/ateapi.proto` and
`credprovider.proto`, and the PR descriptions of [#3318][ag3318], [#3411][ag3411],
[#3524][ag3524] and [#3990][ag3990].

This is the only part of agentgateway that has the same *shape* as ADR 0005. A caller
identity selects a per-caller policy. The policy names credentials. An external service
resolves those credentials. The gateway swaps a placeholder for the real value.

### 4.1 How it works

1. **Identity is mTLS on the CONNECT.**
   - The client cert must carry exactly one URI SAN,
     `spiffe://substrate-actor.local/worker-for-actor/<atespace>/<actor>`. The trust domain
     is hardcoded (`egress_actor_resolution.rs`).
   - A CA cert is refused, and so is a cert issued directly to the actor: *"a certificate
     issued directly to an actor must not authorize a tunnel"*. The cert is expected to
     belong to a worker that acts for the actor.
2. **There are two gRPC services, and we could serve both.** Neither needs Substrate itself
   to be running.
   - **`ateapi.Control`.** `GetActor` runs before CONNECT is answered.
     `GetActorEgressPolicy` runs at the inner ClientHello and again on every HTTP request,
     so revoking a rule takes effect on the next request. Both are configured as an
     ordinary backend, `substrateEgressActorResolution.target`.
   - **`credprovider.CredentialProvider`.** `FetchSecret(uri, actor_spiffe_id) →
     opaque_bytes`. The URI has the form `ate-secret://<provider>/<tail>`. The proto says
     *"The provider owns authorization and secret storage; AgentGateway only brokers"*.
     Results are cached for 300 s.
3. **Policy is allow rules matched on hostname and port.**
   - Rules come in three kinds: `HTTPRule`, `HTTPSRule` and `TLSPassthroughRule`. The most
     specific hostname wins, then the most specific port.
   - Anything unmatched is intercepted so that the gateway can answer 403.
   - A rule whose kind is HTTPS decides whether that host is MITM'd or passed through.
     This is our `cert.go` intercept list, decided per actor.
4. **Injection is `replace_headers`.**
   - Each entry is `{header, prefix, credential_uri | actor_jwt}`.
   - The comment on it reads *"Replace only headers already present in the actor's
     request"*. That is our sentinel pattern minus the sentinel check: *any* value in
     that header gets replaced, not only a sentinel value.
5. **The upstream is bound to the rule.**
   - The authority is authorized against the actor's rules independently of the SNI, and
     effects come only from the winning rule.
   - The upstream is that authority, resolved through DNS, on the original CONNECT
     port. Per the README, the IP from the CONNECT request, a port in the authority, and
     a target expression all *"cannot override that destination"*.
   - This closes the off-host leak that §2's generic `dynamic` backend may have.
6. **What it refuses:**
   - WebSocket upgrades and CONNECT nested inside the tunnel, both with fixed 403 bodies;
   - replacing the `proxy-authorization` and `host` headers (`egress.rs:371-384`).

### 4.2 Mapped onto ADR 0005

| Ours today | Under Substrate |
| --- | --- |
| `mint.go`, per-run cache, scope from annotations | A `FetchSecret` server we write. It maps the SPIFFE id to a run, then the run to `owner/repo`, then mints. The App key stays out of the gateway. **Re-authorize in the provider:** the gateway hands it only a URI and an id. |
| Two credential shapes: git `Basic` and `gh`'s `token` | Two rules on different hosts. `github.com` uses prefix `Basic ` and the provider returns `base64("x-access-token:"+tok)`. `api.github.com` uses prefix `token `. |
| Run annotations → identity → scope | A `Control` server, which the orchestrator could host: actor = run, and rules generated from the Job. **The egress allowlist (#16) comes for free here.** |
| HMAC run credential + pod-IP agreement (`auth.go`, `pods.go`, `identity.go`) | Replaced by the SPIFFE client cert. **New work:** a cert issuer for sandboxes, plus a way for every tool to present the cert on CONNECT. |
| `proxy.go`, `cert.go`, `creds.go` | Deleted. |
| GAR and Vertex tokens | Could be `FetchSecret` URIs that return an ADC token with prefix `Bearer `. |
| `vertex.go`: one model call, POST only | **Lost.** Rules match on hostname and port, never on path or method. It is unverified whether ordinary route policies can stack on top. |
| Push ref enforcement (not built) | Still ours. It would go in ext_proc or on the provider side. |

### 4.3 Against it

- **The client cert is the deciding unknown.**
  - **Partly answered:** on Substrate's own runtime, the cert never reaches the sandbox.
    nftables redirects TCP, and `atunnel` outside the sandbox holds the per-actor cert
    (see [substrate-instead-of-the-implementer.md][subnote] §3). Bare Jobs get no such
    thing, so the question below stands for them.
  - git has `http.proxySSLCert`/`http.proxySSLKey`, and curl has `--proxy-cert`.
  - Whether `gh` (Go) and Claude Code (Node) can present a client cert to an HTTPS proxy
    is **unchecked**.
  - The worker-for-actor identity suggests Substrate expects a separate worker process
    to hold the cert. For us that likely means a sidecar in every sandbox, plus the
    question of how the sandbox authenticates *to the sidecar*.
- **The contract is still moving.**
  - The README pins Substrate [PR #1751][sub1751]'s protocol-based API.
  - [#3524][ag3524] re-aligned the gateway to a new proto contract.
  - [#3990][ag3990] fixed an upstream rename of the SPIFFE path, from `ateom-for-actor` to
    `worker-for-actor`. Until it landed, every egress tunnel was refused with a 403 and
    the credential-injection tests broke.
  - That is three breaking shifts in roughly six weeks.
- **It is Substrate-shaped.** It has a hardcoded trust domain, the names `atespace` and
  actor, and resume semantics (`ResumeActor`). We would be implementing someone else's
  control-plane API by imitation, not consuming a stable one.

### 4.4 For it

- The data path stops being ours. MITM, the CA, HTTP/1.1 and HTTP/2, cert reload,
  header hygiene and the allowlist would all be agentgateway's to maintain.
- What remains ours are two small, stateless gRPC servers containing exactly the logic
  that is specific to us: mint, scope and policy. They sit off the data path, and they
  are easy to test.
- The design converges with ADR 0005. Placeholder replacement, credentials bound to the
  winning rule, and secrets fetched by an identity the caller cannot forge are the
  properties we argued for. A second implementation arriving at them independently is a
  signal.
- Its upstream has CI against gVisor and microVM sandboxes and a Helm e2e. The PR
  description of #3990 claims these pass with credential injection and trust rotation
  included.

## 5. Recommendation

- **Keep `proxy/` running. Seriously consider Substrate, gated on one prototype.** §4
  changes the August answer from "agentgateway is the wrong shape" to "it is the right
  shape with one unproven link".
- **The prototype should answer one question:** can a sandbox present a SPIFFE client
  cert on CONNECT for git, `gh`, and the model call without a sidecar? If not, what does
  the sidecar cost? It needs:
  - agentgateway standalone, configured with `substrateEgressActorResolution` and
    `substrateEgress`;
  - a stub of about 100 lines of Go serving `Control` + `CredentialProvider` (mint behind
    a fake);
  - a cert from a throwaway CA carrying a `worker-for-actor` SAN;
  - a run of `git clone`/`git push` and `gh api` through it.
- **The decision rule:** if all three tools work with a cert and no sidecar, write an ADR
  that supersedes the data-path half of ADR 0005, and keep `mint.go` as the provider.
  Otherwise, keep `proxy/`.
- **Either way,** don't start ref enforcement (#37 follow-up) inside `proxy.go` until this
  is decided.
- **Update ADR 0005's revisit trigger.** The current wording is overtaken: #2416 closed
  without the feature it asked for. A suggested replacement: *revisit when agentgateway's
  generic (non-Substrate) path can scope a `CredentialProvider` call to a caller identity
  we can issue, and the Substrate egress API has been stable for a release.*
- **The cheap hedge**, if we want one:
  - keep `mint.go` free of HTTP-proxy concerns, as it already is, so that it could be
    wrapped behind `credprovider.proto`'s `FetchSecret(uri, spiffe_id) → bytes` later;
  - look at that contract when building ref enforcement (#37's follow-up), because
    ext_proc is where agentgateway would want it.
- **Not worth doing yet:** a prototype that checks SNI-listener selective MITM with
  `dynamicCa`. It answers a question we would only ask after the identity gap closes.
- **Already done upstream:** [#3668][ag3668] (an MCP-auth RFC 9207 `iss` mismatch, no
  maintainer reply) is unrelated to this question. [ghait#119][ghait119] (the `vault:vN:`
  prefix after a transit key rotation) is merged and is in our ghait version.

[prior]: credential-brokers-and-whether-to-buy-the-proxy.md
[subnote]: substrate-instead-of-the-implementer.md
[adr5]: ../adr/0005-credentials-terminate-at-the-credential-proxy.md
[p1]: https://agentgateway.dev/blog/2026-07-27-credential-injection-ai-agent-egress-cb4a/
[p2]: https://agentgateway.dev/blog/2026-08-03-protecting-token-vault-kms-credential-injection/
[p25]: https://agentgateway.dev/blog/2026-10-20-credential-brokering-mode-3-at-scale/
[ag2416]: https://github.com/agentgateway/agentgateway/issues/2416
[ag1846]: https://github.com/agentgateway/agentgateway/pull/1846
[ag2134]: https://github.com/agentgateway/agentgateway/pull/2134
[ag3318]: https://github.com/agentgateway/agentgateway/pull/3318
[ag3411]: https://github.com/agentgateway/agentgateway/pull/3411
[ag3524]: https://github.com/agentgateway/agentgateway/pull/3524
[ag3990]: https://github.com/agentgateway/agentgateway/pull/3990
[ag3668]: https://github.com/agentgateway/agentgateway/issues/3668
[sub1751]: https://github.com/agent-substrate/substrate/pull/1751
[ghait119]: https://github.com/isometry/ghait/pull/119
