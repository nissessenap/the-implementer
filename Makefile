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
	# And the e2e's build is a third one. Here rather than only in the stage that
	# uses it, so a break in the self-hosted signer costs a `make test` and not a
	# cluster.
	go build -tags ghait.vault,ghait.no_file -o /dev/null ./cmd/proxy
	# The package again, because proxy/mint_vault_test.go only compiles under this
	# tag: it is ghait's *vault* signing path, which nothing else here reaches —
	# the e2e's OpenBao stage cannot call it without a GitHub App to mint for.
	go test -tags ghait.vault ./proxy
	# The chart's refusals, which nothing else exercises: the e2e only ever renders
	# the shapes that work, so an inverted guard is invisible until an operator
	# hits it. Cheap because `helm template` needs no cluster.
	! helm template charts/proxy --set githubApp.appId=1 --set-string githubApp.provider=vault --set-string githubApp.key=transit/app >/dev/null 2>&1
	! helm template charts/proxy --set-string githubApp.vault.addr=http://openbao:8200 >/dev/null 2>&1
	helm template charts/proxy --set githubApp.appId=1 --set-string githubApp.provider=vault \
	  --set-string githubApp.key=transit/app --set-string githubApp.vault.addr=http://openbao:8200 \
	  --set-string githubApp.vault.tokenSecretName=openbao-token >/dev/null

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
# The e2e signs through OpenBao transit and so builds `ghait.vault,ghait.no_file` —
# the same shape as production, one provider swapped, and no PEM signer in either.
GO_TAGS ?= ghait.gcp,ghait.no_file

# Prints the image reference on stdout and nothing else. e2e/30-proxy.sh reads it.
# GOFLAGS rather than ko's own `flags:`, because ko shells out to `go build` and
# so `go test`, `go vet` and this target can all be handed the same tag set.
image:
	@GOFLAGS="-tags=$(GO_TAGS)" $(KO) build --bare ./cmd/proxy
