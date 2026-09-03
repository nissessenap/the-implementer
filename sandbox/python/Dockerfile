# The `python` language image: a toolchain and nothing else. No pip.conf, no index
# URL, no credential — an organization that needs a private PyPI bakes it into its
# own derived image, which is what the BYO contract is for.
ARG BASE=ghcr.io/nissessenap/implementer-base:v1
FROM ${BASE}

USER 0

# The distribution's python, unlike go and node above, because building CPython
# from source to gain a couple of minor versions is not a trade this image should
# make. A repository pinning a `requires-python` this image does not carry is the
# agent's problem to solve with a venv, and it completes rather than failing at
# the image.
RUN apt-get update && apt-get install -y --no-install-recommends \
      python3 python3-pip python3-venv \
 && rm -rf /var/lib/apt/lists/*

# PEP 668's marker, removed. Debian marks the system interpreter
# externally-managed so `pip install` refuses outside a venv — correct on a laptop
# and wrong here: this filesystem is read-only at run time, the container is
# ephemeral and single-purpose, and the alternative is every run failing at
# `pip install -r requirements.txt` with an error about the operating system's
# package manager. Not PIP_BREAK_SYSTEM_PACKAGES, which only covers pip itself and
# not the installers a repository's own tooling shells out to.
RUN rm -f /usr/lib/python3*/EXTERNALLY-MANAGED

# The other half of that decision. With /usr read-only at run time pip falls back
# to a user install, so a repository's console scripts land in ~/.local/bin — and
# without this the agent installs a tool it then cannot invoke. The path is fixed
# rather than derived: the chart sets HOME and the base's passwd entry agrees.
ENV PATH=/home/agent/.local/bin:$PATH

USER 1000
