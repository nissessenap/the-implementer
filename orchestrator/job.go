package orchestrator

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"os"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/nissessenap/the-implementer/proxy"
)

// The one label the builder writes, and the selector the informer lists and watches
// on. Two constants rather than a map literal because the informer needs it as a
// selector string: identity itself is in *annotations*, because a label value caps
// at 63 characters and repository names run past it.
const (
	runLabelKey   = "app"
	runLabelValue = "implementer"
)

// The port the proxy listens on. Not a knob here for the same reason it is not
// one there: the sandbox is handed a URL naming it, so a second place to change
// it is only a second place to get it wrong.
const proxyPort = "8080"

// trustBundle is where the phase script concatenates the proxy's CA onto the
// system roots, and so the value every trust variable below takes. A constant
// rather than a value because both ends of it are ours: the script writes this
// path and the PodSpec names it. /tmp because /etc/ssl/certs is read-only under
// `readOnlyRootFilesystem: true`.
const trustBundle = "/tmp/ca-bundle.crt"

// trustVars is ADR 0001's seven, measured rather than assumed — the tools share
// no convention, and an image built to the older three-variable wording fails
// against the proxy looking like a certificate problem.
//
// All seven get the *bundle*, never the bare CA. Six of them **replace** the
// trust store rather than adding to it, so the bare CA leaves the sandbox unable
// to verify anything else on the internet — a failure that surfaces far from its
// cause, on the first unrelated HTTPS call. NODE_EXTRA_CA_CERTS is the one that
// is genuinely additive and could take the bare CA; handing all seven the same
// value is one assignment instead of a special case.
var trustVars = []string{
	"SSL_CERT_FILE",      // gh, and Go's crypto/x509 generally
	"GIT_SSL_CAINFO",     // git, via libcurl
	"CURL_CA_BUNDLE",     // curl
	"PIP_CERT",           // pip, which carries its own bundle and ignores SSL_CERT_FILE
	"REQUESTS_CA_BUNDLE", // python requests/httpx/urllib3/botocore
	"AWS_CA_BUNDLE",      // aws cli, botocore
	"NODE_EXTRA_CA_CERTS",
}

// Config is everything the builder needs that is not the run. The knobs an
// operator turns are not here: they are in the Job template the chart renders,
// which is where `runtimeClassName`, the uid, the image, the resources, the
// volumes and the three Job-level limits live. Nothing in this struct is a
// posture decision.
type Config struct {
	// Namespace is the dedicated namespace ADR 0004 makes load-bearing: it is the
	// uniqueness scope for the Job name, which is why the name carries no prefix.
	Namespace string

	// ProxyHost is the credential proxy's Service name. It appears in four
	// variables below and must be a name the sandbox can resolve.
	ProxyHost string

	// Key is the one long-lived shared key the run credential derives from,
	// mounted from a Secret this chart reads and does not own. The proxy mounts
	// the same one and recomputes the credential rather than being told it, which
	// is what keeps "no per-run Secret and no orchestrator-to-proxy channel" true.
	Key []byte

	// Template is the Job the chart rendered. Everything per-run is overwritten
	// below; everything else is the operator's.
	Template *batchv1.Job

	// Toolchain is ADR 0003's answer for this repository, and **unset is legal**:
	// it means the review phase runs without a language subagent.
	Toolchain string

	// Docker turns on ADR 0001's rootlesskit wrap for this run, and it defaults
	// off. Per-run rather than per-install because the wrap is not free: inside
	// rootlesskit the process reads its own uid as 0, which trips the agent CLI's
	// root gate and puts bubblewrap at risk, and the container needs the two
	// capabilities below back. A run that does not ask for it pays none of that.
	//
	// Nothing here checks that the image can honour it — only the `go` image
	// carries the stack, and the run plan refuses by name when it does not. This
	// is not derived from the toolchain either: whether a repository *wants* a
	// container runtime needs far more of it than ADR 0003 ever looks at, and
	// guessing it wrong is the silent-failure class that ADR exists to avoid.
	Docker bool
}

// LoadTemplate reads the Job template the chart renders. In a cluster this is a
// mounted ConfigMap; from the command line it is a file.
func LoadTemplate(path string) (*batchv1.Job, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var j batchv1.Job
	// Strict, because the failure it catches is a misspelled PodSpec field that
	// renders, applies, and silently does not do what the operator wrote —
	// `runtimeClassname` being the one that matters here.
	if err := yaml.UnmarshalStrict(b, &j); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(j.Spec.Template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("%s: the template has no containers", path)
	}
	return &j, nil
}

// Build is the whole of "turn a run into an object". One object per run: no
// Secret, no ownerReference sweep, no dedupe record.
func (c Config) Build(r proxy.Run) *batchv1.Job {
	j := c.Template.DeepCopy()
	j.Name = JobName(r)
	j.Namespace = c.Namespace

	// Written twice, and neither copy is redundant. A Job's own metadata does not
	// propagate to its pods: the pod copy is the one the credential proxy resolves
	// a source IP to, and the Job copy is what a human reads in `kubectl get job
	// -o yaml` and what the relist-on-restart reconciliation will read. From one
	// struct, so they cannot drift.
	ann := map[string]string{
		proxy.AnnOwner:  r.Owner,
		proxy.AnnRepo:   r.Repo,
		proxy.AnnIssue:  r.Issue,
		proxy.AnnRunUID: r.UID,
	}
	j.Annotations = merge(j.Annotations, ann)
	j.Spec.Template.Annotations = merge(j.Spec.Template.Annotations, ann)

	// Identity is in annotations and not labels, because a label *value* caps at
	// 63 characters and repository names run past it — the same cliff the name
	// hits. One label exists, for listing.
	lbl := map[string]string{runLabelKey: runLabelValue}
	j.Labels = merge(j.Labels, lbl)
	j.Spec.Template.Labels = merge(j.Spec.Template.Labels, lbl)

	// ponytail: appended, not merged. No template the chart renders sets a name
	// the builder owns, so there is nothing to resolve — the day one does, the
	// kubelet's behaviour on a duplicate is defined nowhere either of us can cite
	// and this wants a drop-what-we-own pass before the append.
	//
	// Containers only, deliberately: there are no init containers, and wiring the
	// environment into a list that is always empty reads as support for something
	// that has never been tried. The day the trust bundle moves out of the probe
	// into one, it needs this loop too — without it there is no SSL_CERT_FILE and
	// no https_proxy, and it fails looking like a certificate problem.
	env := c.env(r)
	for i := range j.Spec.Template.Spec.Containers {
		ct := &j.Spec.Template.Spec.Containers[i]
		ct.Env = append(ct.Env, env...)
		if c.Docker {
			relaxForRootlesskit(ct)
		}
	}
	return j
}

// relaxForRootlesskit is the posture a Docker run costs — exactly two fields, and
// written here rather than left to the operator because without them the run dies
// inside rootlesskit with `newuidmap: write to uid_map failed: Operation not
// permitted`, a message that names neither cause. `newuidmap` is privileged by a
// file capability, and no_new_privs and an emptied bounding set each independently
// neuter one. Nothing else moves: not the uid, the read-only rootfs, the seccomp
// profile or the runtime class. Measured; see docs/architecture.md#8.
//
// ⚠️ These two fields are also what Pod Security Standards `restricted` forbids,
// so a `-docker` run in such a namespace has every pod rejected at admission and
// burns its deadline saying nothing. Stated rather than worked around — the wrap
// genuinely needs the capability. A default run is unaffected.
func relaxForRootlesskit(ct *corev1.Container) {
	if ct.SecurityContext == nil {
		ct.SecurityContext = &corev1.SecurityContext{}
	}
	yes := true
	ct.SecurityContext.AllowPrivilegeEscalation = &yes
	if ct.SecurityContext.Capabilities == nil {
		ct.SecurityContext.Capabilities = &corev1.Capabilities{}
	}
	ct.SecurityContext.Capabilities.Add = append(ct.SecurityContext.Capabilities.Add,
		corev1.Capability("SETUID"), corev1.Capability("SETGID"))
}

// env is the sandbox environment. What is *not* here is the point: no model
// credential, no cloud credential, and the GitHub one is a sentinel.
func (c Config) env(r proxy.Run) []corev1.EnvVar {
	host := c.ProxyHost + ":" + proxyPort

	// The credential as userinfo, so every client derives Proxy-Authorization from
	// one variable unaided — which is what makes a per-run Secret and an
	// orchestrator-to-proxy channel both unnecessary. url.UserPassword encodes it;
	// the commas in the username are legal there, which is why proxy.Cred spells
	// the claim with commas rather than the '/' and '#' the identity uses.
	user, pass := proxy.Cred(c.Key, r)
	proxyURL := (&url.URL{Scheme: "http", User: url.UserPassword(user, pass), Host: host}).String()

	e := []corev1.EnvVar{
		{Name: "REPO", Value: r.Owner + "/" + r.Repo},
		{Name: "ISSUE", Value: r.Issue},

		// The worthless string standing where the GitHub credential would be. The
		// sandbox's code path is unchanged — the phase script still builds an
		// authenticated URL — the value is just no longer worth stealing. From the
		// proxy's own constant, so the string the proxy matches and the string the
		// sandbox holds are the same string.
		{Name: "GH_TOKEN", Value: proxy.Sentinel},

		// Both cases: Node reads the lowercase forms first, libcurl reads
		// lowercase, and plenty of tooling reads only the uppercase.
		{Name: "https_proxy", Value: proxyURL},
		{Name: "HTTPS_PROXY", Value: proxyURL},

		// Must name the proxy, or the model base URL below is tunnelled through
		// the proxy to the proxy.
		{Name: "no_proxy", Value: c.ProxyHost},
		{Name: "NO_PROXY", Value: c.ProxyHost},

		// Pre-send Basic to the *proxy*. The reason is sharper than "git 407s":
		// pre-sending is what stops git invoking a credential helper for the proxy
		// URL — a sandbox has none, and the prompt is what a run hangs on.
		// Unrelated to the upstream credential the swap replaces.
		{Name: "GIT_CONFIG_PARAMETERS", Value: "'http.proxyAuthMethod=basic'"},

		// An unattended run has nobody to type a password at, so a prompt is a hang
		// that burns activeDeadlineSeconds and reports nothing. The one case that
		// reaches it is a URL the proxy's credential cannot open — which is the
		// mint scope working, and it must fail rather than wait.
		{Name: "GIT_TERMINAL_PROMPT", Value: "0"},

		// The model route: an ordinary base URL, http:// because neither direction
		// carries a credential. The sandbox holds no model credential at all — not
		// blanked, absent — so there is nothing here but a hostname.
		{Name: "ANTHROPIC_VERTEX_BASE_URL", Value: "http://" + host + "/vertex"},
	}
	for _, v := range trustVars {
		e = append(e, corev1.EnvVar{Name: v, Value: trustBundle})
	}
	// Unset is legal and means the review phase runs without a language subagent,
	// so an empty value is *omitted* rather than written as "": the phase script
	// tests for presence.
	if c.Toolchain != "" {
		e = append(e, corev1.EnvVar{Name: "TOOLCHAIN", Value: c.Toolchain})
	}
	// Off is the *absence* of the variable rather than "0", for the same reason
	// as TOOLCHAIN above: the run plan tests one thing, and a default run's
	// environment says nothing about a feature it is not using.
	if c.Docker {
		e = append(e, corev1.EnvVar{Name: "SANDBOX_DOCKER", Value: "1"})
	}
	return e
}

// Create is the other half of idempotency: the name collides at the apiserver and
// AlreadyExists is swallowed. Reports whether it created the Job, so a caller can
// say which of the two happened.
func Create(ctx context.Context, c kubernetes.Interface, j *batchv1.Job, dryRun bool) (created bool, err error) {
	opts := metav1.CreateOptions{}
	if dryRun {
		// Server dry-run, so the apiserver's own validation answers "would this
		// name be accepted" — which is the question the prototype asked k3s and
		// the one no amount of reasoning about DNS-1123 settles.
		opts.DryRun = []string{metav1.DryRunAll}
	}
	_, err = c.BatchV1().Jobs(j.Namespace).Create(ctx, j, opts)
	if apierrors.IsAlreadyExists(err) {
		return false, nil
	}
	return err == nil, err
}

// Phase reads the condition off a Job whose creation was swallowed, and exists
// because `exists` alone is not the whole answer. A collided name is a redelivery
// only while the Job is *running*: `ttlSecondsAfterFinished` keeps a finished one
// for a day (ADR 0004), so for that day a re-run of a *failed* issue reports
// `exists`, exits 0, and does nothing — indistinguishable from the redelivery it
// is not. `get` is in the Role for exactly this reading.
//
// Best-effort by design: this decorates a line an operator reads, so an unreadable
// Job is a word rather than a failure.
func Phase(ctx context.Context, c kubernetes.Interface, ns, name string) string {
	j, err := c.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "unreadable"
	}
	if cond := terminal(j); cond != nil {
		return string(cond.Type)
	}
	return "active"
}

// terminal is the one definition of "this run has ended", shared by the line above
// and by the informer's decision to report — so a run the operator is told is
// `active` is exactly a run the informer has not commented on.
//
// The two conditions that mean it, and only those: newer Kubernetes sets interim
// conditions (`SuccessCriteriaMet`, `FailureTarget`) on the way to them, and
// reporting on one of those would comment before the pod's own status has settled.
func terminal(j *batchv1.Job) *batchv1.JobCondition {
	for i, cond := range j.Status.Conditions {
		if cond.Status == corev1.ConditionTrue &&
			(cond.Type == batchv1.JobComplete || cond.Type == batchv1.JobFailed) {
			return &j.Status.Conditions[i]
		}
	}
	return nil
}

func merge(into, from map[string]string) map[string]string {
	if into == nil {
		into = make(map[string]string, len(from))
	}
	maps.Copy(into, from)
	return into
}

// ParseIssue reads `owner/repo#n`, the one spelling the whole system uses.
func ParseIssue(s string) (proxy.Run, error) {
	owner, rest, ok := strings.Cut(s, "/")
	if !ok {
		return proxy.Run{}, fmt.Errorf("%q is not owner/repo#issue", s)
	}
	repo, issue, ok := strings.Cut(rest, "#")
	if !ok || owner == "" || repo == "" || issue == "" {
		return proxy.Run{}, fmt.Errorf("%q is not owner/repo#issue", s)
	}
	if strings.IndexFunc(issue, func(c rune) bool { return c < '0' || c > '9' }) >= 0 {
		return proxy.Run{}, fmt.Errorf("issue %q is not a number", issue)
	}
	// The alphabet is proxy.Cred's contract, not cosmetics: the claim it signs is
	// comma-joined, so a comma in an owner or a repo makes the proxy see five
	// fields and answer 407 for the rest of activeDeadlineSeconds. GitHub allows
	// only these characters, so refusing here turns a run that burns its deadline
	// into a named error on this line.
	for _, f := range []string{owner, repo} {
		if i := strings.IndexFunc(f, func(c rune) bool {
			return !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
				c == '.' || c == '_' || c == '-')
		}); i >= 0 {
			return proxy.Run{}, fmt.Errorf("%q is not a GitHub name: %q is not [A-Za-z0-9._-]", f, f[i:i+1])
		}
	}
	return proxy.Run{Owner: owner, Repo: repo, Issue: issue}, nil
}
