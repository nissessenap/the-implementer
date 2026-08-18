# The e2e harness is KUBECONFIG-driven and makes no assumption about the cluster's
# flavour, so `kind-up` is one target and a local k3s needs none. Nothing here
# builds or loads an image yet; when the proxy arrives (#50) that costs one
# image-load variable and no other flavour knowledge.
KIND_CLUSTER ?= implementer-e2e

# Stages run in filename order. Later tickets add a file; this list needs no edit.
E2E_STAGES := $(sort $(wildcard e2e/[0-9][0-9]-*.sh))

.PHONY: e2e kind-up kind-down

e2e:
	@set -e; for s in $(E2E_STAGES); do "$$s"; done

# A separate kubeconfig rather than ~/.kube/config, which on a developer laptop
# points at a real cluster. e2e/lib.sh looks for this exact path when KUBECONFIG is
# unset, which is why it is a literal and not an overridable variable.
kind-up:
	kind create cluster --name $(KIND_CLUSTER) --kubeconfig $(CURDIR)/.kind.kubeconfig

kind-down:
	kind delete cluster --name $(KIND_CLUSTER) --kubeconfig $(CURDIR)/.kind.kubeconfig
