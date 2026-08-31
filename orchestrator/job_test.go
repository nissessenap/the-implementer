package orchestrator

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nissessenap/the-implementer/proxy"
)

// The minimum a chart-rendered template has to be for the builder to patch it.
// Deliberately not a copy of the chart's — the posture the chart renders is the
// chart's business and asserting it here would only pin one file to another.
const miniTemplate = `
apiVersion: batch/v1
kind: Job
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 3600
  template:
    spec:
      restartPolicy: Never
      runtimeClassName: runsc
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
      containers:
        - name: agent
          image: implementer/sandbox:dev
          env:
            - { name: HOME, value: /home/agent }
            - { name: GIT_AUTHOR_NAME, value: the-implementer }
`

func template(t *testing.T, body string) *batchv1.Job {
	t.Helper()
	p := filepath.Join(t.TempDir(), "job.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	j, err := LoadTemplate(p)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func build(t *testing.T) (*batchv1.Job, proxy.Run) {
	t.Helper()
	r := proxy.Run{Owner: "acme", Repo: "widgets", Issue: "5", UID: "run1234"}
	cfg := Config{
		Namespace: "implementer",
		ProxyHost: "credential-proxy",
		Key:       []byte("a-shared-run-key"),
		Template:  template(t, miniTemplate),
	}
	return cfg.Build(r), r
}

func envOf(j *batchv1.Job) map[string]string {
	m := map[string]string{}
	for _, v := range j.Spec.Template.Spec.Containers[0].Env {
		m[v.Name] = v.Value
	}
	return m
}

// Both copies, from one struct. A Job's own metadata does not propagate to its
// pods, and the pod copy is the one the proxy resolves a source IP to — so a
// builder that wrote only the Job's would leave the proxy unable to see run
// identity at all.
func TestRunIdentityIsWrittenTwice(t *testing.T) {
	j, r := build(t)
	want := map[string]string{
		proxy.AnnOwner:  r.Owner,
		proxy.AnnRepo:   r.Repo,
		proxy.AnnIssue:  r.Issue,
		proxy.AnnRunUID: r.UID,
	}
	for k, v := range want {
		if got := j.Annotations[k]; got != v {
			t.Errorf("job annotation %s = %q, want %q", k, got, v)
		}
		if got := j.Spec.Template.Annotations[k]; got != v {
			t.Errorf("pod annotation %s = %q, want %q", k, got, v)
		}
	}
	// And the proxy can read its own Run back out of the pod copy, which is the
	// comparison that actually happens at request time.
	a := j.Spec.Template.Annotations
	got := proxy.Run{Owner: a[proxy.AnnOwner], Repo: a[proxy.AnnRepo], Issue: a[proxy.AnnIssue], UID: a[proxy.AnnRunUID]}
	if got != r {
		t.Errorf("the pod says %+v, the run is %+v", got, r)
	}
	if j.Labels["app"] != "implementer" || j.Spec.Template.Labels["app"] != "implementer" {
		t.Error("the one listing label is missing from the Job or its pod template")
	}
}

// The seven, all pointing at the bundle and never at the bare CA: six of them
// replace the trust store rather than adding to it.
func TestSandboxTrustVariables(t *testing.T) {
	j, _ := build(t)
	env := envOf(j)
	if len(trustVars) != 7 {
		t.Errorf("ADR 0001's seam is seven variables, this is %d", len(trustVars))
	}
	for _, v := range trustVars {
		if env[v] != trustBundle {
			t.Errorf("%s = %q, want the bundle %q", v, env[v], trustBundle)
		}
	}
	// The failing shape this pins: a bare CA path. It would verify the proxy and
	// nothing else on the internet, and the symptom appears on an unrelated call.
	if strings.HasSuffix(trustBundle, "/ca.crt") {
		t.Errorf("the trust variables point at %q, which looks like the bare CA", trustBundle)
	}
}

// The proxy URL carries the credential as userinfo, so every client derives
// Proxy-Authorization from one variable — and the credential is the proxy's own
// HMAC, not a second implementation of it.
func TestSandboxProxyEnv(t *testing.T) {
	j, r := build(t)
	env := envOf(j)

	u, err := url.Parse(env["https_proxy"])
	if err != nil {
		t.Fatalf("https_proxy is not a URL: %v", err)
	}
	wantUser, wantPass := proxy.Cred([]byte("a-shared-run-key"), r)
	gotPass, _ := u.User.Password()
	if u.User.Username() != wantUser || gotPass != wantPass {
		t.Errorf("userinfo is %s:%s, want %s:%s", u.User.Username(), gotPass, wantUser, wantPass)
	}
	// Independently, so this test fails if proxy.Cred and the sandbox URL ever
	// stop agreeing on what the claim is spelled like.
	m := hmac.New(sha256.New, []byte("a-shared-run-key"))
	m.Write([]byte("acme,widgets,5,run1234"))
	if gotPass != hex.EncodeToString(m.Sum(nil)) {
		t.Errorf("the secret is not HMAC-SHA256(key, %q)", "acme,widgets,5,run1234")
	}
	if u.Host != "credential-proxy:8080" {
		t.Errorf("proxy host is %q", u.Host)
	}
	if env["HTTPS_PROXY"] != env["https_proxy"] {
		t.Error("only one case of the proxy variable is set")
	}

	// no_proxy must name the proxy, or the model base URL is tunnelled through the
	// proxy to the proxy.
	base := env["ANTHROPIC_VERTEX_BASE_URL"]
	if env["no_proxy"] != "credential-proxy" || env["NO_PROXY"] != env["no_proxy"] {
		t.Errorf("no_proxy is %q/%q, want the proxy Service", env["no_proxy"], env["NO_PROXY"])
	}
	if !strings.Contains(base, env["no_proxy"]) {
		t.Errorf("no_proxy %q does not cover the model base URL %q", env["no_proxy"], base)
	}

	// Pre-sending Basic is what stops git reaching for a credential helper the
	// sandbox does not have.
	if env["GIT_CONFIG_PARAMETERS"] != "'http.proxyAuthMethod=basic'" {
		t.Errorf("GIT_CONFIG_PARAMETERS = %q", env["GIT_CONFIG_PARAMETERS"])
	}
	if env["GH_TOKEN"] != proxy.Sentinel {
		t.Errorf("GH_TOKEN = %q, want the proxy's sentinel", env["GH_TOKEN"])
	}
}

// The assertion the whole design is for: the sandbox holds no credential worth
// stealing. Anything that looks like one, other than the sentinel and the run
// secret in the proxy URL, is a regression.
func TestSandboxHoldsNoCredential(t *testing.T) {
	j, _ := build(t)
	for _, v := range j.Spec.Template.Spec.Containers[0].Env {
		switch v.Name {
		case "GH_TOKEN", "https_proxy", "HTTPS_PROXY":
			continue // the sentinel and the run secret, both worthless outside this cluster
		}
		up := strings.ToUpper(v.Name)
		for _, bad := range []string{"API_KEY", "OAUTH", "TOKEN", "GOOGLE_APPLICATION", "SECRET", "CREDENTIAL", "PASSWORD", "AWS_ACCESS"} {
			if strings.Contains(up, bad) {
				t.Errorf("the sandbox holds %s", v.Name)
			}
		}
		if v.ValueFrom != nil && v.ValueFrom.SecretKeyRef != nil {
			t.Errorf("%s reads a Secret", v.Name)
		}
	}
	// And no Secret volume, which is the other way a credential arrives.
	for _, vol := range j.Spec.Template.Spec.Volumes {
		if vol.Secret != nil {
			t.Errorf("the sandbox mounts Secret %s", vol.Secret.SecretName)
		}
	}
}

// Unset is legal: it means the review phase runs with no language subagent. An
// empty TOOLCHAIN would be a value the phase script tests as present.
func TestToolchainUnsetIsOmitted(t *testing.T) {
	j, _ := build(t)
	if _, ok := envOf(j)["TOOLCHAIN"]; ok {
		t.Error("TOOLCHAIN is set with no toolchain configured")
	}
	cfg := Config{Namespace: "n", ProxyHost: "p", Key: []byte("k"), Template: template(t, miniTemplate), Toolchain: "go"}
	if got := envOf(cfg.Build(proxy.Run{Owner: "a", Repo: "b", Issue: "1", UID: "u"}))["TOOLCHAIN"]; got != "go" {
		t.Errorf("TOOLCHAIN = %q", got)
	}
}

// The template is the operator's: the builder patches per-run fields and leaves
// the posture alone. This is the clause that keeps the uid out of Go.
func TestTemplateSurvivesTheBuilder(t *testing.T) {
	j, _ := build(t)
	s := j.Spec.Template.Spec
	if s.RuntimeClassName == nil || *s.RuntimeClassName != "runsc" {
		t.Error("runtimeClassName did not survive")
	}
	if s.SecurityContext == nil || s.SecurityContext.RunAsUser == nil || *s.SecurityContext.RunAsUser != 1000 {
		t.Error("the pod securityContext did not survive")
	}
	if j.Spec.BackoffLimit == nil || *j.Spec.BackoffLimit != 0 {
		t.Error("backoffLimit did not survive")
	}
	if j.Spec.ActiveDeadlineSeconds == nil || *j.Spec.ActiveDeadlineSeconds != 3600 {
		t.Error("activeDeadlineSeconds did not survive")
	}
	// The template's own env survives alongside the builder's, and the builder
	// wins on a name they share rather than leaving a duplicate for the kubelet
	// to resolve.
	env := envOf(j)
	if env["HOME"] != "/home/agent" || env["GIT_AUTHOR_NAME"] != "the-implementer" {
		t.Error("the template's env did not survive")
	}
	seen := map[string]int{}
	for _, v := range j.Spec.Template.Spec.Containers[0].Env {
		seen[v.Name]++
		if seen[v.Name] > 1 {
			t.Errorf("%s appears twice", v.Name)
		}
	}
}

// A misspelled PodSpec field renders, applies, and silently does nothing — the
// operator wrote a posture the cluster never saw. Strict decoding turns that into
// an error at the orchestrator rather than a run with no gVisor.
//
// It catches *unknown* fields and not *miscased* ones: encoding/json matches
// field names case-insensitively, so `runtimeClassname` is accepted and works.
// Worth knowing before trusting this as a spell-checker.
func TestLoadTemplateIsStrict(t *testing.T) {
	p := filepath.Join(t.TempDir(), "job.yaml")
	os.WriteFile(p, []byte("apiVersion: batch/v1\nkind: Job\nspec:\n  template:\n    spec:\n      runtimeClas: runsc\n      containers: [{name: a, image: b}]\n"), 0o600)
	if _, err := LoadTemplate(p); err == nil {
		t.Error("an unknown PodSpec field loaded without complaint")
	}
}

// Idempotency is the name plus a swallowed AlreadyExists, and nothing else: no
// dedupe table, because there is nothing to keep one in.
func TestCreateSwallowsAlreadyExists(t *testing.T) {
	j, _ := build(t)
	c := fake.NewSimpleClientset()
	created, err := Create(context.Background(), c, j, false)
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}
	created, err = Create(context.Background(), c, j.DeepCopy(), false)
	if err != nil {
		t.Fatalf("redelivery is an error: %v", err)
	}
	if created {
		t.Error("redelivery created a second Job")
	}
	list, _ := c.BatchV1().Jobs("implementer").List(context.Background(), metav1.ListOptions{})
	if len(list.Items) != 1 {
		t.Errorf("%d Jobs after two runs of the same issue", len(list.Items))
	}
}

func TestParseIssue(t *testing.T) {
	r, err := ParseIssue("nissessenap/the-implementer#70")
	if err != nil {
		t.Fatal(err)
	}
	if r != (proxy.Run{Owner: "nissessenap", Repo: "the-implementer", Issue: "70"}) {
		t.Errorf("got %+v", r)
	}
	for _, bad := range []string{"", "owner/repo", "owner#5", "/repo#5", "owner/#5", "owner/repo#", "owner/repo#x"} {
		if _, err := ParseIssue(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
