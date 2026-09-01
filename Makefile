.PHONY: e2e kind-up test image orchestrator-image sandbox-image sandbox-images

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
	# The orchestrator's informer authenticates as the App through the same mint
	# path, so it has the same two builds — and `ghait.no_file` is what makes "the
	# orchestrator holds no App private key" a property of the binary rather than a
	# promise. The e2e signs with the file provider precisely because its build has
	# one; production cannot.
	go build -tags $(GO_TAGS) -o /dev/null ./cmd/orchestrator
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
	! helm template charts/proxy --set githubApp.appId=1 >/dev/null 2>&1
	helm template charts/proxy --set githubApp.appId=1 --set-string githubApp.provider=vault \
	  --set-string githubApp.key=transit/app --set-string githubApp.vault.addr=http://openbao:8200 \
	  --set-string githubApp.vault.tokenSecretName=openbao-token >/dev/null
	# And the orchestrator chart's, for the same reason. The e2e only ever renders
	# the shape that works.
	! helm template charts/orchestrator >/dev/null 2>&1
	! helm template charts/orchestrator --set-string sandbox.image=i --set vertex.enabled=true >/dev/null 2>&1
	! helm template charts/orchestrator --set-string sandbox.image=i --set-string vertex.projectId=p >/dev/null 2>&1
	helm template charts/orchestrator --set-string sandbox.image=i >/dev/null
	# Its Deployment half, whose guard is the one that is a security property: an
	# image with no webhook secret renders an *open trigger* rather than a broken
	# one, because go-github validates nothing when the secret is empty and every
	# legitimate delivery still works. Nothing about a running installation shows it.
	! helm template charts/orchestrator --set-string sandbox.image=i --set-string image=ko.local/o:1 >/dev/null 2>&1
	helm template charts/orchestrator --set-string sandbox.image=i --set-string image=ko.local/o:1 \
	  --set-string webhook.secretName=orchestrator-webhook >/dev/null
	# "There is no call to the collaborator-permission endpoint anywhere in the
	# path", as a grep. Labelling needs Triage, so the event proves triage and
	# nothing more — and which fine-grained permission that endpoint requires could
	# not be established, because GitHub's public OpenAPI encodes none for it.
	#
	# What actually makes it true is that orchestrator.Webhook holds no GitHub
	# client, so there is nothing there to call anything with; this catches the
	# endpoint being *named*, which is how it would come back. A URL assembled from
	# parts would slip past — the day this package has a client, the assertion has
	# to become a test of what that client is allowed to reach.
	! grep -rn collaborators orchestrator cmd/orchestrator

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

KO_ORCHESTRATOR_REPO ?= ko.local/implementer-orchestrator

# The orchestrator's own image — the webhook front-end's Deployment runs it, and
# e2e/95-webhook.sh reads the reference off this target. The same tag set as the
# proxy's: this binary carries the informer too, and `ghait.no_file` is what makes
# "the orchestrator holds no App private key" a property of the build rather than a
# promise. KO_DOCKER_REPO is set per-recipe rather than exported, because the two
# images are two names.
orchestrator-image:
	@GOFLAGS="-tags=$(GO_TAGS)" KO_DOCKER_REPO=$(KO_ORCHESTRATOR_REPO) $(KO) build --bare ./cmd/orchestrator

# The sandbox base image — ADR 0001's BYO contract, built from sandbox/Dockerfile.
# The tag versions the agent CLI and the baked plugins together, and the image is a
# matched, digest-pinned pair with the orchestrator: nothing inside it floats.
#
# This target is the local build. The published one is
# .github/workflows/sandbox-image.yaml, which pushes the moving :v1 and the
# immutable :v1.2.3 and prints the digest Helm should pin — a `docker push` from a
# laptop would be a second, unpinnable way to publish the same name.
SANDBOX_IMAGE ?= ghcr.io/nissessenap/implementer-base
# The language images are `implementer-go`, `-node`, `-python` — a separate name
# rather than a tag on the base, because they are separate images with separate
# CVE surfaces and ADR 0003's `sandbox.images` map points at one per toolchain.
SANDBOX_IMAGE_PREFIX ?= ghcr.io/nissessenap/implementer
SANDBOX_TAG ?= dev

sandbox-image:
	docker build -t $(SANDBOX_IMAGE):$(SANDBOX_TAG) sandbox
	sandbox/contract.sh $(SANDBOX_IMAGE):$(SANDBOX_TAG)

# The base plus ADR 0003's three language derivatives, each built FROM the base
# this target just produced rather than from a published one — the same thing the
# publishing workflow does, for the same reason. Each is checked against the
# contract as it is built, which is the half of the acceptance criteria that needs
# no cluster and no dollar; the other half is e2e/85.
SANDBOX_TOOLCHAINS ?= go node python

sandbox-images: sandbox-image
	@set -e; for t in $(SANDBOX_TOOLCHAINS); do \
	  docker build --build-arg BASE=$(SANDBOX_IMAGE):$(SANDBOX_TAG) \
	    -t $(SANDBOX_IMAGE_PREFIX)-$$t:$(SANDBOX_TAG) sandbox/$$t; \
	  sandbox/contract.sh $(SANDBOX_IMAGE_PREFIX)-$$t:$(SANDBOX_TAG) $$t; \
	done
