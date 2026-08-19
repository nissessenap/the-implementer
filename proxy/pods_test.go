package proxy

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// finished is a pod whose containers have exited. Its Pod object keeps
// status.podIP until it is deleted, which is the whole point of the case below.
func finished(p *corev1.Pod) *corev1.Pod {
	p.Status.Phase = corev1.PodSucceeded
	return p
}

func pod(name, ip string, ann map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "runs", Annotations: ann},
		Status:     corev1.PodStatus{PodIP: ip},
	}
}

// The lookup the mint scope hangs off: an IP resolves to exactly one pod, and its
// identity comes off the Pod's annotations — ADR 0004 writes them there for this
// reader, so no Job hop and no RBAC beyond pods.
func TestPodsRun(t *testing.T) {
	run := map[string]string{AnnOwner: "acme", AnnRepo: "widgets", AnnIssue: "5", AnnRunUID: "run-1"}
	partial := map[string]string{AnnOwner: "acme", AnnRepo: "widgets", AnnIssue: "5"}
	cs := fake.NewClientset(
		pod("sandbox", "10.0.0.1", run),
		pod("bystander", "10.0.0.2", nil), // any other workload in the namespace
		pod("half-annotated", "10.0.0.3", partial),
		pod("pending", "", run), // no IP yet: must not index as ""
		// The run that held 10.0.0.1 before this one. Retained on purpose — pods
		// stick around so their logs stay readable — and still carrying the IP the
		// CNI has since handed to the sandbox above. Indexing it would make every
		// lookup of that IP ambiguous, and lock the live run out for as long as the
		// dead one is kept.
		finished(pod("previous-run", "10.0.0.1",
			map[string]string{AnnOwner: "acme", AnnRepo: "widgets", AnnIssue: "9", AnnRunUID: "run-0"})),
	)
	pods, err := WatchPods(t.Context(), cs, "runs")
	if err != nil {
		t.Fatal(err)
	}

	pods.wait = 0 // no cache to wait for: the fixture is already in the cache

	got, err := pods.Run(t.Context(), "10.0.0.1")
	if err != nil {
		t.Fatalf("the run's own IP: %v", err)
	}
	if want := (Run{"acme", "widgets", "5", "run-1"}); got != want {
		t.Errorf("Run = %+v, want %+v", got, want)
	}
	for _, ip := range []string{"10.0.0.2", "10.0.0.3", "10.0.0.4", ""} {
		if _, err := pods.Run(t.Context(), ip); err == nil {
			t.Errorf("Run(%q) resolved, want a refusal", ip)
		}
	}
}

// The wait, which is the path every run's first CONNECT actually takes: the
// kubelet publishes podIP on its own sync cadence, so a sandbox beats its own
// status update — measured at ~4 s on kind. Zeroed in the test above, so it is
// tested here or nowhere.
func TestPodsRunWaitsForTheCache(t *testing.T) {
	cs := fake.NewClientset()
	pods, err := WatchPods(t.Context(), cs, "runs")
	if err != nil {
		t.Fatal(err)
	}
	// Not resolveWait: a test should not sit out the real one, only prove the loop
	// outlasts a pod that is late.
	pods.wait = 5 * time.Second

	run := map[string]string{AnnOwner: "acme", AnnRepo: "widgets", AnnIssue: "5", AnnRunUID: "run-1"}
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = cs.CoreV1().Pods("runs").Create(
			context.Background(), pod("late", "10.0.0.7", run), metav1.CreateOptions{})
	}()

	got, err := pods.Run(t.Context(), "10.0.0.7")
	if err != nil {
		t.Fatalf("a pod that appeared during the wait: %v", err)
	}
	if want := (Run{"acme", "widgets", "5", "run-1"}); got != want {
		t.Errorf("Run = %+v, want %+v", got, want)
	}

	// And a caller that hangs up stops costing us immediately: the wait is for a
	// run's first request, not a tarpit we hold a goroutine in.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	if _, err := pods.Run(ctx, "10.0.0.8"); err == nil {
		t.Error("a cancelled caller resolved")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("a cancelled caller took %v, want an immediate refusal", d)
	}
}
