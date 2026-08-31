package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v88/github"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/nissessenap/the-implementer/proxy"
	"github.com/nissessenap/the-implementer/sandbox"
)

// runSelector is the builder's own label, spelled as a selector — from the same two
// constants job.go writes it from, because two copies of "app=implementer" is
// exactly the drift runOf's comment claims this package does not have. It is the
// whole of the list scope, and what keeps every watch here off other workloads in
// the namespace.
const runSelector = runLabelKey + "=" + runLabelValue

// resync is the informer's periodic relist, and it is the safety net under every
// event this component could miss: a resync redelivers an Update for everything in
// the cache, so a run whose terminal event was dropped is reported at worst one
// resync late rather than never.
const resync = 10 * time.Minute

// GitHub is the orchestrator's own GitHub, for the orchestrator's own calls only —
// `contents: read`, `pull_requests: write`, `issues: write`. It does not mint the
// sandbox's token: ADR 0005 moved that to the credential proxy, and this component
// creates exactly one object per run.
type GitHub struct {
	// BaseURL is the seam, and it is the same seam on both clients: go-github
	// takes a base URL and ghait's WithURLs retargets the mint path. The e2e
	// points it at an in-cluster mock, so the comment path is asserted — method,
	// path and body — with no write to any real repository. Empty is api.github.com.
	BaseURL string

	// Token is what the orchestrator may do for this run, and it is deliberately
	// the credential proxy's own mint path (proxy.MintedGitHub): one external
	// signer, one build-tag choice of provider, and no App private key in either
	// process. Handing it the Run is what scopes the token to the run's own
	// repository rather than to every repository the App is installed on.
	Token func(context.Context, proxy.Run) (string, error)
}

// client is one authenticated client for one run.
func (g *GitHub) client(ctx context.Context, r proxy.Run) (*github.Client, error) {
	tok, err := g.Token(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("no GitHub token for %s: %w", r, err)
	}
	opts := []github.ClientOptionsFunc{github.WithAuthToken(tok)}
	if g.BaseURL != "" {
		// WithURLs and not WithEnterpriseURLs: the latter appends `/api/v3/`, which
		// is right for GitHub Enterprise Server and wrong for a mock. ghait's
		// WithURLs has the same "used as given" contract, so one environment
		// variable means one thing in both clients.
		opts = append(opts, github.WithURLs(&g.BaseURL, &g.BaseURL))
	}
	return github.NewClient(opts...)
}

// Report posts the run's one issue comment, unless the thread already carries it.
// Reports whether it wrote.
//
// The thread *is* the exactly-once record. There is no database (ADR 0004) and the
// orchestrator's RBAC is read-only on Pods and Jobs, so it cannot mark the run as
// reported in Kubernetes either — which leaves GitHub, where the other half of
// this system's state already lives. A restart mid-run, a redelivery and a second
// label all end here and find the marker.
func (g *GitHub) Report(ctx context.Context, o Outcome) (bool, error) {
	n, err := strconv.Atoi(o.Run.Issue)
	if err != nil {
		return false, fmt.Errorf("run %s: issue %q is not a number", o.Run, o.Run.Issue)
	}
	c, err := g.client(ctx, o.Run)
	if err != nil {
		return false, err
	}

	reported, err := g.reported(ctx, c, o.Run, n)
	if err != nil {
		return false, err
	}
	if reported {
		return false, nil
	}

	if _, _, err := c.Issues.CreateComment(ctx, o.Run.Owner, o.Run.Repo, n,
		&github.IssueComment{Body: github.Ptr(o.Comment())}); err != nil {
		return false, fmt.Errorf("commenting on %s: %w", o.Run, err)
	}
	return true, nil
}

// reported is "does this thread already carry this run's marker".
//
// Every page of it, and that is not thoroughness. `sort` and `direction` are
// parameters of the *repository*-level comments endpoint and not of this one, which
// returns oldest first and ignores them — so asking for one page of 100 reads the
// *oldest* hundred. On an issue with a longer thread than that the marker would
// never be found and every resync would comment again, which is the one failure
// this whole mechanism exists to prevent.
//
// Only comments the App itself wrote count. The marker is not a secret — it is in
// the comment — so without this, anyone who can comment on the issue can silence a
// run's report by posting the marker first, and a silenced report is precisely what
// this component exists to make impossible. `Bot` rather than a specific login,
// because the App's slug is the operator's and nothing here knows it; the run uid
// is what makes the marker hard to *guess*, and this is what stops it being worth
// copying.
func (g *GitHub) reported(ctx context.Context, c *github.Client, run proxy.Run, issue int) (bool, error) {
	marker := Marker(run.UID)
	opt := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		page, resp, err := c.Issues.ListComments(ctx, run.Owner, run.Repo, issue, opt)
		if err != nil {
			return false, fmt.Errorf("reading the thread of %s: %w", run, err)
		}
		for _, cm := range page {
			if cm.GetUser().GetType() == "Bot" && strings.Contains(cm.GetBody(), marker) {
				return true, nil
			}
		}
		if resp.NextPage == 0 {
			return false, nil
		}
		opt.Page = resp.NextPage
	}
}

// Reporter is ADR 0004's informer half: it watches the runs in one namespace and
// gives every ending an issue comment.
//
// **This is where the argument for it lives**, because the cheap-looking version of
// this system is the one that goes silent, and the case for deleting this component
// will look reasonable to whoever makes it.
//
// It exists because of the *silent* death rather than for determinism. `OOMKilled`,
// `activeDeadlineSeconds`, eviction and `ImagePullBackOff` all end a run with no
// in-pod code running at all — no trap, no phase script, nothing on
// /dev/termination-log — so without something watching from outside, the issue sits
// there labelled `ready-for-agent` and nobody is ever told the run happened. #31
// measured a transient 529 burning a run, so that is routine rather than
// pathological. Do not move this into the sandbox: a pod cannot report its own
// death, which is the same argument ADR 0004 uses against the in-pod PR builder —
// moving the reporting inside the sandbox moves the failure path into the thing
// that fails.
type Reporter struct {
	Kube kubernetes.Interface
	NS   string
	GH   *GitHub

	// ponytail: one lock over every run, held across the GitHub round trip, so two
	// runs ending at once are reported one after the other. Reports are two API
	// calls at the end of a multi-minute run, so the contention is theoretical —
	// and the lock is what makes "exactly once" hold when the Pod handler and the
	// Job handler both fire for the same run. Per-run locks if that stops being
	// true.
	//
	// It is a *process* lock, so this assumes one replica: the read and the write
	// below are not one atomic operation, and two orchestrators would both find no
	// marker and both comment. There is no Deployment yet, and the webhook ticket
	// that adds one is where controller-runtime's leader election goes — which ADR
	// 0004 already counts on ("no leader-election story beyond what
	// controller-runtime gives for free"). Do not scale this to two replicas before
	// then.
	mu sync.Mutex
	// done is the fast path only, and never the correctness argument: it keeps a
	// resync from asking GitHub about runs already reported. The marker in the
	// thread is what survives a restart, which is why nothing here is persisted.
	//
	// ponytail: never swept, so it is one 8-byte run uid per run this process ever
	// sees — bounded by the process's lifetime, and the Jobs it was reading are
	// collected after a day anyway. A sweep when a single orchestrator outlives
	// enough runs for that to be a number.
	done map[string]struct{}
}

// Reconcile is restart-is-a-relist: list the runs in the namespace and report
// every one that has ended and has not been reported. The ordinary controller
// loop, not recovery code — nothing is held that cannot be rebuilt from this.
func (r *Reporter) Reconcile(ctx context.Context) error {
	jobs, err := r.Kube.BatchV1().Jobs(r.NS).List(ctx, metav1.ListOptions{LabelSelector: runSelector})
	if err != nil {
		return fmt.Errorf("listing runs in %s: %w", r.NS, err)
	}
	// Joined rather than returned at the first failure: one repository the App
	// cannot see must not stop every other run being reported.
	var errs []error
	for i := range jobs.Items {
		errs = append(errs, r.report(ctx, &jobs.Items[i]))
	}
	return errors.Join(errs...)
}

// Watch is Reconcile, continuously.
//
// Two informers, and the Job one is not redundant: the *result* is pod-level —
// the blob is the container's terminated message, the resolved digest is its
// imageID, the transcript is `pods/log` — which is why this watches Pods at all.
// But when `activeDeadlineSeconds` expires the Job controller **deletes** the
// active pods, so the deadline is the one ending that produces no terminal pod and
// no further pod event. The Job's condition is the only record left of it, and
// that is the ending a human has no other way to learn about.
//
// Both handlers funnel into one place keyed by the Job, so there is a single
// decision path whichever object woke it.
func (r *Reporter) Watch(ctx context.Context) error {
	f := informers.NewSharedInformerFactoryWithOptions(r.Kube, resync,
		informers.WithNamespace(r.NS),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = runSelector }))

	pods, jobs := f.Core().V1().Pods().Informer(), f.Batch().V1().Jobs().Informer()
	if _, err := pods.AddEventHandler(handler(func(o any) {
		if p, ok := o.(*corev1.Pod); ok {
			r.reportJob(ctx, p.Labels["job-name"])
		}
	})); err != nil {
		return err
	}
	if _, err := jobs.AddEventHandler(handler(func(o any) {
		if j, ok := o.(*batchv1.Job); ok {
			r.reportJob(ctx, j.Name)
		}
	})); err != nil {
		return err
	}

	f.Start(ctx.Done())
	// Bounded, and separately from ctx — which is the informer's whole lifetime. An
	// apiserver we cannot list from is an orchestrator that reports nothing, and
	// saying so at boot beats a readiness probe that never passes.
	sync, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for t, ok := range f.WaitForCacheSync(sync.Done()) {
		if !ok {
			return fmt.Errorf("informer cache never synced (%v)", t)
		}
	}
	log.Printf("informer: watching runs in %s", r.NS)
	// The initial list arrives as Add events, so the relist above *is* the
	// reconcile: a run that finished while this process was down is reported here.
	<-ctx.Done()
	return nil
}

// handler is Add and Update, and deliberately not Delete: a pod deleted before its
// Job reached a condition has nothing to report yet, and the Job event that follows
// is the one that does.
func handler(f func(any)) cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    f,
		UpdateFunc: func(_, o any) { f(o) },
	}
}

func (r *Reporter) reportJob(ctx context.Context, name string) {
	if name == "" {
		return
	}
	j, err := r.Kube.BatchV1().Jobs(r.NS).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		// Not fatal and not retried here: the resync brings it round again, and a
		// Job that is genuinely gone has nothing left to report.
		log.Printf("informer: reading job %s: %v", name, err)
		return
	}
	if err := r.report(ctx, j); err != nil {
		log.Printf("informer: %v", err)
	}
}

// report is the whole decision: is this a run of ours, has it ended, was it
// already reported, and what does it say.
func (r *Reporter) report(ctx context.Context, j *batchv1.Job) error {
	run := runOf(j.Annotations)
	// Anything in this namespace that is not one of our runs. The label narrowed
	// the list; the annotations are what actually identify a run.
	if !run.Complete() || terminal(j) == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.done[run.UID]; ok {
		return nil
	}

	o := Outcome{Run: run}
	pod, err := r.podOf(ctx, j.Name)
	if err != nil {
		return fmt.Errorf("run %s: reading the pod of %s: %w", run, j.Name, err)
	}
	o.read(j, pod)

	posted, err := r.GH.Report(ctx, o)
	if err != nil {
		// Not marked done, so the next resync tries again. A transient GitHub
		// failure must not be the thing that makes a run silent.
		return err
	}
	if r.done == nil {
		r.done = map[string]struct{}{}
	}
	r.done[run.UID] = struct{}{}
	if posted {
		log.Printf("informer: commented on %s (run %s)", run, run.UID)
	}
	return nil
}

// podOf finds the run's pod. `backoffLimit: 0` means there is at most one, and nil
// is a legitimate answer: the Job controller deletes the pod when the deadline
// expires, which is exactly the ending the Job's own condition has to cover.
func (r *Reporter) podOf(ctx context.Context, job string) (*corev1.Pod, error) {
	pods, err := r.Kube.CoreV1().Pods(r.NS).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + job,
	})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	return &pods.Items[0], nil
}

// runOf reads run identity off the annotations the Job builder writes. The same
// four constants the credential proxy reads off the Pod, so there is one set of
// strings in the system rather than two that can drift.
func runOf(a map[string]string) proxy.Run {
	return proxy.Run{Owner: a[proxy.AnnOwner], Repo: a[proxy.AnnRepo],
		Issue: a[proxy.AnnIssue], UID: a[proxy.AnnRunUID]}
}

// read fills in the half of the Outcome that comes off Kubernetes: the blob if the
// run plan wrote one, and otherwise the reason it did not.
//
// Here rather than beside the type it fills, so comment.go stays free of Kubernetes
// types entirely — which is what lets the body a human reads be built and asserted
// without a cluster, a Pod or a fake client anywhere near it.
func (o *Outcome) read(j *batchv1.Job, p *corev1.Pod) {
	cs := agent(p)
	if cs != nil {
		// ADR 0001 requires the resolved digest, and it is the one field worth
		// having even on the paths where nothing else exists.
		o.Image = cs.ImageID
	}
	if cs != nil && cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
		var res sandbox.Result
		// Status, not just a successful decode: the kubelet truncates the
		// termination message blind at 4096 bytes, and `{}` unmarshals happily.
		// phase.sh bounds every field so this should not happen — if it does, the
		// message is reported as text rather than silently read as an empty run.
		if err := json.Unmarshal([]byte(cs.State.Terminated.Message), &res); err == nil && res.Status != "" {
			o.Result = &res
			return
		}
	}
	o.Reason = why(j, p)
}

// why is the Kubernetes-level ending, most specific part first. It is all the
// information that exists about a run that wrote nothing, so it says everything it
// has rather than picking one field.
func why(j *batchv1.Job, p *corev1.Pod) string {
	cs := agent(p)
	var parts []string
	switch {
	case p == nil:
		parts = append(parts, "the pod is gone — the Job controller deletes it when "+
			"activeDeadlineSeconds expires")
	case cs != nil && cs.State.Terminated != nil:
		t := cs.State.Terminated
		reason := t.Reason
		if reason == "" {
			reason = "the container exited"
		}
		parts = append(parts, fmt.Sprintf("%s (exit code %d)", reason, t.ExitCode))
		if t.Message != "" {
			// It wrote *something* and it was not the blob. Reported rather than
			// dropped: a malformed result is a bug worth seeing, not a silent run.
			parts = append(parts, "and wrote something that is not a result blob: "+trunc(t.Message))
		}
	case cs != nil && cs.State.Waiting != nil:
		parts = append(parts, fmt.Sprintf("the container never started: %s: %s",
			cs.State.Waiting.Reason, trunc(cs.State.Waiting.Message)))
	}
	if p != nil {
		// Where an eviction says so, and the phase is the fallback for a pod that
		// ended with no container status at all.
		if p.Status.Reason != "" {
			parts = append(parts, fmt.Sprintf("pod %s: %s", p.Status.Reason, trunc(p.Status.Message)))
		} else if len(parts) == 0 {
			parts = append(parts, "pod "+string(p.Status.Phase))
		}
	}
	// The Job's own last word, last: with `backoffLimit: 0` it is usually
	// `BackoffLimitExceeded`, which says less than the pod does — and it is the only
	// thing there is when the pod has been deleted.
	if c := terminal(j); c != nil && c.Type == batchv1.JobFailed {
		parts = append(parts, fmt.Sprintf("Job %s: %s", c.Reason, trunc(c.Message)))
	}
	if len(parts) == 0 {
		return "the run ended and left no reason anywhere"
	}
	return strings.Join(parts, " — ")
}

// agent is the run's container status. The Job template has exactly one container,
// so this is the first one — matched by name where there is one to match, because a
// BYO image's PodSpec is the operator's and the name is theirs to choose.
func agent(p *corev1.Pod) *corev1.ContainerStatus {
	if p == nil || len(p.Status.ContainerStatuses) == 0 {
		return nil
	}
	if len(p.Spec.Containers) > 0 {
		for i, cs := range p.Status.ContainerStatuses {
			if cs.Name == p.Spec.Containers[0].Name {
				return &p.Status.ContainerStatuses[i]
			}
		}
	}
	return &p.Status.ContainerStatuses[0]
}

// trunc keeps a kubelet-written message from being most of the comment. Codepoints,
// so what comes out is still valid UTF-8.
func trunc(s string) string {
	if r := []rune(s); len(r) > 300 {
		return string(r[:300]) + "…"
	}
	return s
}
