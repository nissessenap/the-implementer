#!/usr/bin/env bash
# PROTOTYPE probe — throwaway. Answers one question: can a run start Docker
# inside its own sandbox? That is what makes a dev-shaped workload (testcontainers,
# `docker build`, compose) possible without granting anything on the host.
#
#   ./proto/dind.sh
#
# Requires the gvisor-dind RuntimeClass (handler runsc-dind, configured with
# -net-raw and -allow-packet-socket-write). See docs/research/prototype-findings.md.
set -euo pipefail

# RUNTIME=gvisor tests whether the extra -net-raw/-allow-packet-socket-write
# grants are needed at all, or whether the plain sandbox suffices.
RUNTIME=${RUNTIME:-gvisor-dind}
NS=implementer-proto
POD=dind-probe
DIR=$(cd "$(dirname "$0")" && pwd)

# Same cluster guard as go.sh: this laptop's ~/.kube/config points at real GKE.
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

kubectl get runtimeclass "$RUNTIME" >/dev/null || {
  echo "no $RUNTIME RuntimeClass — see docs/research/prototype-findings.md" >&2; exit 1; }
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" delete pod "$POD" --ignore-not-found --wait >/dev/null

# ROOTLESS=1 runs dockerd as uid 1000 instead of root, which is the posture the
# agent Job actually uses — the root+privileged variant proves gVisor can host
# Docker, not that our sandbox can.
# PRIV=0 drops privileged, to find out whether gVisor alone is enough.
if [[ -n ${ROOTLESS:-} ]]; then
  IMAGE=docker:29-dind-rootless
  # The rootless image's user is uid 1000 and its data root is under $HOME.
  POD_SECURITY='{ runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000 }'
  # Mount the PARENT, not the data root itself: an emptyDir arrives root:root and
  # fsGroup only makes it group-writable, so dockerd's chmod of its own data root
  # fails with EPERM inside the userns. Mounting the parent lets dockerd mkdir a
  # directory it actually owns.
  DATA_ROOT=/home/rootless/.local/share
  # dockerd-entrypoint.sh cannot be used: it runs an unguarded `iptables --version`
  # under `set -e`, and iptables-nft cannot initialise under gVisor
  # ("Failed to initialize nft: Protocol not supported"), so the entrypoint dies
  # before dockerd starts. This is the rootlesskit invocation it would have run.
  DOCKERD='rootlesskit --net=slirp4netns --mtu=1500 --disable-host-loopback --port-driver=builtin --copy-up=/etc --copy-up=/run dockerd'
  # --bridge=none: creating the default bridge needs to write
  # /proc/sys/net/ipv4/ip_forward, which is EPERM for an unprivileged userns under
  # gVisor. So inner containers get --network=none. dockerd itself still has
  # slirp4netns, so image *pulls* work; it is the inner containers that have no
  # network. Deliberate: a sandboxed run has no business opening a network path
  # onto the node.
  DOCKERD_EXTRA='--bridge=none'
  RUN_NET='--network=none'
  DOCKER_SOCK=unix:///run/user/1000/docker.sock
else
  IMAGE=docker:29-dind
  POD_SECURITY='{}'
  DATA_ROOT=/var/lib/docker
  DOCKERD=dockerd
  DOCKERD_EXTRA=''
  RUN_NET=''
  DOCKER_SOCK=unix:///var/run/docker.sock
fi
if [[ ${PRIV:-1} == 0 ]]; then
  CTR_SECURITY='{ privileged: false }'
else
  CTR_SECURITY='{ privileged: true }'
fi
echo "==> runtime=$RUNTIME image=$IMAGE pod=$POD_SECURITY ctr=$CTR_SECURITY"

sed -e "s@__IMAGE__@$IMAGE@" -e "s@__POD_SECURITY__@$POD_SECURITY@" \
    -e "s@__CTR_SECURITY__@$CTR_SECURITY@" -e "s@__DATA_ROOT__@$DATA_ROOT@" \
    -e "s@__DOCKERD__@$DOCKERD@" -e "s@__DOCKERD_EXTRA__@$DOCKERD_EXTRA@" \
    -e "s@__RUN_NET__@$RUN_NET@" -e "s@__DOCKER_SOCK__@$DOCKER_SOCK@" -e "s@__RUNTIME__@$RUNTIME@" <<'EOF' | kubectl -n "$NS" apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: dind-probe
  namespace: implementer-proto
spec:
  restartPolicy: Never
  runtimeClassName: __RUNTIME__
  securityContext: __POD_SECURITY__
  containers:
    - name: dind
      image: __IMAGE__
      # privileged under gVisor is not what it sounds like: the sandbox never
      # holds capabilities on the host kernel, so this is privilege inside the
      # sentry only. On GKE Sandbox this same pod is rejected at admission,
      # which is the gap worth knowing about.
      securityContext: __CTR_SECURITY__
      env:
        - { name: XDG_RUNTIME_DIR, value: /run/user/1000 }
        - { name: DOCKER_HOST, value: __DOCKER_SOCK__ }
      command: ["/bin/sh", "-c"]
      args:
        - |
          set -eu
          echo "PROBE sandbox          $(dmesg 2>/dev/null | head -1)"
          echo "PROBE whoami           uid=$(id -u) gid=$(id -g)"

          # --iptables/--ip6tables=false: gVisor cannot serve dockerd's iptables
          # setup. Cost: `-p` port publishing does not work for inner containers.
          __DOCKERD__ --iptables=false --ip6tables=false __DOCKERD_EXTRA__ >/tmp/dockerd.log 2>&1 &
          for i in $(seq 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
          docker info >/dev/null 2>&1 || {
            echo "PROBE dockerd          FAILED to come up"
            tail -40 /tmp/dockerd.log; exit 1; }
          echo "PROBE dockerd          up ($(docker version --format '{{.Server.Version}}'), driver=$(docker info -f '{{.Driver}}'))"

          # 1. does a container start and run at all
          echo "PROBE docker-run       $(docker run --rm __RUN_NET__ busybox echo container-ran)"

          # 2. a bind mount, because that is what a dev workload actually does
          mkdir -p /tmp/bind && echo from-the-sandbox > /tmp/bind/f
          echo "PROBE bind-mount       $(docker run --rm __RUN_NET__ -v /tmp/bind:/m busybox cat /m/f)"

          # 3. compose, since that is the other thing someone reaches for.
          # network_mode: none for the same reason as --network=none above:
          # compose would otherwise create a bridge network we cannot have.
          mkdir -p /tmp/c && cat > /tmp/c/compose.yaml <<'YAML'
          services:
            hi:
              image: busybox
              command: echo compose-ran
              network_mode: none
          YAML
          sed -i 's/^          //' /tmp/c/compose.yaml
          echo "PROBE compose          $(docker compose -f /tmp/c/compose.yaml up 2>/dev/null | grep -o compose-ran | head -1)"
      volumeMounts:
        # tmpfs, because overlayfs cannot stack on overlayfs — the documented
        # Docker v29 failure. The alternative is dockerd
        # --feature containerd-snapshotter=false.
        - { name: docker-storage, mountPath: __DATA_ROOT__ }
        - { name: runtime-dir, mountPath: /run/user/1000 }
  volumes:
    - name: docker-storage
      emptyDir: { medium: Memory, sizeLimit: 4Gi }
    - name: runtime-dir
      emptyDir: {}
EOF

echo "==> waiting for $POD"
kubectl -n "$NS" wait --for=condition=Ready "pod/$POD" --timeout=180s 2>/dev/null || true
kubectl -n "$NS" logs -f "$POD" 2>&1 || true
for _ in $(seq 30); do
  case $(kubectl -n "$NS" get "pod/$POD" -o jsonpath='{.status.phase}') in
    Succeeded | Failed) break ;;
  esac
  sleep 2
done
echo "==> phase=$(kubectl -n "$NS" get "pod/$POD" -o jsonpath='{.status.phase}')"
