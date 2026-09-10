# The spike's actor image: implementer-base, unmodified, plus one HTTP shim.
# ADR 0001's contract is the base image and this adds a layer rather than
# editing it — phase.sh, the skills, the schemas and USER 1000 all arrive from
# below and nothing here may change them.
#
# Built and pushed by run.sh to the kind cluster's local registry, because an
# ActorTemplate's image must be pinned by digest and `kind load docker-image`
# produces no registry digest to pin.

ARG BASE=ghcr.io/nissessenap/implementer-base:dev

FROM --platform=linux/amd64 golang:1.26 AS build
WORKDIR /src
# Its own module: the shim is stdlib-only, so it does not need — and must not
# drag in — the orchestrator's go.mod. Statically linked, so the builder's
# distro is irrelevant to the layer below.
COPY shim/main.go .
RUN go mod init shim && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /shim .

FROM ${BASE}
USER 0
COPY --from=build --chmod=0755 /shim /usr/local/bin/shim
# phase.sh's WORKSPACE, which the Job gets as an emptyDir. Here the actor's
# snapshot-backed rootfs is the writable storage, so the directory just has to
# exist and belong to the agent.
RUN mkdir -p /workspace && chown 1000:1000 /workspace
USER 1000
ENTRYPOINT ["/usr/local/bin/shim"]
