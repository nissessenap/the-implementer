# The `go` language image, and ADR 0001's worked example of what a derivative may
# legitimately add — which is the more valuable output than the capability, since
# other organizations' images derive from the same base.
#
# It carries a toolchain and a container runtime and **nothing else**. No registry
# configuration: an organization that needs a private module proxy bakes it into
# its own derived image, which is exactly what the BYO contract exists for. Putting
# it here would make our images the place everyone patches.
#
# The base is a build argument so CI derives from the base it just built rather
# than from a published one — the four images are one matched set out of one
# workflow run, and what gets pinned is each published derivative's own digest.
ARG BASE=ghcr.io/nissessenap/implementer-base:v1
FROM ${BASE}

# The base ends at USER 1000, which is the contract. Back to it at the bottom.
USER 0

# uidmap for newuidmap/newgidmap, slirp4netns for rootlesskit's network, iproute2
# because slirp4netns shells out to `ip` to create its tap device (missing, the
# only symptom is `nsenter: failed to execute ip` from inside rootlesskit), and
# fuse-overlayfs because the kernel's overlayfs is not usable in an unprivileged
# user namespace. No iptables: dockerd runs with it off, and iptables-nft cannot
# initialise under gVisor anyway.
RUN apt-get update && apt-get install -y --no-install-recommends \
      uidmap slirp4netns fuse-overlayfs iproute2 \
 && rm -rf /var/lib/apt/lists/*

# The toolchain, as the upstream tarball pinned by checksum rather than the
# distribution's package: the same posture as the agent CLI in the base, and it is
# what Renovate bumps.
#
# GOTOOLCHAIN is deliberately left at its default of `auto`. A repository whose
# go.mod asks for a version this image does not carry then downloads it mid-run and
# completes — a case #31 measured, and one that is routine rather than exotic
# because Go's own auto-download makes it so. Setting it to `local` would turn that
# into a hard failure, which is the opposite of what is wanted. The cost is an
# egress dependency on the toolchain hosts, which the allowlist has to price
# alongside the module hosts when it lands (#64, #16).
RUN curl -fsSL -o /tmp/go.tgz https://go.dev/dl/go1.27.0.linux-amd64.tar.gz \
 && echo "675c26c449cbb18fc24b74650de1eabbae6e16f64326fd85a283fb3b58280685  /tmp/go.tgz" \
      | sha256sum -c - \
 && tar -C /usr/local -xzf /tmp/go.tgz \
 && rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:$PATH

# The rootless Docker stack, from the static tarballs rather than Docker's apt
# repository: no third-party keyring to maintain, both halves pinned by checksum,
# and the version is the one architecture §8 measured green (v29). The rootless
# extras tarball is where `rootlesskit` itself comes from, and it is the only thing
# taken out of it: the run plan starts dockerd from inside the namespace, so
# upstream's dockerd-rootless.sh — which exists to create that namespace — would be
# a second, divergent way to do the same thing.
#
# GKE Sandbox supports Docker v27/v28 only. This is v29 and flag-free under plain
# gVisor; any yes on GKE needs re-measuring there.
RUN curl -fsSL -o /tmp/d.tgz https://download.docker.com/linux/static/stable/x86_64/docker-29.7.2.tgz \
 && echo "803d433f226db4776e1768fd319fc6c6e4935a456acf84fcc0080818b854bc8f  /tmp/d.tgz" \
      | sha256sum -c - \
 && curl -fsSL -o /tmp/r.tgz https://download.docker.com/linux/static/stable/x86_64/docker-rootless-extras-29.7.2.tgz \
 && echo "15a5cb81f2c5cf15ea21427f2e8241eac0deb2221175f993b5e76926e705ec6a  /tmp/r.tgz" \
      | sha256sum -c - \
 && tar -C /usr/local/bin --strip-components=1 -xzf /tmp/d.tgz \
 && tar -C /usr/local/bin --strip-components=1 -xzf /tmp/r.tgz \
      docker-rootless-extras/rootlesskit \
 && rm /tmp/d.tgz /tmp/r.tgz

# ---------------------------------------------------------------------------
# THIS BLOCK IS NOT REMOVABLE. It reads like leftover scaffolding and it is the
# half that three attempts to graft rootless Docker onto this base died on.
#
# rootlesskit maps a range of subordinate ids into its user namespace, and the only
# way an unprivileged process may map more than its own single id is to hand the
# job to newuidmap/newgidmap — which need a range declared for the user in
# /etc/subuid and /etc/subgid, and need to be privileged themselves. Without the
# ranges rootlesskit falls back to a single-id mapping and dockerd fails later, a
# long way from the cause.
#
# **File capabilities, and not the setuid bit Debian's uidmap ships.** Measured,
# and it is the whole difference between a working image and one whose only symptom
# is `newuidmap: write to uid_map failed: Operation not permitted`: under gVisor a
# setuid-root exec raises euid to 0 and grants **no capabilities at all** — CapPrm
# stays 0, where host runc hands over the entire bounding set — so a setuid
# newuidmap is unprivileged exactly where we run it. A file capability is honoured.
# The setuid bit goes with it, so there is one mechanism rather than two and no
# illusion that the other is doing anything. (Alpine's shadow-uidmap ships
# cap_setuid=ep for the same reason, which is why the spike's image worked.)
#
# Two consequences for the PodSpec of a run that turns the wrap on, both real and
# both recorded here next to their cause: `allowPrivilegeEscalation: false` sets
# no_new_privs, which blocks a file capability exactly as it blocks setuid, and
# `capabilities: drop: [ALL]` empties the bounding set a file capability is
# intersected with. So such a run needs `allowPrivilegeEscalation: true` and
# `add: [SETUID, SETGID]`. A run with the wrap off — the default — keeps the
# posture the chart renders entirely untouched.
RUN apt-get update && apt-get install -y --no-install-recommends libcap2-bin \
 && echo 'agent:100000:65536' > /etc/subuid \
 && echo 'agent:100000:65536' > /etc/subgid \
 && setcap cap_setuid+ep /usr/bin/newuidmap \
 && setcap cap_setgid+ep /usr/bin/newgidmap \
 && chmod 0755 /usr/bin/newuidmap /usr/bin/newgidmap \
 && rm -rf /var/lib/apt/lists/*
# ---------------------------------------------------------------------------

USER 1000
