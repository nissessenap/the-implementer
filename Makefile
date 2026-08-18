.PHONY: e2e kind-up

# Stages run in filename order. Later tickets add a file; this needs no edit.
e2e:
	@set -e; for s in e2e/[0-9][0-9]-*.sh; do "$$s"; done

# A separate kubeconfig rather than ~/.kube/config, which on a developer laptop
# points at a real cluster. e2e/lib.sh looks for this exact path.
kind-up:
	kind create cluster --name implementer-e2e --kubeconfig $(CURDIR)/.kind.kubeconfig
