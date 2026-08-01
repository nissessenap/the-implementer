#!/usr/bin/env bash
# PROTOTYPE probe — throwaway. Answers issue #28: can an inner Docker container
# reach the agent process?
#
# dind.sh proved dockerd runs unprivileged under gVisor, but ran it with
# --net=slirp4netns --bridge=none and only ever started short-lived containers, so
# inner containers had no address the sandbox could reach. That makes
# testcontainers-shaped work impossible, which is the whole reason to want Docker.
#
# The fix under test: run the ENTIRE phase script inside rootlesskit, not just
# dockerd. Then the agent process, dockerd and inner containers on --network=host
# all sit in one network namespace and share localhost.
#
#   ./proto/dind-net.sh            # MODE=wrap  (slirp4netns, default)
#   MODE=host ./proto/dind-net.sh  # rootlesskit --net=host — reproduces a moby panic
#
# Plain `gvisor` RuntimeClass — no privileged, no extra runsc flags.
set -euo pipefail

RUNTIME=${RUNTIME:-gvisor}
NS=implementer-proto
DIR=$(cd "$(dirname "$0")" && pwd)

# MODE=wrap — rootlesskit --net=slirp4netns, the configuration dind.sh already
#             proved boots. Everything runs inside it.
# MODE=host — rootlesskit --net=host, so dockerd shares the pod netns (path 1 in
#             #28). BLOCKED UPSTREAM: `docker version` panics dockerd with a nil
#             deref at daemon/server/router/system/system_routes.go:132, so the CLI
#             cannot negotiate an API version and every later command fails.
#             `docker info` succeeds, which is what makes it look like it works.
#             Kept runnable so the bug stays reproducible.
MODE=${MODE:-wrap}
case $MODE in
  wrap) NET=slirp4netns; NETARGS="--mtu=1500 --disable-host-loopback --port-driver=builtin" ;;
  host) NET=host;        NETARGS="" ;;
  *) echo "MODE must be wrap or host" >&2; exit 1 ;;
esac
POD=dind-net-$MODE

# Same cluster guard as go.sh/dind.sh: this laptop's ~/.kube/config points at real
# GKE, so refuse anything that is not local k3s.
if [[ -z ${KUBECONFIG:-} ]]; then
  KUBECONFIG="$DIR/.k3s.kubeconfig"
  if [[ ! -r $KUBECONFIG ]]; then
    sudo cat /etc/rancher/k3s/k3s.yaml > "$KUBECONFIG"
    chmod 600 "$KUBECONFIG"
  fi
fi
export KUBECONFIG
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
case $SERVER in
  https://127.0.0.1:* | https://localhost:* | 'https://[::1]:'*) ;;
  *) echo "REFUSING: API server '$SERVER' is not local k3s" >&2; exit 1 ;;
esac

kubectl get runtimeclass "$RUNTIME" >/dev/null
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" delete pod "$POD" --ignore-not-found --wait >/dev/null

# The probe body goes in a ConfigMap rather than inline args: it has to run *inside*
# rootlesskit, and nesting it as a heredoc-within-heredoc is not worth debugging.
kubectl -n "$NS" delete configmap dind-probe --ignore-not-found >/dev/null
kubectl -n "$NS" create configmap dind-probe --from-file=probe.sh=/dev/stdin >/dev/null <<'PROBE'
#!/bin/sh
# Runs INSIDE rootlesskit. dockerd is started here, not by rootlesskit's argv, so
# the agent process and dockerd share a network namespace by construction.
set -u
echo "PROBE netns          $(readlink /proc/self/ns/net)"
# Inside rootlesskit's user namespace the pod's uid 1000 maps to 0. That is a real
# consequence for us, not cosmetic: the agent CLI's root gate is
# `getuid()===0 && IS_SANDBOX!=="1" && !CLAUDE_CODE_BUBBLEWRAP`, so wrapping the
# phase script in rootlesskit trips it. Record the mapping so the cost is explicit.
echo "PROBE whoami         uid=$(id -u) gid=$(id -g)"
echo "PROBE uid_map        $(tr -s ' ' < /proc/self/uid_map | tr '\n' '|')"

# --bridge=none stays regardless of --net: creating a bridge writes
# /proc/sys/net/ipv4/ip_forward, EPERM for an unprivileged userns under gVisor.
# --iptables=false because gVisor cannot serve dockerd's iptables setup.
dockerd --iptables=false --ip6tables=false --bridge=none ${DOCKERD_EXTRA:-} >/tmp/dockerd.log 2>&1 &

# Gate on `docker version`, NOT `docker info`: info answers while buildkit is still
# initialising (~6s), so an info-only gate races the daemon. This is also the call
# that panics dockerd under --net=host, so it doubles as the mode check.
for i in $(seq 90); do docker version >/dev/null 2>&1 && break; sleep 1; done
if ! docker version >/dev/null 2>&1; then
  echo "RESULT dockerd=FAIL"
  grep -m1 -A4 'panic' /tmp/dockerd.log || tail -20 /tmp/dockerd.log
  exit 1
fi
echo "PROBE dockerd        up ($(docker version --format '{{.Server.Version}}'), driver=$(docker info -f '{{.Driver}}'))"
echo "RESULT dockerd=PASS"

docker pull -q busybox >/dev/null 2>&1 \
  && echo "RESULT image-pull=PASS" || echo "RESULT image-pull=FAIL"

# ---- THE question: inner container listens, the sandbox's own process connects ----
docker run -d --name listener --network=host busybox sh -c \
  'echo hello-from-inner > /tmp/i && httpd -f -p 18080 -h /tmp' >/dev/null 2>&1 \
  || echo "PROBE inner-start    FAILED"
sleep 3
GOT=$(wget -qO- --timeout=5 http://127.0.0.1:18080/i 2>/dev/null || echo "")
if [ "$GOT" = "hello-from-inner" ]; then
  echo "RESULT sandbox-to-inner=PASS"
else
  echo "RESULT sandbox-to-inner=FAIL (got '$GOT')"
  echo "PROBE listener-logs  $(docker logs listener 2>&1 | tr '\n' ' ' | cut -c1-200)"
fi

# ---- reverse direction: testcontainers health checks need this too ----
# nc on both sides, not httpd: this image's busybox has no httpd applet (the busybox
# *image* used for inner containers does, which is why the forward probe worked).
(echo hello-from-sandbox | nc -l -p 18081 >/dev/null 2>&1 &)
sleep 2
BACK=$(docker run --rm --network=host busybox nc -w 5 127.0.0.1 18081 2>/dev/null || echo "")
[ "$BACK" = "hello-from-sandbox" ] \
  && echo "RESULT inner-to-sandbox=PASS" \
  || echo "RESULT inner-to-sandbox=FAIL (got '$BACK')"

# ---- inner container egress: deps get pulled during tests ----
docker run --rm --network=host busybox \
  wget -q --timeout=8 -O /dev/null https://proxy.golang.org/ >/dev/null 2>&1 \
  && echo "RESULT inner-egress=PASS" || echo "RESULT inner-egress=FAIL"

# ---- two inner containers talking, which on a shared netns needs no bridge ----
docker run -d --name svc --network=host busybox sh -c \
  'echo svc-alive > /tmp/s && httpd -f -p 18082 -h /tmp' >/dev/null 2>&1
sleep 3
PEER=$(docker run --rm --network=host busybox \
  wget -qO- --timeout=5 http://127.0.0.1:18082/s 2>/dev/null || echo "")
[ "$PEER" = "svc-alive" ] \
  && echo "RESULT inner-to-inner=PASS" \
  || echo "RESULT inner-to-inner=FAIL (got '$PEER')"

# ---- bridge, expected to still fail; cheap to confirm ----
docker network create probe-br >/dev/null 2>&1 \
  && echo "RESULT bridge-create=PASS (unexpected)" \
  || echo "RESULT bridge-create=FAIL (expected: ip_forward EPERM)"

# ---- docker build, recorded as unknown by the first spike ----
# --network=none on the build: buildkit defaults RUN steps to a bridge network, which
# we cannot have (ip_forward EPERM). Cost: a RUN that fetches from the network fails,
# so this only covers builds whose steps are self-contained.
mkdir -p /tmp/b && printf 'FROM busybox\nRUN echo built-ok > /x\n' > /tmp/b/Dockerfile
if docker build -q --network=none -t probe-build /tmp/b >/tmp/build.log 2>&1; then
  echo "RESULT docker-build-nonet=PASS ($(docker run --rm --network=host probe-build cat /x 2>/dev/null))"
else
  echo "RESULT docker-build-nonet=FAIL $(tail -3 /tmp/build.log | tr '\n' ' ' | cut -c1-160)"
fi
# And the case a real repo actually has: a RUN that needs the network.
printf 'FROM busybox\nRUN wget -q -O /y https://proxy.golang.org/ && echo fetched\n' > /tmp/b/Dockerfile
docker build -q -t probe-build-net /tmp/b >/tmp/build2.log 2>&1 \
  && echo "RESULT docker-build-net=PASS" \
  || echo "RESULT docker-build-net=FAIL $(tail -2 /tmp/build2.log | tr '\n' ' ' | cut -c1-120)"

# ---- bubblewrap inside rootlesskit: ADR 0001 requires the binary, and #22 found it
# ---- fails as uid 0 under gVisor. Inside rootlesskit we ARE uid 0, so re-check.
if apk add --no-cache bubblewrap >/tmp/apk.log 2>&1; then
  bw() { bwrap --ro-bind / / "$@" true 2>&1 >/dev/null | tr -d '\n' | cut -c1-90; }
  P=$(bw); N=$(bw --unshare-net)
  echo "RESULT bwrap-plain=$([ -z "$P" ] && echo PASS || echo "FAIL: $P")"
  echo "RESULT bwrap-unshare-net=$([ -z "$N" ] && echo PASS || echo "FAIL: $N")"
else
  echo "RESULT bwrap=UNTESTED ($(tail -2 /tmp/apk.log | tr '\n' ' ' | cut -c1-140))"
fi

# ---- compose with a dependency the app talks to: the real testcontainers shape ----
mkdir -p /tmp/c
cat > /tmp/c/compose.yaml <<'YAML'
services:
  db:
    image: busybox
    network_mode: host
    command: sh -c "echo db-up > /tmp/d && httpd -f -p 18083 -h /tmp"
YAML
docker compose -f /tmp/c/compose.yaml up -d >/dev/null 2>&1
sleep 4
CMP=$(wget -qO- --timeout=5 http://127.0.0.1:18083/d 2>/dev/null || echo "")
[ "$CMP" = "db-up" ] \
  && echo "RESULT compose-reachable=PASS" \
  || echo "RESULT compose-reachable=FAIL (got '$CMP')"

echo "PROBE done"
PROBE

kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: { name: $POD, namespace: $NS }
spec:
  restartPolicy: Never
  runtimeClassName: $RUNTIME
  securityContext: { runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000 }
  containers:
    - name: dind
      image: docker:29-dind-rootless
      securityContext: { privileged: false }
      env:
        - { name: XDG_RUNTIME_DIR, value: /run/user/1000 }
        - { name: DOCKER_HOST, value: "unix:///run/user/1000/docker.sock" }
        - { name: DOCKERD_EXTRA, value: "${DOCKERD_EXTRA:-}" }
      # rootlesskit wraps the whole probe, not just dockerd. That is the change
      # being tested. dockerd-entrypoint.sh is still unusable: unguarded
      # \`iptables --version\` under \`set -e\`, and iptables-nft cannot init under gVisor.
      command: ["rootlesskit", "--net=$NET", $( [[ -n $NETARGS ]] && for a in $NETARGS; do printf '"%s", ' "$a"; done )"--copy-up=/etc", "--copy-up=/run", "/bin/sh", "/probe/probe.sh"]
      volumeMounts:
        - { name: probe, mountPath: /probe }
        - { name: docker-storage, mountPath: /home/rootless/.local/share }
        - { name: runtime-dir, mountPath: /run/user/1000 }
  volumes:
    - name: probe
      configMap: { name: dind-probe }
    - name: docker-storage
      emptyDir: { medium: Memory, sizeLimit: 4Gi }
    - name: runtime-dir
      emptyDir: {}
EOF

echo "==> mode=$MODE net=$NET runtime=$RUNTIME uid=1000 unprivileged"
kubectl -n "$NS" wait --for=condition=Ready "pod/$POD" --timeout=180s 2>/dev/null || true
kubectl -n "$NS" logs -f "$POD" 2>&1 || true
for _ in $(seq 60); do
  case $(kubectl -n "$NS" get "pod/$POD" -o jsonpath='{.status.phase}') in
    Succeeded | Failed) break ;;
  esac
  sleep 2
done
echo "==> phase=$(kubectl -n "$NS" get "pod/$POD" -o jsonpath='{.status.phase}')"
