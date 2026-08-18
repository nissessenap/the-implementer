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

# Prints the image reference on stdout and nothing else. e2e/30-proxy.sh reads it.
image:
	@$(KO) build --bare ./cmd/proxy
