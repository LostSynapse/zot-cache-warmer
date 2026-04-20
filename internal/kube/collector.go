// Package kube talks to the Kubernetes API server, lists every workload kind
// that carries a pod spec, and extracts the complete set of container image
// references visible to the cluster.
//
// Both running Pods and their parent controllers (Deployments, StatefulSets,
// DaemonSets, ReplicaSets, Jobs, CronJobs) are scanned. Scanning controllers
// is required so that suspended CronJobs, scaled-to-zero Deployments, and
// rollback-candidate ReplicaSets have their images warmed before a restart
// or unsuspend triggers a cold pull during an ISP outage.
package kube

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/pager"
	"k8s.io/client-go/util/retry"
)

// Filter constrains which namespaces are scanned. Empty Include means "every
// namespace"; Exclude is applied after Include. Both lists are exact-match
// strings (no globbing).
type Filter struct {
	Include []string
	Exclude []string
}

// allowed reports whether the given namespace passes the filter.
func (f Filter) allowed(ns string) bool {
	if len(f.Include) > 0 {
		included := false
		for _, n := range f.Include {
			if n == ns {
				included = true
				break
			}
		}
		if !included {
			return false
		}
	}
	for _, n := range f.Exclude {
		if n == ns {
			return false
		}
	}
	return true
}

// NewInClusterClient returns a typed Clientset authenticated by the pod's
// projected service-account token. Token rotation is handled transparently by
// client-go: rest.InClusterConfig populates Config.BearerTokenFile, and the
// transport's cachingTokenSource re-reads that file every ~60s. The kubelet
// rotates the token at 80% of its 1-hour TTL, so the pod always sees a valid
// bearer token.
func NewInClusterClient(userAgent string) (*kubernetes.Clientset, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("rest.InClusterConfig: %w", err)
	}

	// Defaults (5 QPS / 10 Burst) serialize the 7-way paginated fan-out.
	// Raising to 20/30 completes a small-cluster scan in seconds while staying
	// well inside the workload-low APF priority level's budget.
	cfg.QPS = 20
	cfg.Burst = 30

	// Per-call deadlines come from context.WithTimeout; this is a ceiling
	// against hung TCP connections / HTTP/2 streams leaking goroutines.
	cfg.Timeout = 30 * time.Second

	// Identify ourselves in apiserver audit logs and flow-control metrics.
	cfg.UserAgent = userAgent

	// Protobuf is smaller and faster than JSON for built-in types.
	cfg.ContentType = "application/vnd.kubernetes.protobuf"

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes.NewForConfig: %w", err)
	}
	return cs, nil
}

// transientBackoff covers typical apiserver/etcd blips and APF throttling.
// Total worst-case sleep ≈ 12.6s plus jitter, which fits inside the per-
// resource context.WithTimeout budget.
var transientBackoff = wait.Backoff{
	Steps:    6,
	Duration: 200 * time.Millisecond,
	Factor:   2.0,
	Jitter:   0.2,
	Cap:      10 * time.Second,
}

// isTransient returns true only for errors that can plausibly succeed on retry.
// Permission (403), validation (422), and not-found (404) are never retried —
// retrying them would only amplify API server load.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case apierrors.IsTooManyRequests(err),
		apierrors.IsServerTimeout(err),
		apierrors.IsTimeout(err),
		apierrors.IsServiceUnavailable(err),
		apierrors.IsInternalError(err):
		return true
	}
	// Context errors must propagate, not retry.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	return false
}

// streamAll issues a paginated, watch-cache-served List and invokes onItem
// once per decoded object. The pager transparently handles continue tokens
// and restarts from scratch if the continuation token expires.
//
// The caller supplies a context-less list function (the closures below all
// capture ctx from the enclosing scope). pager.SimplePageFunc adapts it to
// the current pager.ListPageFunc signature which takes (ctx, opts).
func streamAll(
	ctx context.Context,
	fn func(metav1.ListOptions) (runtime.Object, error),
	onItem func(runtime.Object),
) error {
	return retry.OnError(transientBackoff, isTransient, func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p := pager.New(pager.SimplePageFunc(fn))
		p.PageSize = 500
		return p.EachListItem(ctx,
			// ResourceVersion "0" permits watch-cache serving — cheaper than
			// a quorum etcd read.
			metav1.ListOptions{ResourceVersion: "0"},
			func(obj runtime.Object) error {
				onItem(obj)
				return nil
			})
	})
}

// extractImagesFromPodSpec returns every non-empty image reference in a
// PodSpec: init, main, and ephemeral containers. Order is preserved for
// logging determinism; dedup happens at a higher level.
func extractImagesFromPodSpec(spec *corev1.PodSpec) []string {
	if spec == nil {
		return nil
	}
	out := make([]string, 0,
		len(spec.Containers)+len(spec.InitContainers)+len(spec.EphemeralContainers))

	for i := range spec.InitContainers {
		if img := spec.InitContainers[i].Image; img != "" {
			out = append(out, img)
		}
	}
	for i := range spec.Containers {
		if img := spec.Containers[i].Image; img != "" {
			out = append(out, img)
		}
	}
	for i := range spec.EphemeralContainers {
		if img := spec.EphemeralContainers[i].EphemeralContainerCommon.Image; img != "" {
			out = append(out, img)
		}
	}
	return out
}

// CollectImages enumerates Pods and every workload controller kind across the
// cluster (or the filter-constrained namespace set), extracts image references
// from each PodSpec, and returns the deduplicated set as a slice.
//
// Each List call is independent: a failure on one kind (e.g., Jobs) does not
// prevent the others from completing. The returned errs slice carries the
// per-kind errors for observability; a partial result is still useful.
func CollectImages(
	ctx context.Context,
	cs kubernetes.Interface,
	filter Filter,
) (images []string, errs []error) {

	imageSet := make(map[string]struct{})
	add := func(imgs []string) {
		for _, i := range imgs {
			if i != "" {
				imageSet[i] = struct{}{}
			}
		}
	}

	// Pods — runtime truth for what's actually scheduled.
	if err := streamAll(ctx,
		func(o metav1.ListOptions) (runtime.Object, error) {
			return cs.CoreV1().Pods("").List(ctx, o)
		},
		func(o runtime.Object) {
			p := o.(*corev1.Pod)
			if filter.allowed(p.Namespace) {
				add(extractImagesFromPodSpec(&p.Spec))
			}
		},
	); err != nil {
		errs = append(errs, fmt.Errorf("list pods: %w", err))
	}

	// Deployments — includes scaled-to-zero and paused.
	if err := streamAll(ctx,
		func(o metav1.ListOptions) (runtime.Object, error) {
			return cs.AppsV1().Deployments("").List(ctx, o)
		},
		func(o runtime.Object) {
			d := o.(*appsv1.Deployment)
			if filter.allowed(d.Namespace) {
				add(extractImagesFromPodSpec(&d.Spec.Template.Spec))
			}
		},
	); err != nil {
		errs = append(errs, fmt.Errorf("list deployments: %w", err))
	}

	// StatefulSets — scaled-to-zero StatefulSets still carry image refs.
	if err := streamAll(ctx,
		func(o metav1.ListOptions) (runtime.Object, error) {
			return cs.AppsV1().StatefulSets("").List(ctx, o)
		},
		func(o runtime.Object) {
			s := o.(*appsv1.StatefulSet)
			if filter.allowed(s.Namespace) {
				add(extractImagesFromPodSpec(&s.Spec.Template.Spec))
			}
		},
	); err != nil {
		errs = append(errs, fmt.Errorf("list statefulsets: %w", err))
	}

	// DaemonSets — pods on cordoned nodes still need the image cached.
	if err := streamAll(ctx,
		func(o metav1.ListOptions) (runtime.Object, error) {
			return cs.AppsV1().DaemonSets("").List(ctx, o)
		},
		func(o runtime.Object) {
			d := o.(*appsv1.DaemonSet)
			if filter.allowed(d.Namespace) {
				add(extractImagesFromPodSpec(&d.Spec.Template.Spec))
			}
		},
	); err != nil {
		errs = append(errs, fmt.Errorf("list daemonsets: %w", err))
	}

	// ReplicaSets — captures old rollout revisions kept under
	// revisionHistoryLimit so rollback targets are pre-warmed.
	if err := streamAll(ctx,
		func(o metav1.ListOptions) (runtime.Object, error) {
			return cs.AppsV1().ReplicaSets("").List(ctx, o)
		},
		func(o runtime.Object) {
			r := o.(*appsv1.ReplicaSet)
			if filter.allowed(r.Namespace) {
				add(extractImagesFromPodSpec(&r.Spec.Template.Spec))
			}
		},
	); err != nil {
		errs = append(errs, fmt.Errorf("list replicasets: %w", err))
	}

	// Jobs — includes completed but not-yet-reaped Jobs.
	if err := streamAll(ctx,
		func(o metav1.ListOptions) (runtime.Object, error) {
			return cs.BatchV1().Jobs("").List(ctx, o)
		},
		func(o runtime.Object) {
			j := o.(*batchv1.Job)
			if filter.allowed(j.Namespace) {
				add(extractImagesFromPodSpec(&j.Spec.Template.Spec))
			}
		},
	); err != nil {
		errs = append(errs, fmt.Errorf("list jobs: %w", err))
	}

	// CronJobs — CRITICAL. Suspended CronJobs produce no pods but their
	// images must still be cached for the next unsuspended run.
	if err := streamAll(ctx,
		func(o metav1.ListOptions) (runtime.Object, error) {
			return cs.BatchV1().CronJobs("").List(ctx, o)
		},
		func(o runtime.Object) {
			c := o.(*batchv1.CronJob)
			if filter.allowed(c.Namespace) {
				add(extractImagesFromPodSpec(&c.Spec.JobTemplate.Spec.Template.Spec))
			}
		},
	); err != nil {
		errs = append(errs, fmt.Errorf("list cronjobs: %w", err))
	}

	images = make([]string, 0, len(imageSet))
	for i := range imageSet {
		images = append(images, i)
	}
	return images, errs
}
