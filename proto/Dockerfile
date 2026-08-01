# PROTOTYPE — throwaway. Not the real base image.
# Exists to answer the open questions in ADR 0001 / issue #22 with a real cluster.
#
# Deliberately follows the ADR 0001 contract so the answers transfer:
#   glibc, a shell, non-root UID 1000, git, gh, bubblewrap, agent CLI on PATH,
#   baked+pinned skills, phase script at a known path, no node/npm, no credentials.

FROM debian:trixie-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git gh bubblewrap jq \
 && rm -rf /var/lib/apt/lists/*

# ponytail: real user rather than a bare `USER 1000` — some tools getpwuid() and
# a missing passwd entry is a confusing failure that has nothing to do with what
# we are measuring. The orchestrator still holds no UID constant; the PodSpec does.
RUN useradd -u 1000 -m -d /home/agent -s /bin/sh agent

# Agent CLI: the bare pinned binary, not install.sh. install.sh writes a launcher
# into $HOME, and $HOME is an ephemeral emptyDir at run time. Also avoids node/npm.
ARG CLAUDE_VERSION=2.1.220
ARG CLAUDE_SHA256=674f61f20ff306f3100cf9200e4c36c4b70278b5bef2884549819b942a89c863
RUN curl -fsSL -o /usr/local/bin/claude \
      "https://downloads.claude.ai/claude-code-releases/${CLAUDE_VERSION}/linux-x64/claude" \
 && echo "${CLAUDE_SHA256}  /usr/local/bin/claude" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/claude

# Baked skills, pinned. Repo root carries .claude-plugin/plugin.json, so the repo
# root IS the plugin dir. Loaded via --plugin-dir, never discovered from $HOME.
ARG SKILLS_SHA=2ab958093e83e0ec752e6c1c5932da465bf23e0c
RUN git clone -q https://github.com/mattpocock/skills /opt/skills \
 && git -C /opt/skills checkout -q "${SKILLS_SHA}" \
 && rm -rf /opt/skills/.git

COPY phase.sh /usr/local/bin/phase.sh
COPY result.schema.json /opt/result.schema.json
RUN chmod 0755 /usr/local/bin/phase.sh

USER 1000
ENV DISABLE_AUTOUPDATER=1
ENTRYPOINT ["/usr/local/bin/phase.sh"]
