package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
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
// constants job.go writes it from, because two copies of "app=implementer" is the
// same drift proxy.RunFromAnnotations exists to keep out of the annotations. It is
// the whole of the list scope, and what keeps every watch here off other workloads
// in the namespace.
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
			// HasPrefix and not Contains: Comment() writes the marker as the first
			// line, and a bot that mirrors or quotes the thread reproduces it behind
			// a blockquote or an indent. Counting that as "already reported" is the
			// silence this check exists to prevent, one step removed.
			if cm.GetUser().GetType() == "Bot" && strings.HasPrefix(cm.GetBody(), marker) {
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

	// done is the fast path only, and never the correctness argument: it keeps a
	// resync from minting a token and reading a thread for runs it already reported.
	// The marker in the thread is what survives a restart, which is why nothing
	// here is persisted.
	//
	// ponytail: unguarded, because there is one informer and its handlers run on one
	// goroutine, and `watch -once` is Reconcile with no informer at all. A second
	// informer, a worker pool or a second replica needs a lock here — and note that
	// a lock would only ever have been a *process* one, so it was never what made
	// "exactly once" hold across processes either. The thread is. ADR 0004 puts
	// leader election on the webhook ticket that first adds a Deployment; do not run
	// two replicas before then.
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
// Jobs, and only Jobs, though the *result* is pod-level — the blob is the
// container's terminated message, the resolved digest is its imageID, the
// transcript is `pods/log`, all of which report reads off the pod directly
// (podOf). The Job's terminal condition is the *trigger* because it is the one
// signal that covers every ending: the Job controller defers it until the pods are
// terminal (1.31), so the blob is already readable by the time it arrives — and
// when `activeDeadlineSeconds` expires the controller **deletes** the pod, leaving
// the condition as the only record of the ending a human has no other way to learn
// about.
//
// ponytail: a Pod informer as well would fire either before the condition, where
// report has nothing to say, or after it, where the Job event has already said it.
// Add one when there is an ending the Job's condition does not follow.
func (r *Reporter) Watch(ctx context.Context) error {
	f := informers.NewSharedInformerFactoryWithOptions(r.Kube, resync,
		informers.WithNamespace(r.NS),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = runSelector }))

	// The cache's own object rather than a re-Get of it: report only reads the Job.
	report := func(o any) {
		j, ok := o.(*batchv1.Job)
		if !ok {
			return
		}
		if err := r.report(ctx, j); err != nil {
			log.Printf("informer: %v", err)
		}
	}
	// Add and Update, and deliberately not Delete: a Job deleted before it reached
	// a condition has nothing left to report, and one deleted after it was already
	// reported on.
	if _, err := f.Batch().V1().Jobs().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    report,
		UpdateFunc: func(_, o any) { report(o) },
	}); err != nil {
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
			if ctx.Err() != nil {
				// Cancelled rather than timed out: a rollout during the first sync
				// is a clean stop, and log.Fatal on it is a crash in every rollout.
				return nil
			}
			return fmt.Errorf("informer cache never synced (%v)", t)
		}
	}
	log.Printf("informer: watching runs in %s", r.NS)
	// The initial list arrives as Add events, so the relist above *is* the
	// reconcile: a run that finished while this process was down is reported here.
	<-ctx.Done()
	return nil
}

// report is the whole decision: is this a run of ours, has it ended, was it
// already reported, and what does it say.
func (r *Reporter) report(ctx context.Context, j *batchv1.Job) error {
	// The same reader the credential proxy uses on the Pod, so there is one
	// spelling of the four annotations in the system rather than two that can drift.
	run := proxy.RunFromAnnotations(j.Annotations)
	// Anything in this namespace that is not one of our runs. The label narrowed
	// the list; the annotations are what actually identify a run.
	if !run.Complete() || terminal(j) == nil {
		return nil
	}

	if _, ok := r.done[run.UID]; ok {
		return nil
	}

	o := Outcome{Run: run}
	pod, err := r.podOf(ctx, run)
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

// podOf finds the run's pod, and it is the *run* it matches on rather than the Job
// name: JobName is per-issue on purpose (jobname.go), so a re-run of the same issue
// reuses the name — and between the old Job's deletion and its pod's collection,
// `job-name=` answers with the previous run's pod. Reporting run N-1's branch,
// commits and cost under run N's identity is silent and indistinguishable from a
// correct report, so the annotation the builder writes to the pod template is what
// decides.
//
// nil is a legitimate answer: the Job controller deletes the pod when the deadline
// expires, which is exactly the ending the Job's own condition has to cover.
func (r *Reporter) podOf(ctx context.Context, run proxy.Run) (*corev1.Pod, error) {
	pods, err := r.Kube.CoreV1().Pods(r.NS).List(ctx, metav1.ListOptions{
		LabelSelector: runSelector,
	})
	if err != nil {
		return nil, err
	}
	for i := range pods.Items {
		if pods.Items[i].Annotations[proxy.AnnRunUID] == run.UID {
			return &pods.Items[i], nil
		}
	}
	return nil, nil
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
		// The fact and not a cause. The expired deadline is the common way to get
		// here and the Job's own condition below names it precisely, but pod GC and
		// a drained node reach this line too — including for a run that succeeded.
		parts = append(parts, "the pod is gone")
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
	//
	// Unconditional, and the type before the reason: `Complete` carries no reason at
	// all, and a run that succeeded and then lost its pod has nothing else anywhere
	// in this comment saying that it succeeded.
	if c := terminal(j); c != nil {
		s := "Job " + string(c.Type)
		if c.Reason != "" {
			s += " " + c.Reason
		}
		if c.Message != "" {
			s += ": " + trunc(c.Message)
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "the run ended and left no reason anywhere"
	}
	return strings.Join(parts, " — ")
}

// agent is the run's container status. The Job template has exactly one container
// (charts/orchestrator/templates/job-template.yaml), so this is that one.
//
// ponytail: index 0 rather than a match on the container's name. A BYO PodSpec that
// adds a second *plain* container wants the name match back — a native sidecar does
// not, because those land in initContainerStatuses.
func agent(p *corev1.Pod) *corev1.ContainerStatus {
	if p == nil || len(p.Status.ContainerStatuses) == 0 {
		return nil
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
