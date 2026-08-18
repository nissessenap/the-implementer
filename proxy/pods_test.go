package proxy

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

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
