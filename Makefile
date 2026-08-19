.PHONY: e2e kind-up test image

# Stages run in filename order. Later tickets add a file; this needs no edit.
e2e:
	@set -e; for s in e2e/[0-9][0-9]-*.sh; do "$$s"; done

# A separate kubeconfig rather than ~/.kube/config, which on a developer laptop
# points at a real cluster. e2e/lib.sh looks for this exact path.
kind-up:
	kind create cluster --name implementer-e2e --kubeconfig $(CURDIR)/.kind.kubeconfig

test:
	go test ./...
	# The shipped binary is a *different* build: production links the GCP signer
	# and drops the PEM one, so `go test`'s tag set never compiles it. Nothing
	# else here would catch a break in that half until release.
	go build -tags $(GO_TAGS) -o /dev/null ./cmd/proxy

# ko rather than a Dockerfile: no base image to keep patched, no build stage to
# get wrong, and no build context to .dockerignore. The tag it prints is a hash
# of the binary, so a rebuilt proxy is a new image reference — which is what
# makes a redeploy replace the pod with no rollout restart.
#
# Pinned, and run through `go run`, so CI needs nothing installed but Go. Point
# KO_DOCKER_REPO at a real registry and the same target pushes there instead;
# that is the separate CI task, not this one.
KO ?= go run github.com/google/ko@v0.18.0
KO_DOCKER_REPO ?= ko.local/implementer-proxy
export KO_DOCKER_REPO

# Which key provider is linked into the binary. A build tag and never a blanket
# underscore import: ghait's provider registry is a global map with no identity
# check, so anything linked in can shadow a provider — the smaller the set in the
# binary, the smaller that surface. `ghait.no_file` drops the PEM-on-disk signer
# that ghait registers by default, which production must not have.
#
# The e2e signs with a local PEM and so builds `GO_TAGS=` — the default set, which
# is the file provider alone.
GO_TAGS ?= ghait.gcp,ghait.no_file

# Prints the image reference on stdout and nothing else. e2e/30-proxy.sh reads it.
# GOFLAGS rather than ko's own `flags:`, because ko shells out to `go build` and
# so `go test`, `go vet` and this target can all be handed the same tag set.
image:
	@GOFLAGS="-tags=$(GO_TAGS)" $(KO) build --bare ./cmd/proxy
