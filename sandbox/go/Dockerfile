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

# uidmap for newuidmap/newgidmap, slirp4netns for rootlesskit's network, and
# iproute2 because slirp4netns shells out to `ip` to create its tap device
# (missing, the only symptom is `nsenter: failed to execute ip` from inside
# rootlesskit). No iptables: dockerd runs with it off, and iptables-nft cannot
# initialise under gVisor anyway.
#
# No fuse-overlayfs, though Docker's rootless prerequisites list it and #74 first
# copied that list: it is a FUSE filesystem and the PodSpec mounts no /dev/fuse,
# so dockerd can never select it. The fallback is overlay2, or vfs where the data
# root's own filesystem will not stack — e2e/85 prints which.
RUN apt-get update && apt-get install -y --no-install-recommends \
      uidmap slirp4netns iproute2 \
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

# THIS BLOCK IS NOT REMOVABLE. It reads like leftover scaffolding and it is the
# half that three attempts to graft rootless Docker onto this base died on.
# rootlesskit cannot map a range of subordinate ids itself: it hands the job to
# newuidmap/newgidmap, which need the ranges declared here and need to be
# privileged themselves.
#
# **A file capability, and not the setuid bit Debian's uidmap ships** — under
# gVisor a setuid-root exec grants no capabilities at all, so a setuid newuidmap
# is unprivileged exactly where we run it. The setuid bit goes with it, so there
# is one mechanism rather than two. This is also why a wrapped run needs two
# securityContext fields back; see docs/architecture.md#8 for the measurement and
# orchestrator/job.go's relaxForRootlesskit for the patch.
RUN apt-get update && apt-get install -y --no-install-recommends libcap2-bin \
 && echo 'agent:100000:65536' > /etc/subuid \
 && echo 'agent:100000:65536' > /etc/subgid \
 && setcap cap_setuid+ep /usr/bin/newuidmap \
 && setcap cap_setgid+ep /usr/bin/newgidmap \
 && chmod 0755 /usr/bin/newuidmap /usr/bin/newgidmap \
 && rm -rf /var/lib/apt/lists/*

USER 1000
