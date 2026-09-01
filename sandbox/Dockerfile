# The sandbox base image. This Dockerfile *is* ADR 0001's BYO contract: an
# organization either does `FROM ghcr.io/nissessenap/implementer-base:v1` and adds
# its toolchain, or copies these RUN lines into its own hardened base image. That
# second path is why there is no separate installer artifact — it is the same
# lines, published as documentation rather than as a second code path.
#
# Contract terms, all of them below: glibc, a shell, uid 1000 as the image default,
# git, gh, bubblewrap, an agent CLI on PATH as a bare pinned binary, baked and
# pinned plugins at a read-only path, the phase script at a known path, a system CA
# bundle at the conventional location, no node/npm, and no credentials.
#
# Nothing floats: the tag versions the agent CLI and the plugins together, and the
# image is a matched, digest-pinned pair with the orchestrator. Renovate opens the
# bump PRs.

# glibc, because the claude binary is a dynamically-linked ELF against
# /lib64/ld-linux-x86-64.so.2. Distroless is ruled out: no shell, no git, no gh.
#
# --platform, because the agent CLI below is fetched as linux-x64 and its checksum
# passes on any host: an arm64 laptop would otherwise build an image whose every
# phase dies with an exec-format error the preflight cannot see. Emulated and
# correct beats native and broken, and `make sandbox-image` is a documented path.
FROM --platform=linux/amd64 debian:trixie-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132

# Static, and the reason it is not metadata-action's: the workflow builds every
# image with plain `docker build` so the derivatives come from the base it just
# built, which leaves nowhere for the action's generated labels to be applied.
# This is the one that earns its keep — ghcr links a package to its repository by
# it — and children inherit it, so all four are linked from one line.
LABEL org.opencontainers.image.source=https://github.com/nissessenap/the-implementer

# ca-certificates puts the system bundle at /etc/ssl/certs/ca-certificates.crt,
# which the phase script concatenates the proxy's CA onto. jq is the run plan's
# only string escaper and its only JSON reader. No node, no npm, no sudo, no tar
# beyond what the base carries — we never use pod exec, so nothing in-image serves
# the control plane.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git gh bubblewrap jq \
 && rm -rf /var/lib/apt/lists/*

# A real passwd entry rather than a bare `USER 1000`: some tools getpwuid(), and a
# missing entry is a confusing failure a long way from its cause. The orchestrator
# still holds no UID constant — the PodSpec owns the uid, and `fsGroup` is what
# makes the emptyDir volumes writable by it.
RUN useradd -u 1000 -m -d /home/agent -s /bin/sh agent

# The agent CLI: the bare pinned binary, never install.sh. install.sh writes a
# launcher into $HOME, and $HOME is an ephemeral emptyDir at run time — and the
# npx path would drag node and npm in with it.
#
# Floor: 2.1.214. Below it a large piped response can truncate the final line and
# omit the `result` message, which the run plan reads as the phase's whole verdict.
RUN curl -fsSL -o /usr/local/bin/claude \
      "https://downloads.claude.ai/claude-code-releases/2.1.220/linux-x64/claude" \
 && echo "674f61f20ff306f3100cf9200e4c36c4b70278b5bef2884549819b942a89c863  /usr/local/bin/claude" \
      | sha256sum -c - \
 && chmod 0755 /usr/local/bin/claude

# The plugins, pinned to a commit and loaded via --plugin-dir from this read-only
# path — never discovered from $HOME, which is an ephemeral volume at run time and
# would otherwise mask anything baked into ~/.claude/plugins/. Fetching them at run
# time is rejected: it needs egress, it makes runs non-reproducible, and it makes
# evaluation impossible, since a behaviour change could come from an upstream commit.
#
# The .git directories go, so the layer carries the skills and not their history.
RUN git clone -q https://github.com/mattpocock/skills /opt/skills \
 && git -C /opt/skills checkout -q 2ab958093e83e0ec752e6c1c5932da465bf23e0c \
 && git clone -q https://github.com/DietrichGebert/ponytail /opt/ponytail \
 && git -C /opt/ponytail checkout -q 16f29800fd2681bdf24f3eb4ccffe38be3baec6b \
 && rm -rf /opt/skills/.git /opt/ponytail/.git

# ponytail's plugin.json declares three node hooks. This image bakes no node, so
# they fail 127 on every phase, are surfaced in the stream, and are not fatal — the
# plugin is baked for its skills, not its hooks (#43).
#
# ⚠️ That is a fact about *this* image, not about the plugin. The `node` derivative
# carries node, so there the three hooks run: ponytail-activate.js writes a flag
# file and injects the ruleset as hidden SessionStart context, which #43's premise
# was measured without. A ponytail phase on the node image is therefore not the
# phase measured here, and nobody has measured that one.
#
# The plugin *names*, which is the half of the invocation string preflight cannot
# check: `/<plugin name>:<skill directory>` — the directories it verifies itself on
# every run, in this image and in a BYO one, before anything is spent. An upstream
# rename fails the build here instead of surfacing as `Unknown command` a dollar in.
# `plugin_errors` is not the check it looks like — the key is simply absent on
# 2.1.220, so asserting it is null passes vacuously.
RUN set -eu; \
    [ "$(jq -r .name /opt/skills/.claude-plugin/plugin.json)" = mattpocock-skills ]; \
    [ "$(jq -r .name /opt/ponytail/.claude-plugin/plugin.json)" = ponytail ]

# The run plan and the two output schemas, at the known paths the PodSpec's
# `command` and the script itself name. Baking costs nothing in rebuild churn: the
# image and the orchestrator are a matched pair, so a version bump rebuilds both
# anyway — and it buys an image that is runnable on its own for debugging.
COPY --chmod=0755 phase.sh /usr/local/bin/phase.sh
COPY result.schema.json review.schema.json /opt/

# Non-root as the image default: under gVisor bubblewrap fails outright as uid 0,
# and the agent CLI's own root refusal is the weaker reason since it is bypassable.
USER 1000
# Versions are pinned and the rootfs is read-only, so an autoupdater can only fail.
ENV DISABLE_AUTOUPDATER=1
ENTRYPOINT ["/usr/local/bin/phase.sh"]
