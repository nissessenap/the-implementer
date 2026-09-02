#!/bin/sh
# ADR 0001's contract, checked against a *built image* rather than trusted.
#
#   sandbox/contract.sh <image> [toolchain]
#
# The base image's Dockerfile is the contract, and the run plan's preflight is the
# contract as code — so the first and largest check here is simply to run the
# shipped phase script and require it to get *past* preflight. It dies at the
# clone, because there is no network and no credential; what matters is which
# phase it died in. That is one check instead of a second copy of the list, and a
# second copy is what would drift.
#
# The rest is what preflight cannot see from inside a run that is already going:
# the image's default uid, the absence of registry configuration, and — for a
# derivative — that the toolchain it exists to carry is actually on PATH.
set -eu

IMG=${1:?usage: contract.sh <image> [toolchain]}
TOOLCHAIN=${2:-}
FAILED=0

ok()   { echo "  ok    $1"; }
fail() { echo "  FAIL  $1"; FAILED=1; }
# Runs a command in the image with the entrypoint out of the way. No network:
# nothing here should need one, and a check that quietly reaches the internet is a
# check that passes for the wrong reason.
in_image() { docker run --rm --network none --entrypoint /bin/sh "$IMG" -c "$1"; }

# say <label> <command> — report what it printed, but only if it *worked*. The
# `if` is load-bearing: `ok "go $(in_image ...)"` would throw the status away and
# print `ok go` for an image with no toolchain at all.
say() { if _o=$(in_image "$2"); then ok "$1 $_o"; else fail "$1: \`$2\` failed in the image"; fi; }

echo "== $IMG${TOOLCHAIN:+ ($TOOLCHAIN)}"

# --- the run plan's own preflight -------------------------------------------
# WORKSPACE and TERM_LOG point at writable paths because this is `docker run` and
# not the PodSpec; in a pod they are the emptyDir and the kubelet's bind mount.
out=$(docker run --rm --network none \
  -e REPO=owner/repo -e ISSUE=1 -e GH_TOKEN=sentinel \
  -e HOME=/home/agent -e WORKSPACE=/tmp/ws -e TERM_LOG=/tmp/result.json \
  "$IMG" 2>&1) || true
case $out in
  *"image contract violation"*) fail "preflight: $(echo "$out" | grep -m1 'image contract violation')" ;;
  *"=== clone"*) ok "the run plan's preflight passes and the run reaches the clone" ;;
  *) fail "the run plan did not reach the clone: $(echo "$out" | tail -3 | tr '\n' ' ')" ;;
esac

# --- the image's own default uid ---------------------------------------------
# Not the pod's: the PodSpec owns the uid at run time, and this is the image's
# default, which the contract fixes at 1000 because bubblewrap fails outright as
# uid 0 under gVisor.
uid=$(in_image 'id -u') || uid="<the image would not run>"
[ "$uid" = 1000 ] && ok "the image's default uid is 1000" || fail "the image's default uid is $uid, want 1000"

# --- no credentials, and no registry configuration ---------------------------
# The second half is a decision, not hygiene: an organization that needs a private
# npm, pip or module registry bakes its own config into its own derived image.
# Shipping one here would make our images the place everyone patches.
clean=yes
for f in /home/agent/.npmrc /usr/local/etc/npmrc /home/agent/.netrc /etc/netrc \
  /etc/pip.conf /home/agent/.config/pip/pip.conf /home/agent/.pip/pip.conf \
  /home/agent/.docker/config.json /home/agent/.config/gh/hosts.yml; do
  in_image "[ ! -e $f ]" || { fail "$f is baked into the image"; clean=no; }
done
# Guarded, because this line is what a human reads to decide whether to publish:
# an unconditional `ok` under a loop that just printed a FAIL is output that
# contradicts itself.
[ "$clean" = yes ] && ok "no registry configuration and no credential files"

# --- the toolchain the derivative exists to carry ----------------------------
case $TOOLCHAIN in
  '') ;;
  go)
    # `go env` and not `go version | cut`: a pipeline's status is the *last*
    # command's, so a missing toolchain would leave `cut` reporting success and
    # print `ok go` with an empty version — the vacuous pass `say` above exists to
    # prevent, walked back in through the pipe.
    say go "go env GOVERSION"
    # `auto`, so a repository whose go.mod asks for a version this image does not
    # carry downloads it and completes. `local` would make that a hard failure,
    # and the failure would look like the repository's fault.
    #
    # Read with `go env` and not `echo $GOTOOLCHAIN`: the variable is only one of
    # the places the setting comes from, so a pin written with `go env -w` — which
    # lands in $GOROOT/go.env — would be invisible to an echo and pass the one
    # check this line exists to fail. Unreadable is a FAIL rather than a default,
    # for the same reason `say` takes the status: a fallback value here would let
    # an image with no toolchain at all print an `ok` about its toolchain.
    if ! gt=$(in_image 'go env GOTOOLCHAIN'); then
      fail "GOTOOLCHAIN is unreadable: \`go env\` does not run in this image"
    elif [ "$gt" = local ]; then
      fail "GOTOOLCHAIN=local turns a version mismatch into a failed run"
    else
      ok "GOTOOLCHAIN=$gt, so a version this image lacks is downloaded, not refused"
    fi
    # The one thing about the rootless Docker stack a successful build does not
    # already prove. Every binary in it, and the subuid/subgid ranges, come from a
    # RUN that fails the build if it fails — but a file capability is an xattr,
    # and an xattr is what a layer round-trip can quietly drop. Whether the stack
    # *works* needs gVisor and a cluster, which is e2e/85's job.
    if in_image 'getcap /usr/bin/newuidmap 2>/dev/null | grep -q cap_setuid'; then
      ok "newuidmap carries its cap_setuid file capability"
    else
      fail "newuidmap has no cap_setuid file capability — the mapping rootlesskit needs is the one it cannot make itself"
    fi
    ;;
  node)
    say node "node --version"
    say npm "npm --version"
    ;;
  python)
    say python "python3 --version"
    say pip "pip --version"   # unpiped, for the reason spelled out under `say go`
    # PEP 668's marker, removed deliberately: this filesystem is read-only at run
    # time and the container is ephemeral, so refusing an install outside a venv
    # only fails the run.
    if in_image 'ls /usr/lib/python3*/EXTERNALLY-MANAGED >/dev/null 2>&1'; then
      fail "the externally-managed marker is back: pip install refuses outside a venv"
    else
      ok "pip installs without a venv"
    fi
    ;;
  *) fail "unknown toolchain $TOOLCHAIN" ;;
esac

[ "$FAILED" = 0 ] || { echo "== $IMG FAILED"; exit 1; }
echo "== $IMG ok"
