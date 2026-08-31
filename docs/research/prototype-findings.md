# The prototype's findings — the measurement record

`proto/` answered the open questions in [ADR 0001][adr1] / [ADR 0002][adr2] and
issues [#22][22], [#28][28], [#31][31], [#33][33] and [#34][34] against a real
cluster before any Go was written. This file is what it measured, kept because the
ADRs and `docs/architecture.md` cite these numbers as their evidence.

**The prototype itself is mostly gone**, promoted into shipped code by [#71][71]:

| prototype | shipped as |
|---|---|
| `proto/Dockerfile`, `proto/phase.sh`, the two schemas | `sandbox/` — the base image and the run plan, with the probes removed |
| `proto/proxy/` | `proxy/`, `cmd/proxy`, `charts/proxy` |
| `proto/job.yaml`, `proto/go.sh`, `proto/jobname.sh` | `charts/orchestrator`, `orchestrator/`, `cmd/orchestrator`, `e2e/` |
| `proto/dind.sh`, `proto/dind-net.sh` | still `proto/` — Docker in the sandbox is [#28][28], still open |

Everything below is the record as it was written, in the present tense of the
prototype: the commands and file names in it are the prototype's, not the shipped
system's. The host setup immediately below is the exception — it is a live
prerequisite for the e2e against a local k3s, and `RUNTIME_CLASS=gvisor` is how the
harness asks for it.

## Host setup: gVisor on k3s

**k3s does not autodetect runsc.** [docs.k3s.io/advanced][k3sadv] lists
`crun, lunatic, nvidia, nvidia-cdi, nvidia-experimental, slight, spin, wasmedge,
wasmer, wasmtime, wws` — no gVisor. It was named as future work in the
runtime-discovery PR ([k3s#8751][k3s8751]) and never landed, so both the
containerd runtime and the RuntimeClass are hand-written.

k3s's generated `config.toml` already carries
`imports = [".../config-v3.toml.d/*.toml"]`, so a **drop-in** is enough — no
`config-v3.toml.tmpl` override to keep in sync with the base k3s renders.
Verified against k3s v1.36.2+k3s1 / containerd 2.3.2 (`version = 3` schema, key
`plugins.'io.containerd.cri.v1.runtime'`, *not* the `io.containerd.grpc.v1.cri`
key the gVisor docs still show).

```sh
# 1. install runsc + the containerd shim (Fedora: no apt repo, use the binaries)
U=https://storage.googleapis.com/gvisor/releases/release/latest/x86_64
curl -fsSLO $U/runsc -O $U/runsc.sha512 -O $U/containerd-shim-runsc-v1 -O $U/containerd-shim-runsc-v1.sha512
sha512sum -c runsc.sha512 containerd-shim-runsc-v1.sha512
chmod a+rx runsc containerd-shim-runsc-v1
sudo cp runsc containerd-shim-runsc-v1 /usr/local/bin/   # already on k3s's PATH

# 2. drop-in: the plain sandbox the agent Job uses
D=/var/lib/rancher/k3s/agent/etc/containerd
sudo mkdir -p $D/config-v3.toml.d
sudo tee $D/config-v3.toml.d/gvisor.toml <<'EOF'
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
EOF

# 3. drop-in: a second, DinD-capable sandbox (extra grants, kept off the first)
sudo tee $D/runsc-dind.toml <<'EOF'
[runsc_config]
  net-raw = "true"
  allow-packet-socket-write = "true"
EOF
sudo tee $D/config-v3.toml.d/gvisor-dind.toml <<'EOF'
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc-dind]
  runtime_type = "io.containerd.runsc.v1"
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc-dind.options]
  TypeUrl = "io.containerd.runsc.v1.options"
  ConfigPath = "/var/lib/rancher/k3s/agent/etc/containerd/runsc-dind.toml"
EOF

sudo systemctl restart k3s

# 4. RuntimeClasses (name is ours, handler must match the containerd stanza)
kubectl apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: gvisor }
handler: runsc
---
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: gvisor-dind }
handler: runsc-dind
EOF

# 5. verify — the canonical check is gVisor's own dmesg
kubectl run gvisor-smoke --image=busybox --restart=Never \
  --overrides='{"spec":{"runtimeClassName":"gvisor"}}' --command -- dmesg
kubectl logs gvisor-smoke   # => "Starting gVisor... Ready!"
```

Host prerequisites that turned out **not** to bite: SELinux is `Disabled` on this
box, so the gVisor FAQ's "SELinux is not supported" caveat never applied — it
would need revisiting on an enforcing host. cgroup v2 + systemd 259 satisfies
runsc's "systemd ≥ 244 and unified cgroups" requirement.

Recorded versions: runsc `release-20260727.0`, k3s `v1.36.2+k3s1`,
containerd `2.3.2-k3s2`, kernel `7.1.3-201.fc44`. Inside the sandbox the kernel
reports `4.19.0-gvisor`.

## The posture this spike lands on

`runtimeClassName: gvisor`, **uid 1000**, `seccompProfile: RuntimeDefault`,
`readOnlyRootFilesystem: true`, not privileged, no added capabilities, no extra
runsc flags. Everything below is measured against that unless stated.

| want | uid 1000 + gVisor | uid 0 + gVisor |
|---|---|---|
| the agent run itself | ✅ | ✅ but needs `IS_SANDBOX=1` |
| `bwrap` (plain) | ✅ | ❌ |
| `bwrap --unshare-net` | ❌ upstream bug | ❌ |
| Docker (rootless dockerd) | ✅ unprivileged | ✅ |
| `seccompProfile: RuntimeDefault` | ✅ no-op | ✅ no-op |

Root buys nothing and costs bubblewrap. The one thing genuinely unavailable at
either UID is bubblewrap's **network** namespace.

## Findings

### ⚠️ `seccompProfile: RuntimeDefault` breaks bubblewrap — on host runc

Confirmed in kind and again on k3s, `runAsUser: 1000`, host runc:

| seccomp profile | `bwrap --ro-bind / /` |
|---|---|
| unset (kubelet default) | ok, including `--unshare-net` |
| `RuntimeDefault` | **fails** — `No permissions to create new namespace` |
| Docker default (outside k8s) | **fails** — same |

The default profile blocks `unshare(CLONE_NEWUSER)` without `CAP_SYS_ADMIN`.
`--cap-add SYS_ADMIN` gets further and then fails at `pivot_root`.

This collided head-on with **Pod Security Standards `restricted`, which *requires*
`seccompProfile: RuntimeDefault`**. See the next finding: under gVisor the
collision disappears, which is the cheapest of the three options this finding
originally listed.

### ✅ gVisor dissolves the seccomp/PSS conflict

Same image, same probes, `runtimeClassName: gvisor`:

| posture | seccomp | `bwrap` plain | `bwrap --unshare-net` |
|---|---|---|---|
| gVisor, uid 1000 | unset | ok | **fails** — `loopback: Failed RTM_NEWADDR` |
| gVisor, uid 1000 | `RuntimeDefault` | **ok** | fails (identical) |
| gVisor, uid 0 | unset | **fails** — `make / slave: EPERM` | fails — `new namespace: EPERM` |
| gVisor, uid 0 | `RuntimeDefault` | fails | fails |

`RuntimeDefault` and unset are **indistinguishable** under gVisor: the sentry
serves the syscalls itself, so the host profile no longer gates `unshare`. PSS
`restricted` stops fighting ADR 0001 — no custom seccomp profile needed, and no
`Unconfined` exclusion to justify to a cluster admin.

### ⚠️ Root is *worse* than non-root under gVisor

Counterintuitive and it inverts the obvious "gVisor is the boundary, so who cares
about the UID" reasoning. As uid 0, bwrap fails even the plain `--ro-bind / /`
case: it takes the privileged mount path instead of creating a user namespace, and
gVisor holds **no capabilities on the host kernel** — by design. As uid 1000 it
creates a userns and gets caps inside it, so plain bwrap works.

So the posture choice is not "root is fine now". It is: root buys nothing here and
costs bubblewrap entirely.

### ⚠️ `bwrap --unshare-net` is still broken under gVisor

Reproduced on runsc `release-20260727.0` — newer than the
`release-20260608.0` in the upstream report, and *after*
[gvisor#13532][gv13532]'s netstack loopback fix attempt. Both
[bubblewrap#745][bubblewrap#745] and [gvisor#13438][gv13438] remain open. Our
errno differs (`No child processes` vs the reported `Operation not permitted`),
same `loopback_setup()` call site.

Lead, not yet chased: **`runsc bwrap` ships in this release** — a
bubblewrap-CLI-compatible reimplementation ("Causeway",
[gvisor#13347][gv13347]) — even though its documentation PR
[gvisor#13912][gv13912] is still open. The agent CLI's root check reads
`CLAUDE_CODE_BUBBLEWRAP`, so pointing that at `runsc bwrap` is worth a probe
before concluding env-scrub is unavailable.

### ⚠️ The agent CLI refuses `--dangerously-skip-permissions` as root

Cost $0 and 1s to discover, before any turn. The gate, read out of the 2.1.220
binary:

```js
process.getuid() === 0 && process.env.IS_SANDBOX !== "1" && !Z.CLAUDE_CODE_BUBBLEWRAP
```

The internal name is `isRootOutsideDeliberateSandbox()`, so root **is** intended
to work in a deliberate sandbox — `IS_SANDBOX=1` (or `CLAUDE_CODE_BUBBLEWRAP`)
lifts it. `job.yaml` sets `IS_SANDBOX=1` unconditionally; a gVisor-sandboxed Job
is exactly the case the check carves out. Any future run-as-root posture must
carry this env var or it fails instantly.

### ✅ Docker runs inside a gVisor sandbox — as uid 1000, unprivileged

`dind.sh`. The posture that matters is the last row, and it is the cheapest one:

| uid | image | privileged | RuntimeClass | result |
|---|---|---|---|---|
| 0 | `docker:29-dind` | yes | `gvisor-dind` | ✅ |
| 1000 | `docker:29-dind-rootless` | yes | `gvisor-dind` | ✅ |
| 1000 | `docker:29-dind-rootless` | **no** | `gvisor-dind` | ✅ |
| **1000** | `docker:29-dind-rootless` | **no** | **`gvisor`** | ✅ |

dockerd **29.6.2**, `overlayfs` driver, and all three probes pass: a container
runs, a **bind mount** round-trips, and **`docker compose up`** runs a service.

So the extra machinery is **not needed**: no `privileged`, no `-net-raw`, no
`-allow-packet-socket-write`, no second RuntimeClass. The `gvisor-dind` handler in
the setup above is retained only because it is what the wider matrix was measured
against — **the plain `gvisor` class is sufficient** and is what to ship. That
also sidesteps the fact that **GKE Sandbox rejects privileged pods at admission**
([GKE Sandbox limits][gkesandbox]).

What it does cost:

- **Rootless dockerd, not the standard dind image.** `docker:29-dind-rootless`
  under `rootlesskit --net=slirp4netns`. `dockerd-entrypoint.sh` cannot be used —
  it runs an unguarded `iptables --version` under `set -e` and iptables-nft cannot
  initialise under gVisor, so it dies before dockerd starts. Invoke rootlesskit
  directly.
- **No bridge network for inner containers.** Creating the default bridge writes
  `/proc/sys/net/ipv4/ip_forward`, which is EPERM for an unprivileged userns under
  gVisor. So `--bridge=none` and inner containers get `--network=none`. dockerd
  itself keeps slirp4netns, so **image pulls still work** — it is only the inner
  containers that have no network. For a sandboxed run that is a feature.
  Consequence: no `docker run -p`, and `compose` services need `network_mode: none`.
- **tmpfs for the data root**, because overlayfs cannot stack on overlayfs. Mount
  the *parent* (`~/.local/share`), not the data root itself — an `emptyDir` arrives
  `root:root` and `fsGroup` only makes it group-writable, so dockerd's `chmod` of
  its own data root fails with EPERM inside the userns.
- **`docker build` not pursued.** It got past the network and hit an intermittent
  containerd snapshotter mount lock (`ref ... locked ... unavailable`). Running a
  container, mounting, and compose were the questions; building was not, so this is
  recorded as unknown rather than chased.

### ➖ Will this run on production GKE? Mostly yes, with two unknowns

Not tested — this is from GKE's docs, and one answer is "not published".

- **The GKE runsc version is not published anywhere.** No docs page, no release
  note, no version-mapping table, no node label. The node label
  `sandbox.gke.io/runtime=gvisor` only confirms sandboxing is *on*. So we cannot
  know in advance whether a given GKE cluster has the bwrap loopback fix, or any
  other runsc-version-dependent behaviour. The only in-cluster signal is gVisor's
  `dmesg` banner, which does not carry a version either.
- **The agent Job should run**: non-root uid 1000, `readOnlyRootFilesystem` and a
  `batch/v1` Job are all standard fields gVisor does not touch, and Google's own
  ADK code-executor runs sandboxed **Jobs** as precedent. Requires: node pool with
  `--sandbox type=gvisor` and `--image-type=cos_containerd` (the *only* supported
  image type), **at least two node pools** — "the default node pool can't use GKE
  Sandbox if it's the only node pool" — and a pod referencing the auto-created
  `gvisor` RuntimeClass. Autopilot needs 1.27.4-gke.800+. Sandboxed and
  non-sandboxed pods **cannot share a node pool**, and GKE Sandbox cannot be
  disabled on a pool once enabled — you delete the pool.
- **Docker-in-sandbox is documented on GKE** ([tutorial][dockeringke]) but by a
  *different route*: GKE bakes the runsc flags in per-version rather than exposing
  them, gated to **Docker v27 (GKE 1.29+) and v28 (GKE 1.35.3+) — v29 is not
  supported at all**. It uses `securityContext.capabilities` (`NET_ADMIN`,
  `SYS_ADMIN`, `NET_RAW`, …), never `privileged`. Our result is on Docker **29**,
  so it does not transfer as-is; the unprivileged-rootless posture is a better
  match for GKE than the privileged one, but the Docker version is a real gap.
- **Unknown: `seccompProfile: RuntimeDefault` on GKE Sandbox.** GKE lists "Linux
  kernel security modules (Seccomp, AppArmor, SELinux, …)" as *incompatible* with
  GKE Sandbox, and the seccomp-in-GKE page says nothing about the interaction. Our
  finding that gVisor makes the profile a no-op is consistent with that, but
  whether GKE *rejects* the field or ignores it is untested.
- **Autopilot tension**: Autopilot "prevents you from adding the `NET_RAW`
  permission", while the Docker-in-sandbox path wants it — and Autopilot
  additionally needs `--workload-policies=allow-net-admin` set at **cluster
  creation**. If Docker-in-sandbox matters, Standard is the safer target.

### ✅ `/dev/termination-log` is writable under `readOnlyRootFilesystem: true`

[Issue #22][22] question 1. The kubelet's bind mount survives the read-only
rootfs, and the compact result channel round-trips to
`.status.containerStatuses[].state.terminated.message`. `/` is confirmed
read-only in the same run, so this is not a false positive.

### ⚠️ Matt's skills register **prefixed**, not bare

ADR 0001 records `--plugin-dir` resolving a skill as bare `/probe-implement`.
That was a hand-rolled single-skill plugin. The real marketplace repo loads
cleanly (`plugin_errors: null`, `mattpocock-skills@1.2.0`) but advertises
`mattpocock-skills:implement`, `mattpocock-skills:tdd`,
`mattpocock-skills:code-review` — no bare `implement` in `slash_commands`.
The phase script uses the prefixed form. Bare form untested with credentials.

### ✅ Skills load from a baked, read-only path with an ephemeral `HOME`

`--plugin-dir /opt/skills`, `HOME` an `emptyDir`, no `~/.claude/plugins`.
Cloned at a pinned SHA with `.git` removed.

### ✅ Agent CLI installs as a bare pinned binary

`install.sh` writes a launcher into `$HOME`, which is ephemeral here. Fetching
`downloads.claude.ai/claude-code-releases/$VERSION/linux-x64/claude` directly and
verifying the manifest checksum avoids that, and keeps node/npm out.

### ✅ The whole path works end to end — including under gVisor

Green runs against [tmp-test-repo#1][t1]: preflight → clone → issue fetch →
`claude -p /mattpocock-skills:implement` → commit → push → CI green.

| run | runtime | uid | elapsed | cost | CI |
|---|---|---|---|---|---|
| kind | host runc | 1000 | 162s | $1.01 | ✅ |
| `implementer/issue-1-gvisor-root` | **gVisor** | 0 | 206s | $0.95 | ✅ |
| `implementer/issue-1-gvisor-nonroot` | **gVisor** | 1000 | 202s | $0.88 | ✅ |

gVisor costs roughly **25% wall-clock** on this workload (162s → ~204s) and
nothing in dollars. `readOnlyRootFilesystem: true` and the
`/dev/termination-log` result channel both still work under gVisor, at both UIDs.

- Cost is per-run and visible in the `result` message, so `--max-budget-usd` has
  something real to bound.
- **`--json-schema` round-trips.** `structured_output` came back valid on the
  success path, carrying `status`, `summary`, `pr_title`, `files_changed` — the
  orchestrator can build a PR deterministically from it without parsing prose.
- **`/code-review` spawned parallel subagents** (Standards + Spec) inside `-p`,
  as the capability audit predicted.
- **The agent wrote a conventional-commit message and `Closes #1` unprompted.**
  Neither was in the brief. Worth deciding whether the run plan should own those
  rather than leave them to the model.

### ⚠️ A transient 529 burns the run

One earlier attempt died after **209s** on ten `api_retry` events, all
`529 overloaded`, before emitting any turn. The retry ladder is internal to the
agent CLI and caps at 10; there is no partial progress to resume from. So an
unattended run has a failure mode that costs $0 and three minutes and is fixed
entirely by trying again — which is an argument for orchestrator-level retry on
`is_error` with no turns, distinct from retrying a *failed implementation*.

Also: `structured_output` is **absent** on error paths, so status fell to
`unknown` and the phase script initially mistook an infra failure for "the agent
declined". A `result` carrying `is_error` now fails hard regardless of status.

### ➖ CI triggering is NOT settled

The push triggered the target repo's workflow and it went green — but this
prototype pushes with a **user PAT** (`gh auth token`), not a GitHub App
**installation token**. [Issue #13][13]'s open question is specifically about the
latter. This result does not answer it.

## dind-net.sh — can an inner container reach the agent? (issue [#28][28])

`dind.sh` left Docker looking usable and it was not. It ran dockerd under
`rootlesskit --net=slirp4netns` and only ever started containers that print and
exit, so it never asked the question that decides whether Docker is worth having:
**can the sandbox's own process connect to a service running in an inner
container?** Without that, `testcontainers` and any `compose` where the app talks
to a database are impossible — which is the entire reason a coding sandbox wants
Docker.

`dind-net.sh` asks it. Three paths were on the table in #28; the third wins.

### ✅ Wrap the WHOLE phase script in rootlesskit, not just dockerd

`MODE=wrap` (the default). The agent process, dockerd, and inner containers on
`--network=host` then share one network namespace, so `localhost` is shared and no
bridge is needed. Every networking probe passes, at uid 1000, unprivileged, on the
plain `gvisor` RuntimeClass:

| probe | result |
|---|---|
| dockerd boots | ✅ 29.6.2 |
| image pull | ✅ |
| **sandbox → inner container** (the acceptance criterion) | ✅ |
| **inner container → sandbox** | ✅ |
| inner → inner | ✅ |
| inner container egress | ✅ |
| **`compose` service reachable from the sandbox** | ✅ |
| `docker build`, self-contained `RUN` | ✅ *with `--feature containerd-snapshotter=false`* |
| `docker network create` (bridge) | ❌ expected — `ip_forward` is EPERM |
| `docker build` with a network-using `RUN` | ❌ buildkit wants a bridge for `RUN` |

Two things this corrects in the `dind.sh` section above:

- **`docker build` is no longer unknown.** The "intermittent containerd snapshotter
  mount lock" was the containerd snapshotter, not intermittence. `--feature
  containerd-snapshotter=false` (overlay2) builds cleanly, first try. `RUN` steps
  that fetch from the network still fail — buildkit gives them a bridge network we
  cannot create — so `--network=none` builds work and dependency-fetching ones do not.
- **"No `docker run -p`, no inter-container networking" was the right observation
  from the wrong configuration.** On a shared netns none of it is needed: containers
  talk over `localhost` and publishing is moot.

### ⚠️ Gate on `docker version`, never `docker info`

`docker info` answers while buildkit is still initialising — roughly 6 seconds
before `Daemon has completed initialization`. A readiness loop that breaks on
`docker info` therefore returns before the daemon is usable, and everything after
it fails with `EOF` against the socket. `dind.sh` did not notice because it only
ran short-lived containers. Gate on `docker version`.

### ❌ `rootlesskit --net=host` panics dockerd (upstream bug)

`MODE=host` reproduces it. Path 1 in #28 — drop the netns so dockerd shares the
pod's — was the cheapest option and it is blocked:

```
2026/07/31 19:35:11 http: panic serving @: runtime error: invalid memory address
  or nil pointer dereference
  ...daemon/server/router/system.(*systemRouter).getVersion
     daemon/server/router/system/system_routes.go:132
```

dockerd boots fine and `docker info` returns 0, so it looks healthy. The first
`docker version` panics the handler, the CLI cannot negotiate an API version, and
every later command fails. Docker 29.6.2, runsc `release-20260727.0`. Not seen
under `--net=slirp4netns`, so it is specific to `--net=host`. Worth reporting
upstream.

### ⚠️ The cost: inside rootlesskit the process is uid 0

```
PROBE whoami   uid=0 gid=0
PROBE uid_map   0 1000 1| 1 100000 65536|
```

The pod is still uid 1000 to gVisor and to the kernel — but code inside the
namespace reads its own uid as 0. Wrapping the phase script therefore has two
consequences that land squarely on [ADR 0001][adr1]:

1. **The agent CLI's root gate trips.** It is
   `getuid()===0 && IS_SANDBOX!=="1" && !CLAUDE_CODE_BUBBLEWRAP`, so
   `--dangerously-skip-permissions` refuses unless `IS_SANDBOX=1` is set —
   the exact bypass the audit in [#6][6] found and that the non-root posture was
   chosen to avoid needing.
2. **bubblewrap is at risk, and this run did not settle it.** [#22][22] found bwrap
   fails as uid 0 under gVisor because the sandbox holds no host capabilities, so
   bwrap takes the privileged mount path and is denied. Inside rootlesskit the uid
   *is* 0. ADR 0001 requires the bubblewrap binary, so this needs measuring before
   any adoption. **Status: unmeasured**, for the reason below.

### ➖ Grafting this onto *our* base image is not "add a package"

Three attempts to reproduce the working setup on `implementer-proto:dev` failed on
progressively deeper missing pieces:

| attempt | missing |
|---|---|
| 1 | `slirp4netns` |
| 2 | `newuidmap` (Debian `uidmap`) |
| 3 | `newuidmap: write to uid_map failed: Operation not permitted` — needs `/etc/subuid` ranges plus the privilege to apply them |

`docker:29-dind-rootless` works because it is *purpose-built* as a rootless-docker
image: a dedicated user with subuid/subgid ranges and appropriately privileged
`newuidmap`/`newgidmap`. So the real cost is **"make our base image a
rootless-docker image"** — `docker-cli`, `dockerd`, `containerd`, `runc`,
`rootlesskit`, `slirp4netns`, `fuse-overlayfs`, `uidmap`, plus the subuid plumbing
and the privilege bits — not `apt-get install docker`. It is still only a
Dockerfile, but it is a different image, and it is what blocked the bwrap
measurement above.

### What #28 should decide with this

The ticket's question is answered **yes**: an inner container can reach the agent
process, and the testcontainers shape works. But the path that achieves it costs
uid-0-inside-the-namespace, which collides with two things ADR 0001 decided
deliberately. That is a scope call, not a measurement — the measurement is done.

## proxy/ — does the credential proxy work, and what does it cost? (issue [#33][33])

[#14][14] put the proxy into MVP on its Vertex half: the sandbox holds **zero**
Anthropic or GCP credential, and the proxy carries the identity. That rested on a
first-party mechanism nobody had run. `proxy/` runs it.

One Go binary, one port, two halves of the same question:

| what | how |
|---|---|
| `POST /vertex/…` | `httputil.ReverseProxy` to `{loc}-aiplatform.googleapis.com`, attaching a GCP token from `golang.org/x/oauth2/google` ADC |
| `CONNECT host:443` | plain tunnel, so `https_proxy` in the sandbox funnels `git`/`gh`/`go`. No TLS interception — that is the termination ticket |

The proxy needs no region config: Claude Code puts the location in the request
path, so `rewriteVertex` reads it back out (`main_test.go` covers the three host
shapes — `global`, multi-region `eu`/`us`, regional).

### ✅ It works — a real run on Vertex with no credential in the sandbox

`VERTEX=1 RUNTIME=gvisor`, uid 1000, `readOnlyRootFilesystem`, against
[tmp-test-repo#1][t1]. Preflight → clone → `/mattpocock-skills:implement` →
commit → push, all model traffic over one plaintext in-cluster hop:

| run | provider | model | elapsed | cost | CI |
|---|---|---|---|---|---|
| `implementer/issue-1-gvisor-nonroot` (baseline, [#22][22]) | direct API | CLI default | 202s | $0.88 | ✅ |
| `implementer/issue-1-vertex` | **Vertex via proxy** | claude-sonnet-4-6 | **249s** | $1.02 | ❌ *see below* |

The load-bearing probe is `model-creds`:

```
PROBE model-creds  vertex=1 base=http://proto-proxy…:8080/vertex oauth=absent apikey=absent
```

`go.sh` **omits** `CLAUDE_CODE_OAUTH_TOKEN` from the Secret on `VERTEX=1` rather
than blanking it, so a working run is proof rather than an assertion. The sandbox's
only credential is the GitHub token.

Incidental robustness datapoint: `up.sh` rolled the proxy Deployment **mid-run**
and the agent did not notice — Service DNS plus the CLI's own retry ladder covered
it. Restarting the credential holder under a live run is survivable.

### ✅ An `http://` base URL works — MVP needs no certs for this hop

Question 1, answered yes. `ANTHROPIC_VERTEX_BASE_URL=http://proto-proxy…:8080/vertex`
with `CLAUDE_CODE_SKIP_VERTEX_AUTH=1`; no TLS, no cert-manager, no
`NODE_EXTRA_CA_CERTS`. The sandbox sends no credential over that hop, so plaintext
leaks nothing beyond the prompt. **cert-manager stays out of MVP.**

### ✅ SSE streaming survives `ReverseProxy` natively

No `FlushInterval` tuning, no custom `http.Flusher` plumbing. A 60-token
`:streamRawPredict` arrives as deltas ~20 ms apart, and the proxy's own log shows
`ttfb=2.833s total=3.211s` — a buffering proxy would show those equal. Every
`streamRawPredict` in the real run behaves the same, up to `total=56s` on the long
turns while `ttfb` stayed ~1.5s.

### ✅ `https_proxy` catches `git`, `gh` and `go`

`proxy/probe.sh` runs a throwaway `golang:1.25` pod with nothing but `https_proxy`
set — a toolchain in the sandbox image would cost 450 MB of apt to answer this
once. The evidence is the proxy's own log, not the commands succeeding (direct
egress is open on this cluster, so success alone proves nothing):

```
CONNECT proxy.golang.org:443 established     ← go get / go install
CONNECT sum.golang.org:443 established       ← checksum db
CONNECT github.com:443 established           ← git ls-remote, git clone
CONNECT api.github.com:443 established       ← gh
```

All four go through the tunnel unprompted. That log doubles as the **egress
inventory** the allowlisting ticket will start from.

### ⚠️ The model pin is dictated by the project, and here it cost quality

The GCP project used for this spike can invoke exactly two Claude models —
measured, not read off a docs page, by a 1-token `:rawPredict` per pair through the
proxy. Its id is deliberately nowhere in this repo; it lives in `proto/.vertex.env`,
which is gitignored, and reaches the Job as a ConfigMap value.

| location | invocable |
|---|---|
| `global`, `us-east5`, `europe-west1` | `claude-sonnet-4-6`, `claude-haiku-4-5@20251001` |

Everything else 404s: `claude-opus-5`, `claude-sonnet-5`, `claude-opus-4-8`, and
even `claude-sonnet-4-5@20250929`. Consequences:

- Claude Code's Vertex default primary model **is** `claude-opus-5`, so an unpinned
  deployment starts by falling back. Pinning is not optional here.
- The `opus` **alias** must be remapped too (`ANTHROPIC_DEFAULT_OPUS_MODEL`), or any
  subagent asking for opus 404s. Three variables, not one.
- So the Helm values are `ANTHROPIC_MODEL`, `ANTHROPIC_DEFAULT_SONNET_MODEL`,
  `ANTHROPIC_DEFAULT_OPUS_MODEL`, `ANTHROPIC_DEFAULT_HAIKU_MODEL`, and they are a
  **property of the customer's GCP project**, not of our release. The haiku pin is
  load-bearing: the run's background traffic used it.

And the cost is not only configuration. **This run pushed a red branch.** The agent
wrote `TestMedian`, self-reported `status: completed`, and CI failed on its own
test — a nil-versus-empty-slice comparison in the empty case:

```
--- FAIL: TestMedian/empty
    calc_test.go:114: Median mutated input: got [], want []
```

Baseline runs on the direct API went green on this same issue. n=1, and the
variable is the **model** rather than the proxy — Sonnet 4.6 because that is what
the project has — so this is an observation, not a law. But it is the shape of the
trade: "the sandbox holds no credential" is paid for in whatever models the
customer's project has enabled. Separately, it is an argument for the run plan
owning a test gate, since `status: completed` did not survive contact with `go test`.

### ✅ `--max-budget-usd` still binds, at list rates

`MAX_BUDGET_USD=0.001` on the smoke turn: `rc=1`, `is_error: true`, `result: null`,
and `total_cost_usd: 0.115` still reported. At `10` the same turn returns
`SMOKE-OK` and `rc=0`. So [#12][12]'s per-phase budget keeps working on Vertex —
Claude Code computes cost locally from token counts at Anthropic list rates, which
also means **it stops matching the GCP bill**. It is a runaway-agent guard, not an
accounting figure, and it should be documented as one.

### ➖ What the extra hop costs: not much, but the number is confounded

The hop itself is a plaintext connection inside one node; the proxy's `ttfb`
(0.9–2.6 s on `streamRawPredict`) is the Vertex round trip, not overhead. The
honest end-to-end figure is **202s → 249s, about +23%** — and it bundles the proxy
hop with a provider change *and* a model change. A clean proxy-only number would
need the sandbox talking to Vertex directly, which is the posture this whole ticket
exists to avoid. Recorded as an upper bound.

For scale: gVisor alone already costs ~25% ([#22][22]), so sandbox plus proxy is
roughly 1.5× the wall-clock of an unsandboxed direct-API run.

### ⚠️ The proxy's own auth on k3s — and the quota-project trap

No metadata server on k3s, so Workload Identity is unavailable and the proxy mounts
a credential file (`up.sh` puts `~/.config/gcloud/application_default_credentials.json`
into a Secret). Sanctioned by the ticket for the *proxy* and it must not leak to the
sandbox side. It works: the pod logs `ADC ok, token type=Bearer` and refreshes
itself for the run's duration.

One trap this surfaced that production will not share: a user ADC credential
(`type: authorized_user`) is billed against a **quota project**, so the proxy sends
`X-Goog-User-Project`. `up.sh` sets it only when the credential file is
`authorized_user`, because a service account does not need it. Whether omitting it
actually fails here is **untested** — under Workload Identity the question does not
arise, so it was not worth a run.

### ➖ What this does not answer

TLS interception, cert-manager and the GitHub sentinel swap — answered since, in the
[#34][34] section below, and GAR credential injection with it. Egress allowlisting
and `NetworkPolicy` enforcement are still open.
Also untested: Workload Identity itself (needs GKE), and whether the ~23% is
proxy or provider.

## proxy/ — can the GitHub token stop entering the sandbox? (issue [#34][34])

[#33][33] terminated the *model* credential at the proxy and dodged TLS entirely
with an `http://` base URL. That dodge is unavailable here: the sandbox believes it
is talking to `github.com`, so the proxy has to present a certificate for that name
and the sandbox has to trust whoever signed it.

Scope was deliberately one mechanic. Everything else #34 inherits — push-branch
enforcement, GAR injection, KMS JWT signing, `NetworkPolicy`, egress allowlisting,
and *who mints* the installation token — was left alone, because all of it is cheap
to decide once the pipe exists and expensive to argue before.

```sh
./proxy/ca.sh                 # cert-manager + the private CA, standalone
PREFLIGHT_ONLY=1 VERTEX=1 RUNTIME=gvisor ./go.sh nissessenap/tmp-test-repo 1
```

The sandbox's `GH_TOKEN` is the literal string `proxy-injected`. Nothing else
changed in the run plan's credential path — `phase.sh` still builds
`https://x-access-token:${GH_TOKEN}@github.com/…`, exactly as [#34][34] predicted
the seam would behave.

### ✅ The whole thing works, and the sandbox holds no GitHub credential either

Five probes, all free, no agent turn:

```
PROBE proxy-ca         trusted (151 certs)
PROBE https_proxy      http://proto-proxy…:8080 :: git=ok gh=ok
PROBE gh-token         sentinel=proxy-injected rate_limit=5000
PROBE git-basic        receive-pack=200
PROBE git-sentinel     clone=ok push-dry-run=ok
```

`rate_limit=5000` is the assertion that matters: GitHub gives 60/h anonymous and
5000/h authenticated. The sandbox sent a worthless string and got the authenticated
number back, which can only happen if the proxy substituted a real token *inside* a
TLS session the sandbox believed was GitHub's.

The proxy's own log is the other half:

```
MITM github.com     handshake ok
MITM api.github.com GET  /rate_limit                       -> 200 auth=token-swapped
MITM github.com     GET  /…/info/refs (receive-pack)       -> 401 auth=none
MITM github.com     GET  /…/info/refs (receive-pack)       -> 200 auth=basic-swapped
```

### ✅ `git` and `gh` both honour the CA bundle — but they need different variables

[ADR 0001][adr1] ships the trust seam as `SSL_CERT_FILE` / `NODE_EXTRA_CA_CERTS`
and #34 asked whether it actually reaches `git` and `gh` rather than just the agent.
It does, but only because `phase.sh` sets four variables, not one:

| tool | reads |
|---|---|
| `gh` (Go `crypto/x509`) | `SSL_CERT_FILE` |
| `git` (libcurl) | `GIT_SSL_CAINFO` |
| `curl` | `CURL_CA_BUNDLE` |
| agent CLI (Node) | `NODE_EXTRA_CA_CERTS` |

The trap is that the first three **replace** the trust store rather than adding to
it. Pointing them at `ca.crt` alone leaves the sandbox unable to verify anything
else on the internet — so the bundle is `ca-certificates.crt` *concatenated* with
`ca.crt`, assembled into `/tmp` because `/etc/ssl/certs` is read-only at run time.
`NODE_EXTRA_CA_CERTS` is the one that is genuinely additive. Three lines of run
plan; in production the image ships the bundle or an initContainer writes it.

### ⚠️ `git` does not send credentials preemptively — the swap must survive a 401

The single most transferable finding, and one a header-only proxy would get wrong.

`curl -u` sends `Authorization: Basic` on the first request. **`git` does not**,
even with the token in the URL's userinfo: it makes an anonymous request, takes the
`401`, and only then retries with basic auth. Both round trips are visible above.
A proxy that swaps on the first request of a connection, or that treats an
unauthenticated request as "no credential involved", silently breaks `git push`
while `gh` keeps working.

Also worth naming: on a **public** repo `git clone` authenticates not at all
(`auth=none` throughout). The credential only appears on the push handshake. Any
future push-branch enforcement therefore hangs off `service=git-receive-pack`, not
off "requests that carry a token".

### ✅ The sentinel swap needs two shapes, and [#33][33] only ever produced one

The [#33][33] handoff flagged this and it was right. `swapAuth` handles:

```
Authorization: Basic base64(x-access-token:SENTINEL)   git, from the clone URL
Authorization: token|Bearer SENTINEL                   gh, from GH_TOKEN
```

git also accepts the token as the *username* half, so both are matched. A
credential that is not the sentinel is passed through untouched and logged as
`basic-not-sentinel` — swallowing a smuggled credential would only hide it.
`main_test.go` covers all of it, including an assertion that no verdict string
ever contains a credential, since these lines are the run's audit trail.

### ✅ cert-manager is the right shape, and the SAN list is the only config

`ca.sh` installs cert-manager v1.21.1 and creates the two-Issuer chain a private CA
needs — `selfSigned` Issuer → CA `Certificate` → `ca` Issuer → leaf. A self-signed
*leaf* is not a CA and nothing chains to it; that is the one shape worth knowing
before writing the chart.

The leaf carries five SANs (`github.com`, `api.github.com`, `codeload.`, `objects.`,
`raw.githubusercontent.com`) and **the proxy reads its intercept list back off its
own certificate**. A host it cannot present a cert for is a host it must not
intercept, so the two cannot drift apart and there is no second list to maintain.
Everything not on it still gets the plain `CONNECT` tunnel from [#33][33].

Distribution is a `ConfigMap` holding only `ca.crt`; the key never leaves the
proxy's Secret. cert-manager's `trust-manager` does exactly this in production and
was not worth a second install to learn.

### ➖ Cost: not measurable against the noise

A handshake plus one extra TLS termination per connection. Against 150–350 ms
GitHub round trips it does not show up, and the interesting number ([#33][33]'s
confounded +23%) is a model-call figure this does not touch.

### ➖ What this does not answer

**Who mints the token** — orchestrator-mints-and-hands-over vs proxy-mints is still
open and this prototype deliberately does not discriminate: `up.sh` puts a PAT in a
Secret, which is neither. It is now a cheap question, because the swap point is
proven and both designs feed the same `swapAuth`.

Untouched, and each one now independently additive: push-branch enforcement (the
actual prize — hang it off `git-receive-pack`), KMS JWT signing, egress
allowlisting, `NetworkPolicy`. GAR credential injection is done — next section.
Also untested: certificate rotation
across a run (cert-manager renews at 2/3 of a 720h lifetime; the longest run so far
is 249s), and HTTP/2 — the proxy forces HTTP/1.1 upstream because it writes
responses back verbatim, which no client has yet minded.


## proxy/ — can the sandbox build against a private Go registry? ([#34][34] cont.)

The [#34][34] section above deliberately shipped one mechanic and listed GAR
injection as inherited-but-untouched. This is that piece, and it is the cheapest
thing in the whole ticket: the proxy **already holds the right token**. The Vertex
half mints an ADC access token scoped `cloud-platform`; Artifact Registry accepts
exactly that. So there is no new credential, no new secret and no new mount — only
a decision about *which* hosts get *which* credential.

```sh
./proxy/up.sh && ./proxy/probe.sh    # GAR_REGION / GAR_REPO / GAR_MODULE override
```

### ✅ Proven end to end — a private package installed into a pod with no GCP credential

The strongest result in this file, and it took a correction to get to. The first
pass reported `gcloud artifacts repositories list` returning **zero repositories**
and concluded the fetch was unprovable; that was wrong — the project has plenty.
None of them are **Go** repos, which is the part that was true and the part that
matters, because the prototype only recognised `{region}-go.pkg.dev`.

Artifact Registry's Python endpoint is `{region}-python.pkg.dev` — already covered
by the `*.pkg.dev` SAN, but not by `credFor`, so it was being intercepted and
forwarded anonymously. Measured before changing anything: the Python index accepts
a **plain bearer** exactly like the Go endpoint (anonymous 401, garbage bearer 401,
`Authorization: Bearer <gcp-token>` 200, basic `oauth2accesstoken:<token>` also
200). So one `credFor` arm covers both and no second credential shape was needed.

One pod, `python:3.13-slim`, `https_proxy` and the proxy's CA and **nothing else** —
no key file, no `gcloud`, no `.netrc`, no `GOOGLE_APPLICATION_CREDENTIALS`:

```
PROBE gar-py-direct   refused: 401
PROBE gar-py-install  ok (<pkg> <pkg>-0.6.2.dist-info tools)
```

Direct is a 401. Through the proxy the same `pip install` **completes** — a real
private wheel, unpacked on disk. That is the fetch, not a status code.

The proxy's log shows the whole shape, including a redirect nobody predicted:

```
MITM …-python.pkg.dev GET /…/simple/<pkg>/                  -> 200 auth=gcp-attached
MITM …-python.pkg.dev GET /…/<pkg>-0.6.2-…-linux_x86_64.whl -> 307 auth=gcp-attached
MITM …-python.pkg.dev GET /artifacts-downloads/namespaces/… -> 200 auth=gcp-attached
```

**The 307 stays on the same host**, so the redirect target is still on the cert,
still intercepted, and still gets a token — which it needs. Worth noting against
the Docker worry below: an artifact store that redirects *off*-host would land on a
name the cert does not cover, fall through to the plain `CONNECT` tunnel, and
receive nothing. The SAN list is doing structural work there, not just naming work.

Paths (project, region, repository, package) live only in the gitignored
`proto/.vertex.env`, as `GAR_PY_REGION` / `GAR_PY_REPO` / `GAR_PY_PACKAGE`. The
probe skips silently when they are unset. A customer's registry layout does not
belong in this repo.

### ⚠️ `go mod download` specifically is still unproven

There is no Go repository in the project, so the Go path stops where it did: a
`404` from a repository that does not exist. The credential mechanism is identical
and now demonstrated against a real repository through Python, so this is a gap in
coverage rather than a gap in confidence — but it is a gap, and one Go repo with
one module in it would close it.

### ✅ The injection works: the same URL is 401 direct and 404 proxied

`probe.sh` is now a differential. One pod, `golang:1.25`, `https_proxy` and the
proxy's CA and **nothing else** — no key file, no `gcloud`, no `.netrc`, no
`GOOGLE_APPLICATION_CREDENTIALS`:

```
PROBE gar-direct   401 (401 = anonymous)
PROBE gar-proxied  404 (404 = authenticated, no such repo)
```

and the proxy's own log:

```
MITM europe-west1-go.pkg.dev handshake ok
MITM europe-west1-go.pkg.dev GET /…/nosuchrepo/github.com/x/y/@v/list -> 404 auth=gcp-attached 104ms
```

404 is the *good* answer here. Anonymously GAR returns `401` with
`WWW-Authenticate: Basic realm="https://europe-west1-go.pkg.dev"`; with a garbage
bearer it also returns `401`; only a token GAR actually accepts gets as far as
"that repository does not exist". So the 401→404 flip cannot happen unless a
credential the pod never held reached Artifact Registry — which is the entire
claim. Confirmed from the host too: a plain ADC access token as
`Authorization: Bearer` gives 404, and `Basic oauth2accesstoken:<token>` gives the
same, so the Bearer shape is not a lucky guess.

The `go` command reaches the identical conclusion, which matters because a build
runs `go`, not `curl`:

```
GOPROXY=https://…-go.pkg.dev/…/nosuchrepo go list -m -versions github.com/x/y
go: reading …/@v/list: 404 Not Found
    server response: Repository "nosuchrepo" not found
```

That prose response is generated by GAR *after* authenticating the caller.

### ✅ `go` never sends a credential at all — so #34's swap-on-401 rule inverts here

The most transferable finding, and it is the exact opposite of the GitHub one.
[#34][34] above found `git` makes an anonymous request, takes the 401, and only
then retries with basic auth — so the swap has to survive a 401. `go` does not do
that. Without a `.netrc` it sends nothing, ever, and it **does not retry**: the
401 is simply the error the build dies on.

So the two halves need genuinely different rules, and neither generalises:

| client | sends | proxy must |
|---|---|---|
| `git` / `gh` | a sentinel, sometimes after a 401 | **swap** it, on every request of the connection |
| `go` | nothing | **attach** unconditionally, on the first request |

`attachGCP` also *overwrites* a credential the sandbox smuggled in, where
`swapAuth` deliberately passes one through. Different rule, deliberately: GAR has
no equivalent of "a token that is not ours might still legitimately be the user's",
so passing one through would only 401. Both verdicts are logged
(`gcp-attached`, `gcp-replaced`) and `main_test.go` asserts neither ever contains
the token.

### ✅ One wildcard SAN, and it is deliberately wider than the credential rule

GAR's Go endpoint is `{region}-go.pkg.dev`. Listing regions in the cert would
reintroduce precisely the region configuration [#33][33] worked to remove, so the
SAN is **`*.pkg.dev`**. The tighter `*-go.pkg.dev` is not an option: `crypto/x509`
— and every other TLS stack worth naming — only matches a wildcard that is the
*whole* leftmost label, so a partial wildcard would issue fine and then be rejected
by every client.

That leaves the certificate covering more than the credential rule should, so the
"the SAN list is the only config" property from [#34][34] gains exactly one
qualifier: the SAN list still decides **what is intercepted**, and a three-arm
`credFor(host)` switch decides **what credential it gets**.

| host | intercepted | credential |
|---|---|---|
| `github.com`, `api.github.com`, `*.githubusercontent.com` | ✅ | sentinel swap |
| `{region}-go.pkg.dev` | ✅ | GCP bearer |
| `{region}-docker.pkg.dev` | ✅ (on the cert) | **none** — see below |
| everything else | ➖ plain `CONNECT` | none |

`us-central1-docker.pkg.dev` therefore gets terminated and forwarded with no
credential added, which is a real behaviour change: an OCI pull that would have
tunnelled untouched now goes through us and stays anonymous. Acceptable here
because nothing in this prototype pulls from GAR, and named because it is a trap
for whoever ships this.

### ➖ Docker/OCI pulls: out of scope, and possibly easier than the ticket assumed

Deliberately no code, but worth three measured sentences because the ticket
predicted a two-hop dance. Anonymously `GET /v2/` on `{region}-docker.pkg.dev`
returns `401` with `WWW-Authenticate: Bearer realm=".../v2/token",service=…,scope=…`,
and a client is then expected to go fetch a scoped token from that realm using
credentials the sandbox does not have — that part is exactly as described. But the
challenge only fires on an *unauthenticated* request: the same `/v2/` carrying a
plain ADC access token as `Authorization: Bearer` returns **200**, and a manifest
GET returns 404-not-401. So a proxy that attaches unconditionally may well
suppress the dance entirely rather than have to play it. Two things stop that
being a claim: containerd/Docker may still hit the token endpoint proactively, and
blob fetches redirect to pre-signed storage URLs that must **not** be given a
bearer. Untested, and it stays out of scope.

### ➖ What this does not answer

- **The end-to-end fetch.** No `go mod download` of a real private module has ever
  run. Everything above is one HTTP request's status code.
- **Whether `roles/artifactregistry.reader` is sufficient.** The ADC identity here
  is a *user* with broad org access, so a 404 proves "GAR accepted this identity",
  not "this role was what granted it". Nor can it distinguish 404-not-found from a
  403 a read-denied identity would get, because no repository exists to be denied.
- **`GOPRIVATE` / `GONOSUMDB` in the run plan.** The probe sets `GOSUMDB=off` by
  hand. A real build needs the private module path excluded from the checksum
  database, and `GOPRIVATE` is the wrong tool — it sets `GONOPROXY` too, which
  makes `go` bypass GOPROXY and go direct, defeating the whole mechanism.
- **Token lifetime across a long fetch.** `attachGCP` calls `ts.Token()` per
  request so refresh is covered, but a single large module download outliving the
  token has not been provoked.
- **Anything about OCI**, per above.

Versions: Claude Code `2.1.220`, `golang.org/x/oauth2 v0.36.0`, Go 1.25,
`gcr.io/distroless/static-debian12:nonroot`, runsc `release-20260727.0`,
k3s `v1.36.2+k3s1`.

---

## phase.sh — do the two review phases run unattended? (issue [#31][31])

[#12][12] fixed the run plan at **three** agent phases — `implement`, then
`code-review + fix`, then `ponytail-review + fix` — but every green run above
exercised only `implement`. So two thirds of the decided run plan was unmeasured.
This grows `proto/phase.sh` to three `claude -p` invocations and measures the
whole thing.

**No Kubernetes and no proxy.** [#22][22] settled the posture, [#33][33] and
[#34][34] settled the credential path; neither bears on whether a review phase
hangs. `proto/local.sh` runs the identical `phase.sh` and image under
`docker run` — 30 lines, and it is what produced every number below. `go.sh`
still exists for posture questions and gains only a `TOOLCHAIN` env var.

Target: **[tmp-test-repo#2][t2]**, filed deliberately loose — *"cover count, sum,
min, max, mean and median. Make it easy to extend with more statistics later."*
No acceptance criteria, no out-of-scope section. [#1][t1]'s brief is airtight and
produces a diff clean enough that both review phases would change nothing, which
measures nothing.

### ✅ Three phases, one run, green — and cheaper than projected

| phase | rc | status | elapsed | cost | commits | dirty after | subagents |
|---|---|---|---|---|---|---|---|
| `implement` | 0 | `completed` | 215s | $0.897 | +1 | 0 | 2 |
| `review` | 0 | `completed` | 169s | $0.811 | +1 | 0 | **3** |
| `ponytail` | 0 | `completed` | 52s | $0.322 | +1 | 0 | 0 |
| **run** | | `completed` | **439s** | **$2.03** | **3** | 0 | |

Pushed `implementer/issue-2-3phase`, three commits, and **CI went green**.

[#12][12] projected **~600s and ~$2.70** from the single-phase measurements. Actual
is **439s / $2.03** — the projection was pessimistic by ~27%, because it assumed
three phases of implement-phase size and the two review phases are cheaper than
the implement phase (the third is a third of it). Against these numbers
`activeDeadlineSeconds: 3600` is ~8x headroom on the run and
`--max-budget-usd: 10` is ~11x the largest single phase. Both stand; neither was
approached.

### ⚠️ The fixed-point trap is real, and it is a *silently successful* dead run

[#12][12] flagged `/code-review` step 1 — *"If they didn't specify [a fixed point],
ask for it"* — as an unattended-hang trap. Confirmed, and the failure shape is
worse than a hang. Bare `/mattpocock-skills:code-review` with no SHA:

```
rc=0   is_error=false   turns=2   cost=$0.11
result: "`main` looks like the natural fixed point — the current branch
         (implementer/issue-2-3phase) diverges from main at 1502c1d. Should I
         use `main` as the fixed point for the diff, or did you have something
         else in mind (e.g. a specific SHA)?"
```

One `Bash` call, then a question. **Every mechanical signal says success** —
exit 0, `is_error: false`, a `result` message present — and the review did not
run. This is [#6][6]'s "process success ≠ task success" one layer deeper: not a
model giving up politely, but a *skill step* executing correctly against an
absent human. Injecting `BASE_SHA` (which `phase.sh` already captures at clone
time) is load-bearing, not belt-and-braces.

### ✅ Spec resolution degrades gracefully — via the *branch name*, not the commits

[#12][12] left this one open: step 2 walks commit messages for `#N` then fetches
through `docs/agents/issue-tracker.md`, which target repos are explicitly not
required to carry. Does it fall through to asking, or to "no spec available"?

**Neither — it resolves the spec anyway.** With the SHA supplied but no spec
pointer in the prompt, the skill's own steps were, in order:

```
cat docs/agents/issue-tracker.md          -> MISSING
git log <sha>..HEAD --format='%H%n%B'     -> no #N in any commit message
find docs specs .scratch -type f          -> nothing
git branch --show-current                 -> implementer/issue-2-3phase
gh issue view 2 --repo nissessenap/tmp-test-repo --json title,body,comments
```

It inferred the issue number **from the branch name**, then fetched it with the
pod's `issues: read` token. The Spec axis then produced real findings quoting the
issue's own wording.

⚠️ **That makes [#13][13]'s branch format `implementer/issue-<n>` load-bearing for
something it was not chosen for.** It was picked as a push-prefix for the branch
ruleset; it is now also how the review phase finds its spec when nothing else
carries the number. Worth writing down before someone "tidies" the format.

The mitigation stays anyway — `phase.sh` names the issue and the exact
`gh issue view` command — because relying on inference costs turns (8 turns and
$0.46 for the bare-SHA run, versus a phase that is told) and because a repo whose
branch is not ours would fall through to asking.

### ✅ Both plugins resolve from repeated `--plugin-dir`, in either order

22 `mattpocock-skills:*` commands **and** 6 `ponytail:*` commands in one session,
including both `mattpocock-skills:code-review` and `ponytail:ponytail-review`.
Order-independent — `A B` and `B A` give the same set. [#12][12]'s first spill is
discharged: the ADR 0001 amendment for a second baked plugin is a second
`git clone` and a second flag.

⚠️ **`plugin_errors` does not exist** on 2.1.220's init message. [#12][12] said to
check it is `null`; the key is simply absent, so a probe that asserts
`plugin_errors == null` passes vacuously whether the plugin loaded or not. The
real evidence is the `plugins` array — `[{name: mattpocock-skills, version:
1.2.0}, {name: ponytail, version: 4.8.4}]` — and the `slash_commands` list.

⚠️ **Count, don't eyeball.** The first version of this probe printed the command
list through `cut -c1-500` and reported *zero* ponytail commands, because they
sort last and the truncation ate them. An hour of chasing a bug in the wrong layer.

### ⚠️ ponytail's hooks need `node`, which ADR 0001 forbids — and it is fine

ponytail's `plugin.json` declares three hooks (`SessionStart`, `SubagentStart`,
`UserPromptSubmit`), all `node "${CLAUDE_PLUGIN_ROOT}/hooks/*.js"`. [ADR 0001][adr1]
bakes **no node/npm**. So on every one of the three phases:

```json
{"type":"system","subtype":"hook_response","hook_name":"SessionStart:startup",
 "stderr":"/bin/sh: 1: node: not found\n","exit_code":127,"outcome":"error"}
```

Three failed hooks per run, surfaced in the stream and **not** fatal — the session
initialises and the skills work. `outcome: error` in the stream is the only cost.
**No change to ADR 0001** — but the amendment should say the plugin is baked for
its *skills*, not its hooks.

⚠️ **The failing hooks are not, however, what keeps lazy-mode out of the implement
phase.** Those hooks are the mechanism that makes ponytail an *ambient* always-on
instruction, so it is tempting to read `exit_code: 127` as free protection. It is
not: **none of ponytail's six skills carries `disable-model-invocation`**, and the
`ponytail` skill's own description reads *"Use on ANY coding task: writing, adding,
refactoring, fixing, reviewing, or designing code."* That is maximally
auto-invocable, and with the plugin baked it is reachable from every phase. The
implement phase's histogram shows `Skill=1` which cannot now be attributed.

So `run_phase` passes `--plugin-dir /opt/ponytail` **only to the ponytail phase**.
A plugin dir a phase never sees cannot self-trigger, which is cheaper and more
certain than measuring whether it does. Both flags still go on phase 3, so the
repeatability finding above stands unchanged — and this is the *run plan's* choice,
not the image's: ADR 0001 still bakes both.

### ✅ Three nested subagents work, one of them added by the prompt

[#6][6] verified parallel subagents in `-p`; [#12][12] asked for three at once with
the third injected by the prompt rather than the skill. Phase 2's tool histogram
is `Agent=3`, and the three descriptions are **`Standards axis review`**,
**`Spec axis review`**, **`Go specialist review`** — two from the skill, one from
`TOOLCHAIN=go` interpolated into the prompt. The implement phase separately shows
`Agent=2`, which is `/implement`'s own trailing `/code-review` — the review that
ran in every green run above and that nothing consumed.

### ⚠️ Neither review skill commits, or even fixes — the prompt has to say so

Both are **reporting** skills. `/code-review` ends at "Present the two reports";
`ponytail-review/SKILL.md` ends *"Does not apply the fixes, only lists them."* A
phase that merely invokes them costs a dollar and changes nothing.

So roughly half of each review phase's prompt is the fix half — act on real
findings, **remove** rather than expand what the review calls scope creep, run the
repo's tests, commit to the current branch, do not amend, do not push. With that:
**every phase committed, and `dirty` was 0 after all three.** `phase.sh` carries a
deterministic `git add -A && git commit` fallback for a phase that leaves work
behind; it never fired. **No explicit commit step is needed after each review
phase** — asking is enough — but the fallback stays, because a dirty tree at push
time is work silently dropped and the check is one line.

### ⚠️ #12's "phase 2 widens, phase 3 narrows" is not what happened

The pairing was [#12][12]'s answer to its own worry that a review-that-improves
increases scope creep. On n=1, **both review phases narrowed**:

| commit | phase | net |
|---|---|---|
| `5ce3e73` feat: add Summary/Summarize | implement | **+186** |
| `348f163` refactor: use stdlib slices | review | +4 / **-10** |
| `d006486` chore: trim speculative comment | ponytail | +1 / **-3** |

Phase 2 reported **7 findings and actioned 1** — the one the Go specialist raised,
hand-rolled logic that `slices.Min`/`Clone`/`Sort` cover. It explicitly declined
the widening ones (`Min`/`Max` duplication, exported `Min`, discarded `ok`). It
declined them because **the prompt's rule 2 told it to**, not because phase 3
existed. So the diff-protection came from the prompt, and phase 3 arrived at a diff
already narrowed and found $0.32 worth of -2 lines.

That is not an argument for deleting phase 3 on one data point, but it relocates
the justification: **rule 2 in the phase-2 prompt is what stops review-driven scope
creep**, and phase 3's value is unproven. Worth watching over several runs before
the MVP doc claims the pair is what does the work.

➖ And a live tension between the two: the counterfactual run's Spec axis flagged
that phase 3 had **cut a doc comment signposting the issue's own "easy to extend"
requirement**. Phase 3 narrows without a spec in hand, so it can delete something
phase 2 would have defended. Nothing to fix now; a real cost of ordering them this
way.

### ⚠️ The agent downloads its own Go toolchain, and the next phase inherits it

Not a question the ticket asked, and the most consequential thing the run found.
The prototype image is ADR 0001's *base* — no language toolchain — and it was
pointed at a Go repo, so `go build` failed. The implement phase's response:

```
apt-get install -y golang-go         -> refused (non-root, read-only rootfs)
sudo -n apt-get install -y golang-go -> sudo: command not found
curl -sL https://dl.google.com/go/go1.23.0.linux-amd64.tar.gz | tar -xz  -> OK
export PATH=$HOME/toolchain/go/bin:$PATH && go build ./...               -> OK
```

Then `go 1.26.1` in `go.mod` triggered Go's own toolchain auto-download, so the
`go version` that actually ran the tests was **1.26.1**, fetched through the module
proxy. Three consequences:

1. **This is [ADR 0001][adr1]'s "no pre-agent setup phase, the agent installs deps
   itself" decision working — and its bill.** The agent's remedy for a missing
   toolchain is to pull a binary off the internet. Under [#16][16]/[#37][37]'s egress
   allowlist that either fails the run (no `dl.google.com`) or means the sandbox can
   fetch arbitrary binaries. The allowlist inventory in [#33][33] is missing
   `dl.google.com` and `*.golang.org` for the toolchain itself, not just modules.
2. **Phases 2 and 3 inherited the toolchain through `$HOME`.** Phase 2 hunted for
   it (`ls /home/agent/toolchain/go/bin`), found the implement phase's download,
   exported PATH and verified for real. A fresh `claude -p` per phase is a fresh
   *process*, not a fresh *container* — so the `emptyDir` `HOME` is an accidental
   intra-run dependency cache. It works, and it means **the review phases' ability
   to run tests depends on the implement phase's environment mutation**; if phase 1
   dies before installing, phases 2 and 3 cannot verify.
3. **ADR 0003's toolchain→image mapping was not exercised**, because `local.sh`
   runs the base image regardless of repo. That is the prototype's shortcut, and it
   is what made the finding visible.

All three phases' summaries claimed build/test/gofmt passed, and **all three claims
were true** — checked against the actual `tool_result` output, not the summaries.
Worth stating because the opposite is the obvious suspicion, and one grep settled it.

### ➖ One credential leak fixed on the way past

`probe "model-creds"` printed `oauth=${X:+SET}${X:-absent}`, expecting one branch or
the other. `${X:-absent}` expands to **`$X`** when `X` is set, so the two forms do
not compose and the probe printed `SET` followed by the whole OAuth token into the
pod log. Present since [#33][33]; visible in this run's k3s attempt. Replaced with a
two-branch test.

### ➖ What this does not answer

- **n=1, one tiny Go repo, one loose brief.** Every number above is a single
  observation. The relative phase costs (implement ≈ review ≈ 3× ponytail) are the
  part most likely to move on a real repo.
- **Whether phase 3 earns $0.32.** See above — its findings arrived at an
  already-narrowed diff, and the honest reading is that phase 2's rule 2 did the
  work. Needs several runs.
- **A dead review phase.** `status: completed_unreviewed` and the per-phase
  accounting are wired and were exercised only by the accidental
  `Not logged in` run (all three phases `status=error`, run reported no commits and
  exited 1, per-phase records intact). A 529 mid-review has not been provoked.
- **The language reviewer beyond `go`.** `TOOLCHAIN` is one string interpolated
  into one paragraph; `node` and `python` are untested and unset is the documented
  fallback.
- **Anything about posture.** No gVisor, no `readOnlyRootFilesystem`, no proxy in
  these numbers. `go.sh` is still the instrument for that, and the k3s attempt died
  on an expired GCP ADC (`invalid_grant`, `invalid_rapt`) rather than on anything
  about the phases.

Versions: Claude Code `2.1.220`, `mattpocock/skills@2ab9580` (plugin 1.2.0),
`DietrichGebert/ponytail@16f2980` (plugin 4.8.4), model `claude-opus-5[1m]`,
`debian:trixie-slim`.

[t1]: https://github.com/nissessenap/tmp-test-repo/issues/1
[t2]: https://github.com/nissessenap/tmp-test-repo/issues/2
[16]: https://github.com/nissessenap/the-implementer/issues/16
[31]: https://github.com/nissessenap/the-implementer/issues/31
[37]: https://github.com/nissessenap/the-implementer/issues/37
[12]: https://github.com/nissessenap/the-implementer/issues/12
[13]: https://github.com/nissessenap/the-implementer/issues/13
[14]: https://github.com/nissessenap/the-implementer/issues/14
[33]: https://github.com/nissessenap/the-implementer/issues/33
[34]: https://github.com/nissessenap/the-implementer/issues/34

[adr1]: ../adr/0001-sandbox-image-strategy-and-byo-contract.md
[adr2]: ../adr/0002-a-run-executes-as-a-kubernetes-job.md
[22]: https://github.com/nissessenap/the-implementer/issues/22
[28]: https://github.com/nissessenap/the-implementer/issues/28
[6]: https://github.com/nissessenap/the-implementer/issues/6
[bubblewrap#745]: https://github.com/containers/bubblewrap/issues/745
[gv13438]: https://github.com/google/gvisor/issues/13438
[gv13532]: https://github.com/google/gvisor/issues/13532
[gv13347]: https://github.com/google/gvisor/pull/13347
[gv13912]: https://github.com/google/gvisor/issues/13912
[k3sadv]: https://docs.k3s.io/advanced
[k3s8751]: https://github.com/k3s-io/k3s/pull/8751
[gkesandbox]: https://docs.cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods
[dockeringvisor]: https://gvisor.dev/docs/tutorials/docker-in-gvisor/
[dockeringke]: https://gvisor.dev/docs/tutorials/docker-in-gke-sandbox/
[71]: https://github.com/nissessenap/the-implementer/issues/71
