package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nissessenap/the-implementer/proxy"
)

// A stub that asserts requests, not a GitHub emulator. The question worth
// answering here is "did the orchestrator decide correctly and call correctly" —
// method, path, and the body it sent. A faithful emulator drifts from GitHub and
// then the suite is testing the emulator.
type fakeIssues struct {
	*httptest.Server
	mu       sync.Mutex
	posted   []string         // the bodies, in order
	calls    []string         // "METHOD path", every request, in order
	tokens   []string         // the Authorization header of every request
	comments []string         // the thread, as ListComments will answer it
	authors  []map[string]any // one per comment, in step with it
}

// seed puts a comment in the thread that the orchestrator did not write.
func (f *fakeIssues) seed(body, authorType string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, body)
	f.authors = append(f.authors, map[string]any{"login": "a-person", "type": authorType})
}

func newFakeIssues(t *testing.T) *fakeIssues {
	f := &fakeIssues{}
	mux := http.NewServeMux()
	// The one path the orchestrator writes to, and the one it reads to answer
	// "did I already report this run".
	mux.HandleFunc("/repos/{owner}/{repo}/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.tokens = append(f.tokens, r.Header.Get("Authorization"))
		if r.PathValue("owner") != "acme" || r.PathValue("repo") != "widgets" || r.PathValue("n") != "5" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			// Oldest first and paginated, which is what this endpoint actually does:
			// `sort` and `direction` belong to the *repository*-level comments
			// endpoint, so a client that asks for newest-first silently gets the
			// oldest page. A fake that returned the whole thread could not see that.
			per, page := 30, 1
			if v, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && v > 0 {
				per = v
			}
			if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
				page = v
			}
			from := (page - 1) * per
			to := min(from+per, len(f.comments))
			out := make([]map[string]any, 0, per)
			for i := from; i < to && i < len(f.comments); i++ {
				out = append(out, map[string]any{"id": i + 1, "body": f.comments[i],
					"user": f.authors[i]})
			}
			if to < len(f.comments) {
				w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=%d&per_page=%d>; rel="next"`,
					f.URL, r.URL.Path, page+1, per))
			}
			_ = json.NewEncoder(w).Encode(out)
		case http.MethodPost:
			var in struct{ Body string }
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &in)
			f.posted = append(f.posted, in.Body)
			f.comments = append(f.comments, in.Body)
			// What the App's own comments look like coming back.
			f.authors = append(f.authors, map[string]any{"login": "implementer[bot]", "type": "Bot"})
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": len(f.comments)})
		}
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeIssues) gh() *GitHub {
	return &GitHub{BaseURL: f.URL, Token: func(context.Context, proxy.Run) (string, error) {
		return "t0k", nil
	}}
}

func (f *fakeIssues) snapshot() (posted, calls []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posted...), append([]string(nil), f.calls...)
}

// job is a run's Job as the builder left it: the label the informer lists on, and
// run identity in the annotations.
func job(cond batchv1.JobConditionType, reason, msg string) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "acme-widgets-5-1a2b3c4d", Namespace: "ns",
		Labels: map[string]string{"app": "implementer"},
		Annotations: map[string]string{
			proxy.AnnOwner: run.Owner, proxy.AnnRepo: run.Repo,
			proxy.AnnIssue: run.Issue, proxy.AnnRunUID: run.UID,
		},
	}}
	if cond != "" {
		j.Status.Conditions = []batchv1.JobCondition{
			{Type: cond, Status: corev1.ConditionTrue, Reason: reason, Message: msg},
		}
	}
	return j
}

func pod(phase corev1.PodPhase, cs corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "acme-widgets-5-1a2b3c4d-abcde", Namespace: "ns",
			Labels: map[string]string{"app": "implementer", "job-name": "acme-widgets-5-1a2b3c4d"},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}},
		Status: corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{cs}},
	}
}

func blobPod(status string) *corev1.Pod {
	blob := fmt.Sprintf(`{"status":%q,"branch":"implementer/issue-5","commits":3,`+
		`"cost_usd":2.13,"elapsed_s":452,"pr_title":"feat: helpers","message":"pushed implementer/issue-5",`+
		`"phases":[{"name":"implement","status":"completed","summary":"wrote it"},`+
		`{"name":"review","status":"error","summary":"API error 529"}]}`, status)
	return pod(corev1.PodSucceeded, corev1.ContainerStatus{
		Name: "agent", ImageID: digest,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 0, Reason: "Completed", Message: blob,
		}},
	})
}

func reporter(t *testing.T, f *fakeIssues, objs ...runtime.Object) *Reporter {
	t.Helper()
	return &Reporter{Kube: fake.NewSimpleClientset(objs...), NS: "ns", GH: f.gh()}
}

// A completed run posts one comment built from the blob, and says so at the one
// method and path GitHub documents.
func TestReconcileCommentsFromTheBlob(t *testing.T) {
	f := newFakeIssues(t)
	r := reporter(t, f, job(batchv1.JobComplete, "", ""), blobPod("completed_unreviewed"))

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	posted, calls := f.snapshot()
	if len(posted) != 1 {
		t.Fatalf("%d comments posted, want 1: %q", len(posted), posted)
	}
	want := "POST /repos/acme/widgets/issues/5/comments"
	if calls[len(calls)-1] != want {
		t.Errorf("the write was %q, want %q", calls[len(calls)-1], want)
	}
	if f.tokens[len(f.tokens)-1] != "Bearer t0k" {
		t.Errorf("the write carried %q", f.tokens[len(f.tokens)-1])
	}
	for _, s := range []string{Marker(run.UID), "completed_unreviewed", "review", "API error 529", digest} {
		if !strings.Contains(posted[0], s) {
			t.Errorf("the body does not name %q:\n%s", s, posted[0])
		}
	}

	// Redelivery, a second label and an informer resync all land here with the *same*
	// run uid — the Job name is per-issue and a swallowed AlreadyExists keeps the
	// existing Job's annotations — so none of them may produce a second comment.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posted, _ := f.snapshot(); len(posted) != 1 {
		t.Fatalf("%d comments after two reconciles, want 1", len(posted))
	}
}

// THE case this component exists for: the pod was killed and wrote nothing, so the
// Kubernetes-level reason is all the information there is.
func TestReconcileReportsAPodThatWroteNothing(t *testing.T) {
	f := newFakeIssues(t)
	r := reporter(t, f, job(batchv1.JobFailed, "BackoffLimitExceeded", "reached the backoff limit"),
		pod(corev1.PodFailed, corev1.ContainerStatus{
			Name: "agent", ImageID: digest,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 137, Reason: "OOMKilled",
			}},
		}))

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	posted, _ := f.snapshot()
	if len(posted) != 1 {
		t.Fatalf("%d comments posted, want 1", len(posted))
	}
	for _, s := range []string{"OOMKilled", "137", digest} {
		if !strings.Contains(posted[0], s) {
			t.Errorf("the body does not name %q:\n%s", s, posted[0])
		}
	}
}

// activeDeadlineSeconds: the Job controller deletes the active pods, so the Job's
// own condition is the only record left. Without this the deadline is the one
// ending that goes unreported.
func TestReconcileReportsAJobWhosePodIsGone(t *testing.T) {
	f := newFakeIssues(t)
	r := reporter(t, f, job(batchv1.JobFailed, "DeadlineExceeded", "Job was active longer than the specified deadline"))

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	posted, _ := f.snapshot()
	if len(posted) != 1 {
		t.Fatalf("%d comments posted, want 1", len(posted))
	}
	if !strings.Contains(posted[0], "DeadlineExceeded") {
		t.Errorf("the body does not name the deadline:\n%s", posted[0])
	}
}

// A restart is a relist, and the exactly-once record is the comment itself: a new
// process with no memory must find its own marker in the thread rather than
// commenting twice.
func TestARestartDoesNotCommentTwice(t *testing.T) {
	f := newFakeIssues(t)
	objs := []runtime.Object{job(batchv1.JobComplete, "", ""), blobPod("completed")}
	if err := reporter(t, f, objs...).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A different Reporter: empty in-memory set, same thread.
	if err := reporter(t, f, objs...).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posted, _ := f.snapshot(); len(posted) != 1 {
		t.Fatalf("%d comments after a restart, want 1: %q", len(posted), posted)
	}
}

// Nothing is said about a run that has not ended, and nothing at all about a Job
// that is not a run of ours.
func TestReconcileSaysNothingAboutAnActiveRun(t *testing.T) {
	f := newFakeIssues(t)
	foreign := job(batchv1.JobComplete, "", "")
	foreign.Name, foreign.Annotations = "somebody-elses", nil
	r := reporter(t, f, job("", "", ""), foreign)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posted, calls := f.snapshot(); len(posted) != 0 || len(calls) != 0 {
		t.Fatalf("an active run produced %d comments and %d calls", len(posted), len(calls))
	}
}

// The marker is in the comment, so it is not a secret. Anyone who can comment on the
// issue could post one — and a run whose report is suppressed by a stranger is
// exactly the silence this component exists to prevent, so only the App's own
// comments count as a report.
func TestAStrangersMarkerDoesNotSilenceTheRun(t *testing.T) {
	f := newFakeIssues(t)
	f.seed("nice try "+Marker(run.UID), "User")
	r := reporter(t, f, job(batchv1.JobComplete, "", ""), blobPod("completed"))

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posted, _ := f.snapshot(); len(posted) != 1 {
		t.Fatalf("%d comments posted, want 1 — a stranger's marker silenced the run", len(posted))
	}
}

// And the thread is read to the end. This endpoint returns oldest first and ignores
// `sort`/`direction`, so a single page of it is the *oldest* comments: on a longer
// thread than one page, a report that only read page one would comment again on
// every resync forever.
func TestTheWholeThreadIsRead(t *testing.T) {
	f := newFakeIssues(t)
	// A thread longer than one page *before* the run ends, so our own comment lands
	// past the first page — which is where a single-page read stops looking.
	for i := range 250 {
		f.seed(fmt.Sprintf("unrelated chatter %d", i), "User")
	}
	if err := reporter(t, f, job(batchv1.JobComplete, "", ""), blobPod("completed")).
		Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A fresh Reporter: no memory, so the thread is the only record there is.
	if err := reporter(t, f, job(batchv1.JobComplete, "", ""), blobPod("completed")).
		Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posted, _ := f.snapshot(); len(posted) != 1 {
		t.Fatalf("%d comments, want 1 — the marker was past the first page", len(posted))
	}
}
