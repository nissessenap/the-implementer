#!/usr/bin/env bash
# Stage 20 — a stock TLS server holding the leaf, and the Job fixture that proves
# the trust bundle against it. Credentials: none.
#
# Proving the trust seam before any of our code exists is the whole point: once
# there is a proxy to blame, a handshake failure here is indistinguishable from a
# bug in it.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"
JOB=e2e-trust-bundle

stage "start the TLS server holding the leaf"
# Recreated rather than applied over: a Pod's container command is immutable.
kubectl delete pod -n "$NS" tls-fixture --ignore-not-found --wait >/dev/null
# python's stdlib rather than nginx, which needs a ConfigMap of config to do the
# same job. `openssl s_server -www` is shorter still and not usable: it answers
# HTTP/1.0 without close_notify, which curl reports as error 56 (curl#17471).
# It reads the leaf Secret directly — the *fixture* never sees the key, which is
# the property under test.
kubectl apply -n "$NS" -f - >/dev/null <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: tls-fixture
  labels: { app: tls-fixture }
spec:
  containers:
    - name: server
      image: python:3.14-alpine
      command: ["python3", "-u", "-c"]
      args:
        - |
          import http.server, ssl
          class H(http.server.BaseHTTPRequestHandler):
              # HTTP/1.1 plus a Content-Length, or the body is delimited by the
              # connection closing — and socketserver closes without a TLS
              # close_notify, which curl reports as "unexpected eof while
              # reading" and not as the handshake success it actually was.
              protocol_version = "HTTP/1.1"
              def do_GET(self):
                  body = b"ok\n"
                  self.send_response(200)
                  self.send_header("Content-Length", str(len(body)))
                  self.end_headers()
                  self.wfile.write(body)
          ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
          ctx.load_cert_chain("/tls/tls.crt", "/tls/tls.key")
          srv = http.server.ThreadingHTTPServer(("", 8443), H)
          srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
          srv.serve_forever()
      volumeMounts:
        - { name: tls, mountPath: /tls, readOnly: true }
  volumes:
    - name: tls
      secret: { secretName: proxy-leaf-tls }
---
apiVersion: v1
kind: Service
metadata: { name: tls-fixture }
spec:
  selector: { app: tls-fixture }
  ports: [{ port: 443, targetPort: 8443 }]
YAML
kubectl -n "$NS" wait --for=condition=Ready pod/tls-fixture --timeout=180s

stage "apply the fixture (runtimeClassName=${RUNTIME_CLASS:-<none>})"
run_job "$JOB" "$E2E_DIR/job.yaml"
echo "==> trust bundle proven"
