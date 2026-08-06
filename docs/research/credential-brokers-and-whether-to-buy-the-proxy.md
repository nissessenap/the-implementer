# Credential brokers, CB4a, and whether we should buy the proxy instead of building it

**Date:** 2026-08-06 · **Question asked:** [#37][build] proposes building the credential
proxy. agentgateway's [CB4a post][agpost] and the [IETF draft][draft] describe the same
injection pattern. Is there an open-source thing we can adopt instead — ideally so we do
not have to write and own a piece of technology this load-bearing?

**Short answer:** **No, and the reason is encouraging rather than disappointing.**

The pattern is real, it is 2026-new, and four projects ship on-the-wire credential
substitution today — including OpenAI's and Anthropic's own agent sandboxes. So
[ADR 0005][adr5] is not idiosyncratic. But every one of them stops exactly where our
three hard requirements begin, and **two of the four biggest silently no-op on `git`'s
basic-auth** — the trap ADR 0005 measured and our prototype already handles. We are
ahead of the field on the part the field gets wrong.

What changes is not the design. It is that **four pieces of #37 can be deleted and
replaced with existing code**, one of them worth an ADR amendment on its own:

| #37 work item | Replace with | Saves |
| --- | --- | --- |
| KMS-signed App JWT + the "seam, not work" caveat | [`isometry/ghait`][ghait] (Apache-2.0, Go library) | PKCS#8 conversion, digest-signing, the custom `golang-jwt` method — **and ships AWS/Azure/Vault/local for free** |
| Push-branch enforcement | [`gastownhall/gastown`][gastown]'s `validateReceivePackRefs` (~50 lines, MIT) + [`go-git` v6][gogit] pkt-line decoder | The scariest unknown in #37, and a hand-rolled parser on adversary-controlled bytes |
| Egress allowlist vocabulary | [`gh-aw`][ghaw]'s `ecosystem_domains.json` (41 bundles, MIT) | Already the plan in [architecture §7][arch]; now we know the file |
| Caller authentication ([#37 comment][callercomment]) + run identity | [`kube2iam`][kube2iam]'s mechanism, inverted | One mechanism instead of the two that comment proposes |

**Sources and how far to trust them.** The [blog post][agpost] and the [draft][draft]
were read directly for this document. Everything with a `file:line` citation comes from
three research agents that cloned the repositories and read the code; spot-checks agreed,
but the citations are second-hand and should be re-verified before anything load-bearing
is built on one. Where two agents disagreed, §2.6 says so and says which won.

---

## 1. CB4a: the brand is not available, and the spec is not a spec

### 1.1 What agentgateway actually ships under that name

The [post][agpost] describes a two-leg MCP flow: an MCP client authenticates to the
gateway through the user's IdP, and on the first tool call the gateway elicits
third-party OAuth consent in the user's browser and stores the token in its own STS,
keyed by user identity and resource. Injection is `Authorization` on egress from a route
the client was configured to call.

That is **brokering a human's third-party OAuth tokens for MCP tool calls**. It is not
intercepting an unmodified `git push`.

And it is not open source. `grep -rniI "cb4a|credential-broker|credential broker|hartman|draft-hartman"`
over the whole agentgateway tree at HEAD (`db62d60`, 2026-08-06) returns **zero** real
hits — one false positive, a pypi wheel hash in
`examples/traffic-a2a/strands-agents/uv.lock`. Nothing in the docs either. The post says
the Model-A proxy-gateway broker "is implemented in **Solo Enterprise** for
agentgateway", and the hardening it describes — envelope encryption, KEK/DEK, master key
in KMS, a separate broker authorising every unwrap — is explicitly Part 2.

The connection is now explained: **Christian Posta is Solo.io's Global Field CTO**. He
wrote the posts *and* the `protocol-explorer.dev/cb4a` demo they link — whose
`app/cb4a/happy-path/page.tsx` is 622 bytes rendering hardcoded JSON, in which signatures
are the literal string `"RS256(task_envelope, svid_private_key)"`. His second post
disclaims: *"Although this is not EXACTLY the implementation proposed by the CB4A paper,
it does align with its principals."*

### 1.2 The draft has nothing to conform to

[`draft-hartman-credential-broker-4-agents-00`][draft] — read directly:

- **Rev -00 only, expires 2026-09-30.** Individual submission, Informational, no stream,
  no WG, no responsible AD; IESG state "I-D Exists". [History][hist] shows exactly one
  event, 2026-03-29.
- **No IANA actions** (*"This document makes no requests of IANA"*), no media types, no
  ABNF/CDDL/JSON Schema, zero HTTP verbs. The Task Request Envelope exists as one JSON
  *example*. The single discovery endpoint is in §4.11 "Native Integration Specification
  (**Future**)" and is **misspelled** `.well-known/cba-configuration`.
- Internal inconsistency: the abstract says ten threats, Appendix A defines TM-1…TM-11.
  §3.5, §4.4 and §4.5 give three different, mutually inconsistent triggers for Model A.
- Author: **Kenneth G. Hartman, SANS Institute** — a SANS instructor, this is his only
  IETF draft ever, and he has never posted to another IETF list. (Not Sam Hartman, the
  long-time IETF participant. Different person; easy to confuse.)
- Aimed at **WIMSE** by his own announcement, but absent from [WIMSE's document
  page][wimse] and from [OAuth's][oauthdocs].

**Reception: four messages, one thread, dead since 2026-04-03.** Michał Trojanowski
(Curity): *"Requests these days go through a gateway anyway, so an API gateway that is
already in place could implement the responsibilities that you describe. I don't think
you need a separate component necessarily"* — and on the latency argument for avoiding
the proxy, *"Adding a few ms… is rather negligible."* Liuchunchi Liu (Huawei) went at the
novelty: *"Model A looks like normal APIG behavior. **Model B is just like token
exchange/credential exchange** — can you elaborate more on the differences here?"*
[Hartman conceded][concede] it is *"an architectural specification, not a competing
product"* and promised a -01 with delegation, attenuation and revocation cascade. Four
months on, no -01.

**Zero implementations, verified three ways** (`cba-configuration` → 2 hits, both copies
of the draft; `envelope_version agent_svid` → the same 2; agentgateway → 0 for `cb4a`, 0
for `DPoP`). Including the author's own: `Resistor52/credential-broker-4-agents` is 22
commits of `i-d-template` scaffolding, **no LICENSE**, untouched since submission day.

The strongest engineers who have touched it all did the same thing — adopt the
principles, refuse conformance. Tom Hennen (Google/SLSA, `rein`): *"Compatibility-in-spirit,
not strict compliance."* Daniel Rapp (Proofpoint, `sandy`): *"track CB4A, don't depend on
it."* `scorellis/secretsminter` ADR-0014: *"Docs must say 'aligned with,' never
'conformant to.'"* Meanwhile the category's most-starred tool,
[`Infisical/agent-vault`][agentvault] (2.0k★, v0.39.1 shipped 2026-08-04), references
CB4A, SPIFFE **and** DPoP not at all. The market converged on broker-not-mount without
the draft.

### 1.3 The draft prefers the model we rejected

This is the correction that matters, and it inverts the premise of the question.

- **Model B (§3.2) is its recommended primary**: *"The broker uses the real long-lived
  credential to mint a derivative short-lived token with narrow scope, then hands that
  short-lived token to the agent. The agent uses the short-lived token to call the target
  service **directly**."* Hartman [on the list][concede]: *"CB4A operates as a credential
  authority on the control plane, not as an inline proxy on the data path."*
- **Model A (§3.1) is the fallback**, and even there *"the broker operates a proxy
  endpoint **that the agent calls**"* — an explicit endpoint, not transparent
  interception.

Ours is Model A *with* transparent interception. Model B is precisely what ADR 0005
exists to refuse: a real token, however short-lived, in the same context as untrusted
issue text. And the draft's stated reason for preferring B — proxy latency — is the
objection its own reviewer waved away, and which we have measured at an upper bound of
~23 % that mostly is not the hop.

Transparent interception appears in the draft exactly once, in §4.10.2, as **bypass
prevention**: *"Relying solely on the agent's cooperation to use the broker is
insufficient"* → NetworkPolicy blocks, mesh sidecars, DNS redirection to the proxy. That
is [architecture §7][arch]'s target state, and its threat **TM-11 (broker bypass)** plus
**TM-1 (broker compromise)** are exactly the [caller-authentication gap][callercomment]
logged on #37 on 2026-08-06.

**Verdict: cite it as vocabulary and a threat checklist, never as a dependency.** The
Model A/B/C naming, the PDP/CDP trust separation, TM-1…TM-11, and the genuinely good idea
that `justification` is *"solely for post-incident forensic reconstruction"* and the PDP
`MUST NOT` evaluate it — all useful shorthand for our ADRs. For anything requiring
interop, depend on **RFC 8693** and **RFC 9449** directly and watch
[`draft-ietf-oauth-identity-chaining`][chaining] — the only *adopted* draft in a field of
twenty-odd competing agent-auth submissions.

---

## 2. agentgateway as a substrate: the transport half exists, and four things break it

Set the branding aside. The interesting question is whether the OSS data plane can be
configured into what we need. Surprisingly close — and undocumented.

### 2.1 What is merged and tested

All of it Apache-2.0, Rust data plane (~219k LOC) plus a Go controller (~83k), v1.4.1
released 2026-07-29, ~13–22 commits/day, **none of it behind a feature flag**:

- **CONNECT termination** — `crates/agentgateway/src/proxy/gateway.rs:706`
  `terminate_connect_tunnel()`; non-CONNECT on such a bind → 405. Enabled by
  `binds[].tunnelProtocol: connect`, or the CRD `frontend.connect.mode: Deny|Route|Tunnel`.
- **MITM with a private CA** — listener `tls.mode: dynamicCa` mints a per-SNI leaf from a
  configured CA. Docs: *"dynamic CA mode uses cert/key as a CA for on-demand SNI
  leaf certificate issuance."* K8s form is the annotation
  `agentgateway.dev/tls-certificate-source: DYNAMIC_CA` on a Gateway listener, translated
  at `controller/pkg/agentgateway/translator/conversion.go:1360`. The PR that added it
  ([agentgateway#2134][ag2134], "Add Dynamic SSL Cert Support", merged 2026-06-10) says it
  is *"designed for egress gateway scenarios where the proxy needs to intercept and
  decrypt TLS traffic transparently, then re-encrypt it."*
- **Arbitrary upstreams** — `dynamic: {}` / `dynamicForwardProxy: {}` resolves the
  upstream from the decrypted authority, carrying its own warning: *"this backend type
  can send requests to arbitrary destinations."*
- **On-the-wire header rewrite** — `backendAuth` (`http/auth/mod.rs:196`) plus CEL
  `transformations`, **and `backendAuth` applies on the dynamic backend too**
  (`proxy/httpproxy.rs:2240`).
- **The whole chain asserted in one test** — `crates/agentgateway/tests/tests/connect.rs:915`,
  `connect_tunnel_dynamic_ca_obo_dynamic_backend`, whose doc comment reads: *"CONNECT
  (carrying `x-actor-token`) → Tunnel re-entry by authority port into a dynamic-CA HTTPS
  bind → inner TLS terminated with a minted per-SNI cert → ext-authz RFC 8693 exchange →
  mock STS returns the OBO → `transformations` injects `Authorization: Bearer <obo>` → a
  `dynamic: {}` (DFP) backend forwards to the upstream named by the inner Host."*

Deployment is fine: `agentgateway-standalone` is one Deployment plus a ConfigMap, no
CRDs, no Gateway API, no Istio, **no Envoy** (the data plane is Rust/hyper/rustls).

### 2.2 No selective MITM — and this is a design collision, not a missing knob

Tunnel re-entry matches on the CONNECT authority's **port**, not its hostname
(`gateway.rs:759-780`). So you cannot intercept `github.com` and blind-tunnel everything
else. Open issue **[agentgateway#2416][ag2416]**, "Support hostname-based CONNECT routing
to internal binds for selective MITM", filed 2026-07-02, still open as of 2026-08-06.

[ADR 0005][adr5] depends on the opposite being true. Its certificate is *deliberately*
narrower than the tunnel, and that is what makes "must not leak the token to pre-signed
storage URLs" structural rather than remembered: a wheel download 307s **on-host** and
stays intercepted; an artifact store redirecting **off**-host lands on a name the
certificate does not cover, falls through to a plain tunnel, and receives nothing. Under
port-based re-entry that protection does not exist.

### 2.3 No KMS signing

`jwtSign` takes `signingKey: FileOrInline` PEM, parsed by `SigningKey::from_pem`
(`http/auth/jwt_sign.rs:66-68,128-131`). `grep -rniI "cloudkms|\bkms\b|key.?vault|pkcs11|\bhsm\b|hashicorp|vault"`
across `crates/`, `controller/` and `schema/config.md` → one hit, and it is
`hashicorp/go-multierror` in a linter config.

So adopting agentgateway means **mounting the GitHub App private key into the proxy**.
That deletes the claim ADR 0005 leans on hardest — *"what the proxy holds is not a key
but a revocable, audit-logged capability… with KMS there is no holder"* — which is the
entire answer to "isn't one minting component a concentration of risk?".

### 2.4 No GitHub App flow, and no pod annotations

`oauthTokenExchange` speaks RFC 8693 / 7523 form bodies; GitHub's
`POST /app/installations/{id}/access_tokens` is neither. Buildable via `extAuthz` + CEL +
`transformations`, but the App-JWT minting step has no home.

Caller identity offers three mechanisms — `source.unverifiedWorkload.{name,namespace,serviceAccount}`
from a source-IP lookup (`cel/types.rs:377-394`), SPIFFE/mTLS `source.identity.*`, and
`source.connectHeaders[...]` snapshotted from the CONNECT request. But
**pod labels and annotations are not exposed** (`cel/types.rs:335-345`) — the Istio
workload proto carries none on that path. ADR 0005's non-negotiable is *mint for the
repository named in the run's Job annotation*. Not expressible. And pod resolution needs
**controller mode**, so the standalone chart does not have it at all.

Worth noting the source's own warning, which is the same honest caveat our design owes:
*"Fields are nested under `unverified` to signal that they are derived from the source IP
(not cryptographically authenticated)."*

### 2.5 Git ref enforcement is ours either way

`authorization.rules[]` is real CEL over method, path, query and headers — so
`POST /repo.git/git-receive-pack?service=git-receive-pack` **is** expressible. Ref
restriction is not: CEL has no pkt-line decoder, and `grep -rniI "git-receive-pack|git-upload-pack"`
over the whole repo → not found. The realistic seam is `extProc`, an external gRPC
service — i.e. we write the git logic ourselves regardless.

### 2.6 Two agents disagreed; here is the resolution

One agent's sweep of LLM gateways concluded *"zero CONNECT implementations across 10
repos"* and listed agentgateway among them. That is **wrong**, and the agent that cloned
the repo wins: `gateway.rs:706`, the `tunnelProtocol` enum, the CRD `connect.mode` block,
and a named passing test are specific evidence against a negative grep. Recorded because
the negative is the kind of claim that gets quoted later.

### 2.7 The honest tally

Adopting agentgateway trades **462 lines of Go we understand** (`proto/proxy/main.go`,
plus 148 lines of test) for a 219k-LOC Rust data plane and an 83k-LOC Go controller; and
we would still write the git ext_proc service, still lose selective MITM, still lose the
annotation invariant, and **give up the KMS property**. All of the relevant code is 8–10
weeks old ([CONNECT termination][ag1846] 2026-06-04, [dynamic CA][ag2134] 2026-06-10,
[`connectHeaders`][ag2285] 2026-06-23),
`https_proxy` is never mentioned anywhere in the repo or docs, and there is no example of
this shape — we would be its first user.

**Revisit if** #2416 lands *and* a KMS signer appears. Their dynamic-CA + DFP +
`backendAuth` chain is genuinely the same design as ours, and being able to delete our
data plane later is worth watching for.

---

## 3. The wider field: the category is real, and it stops where we start

### 3.1 Four projects do on-the-wire substitution today

All shipped in 2026. None is Kubernetes-aware.

| project | license / lang | latest | git basic-auth? |
| --- | --- | --- | --- |
| [`openai/codex`][codex] `codex-rs/network-proxy` | Apache-2.0, Rust | rust-v0.146.1, 2026-08-05 | ❌ **silently no-ops** |
| [`anthropic-experimental/sandbox-runtime`][srt] | Apache-2.0, TS | v0.0.70, 2026-08-04 | ❌ documents its own failure |
| [`Infisical/agent-vault`][agentvault] | MIT Expat, Go | v0.39.1, 2026-08-04 | ✅ rebuilds the header |
| [`dedene/claw-wrap`][clawwrap] | MIT, Go | v0.5.0, 2026-02-21 | ❌ static per-route |

The pattern has independently converged on the exact shape ADR 0005 describes, down to
the vocabulary: a **shape-preserving sentinel** in the sandbox's environment, swapped on
the wire.

**OpenAI's** `virtualize_child_env()` replaces `GITHUB_TOKEN`/`GH_TOKEN` in the child
environment with a prefix- and length-preserving fake
(`src/credential_broker.rs:45-73`), swapped by `inject_request_headers()` (`:85-109`),
host-bound so one host's token cannot be laundered through another
(`credential_broker/providers/github.rs:58-76`), with the invariant
`credential_broker requires mitm = true` asserted in `src/state.rs:66-67`.

**Anthropic's** is the best-engineered substitution engine found:
`mintSentinel()` pads the sentinel to the real value's byte length so `Content-Length`
stays invariant (`credential-sentinel.ts:31-39`); substitution runs across all header
values and across **streaming body chunk boundaries**, holding back
`maxSentinelLength-1` bytes (`body-substitution.ts`); each credential has its own
`injectHosts`, validated at config load to be a subset of `allowedDomains`
(`sandbox-config.ts:1139-1158`). Its caller authentication is the strongest in OSS:
`Proxy-Authorization: Basic base64("srt[.<encodedCommand>]:<token>")`, 407 otherwise
(`http-proxy.ts:165-229`) — per-invocation identity, not a shared secret. Its CA comes
from `network.tlsTerminate.{caCertPath,caKeyPath}` — a clean cert-manager seam.

### 3.2 The finding worth the whole survey

**Two of the four — including OpenAI's and Anthropic's — silently fail on `git`'s
basic-auth.**

`codex`'s `select_credential` requires the *plaintext* sentinel to appear in the header
value: `value.contains(&credential.dummy_value)` (`src/credential_broker.rs:202-216`).
Git sends `Authorization: Basic base64("x-access-token:ghs_FAKE…")`, so the substring
never matches and no injection happens — no error, no log, just an unauthenticated push.
Its GitHub provider only ever writes `Bearer` (`providers/github.rs:50-56`).
`sandbox-runtime` concedes the same in its own comment (`body-substitution.ts:12-16`).

[ADR 0005][adr5] names this and our prototype handles both shapes:
*"`Basic base64(x-access-token:SENTINEL)` (git's, from the clone URL's userinfo) and
`token`/`Bearer SENTINEL` (`gh`'s)."* Only `agent-vault` — which re-encodes the userinfo,
`base64(user + ":" + pass)` at `internal/broker/broker.go:304-317` — and an unlicensed
one-author Python project get this right among maintained work.

**This reframes the risk.** The worry behind the question is that the proxy is too
important to write ourselves. But the part of it that is genuinely easy to get wrong is
the part two of the best-resourced labs in the field got wrong, and we have already
measured it. Turn it into a regression assertion rather than a paragraph in an ADR.

### 3.3 What exists nowhere

1. **Pod-IP → Pod annotation → token scope, in a proxy.** Every substituting proxy above
   authenticates its caller by shared token, Proxy-Auth basic, per-command token, or mTLS
   CN. None resolves Kubernetes pod identity.
2. **Git ref enforcement in a forward proxy.** The ceiling among forward proxies is
   method + path blocking.
3. **KMS-backed App-JWT signing inside a proxy.** Every in-proxy minter reads a local PEM
   and calls `rsa.SignPKCS1v15`. KMS signing exists only in standalone brokers.
4. **Curated ecosystem allowlist bundles.** Only `gh-aw` ships them.

### 3.4 GitHub's own agent stack chose a different architecture

Worth knowing, because it is the closest whole-system analogue and it reaches "zero real
credentials in the sandbox" **without** on-the-wire substitution — all MIT, all very
active: [`gh-aw`][ghaw] (4.9k★, v0.85.4 2026-08-06), `gh-aw-firewall` (v0.27.44),
`gh-aw-mcpg` (v0.4.8).

1. **Squid** does interception and allowlisting only — `ssl_bump` with a per-session CA,
   `generate-host-certificates=on`, `ssl_bump terminate all`
   (`gh-aw-firewall/src/squid/ssl-bump.ts:50-86`). Enforcement inside the tunnel is
   **URL-regex ACLs** (`src/dlp.ts:1-13`). No header rewriting anywhere.
2. **Credentials are withheld, not substituted** — `GITHUB_TOKEN`/`GH_TOKEN`/
   `GITHUB_PERSONAL_ACCESS_TOKEN` stripped from the agent container with the comment
   *"the agent must not hold live credentials that can be extracted via
   /proc/self/environ"* (`src/services/agent-environment/excluded-vars.ts:40-57`) — the
   same Microsoft finding [ADR 0005][adr5] cites.
3. **Per-provider reverse proxies on fixed ports**, reached by rewriting each CLI's
   base-URL env var, which then inject the real credential — including GCP tokens minted
   from Actions OIDC → STS → SA impersonation
   (`containers/api-proxy/gcp-oidc-token-provider.js:57-85`). Structurally the same as
   our Vertex leg.
4. **`gh` never sees a token at all** — the agent RPCs `POST /exec` to a `cli-proxy`
   container that runs `gh` itself (`containers/cli-proxy/server.js:5-18`); private-repo
   git uses `GIT_ASKPASS` and a token file in a *separate* trusted container.

**And their deployment shape rules the stack out for us regardless of architecture:**
`gh-aw-firewall` ships as `sudo awf …`, a root CLI that programs **host iptables** and
runs docker-compose (`src/host-iptables-*.ts`). It is not a Deployment and does not want
to be one. Take the ecosystem JSON and read the Squid config; do not take the product.

Their (4) is the road we did not take, and it is worth naming as a rejected alternative:
it dodges the git basic-auth problem entirely, at the cost of an RPC surface for every
tool the agent might use and a hard ceiling on what the agent can do. Ours keeps `git`
and `gh` unmodified — [ADR 0005][adr5] records that the phase script's credential path
never changed, which is the payoff.

Also: Squid is not a viable substrate for us even though it does interception well.
`request_header_replace` substitutes a *config-time literal*
(`src/cf.data.pre:7104`); the dynamic path via external-ACL notes **fails open** —
`src/HeaderMangling.cc:269` substitutes `"-"` when the value is empty, so a failed mint
silently emits `Authorization: -`. Disqualifying for a credential broker.

### 3.5 Envoy is disqualified, definitively

Worth recording so nobody re-opens it: Envoy **cannot generate certificates**.
`grep X509_sign|generate_certificate source/` → zero hits. The TLS-bumping effort never
merged — [envoy#18928][envoy18928] ("TLS bumping: decrypting communications between
internal and external services") still open; #19582, #22582, #23063, #23192 all closed unmerged;
only #22036 landed, and that is SNI-based *selection among preconfigured certs*.
Everything else it would need exists (ext_authz can override `Authorization` and even
names it as the use case; ext_proc has streaming body mutation; source IP is in
`AttributeContext.source`) — but with static certs only, we would pre-issue one
certificate covering the entire allowlist, which is exactly the property §2.2 explains we
must not give up.

### 3.6 One-line kills, for the record

| candidate | what kills it |
| --- | --- |
| `openai/codex` network-proxy | not k8s; no minting; **silently no-ops on git basic-auth** |
| `anthropic-experimental/sandbox-runtime` | in-process bwrap/seatbelt, single host; no minting; same git gap |
| `Infisical/agent-vault` | no minting, no pod identity, no ref policy; denylists `kubernetes.default` (`broker.go:854-858`) |
| `dedene/claw-wrap` | R2 is static per-route, not decode-and-swap; no KMS; 138★, one maintainer, stale |
| `stripe/smokescreen` | **does** MITM (`smokescreen.go:997-1021`) and `add_headers` is real — but static YAML strings; `RoleFromRequest` is a Go hook *"not configurable via YAML"*, so library-only; needs the stripe/goproxy fork |
| `finos/git-proxy` | **no CONNECT** — Express reverse proxy needing a remote-URL rewrite — and it *consumes* the client's `Authorization` and reuses it upstream (`PullRemoteHTTPS.ts:36-79`), so the client must still hold a real credential |
| `cyberark/secretless-broker` | `"CONNECT is not supported." → 405` (`http/proxy_service.go:215`), and fails open on unmatched requests |
| `kubernetes-sigs/agent-sandbox` | no egress proxy at all; FQDN allowlisting is roadmap-only (`roadmap.md:65`). (`google/agent-sandbox` does not exist — 404) |
| `google/martian` | archived 2024-12-18 — ironic, since its API is the best fit of any substrate (`mitm.NewConfig(ca, privateKey)`, and `ipauth.Modifier` derives caller identity from `req.RemoteAddr`, literally R4's seam). Maintained descendant if ever needed: `saucelabs/forwarder`, MPL-2.0, v1.6.0 2026-01-12, vendors martian/v3 |
| `AdguardTeam/gomitmproxy` | GPL-3.0; dormant since 2024-03 |
| Squid + ICAP | fails open (§3.4); and R4/R5 would mean writing a pkt-line parser in C against `c-icap` — 62★, 3 commits in 6 months, whose latest commit is *"Fix off-by-one errors causing out-of-bounds reads and writes"* |
| Pomerium / Teleport / Vault / Boundary / cloudflared / tailscale | reverse proxy / 501 on CONNECT (`proxy_service.go:265-295`) / `X-Vault-Token` only / Enterprise-only / SaaS-only / blind tunnel |
| all LLM gateways | reverse proxies for LLM APIs; zero GitHub App minting (`grep 'installations/.*access_tokens'` → nothing). LiteLLM is MIT **except a proprietary `enterprise/` tree** |
| `mitmproxy` / `elazarl/goproxy` | nothing kills them — they are substrates, and §4.5 explains why we do not need one |

---

## 4. What to actually change in #37

Four adoptions. None of them changes [ADR 0005][adr5]'s architecture; two shrink its
scope, and one of those is worth an amendment.

### 4.1 Adopt a KMS-signing library for the App JWT — the biggest win

[`isometry/ghait`][ghait] (Apache-2.0, Go) is exactly ADR 0005's *"seam, not work"*,
already built, with the second, third and fourth implementations included:

- `provider/provider.go:16-24` — a two-method `Provider{Check, Sign}` interface, wired
  into `ghinstallation.WithSigner` (`ghait.go:122-126`).
- `provider/gcp/gcp.go:51-105` — GCP KMS `AsymmetricSignRequest`, with a `Check()` that
  verifies key state is ENABLED **and** algorithm is `RSA_SIGN_PKCS1_2048_SHA256` —
  precisely the pin ADR 0005 specifies, validated at startup rather than discovered on
  first mint.
- AWS KMS, Azure Key Vault, Vault transit and local-file behind build tags. **The
  non-GCP operator story arrives for free**, which ADR 0005 deliberately deferred.
- `NewTokenWithOptions(ctx, *github.InstallationTokenOptions)` — the scoping surface we
  need.

This deletes the PKCS#8-vs-PKCS#1 trap, the send-the-digest-not-the-message trap, and the
custom `golang-jwt` `SigningMethod` from #37 entirely.

**Risk, stated plainly: 1★, one maintainer, v88.0.0 2026-06-17.** Bounded, and the reason
is worth stating precisely: `bradleyfalzon/ghinstallation` — which we would use anyway —
exposes a `Signer` interface, and that interface *is* how both `ghait` and `octo-sts`
inject KMS. So the whole mechanism is **~40 lines of Go** against an API we already
depend on. Vendor `ghait` for its structure and its four backends; if it dies, we own the
replacement cheaply. This is the rare case where the dependency is worth taking *because*
replacing it is trivial.

**The alternative, and why it is second:** [`octo-sts`][octosts] (Chainguard, Apache-2.0,
385★, v0.8.0 2026-07-29) is a *service* that does the same signing behind a
GCP/AWS/Azure provider switch (`pkg/kms/gcp/gcp.go:26-45`) and mints with
`InstallationTokenOptions{Repositories, Permissions}`. Its architectural draw is real:
the trust policy lives **in the target repository** at
`.github/chainguard/<identity>.sts.yaml`, matched on issuer/subject/audience claim
regexes — so "never mint for the repo the URL names" becomes an invariant the *repo owner*
enforces, not one we enforce. Two things make it second, not first:

- It authenticates callers by **OIDC**, so our proxy would present a projected
  ServiceAccount JWT and octo-sts must be able to reach the cluster's OIDC discovery
  document. On GKE that is workable; **k3s has no published issuer**, which is the same
  class of gap as the missing metadata server.
- The policy file is **scaffolding in the target repository** — and
  [architecture §3][arch] specifically prizes that *"target repositories are required to
  carry no Matt Pocock scaffolding at all."* Worth the trade for a third party's repo;
  not for ours.

Either way, note that `octo-sts`'s KMS package is already consumed standalone by at least
three unrelated projects, so that code path is exercised.

### 4.2 Adopt gastown's ref-validation algorithm for push-branch enforcement

[`gastownhall/gastown`][gastown] (MIT, Go, 17.5k★, pushed 2026-08-05) has **R5,
implemented**: `internal/proxy/git.go:311-363` `validateReceivePackRefs` parses the
pkt-line ref list and rejects any ref not under a **per-run branch prefix**, reads only
to the flush packet under a 256 KiB cap, then reconstructs the body so the packfile still
streams — `r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(pktBytes), r.Body))`
(`:266`). Denials are 403 naming the offending ref; refs are collected before validation
so a denial is still auditable.

It is a git smart-HTTP *server* front-end, not a forward proxy, and injects no
credentials — so this is **copy the algorithm, ~50 lines**, not adopt the project. Pair
it with [`go-git` v6][gogit] `plumbing/protocol/packp/updreq_decode.go:74`
`(*UpdateRequests).Decode(io.Reader)` rather than hand-rolling a parser over
adversary-controlled bytes.

One free fact that removes a step: **git does not gzip receive-pack bodies** —
`rpc.gzip_request=1` appears in `fetch_git`/`stateless_connect` in `remote-curl.c`, not
`push_git`. Large pushes are chunked, not compressed. No inflate needed.

### 4.3 Take gh-aw's ecosystem bundles verbatim

[architecture §7][arch] already says to steal them. The file is
`gh-aw/pkg/workflow/data/ecosystem_domains.json` (MIT) — 41 curated categories (go,
python, python-native, node, node-cdns, rust, java, dotnet, ruby, containers, github,
terraform, playwright, linux-distros, …), embedded and consumed at
`pkg/workflow/domains.go:19-37,239-255`. Nothing else in the survey ships curated
registry host sets.

Check it against the gap the [#37 toolchain comment][ghawcomment] found — whether the
`go` bundle covers `dl.google.com` in its *toolchain*-serving role, not just
`proxy.golang.org`. If it does not, that tension is ours to price either way.

### 4.4 Invert the caller-identity design: `Proxy-Authorization` primary, pod lookup second

[The #37 comment][callercomment] proposes NetworkPolicy as the primary control and a
shared secret header as a backstop. The survey suggests swapping the emphasis, and
collapsing two mechanisms into one.

[`kube2iam`][kube2iam] (BSD-3, Go, 2.0k★, pushed 2026-05-08) is ADR 0005's R4 mechanism,
already proven for AWS: source IP → `PodByIP` off an informer index keyed `byPodIP`
(`k8s/k8s.go:93-123`), scope from a pod **annotation** (`mappings/mapper.go:80`), and —
the exact invariant — it **rejects a role named in the request that does not match the
annotation**: `"Invalid role: does not match annotated role"` → 403
(`server/server.go:333-340`). It also carries a namespace-level allowlist specifically as
defence against pod-IP reuse.

Two mechanism notes worth having before writing this:

- **Use an informer index, not a fieldSelector.** `status.podIP` is a valid Pod field
  selector but only `spec.nodeName` is indexed, so a fieldSelector query is a full
  apiserver list-and-filter.
- **Pod IPs are ephemeral and reused.** kube2iam does not solve this; a recycled IP can
  map to the wrong run. This is a real weakness in "source pod IP → Pod → annotation" as
  the *sole* identity, and ADR 0005 does not mention it.

So: make a **per-run secret in `Proxy-Authorization`** the primary caller identity — it
is injected into the Job by the orchestrator, which already knows the run — and the pod-IP
annotation lookup the **second factor** that must agree. That authenticates the caller
(closing the comment's gap) and pins run identity (closing the IP-reuse hole) with one
mechanism, leaving NetworkPolicy as defence in depth rather than the only control.

⚠️ **Landmine if we do this:** `sandbox-runtime` sets
`GIT_CONFIG_PARAMETERS='http.proxyAuthMethod=basic'` (`sandbox-utils.ts:538`) precisely so
`git` does not 407 against a proxy requiring authentication. Without it, adding
`Proxy-Authorization` breaks `git` and nothing else.

### 4.5 Do not adopt a substrate

`mitmproxy` and `elazarl/goproxy` are both viable and neither is disqualified —
`goproxy` shipped HTTP/2 MITM in v1.9.0 on 2026-08-06, and `TLSConfigFromCA(ca *tls.Certificate)`
takes a cert-manager CA directly. But our proxy is **462 lines of Go** with 148 lines of
test, already green on Vertex, the GitHub sentinel swap on both credential shapes, and a
real private wheel. Adopting a substrate now means porting working measured code to gain
scaffolding we have already written, and in `mitmproxy`'s case moving the most
security-sensitive component in the system into Python with a single asyncio loop and no
multi-worker story.

Two `mitmproxy` traps worth recording anyway, because they would have cost a day each if
we had gone that way: `allow_hosts`/`ignore_hosts` mean **passthrough, not deny**
(`addons/next_layer.py:245-252`), and its CA must be a **single file** containing key and
cert (`certs.py:525`), so cert-manager's split `tls.key`/`tls.crt` needs an initContainer
to concatenate them.

### 4.6 Free hardening from the field, cheap to take

- **We may be short on CA-trust variables.** `sandbox-runtime` sets **13**
  (`sandbox-utils.ts:442-454`); [ADR 0005][adr5]'s trust seam names five. Diff the lists —
  the failure mode of a missing one is an unrelated HTTPS call failing far from its cause,
  which is the exact trap ADR 0005 already documents for the replace-vs-add problem.
- **Pad the sentinel to the real token's byte length** so `Content-Length` stays
  invariant (`credential-sentinel.ts:31-39`). Cheap, and it removes a whole class of
  "why did this request break" question.
- **Bind each credential to its own host set**, validated at config load to be a subset
  of the allowlist (`sandbox-config.ts:1139-1158`). Our `credFor` host switch does this
  by construction; the validation-at-load part is the addition.
- **Pin the upstream to the CONNECT authority**, so an inner `Host:` header cannot
  redirect an intercepted tunnel. `agent-vault` does it deliberately
  (`internal/mitm/connect.go:46-49`) and `mitmproxy` gets it free by running the
  post-CONNECT layer in transparent mode. Verify our proxy does; it is a one-line bug
  otherwise.
- **Rate-limit proxy-auth failures before the hijack** (`brokercore/proxyauth.go`) if
  §4.4 lands.

---

## 5. Answering the question as asked

**"Has agentgateway already implemented something we can use?"** The branded thing, no —
Solo Enterprise. The transport primitives, genuinely yes, but they are 8–10 weeks old,
undocumented for this shape, and missing selective MITM, KMS signing and pod annotations.

**"Are there open-source options?"** For the whole component, no. For four of its parts,
yes, and they are better than what we would write: `ghait` for KMS signing, gastown's
algorithm for ref enforcement, `gh-aw`'s bundles for the allowlist, `kube2iam`'s
mechanism for pod identity.

**"I don't want to write such an important piece of technology."** The piece is already
written and measured, at 462 lines. The field's evidence is that the danger is not in the
writing — it is in one specific detail, git's deferred basic-auth, which OpenAI and
Anthropic both got wrong in shipping code and which our prototype gets right. **Keep the
proxy. Delete the minting and the pkt-line parsing from #37. Turn the git-basic-auth
behaviour into a regression test rather than a paragraph.**

**"I'm willing to change my design if it makes sense."** It does not — but three things
in the design are now known to be weaker than written, independent of any adoption
decision:

1. **Pod-IP identity is not sufficient alone** (IP reuse). §4.4.
2. **The proxy authenticates no caller** — already logged on #37; §4.4 collapses the fix
   into the same mechanism as (1).
3. **ADR 0005 claims the certificate-narrower-than-tunnel property is load-bearing**, and
   §2.2 confirms it by showing that the one credible substrate lacks it. That is worth
   stating as a *requirement* in the ADR rather than an observation, so a future
   substrate evaluation fails fast on it.

[build]: https://github.com/nissessenap/the-implementer/issues/37
[callercomment]: https://github.com/nissessenap/the-implementer/issues/37#issuecomment-5203429677
[ghawcomment]: https://github.com/nissessenap/the-implementer/issues/37#issuecomment-5157688768
[adr5]: ../adr/0005-credentials-terminate-at-the-credential-proxy.md
[arch]: ../architecture.md
[agpost]: https://agentgateway.dev/blog/2026-07-27-credential-injection-ai-agent-egress-cb4a/
[draft]: https://datatracker.ietf.org/doc/html/draft-hartman-credential-broker-4-agents-00
[hist]: https://datatracker.ietf.org/doc/draft-hartman-credential-broker-4-agents/history/
[concede]: https://mailarchive.ietf.org/arch/msg/wimse/pWIHvjETCO66mmdkdxyMHJLV1rA/
[wimse]: https://datatracker.ietf.org/group/wimse/documents/
[oauthdocs]: https://datatracker.ietf.org/group/oauth/documents/
[chaining]: https://datatracker.ietf.org/doc/draft-ietf-oauth-identity-chaining/
[codex]: https://github.com/openai/codex
[srt]: https://github.com/anthropic-experimental/sandbox-runtime
[agentvault]: https://github.com/Infisical/agent-vault
[clawwrap]: https://github.com/dedene/claw-wrap
[ghait]: https://github.com/isometry/ghait
[octosts]: https://github.com/octo-sts/app
[gastown]: https://github.com/gastownhall/gastown
[gogit]: https://github.com/go-git/go-git
[ghaw]: https://github.com/github/gh-aw
[kube2iam]: https://github.com/jtblin/kube2iam
[ag2416]: https://github.com/agentgateway/agentgateway/issues/2416
[ag2134]: https://github.com/agentgateway/agentgateway/pull/2134
[ag1846]: https://github.com/agentgateway/agentgateway/pull/1846
[ag2285]: https://github.com/agentgateway/agentgateway/pull/2285
[envoy18928]: https://github.com/envoyproxy/envoy/issues/18928
