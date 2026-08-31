# Stage 80's sandbox, and the reason the chart has a `sandbox.script` seam: this
# runs in a Job that charts/orchestrator and the Go builder produced between them,
# in place of the phase script the real image bakes in. 80-orchestrator.sh
# substitutes __CLONE_REPO__ and appends orchestrator-push-probe.sh when there is a
# credential to push with.
#
# It holds no credential. Everything below arrives from the builder's environment:
# the run secret is userinfo in $https_proxy, the GitHub credential is a sentinel,
# and there is no model or cloud credential at all.

# The one thing the sandbox assembles for itself, because /etc/ssl/certs is
# read-only under readOnlyRootFilesystem and the CA is minted at install time so no
# image can ship it. Onto the *system roots*: six of the seven variables replace
# the trust store rather than adding to it, so the bare CA would leave this pod
# unable to verify anything the proxy does not terminate.
cat /etc/ssl/certs/ca-certificates.crt /run/proxy-ca/ca.crt > "$SSL_CERT_FILE"

# 1. ADR 0001's seven, as the builder wrote them. Asserted here as well as in
#    orchestrator/job_test.go because a variable that is right in Go and absent in
#    the pod is the failure this stage exists to catch.
for v in SSL_CERT_FILE GIT_SSL_CAINFO CURL_CA_BUNDLE PIP_CERT REQUESTS_CA_BUNDLE AWS_CA_BUNDLE NODE_EXTRA_CA_CERTS; do
  eval "val=\${$v:-}"
  [ -n "$val" ] || { echo "!!! FAIL: $v is unset"; exit 1; }
  [ "$val" = "$SSL_CERT_FILE" ] || { echo "!!! FAIL: $v is '$val', not the bundle"; exit 1; }
done
echo "PROBE trust-vars       7    (all on $SSL_CERT_FILE, $(grep -c 'BEGIN CERTIFICATE' "$SSL_CERT_FILE") certs)"

# 2. no_proxy names the proxy Service, or the model base URL below is tunnelled
#    through the proxy to the proxy.
case $ANTHROPIC_VERTEX_BASE_URL in
  *"$no_proxy"*) ;;
  *) echo "!!! FAIL: no_proxy '$no_proxy' does not cover '$ANTHROPIC_VERTEX_BASE_URL'"; exit 1 ;;
esac
echo "PROBE no-proxy         $no_proxy  (the model base URL is $ANTHROPIC_VERTEX_BASE_URL)"

# 3. Pre-sending Basic to the proxy is what stops git reaching for a credential
#    helper this pod does not have — the 407 is the visible half, the hang is the
#    one that matters.
[ "$GIT_CONFIG_PARAMETERS" = "'http.proxyAuthMethod=basic'" ] \
  || { echo "!!! FAIL: GIT_CONFIG_PARAMETERS is '$GIT_CONFIG_PARAMETERS'"; exit 1; }
echo "PROBE git-proxy-auth   basic (no credential helper is consulted for the proxy URL)"

# 4. What this pod holds, asserted rather than asserted-about: a sandbox carrying a
#    real credential would make everything below pass for the wrong reason. GH_TOKEN
#    is excluded because it is the sentinel, and checked for being one.
#    The same list TestSandboxHoldsNoCredential uses, so the in-cluster half of the
#    assertion is not the weaker one. GH_TOKEN is excluded *by name* rather than by
#    leaving TOKEN out of the pattern, and then checked for being the sentinel.
held=$(env | sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p' | grep -v '^GH_TOKEN$' \
  | grep -E 'API_KEY|OAUTH|TOKEN|GOOGLE_APPLICATION|SECRET|CREDENTIAL|PASSWORD|AWS_ACCESS' || true)
[ -z "$held" ] || { echo "!!! FAIL: the sandbox holds $held"; exit 1; }
case $GH_TOKEN in
  proxy-injected*) ;;
  *) echo "!!! FAIL: GH_TOKEN is not the sentinel"; exit 1 ;;
esac

#    And the credential no environment check would have found: a projected
#    ServiceAccount token, mounted by default, which is a bearer for this cluster's
#    apiserver. automountServiceAccountToken: false in the Job template is what
#    removes it, and this is the only place that can tell.
[ ! -e /var/run/secrets/kubernetes.io/serviceaccount/token ] \
  || { echo "!!! FAIL: the sandbox carries a ServiceAccount token"; exit 1; }
echo "PROBE sandbox-holds    a sentinel and the run secret, nothing else"
echo "PROBE no-sa-token      ok   (automountServiceAccountToken: false)"

# 5. The proxy, reached with the credential the builder put in $https_proxy as
#    userinfo — derived from the annotations this pod carries, which the proxy
#    resolves its source IP back to. Anonymous upstream on purpose: what is under
#    test here is the builder, and the sentinel swap is stage 50's.
#    __CLONE_REPO__ rather than $REPO, because the run's repository may be the
#    private scratch one the push probe below wants and an anonymous clone of that
#    would fail for the wrong reason.
git clone --depth 1 -q "https://github.com/__CLONE_REPO__.git" "$WORKSPACE/clone" \
  || { echo "!!! FAIL: clone through the proxy failed"; exit 1; }
echo "PROBE git-clone        ok   (__CLONE_REPO__, over the proxy's intercepted TLS)"

echo "the orchestrator built this Job and the sandbox held no credential of its own"
