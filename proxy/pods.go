package proxy

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const byPodIP = "byPodIP"

// Pods resolves a source address to the run whose pod holds it, off a watch of
// one namespace.
//
// An informer index rather than a `status.podIP` fieldSelector: podIP is a valid
// selector but only spec.nodeName is indexed, so a fieldSelector is a full
// apiserver list-and-filter — per request, on the path every CONNECT waits on.
//
// RBAC is pods get,list,watch in this namespace and nothing else. Identity is on
// the Pod (ADR 0004 writes it there precisely for this reader), so there is no
// Job hop and no Secret access to grant.
type Pods struct {
	idx cache.Indexer

	// How long to wait for the cache to catch up with a pod IP it has not seen.
	// Not optional: a sandbox's very first request races the status update that
	// publishes its own IP, so without this the first CONNECT of every run is
	// refused. A field so a test can zero it.
	wait time.Duration
}

// Long enough for the podIP status update to arrive, short enough that a pod we
// will never resolve is refused rather than parked. Measured rather than guessed:
// on kind, a fixture's first CONNECT beat its own pod's status update by ~4 s —
// the kubelet publishes podIP on its own sync cadence, not before the container
// starts. So this is the first request of every run, not an edge case, and a
// caller that is really unknown pays the wait only until its failures are
// rate-limited.
const resolveWait = 10 * time.Second

// WatchPods starts the informer and blocks until its cache has synced — a proxy
// serving requests off an empty cache would refuse every run it cannot yet see.
func WatchPods(ctx context.Context, c kubernetes.Interface, ns string) (*Pods, error) {
	f := informers.NewSharedInformerFactoryWithOptions(c, 10*time.Minute, informers.WithNamespace(ns))
	inf := f.Core().V1().Pods().Informer()
	if err := inf.AddIndexers(cache.Indexers{byPodIP: func(o any) ([]string, error) {
		p, ok := o.(*corev1.Pod)
		if !ok || p.Status.PodIP == "" || terminated(p) {
			return nil, nil
		}
		return []string{p.Status.PodIP}, nil
	}}); err != nil {
		return nil, err
	}
	f.Start(ctx.Done())
	// Bounded, and separately from ctx — which is the informer's whole lifetime.
	// An apiserver we cannot list pods from is a proxy that would refuse every
	// run, and saying so at boot beats a readiness probe that never passes.
	sync, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for t, ok := range f.WaitForCacheSync(sync.Done()) {
		if !ok {
			return nil, fmt.Errorf("pod informer cache never synced (%v)", t)
		}
	}
	return &Pods{idx: inf.GetIndexer(), wait: resolveWait}, nil
}

// terminated is "this pod no longer holds its IP". The CNI releases the address
// when the sandbox is torn down, but the Pod object keeps status.podIP until it is
// deleted — and finished run pods are retained on purpose (proto/job.yaml keeps
// them a day, so pods/log stays readable). Without this a recycled IP indexes two
// pods, p.pod refuses the ambiguity, and the *new* run is locked out for as long
// as the old one is kept.
//
// Phase, and only the terminal ones. Not DeletionTimestamp: that is set when
// graceful termination *begins*, while the containers still run and the run is
// still entitled to its egress. Not Pending either — the kubelet publishes podIP
// once the sandbox network is up, before any container starts, which is exactly
// the race resolveWait waits out.
func terminated(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// Run reads run identity off the pod holding ip. A pod that is not a run — any
// other workload that reached the ClusterIP — carries none of these annotations
// and is refused here.
func (p *Pods) Run(ctx context.Context, ip string) (Run, error) {
	pod, err := p.pod(ip)
	for deadline := time.Now().Add(p.wait); err != nil && time.Now().Before(deadline); {
		// ctx, so a caller we will never resolve stops costing us the moment it
		// hangs up — the wait is for a run's first request, not a tarpit.
		select {
		case <-ctx.Done():
			return Run{}, err
		case <-time.After(50 * time.Millisecond):
		}
		pod, err = p.pod(ip)
	}
	if err != nil {
		return Run{}, err
	}
	a := pod.Annotations
	r := Run{Owner: a[AnnOwner], Repo: a[AnnRepo], Issue: a[AnnIssue], UID: a[AnnRunUID]}
	if !r.complete() {
		return Run{}, fmt.Errorf("pod %s carries no run identity", pod.Name)
	}
	return r, nil
}

// pod is the index lookup itself. Two pods on one IP means the cache is
// mid-recycle and we cannot say which one is calling; refusing is the only safe
// answer, and the run secret is what makes that a rare failure rather than a mint
// for the wrong repository.
func (p *Pods) pod(ip string) (*corev1.Pod, error) {
	objs, err := p.idx.ByIndex(byPodIP, ip)
	if err != nil {
		return nil, err
	}
	if len(objs) != 1 {
		return nil, fmt.Errorf("%d pods with IP %s", len(objs), ip)
	}
	// A plain assertion: the index function above returns no key for anything
	// that is not a *Pod, so nothing else can be under this one.
	return objs[0].(*corev1.Pod), nil
}
