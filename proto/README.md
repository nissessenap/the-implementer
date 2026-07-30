# PROTOTYPE — throwaway

Not production code. Not the real base image. Exists to answer open questions in
[ADR 0001][adr1] / [ADR 0002][adr2] and [issue #22][22] against a real cluster,
before any Go is written.

Targets a local single-node **k3s** (originally kind; k3s is what can run gVisor).
`go.sh` refuses any API server that is not loopback — a laptop's `~/.kube/config`
usually points at something real.

```sh
export CLAUDE_CODE_OAUTH_TOKEN=$(claude setup-token)   # or set it however
./go.sh nissessenap/tmp-test-repo 1
```

Probe-only, costs nothing and needs no agent credentials:

```sh
PREFLIGHT_ONLY=1 ./go.sh nissessenap/tmp-test-repo 1
PREFLIGHT_ONLY=1 RUNTIME=gvisor ROOT=1 SECCOMP=RuntimeDefault ./go.sh nissessenap/tmp-test-repo 1
```

Knobs: `RUNTIME=gvisor|gvisor-dind|none`, `ROOT=1` (uid 0), `SECCOMP=`,
`PREFLIGHT_ONLY=1`, `BRANCH_SUFFIX=`, `JOB_SUFFIX=`.

Files: `Dockerfile` (ADR 0001 contract), `phase.sh` (the run plan, as the pod's
`command`), `job.yaml` (ADR 0002 primitive + ADR 0001 runtime posture), `go.sh`
(stands in for the Go orchestrator), `dind.sh` (Docker-in-gVisor probe).

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

[t1]: https://github.com/nissessenap/tmp-test-repo/issues/1
[13]: https://github.com/nissessenap/the-implementer/issues/13

[adr1]: ../docs/adr/0001-sandbox-image-strategy-and-byo-contract.md
[adr2]: ../docs/adr/0002-a-run-executes-as-a-kubernetes-job.md
[22]: https://github.com/nissessenap/the-implementer/issues/22
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
