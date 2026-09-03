# The `node` language image: a toolchain and nothing else. No registry
# configuration and no credential — an organization that needs a private npm bakes
# its own .npmrc into its own derived image, which is what the BYO contract is for.
#
# The base carries no node by design (ADR 0001: the JavaScript ecosystem's CVE
# surface is absent from every run that is not a node run). This is where it lands.
ARG BASE=ghcr.io/nissessenap/implementer-base:v1
FROM ${BASE}

USER 0

# The upstream tarball pinned by checksum, not NodeSource's apt repository: no
# third-party keyring to maintain, and npm and npx arrive with it. The gzip
# tarball rather than the smaller xz one, because the base carries no xz-utils and
# adding a package to unpack a package is the wrong end of that trade. A repository
# whose .nvmrc or `engines` asks for another major still runs — nothing here pins
# a version at the repository's expense, and the agent can fetch what it needs.
RUN curl -fsSL -o /tmp/node.tgz https://nodejs.org/dist/v24.20.0/node-v24.20.0-linux-x64.tar.gz \
 && echo "855d581f8a4eb1a8117e3426de25fe02770592febcfb31369aee1ffbfee9e8ec  /tmp/node.tgz" \
      | sha256sum -c - \
 && tar -C /usr/local --strip-components=1 --no-same-owner -xzf /tmp/node.tgz \
 && rm /tmp/node.tgz /usr/local/CHANGELOG.md /usr/local/LICENSE /usr/local/README.md

USER 1000
