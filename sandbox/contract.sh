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

# say <label> <command> — run it in the image and report what it printed, but only
# if it *worked*. The obvious spelling, `ok "go $(in_image 'go version')"`, throws
# the status away: a command substitution used as an argument leaves `ok`'s own
# status as the simple command's, so a missing toolchain prints `ok go` and the
# contract passes — which would make the one check this section exists for the one
# check that can never fail. A bare assignment is the opposite trap under `set -e`:
# a transient `docker run` failure would abort before the summary, so the status is
# taken here rather than left to the shell.
say() {
  _label=$1
  if _out=$(in_image "$2"); then
    ok "$_label $_out"
  else
    fail "$_label: \`$2\` failed in the image"
  fi
}

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

leaked=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$IMG" |
  grep -Ei '(TOKEN|PASSWORD|SECRET|_KEY|CREDENTIAL|NPM_CONFIG_|PIP_INDEX|GOPRIVATE)=' || true)
[ -z "$leaked" ] && ok "no credential-shaped or registry-shaped environment" ||
  fail "the image config carries $leaked"

# --- the toolchain the derivative exists to carry ----------------------------
case $TOOLCHAIN in
  '') ;;
  go)
    say go "go version | cut -d' ' -f3"
    # `auto`, so a repository whose go.mod asks for a version this image does not
    # carry downloads it and completes. `local` would make that a hard failure,
    # and the failure would look like the repository's fault.
    gt=$(in_image 'echo ${GOTOOLCHAIN:-auto}') || gt='<unreadable>' 
    [ "$gt" != local ] && ok "GOTOOLCHAIN=$gt, so a version this image lacks is downloaded, not refused" ||
      fail "GOTOOLCHAIN=local turns a version mismatch into a failed run"
    # The container runtime, which only this image carries. Presence only — that
    # it *works* needs gVisor and a cluster, which is e2e/85's job.
    stack=yes
    for c in dockerd docker rootlesskit slirp4netns newuidmap newgidmap getcap; do
      in_image "command -v $c >/dev/null" || { fail "$c is missing from the go image"; stack=no; }
    done
    in_image 'grep -q "^agent:" /etc/subuid && grep -q "^agent:" /etc/subgid' ||
      { fail "/etc/subuid or /etc/subgid has no range for uid 1000 — rootlesskit falls back to a single id and dockerd dies later"; stack=no; }
    # File capabilities and not the setuid bit: under gVisor a setuid-root exec
    # grants no capabilities at all, so a setuid newuidmap is unprivileged exactly
    # where we run it. Checked by reading the xattr, because the one way this
    # regresses is an image layer quietly dropping it.
    in_image 'getcap /usr/bin/newuidmap 2>/dev/null | grep -q cap_setuid' ||
      { fail "newuidmap has no cap_setuid file capability — the mapping rootlesskit needs is the one it cannot make itself"; stack=no; }
    # Guarded for the same reason as the credential loop above: a summary line that
    # prints under its own FAILs is a summary of nothing.
    [ "$stack" = yes ] && ok "the rootless Docker stack, its subordinate id ranges and newuidmap's file capability"
    ;;
  node)
    say node "node --version"
    say npm "npm --version"
    ;;
  python)
    say python "python3 --version"
    say pip "pip --version | cut -d' ' -f1-2"
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
