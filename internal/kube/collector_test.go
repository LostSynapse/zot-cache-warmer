package kube

import (
	"context"
	"sort"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func ptrBool(b bool) *bool { return &b }

// mkPod returns a Pod with the given init and main container images.
func mkPod(ns, name string, init, main []string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
	for _, i := range init {
		p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{Name: "i", Image: i})
	}
	for _, i := range main {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "c", Image: i})
	}
	return p
}

// mkDeployment returns a Deployment whose pod template has the given main images.
func mkDeployment(ns, name string, main []string) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
	for _, i := range main {
		d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers,
			corev1.Container{Name: "c", Image: i})
	}
	return d
}

// mkSuspendedCronJob returns a CronJob with Suspend=true and one container.
// This is the critical case: no pod exists, but the image must still be
// cached so the next unsuspended run can pull.
func mkSuspendedCronJob(ns, name, image string) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: batchv1.CronJobSpec{
			Suspend: ptrBool(true),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "c", Image: image}},
						},
					},
				},
			},
		},
	}
}

func TestCollectImages_DedupsAcrossKinds(t *testing.T) {
	cs := fake.NewSimpleClientset(
		mkPod("prod", "p1",
			[]string{"zot.example.com/init:1"},
			[]string{"zot.example.com/app:1"},
		),
		// Duplicate of the Pod's app image — must dedupe to one entry.
		mkDeployment("prod", "web", []string{"zot.example.com/app:1"}),
		// Suspended CronJob — no pod exists, but image must still be seen.
		mkSuspendedCronJob("prod", "nightly", "zot.example.com/cron:9"),
	)

	got, errs := CollectImages(context.Background(), cs, Filter{})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	want := map[string]bool{
		"zot.example.com/init:1": true,
		"zot.example.com/app:1":  true, // deduped across Pod + Deployment
		"zot.example.com/cron:9": true, // caught despite no running pod
	}
	if len(got) != len(want) {
		t.Fatalf("got %d images, want %d: %v", len(got), len(want), got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected image: %s", g)
		}
	}
}

func TestCollectImages_NamespaceFilter(t *testing.T) {
	cs := fake.NewSimpleClientset(
		mkPod("prod", "p1", nil, []string{"a:1"}),
		mkPod("staging", "p2", nil, []string{"b:1"}),
		mkPod("test", "p3", nil, []string{"c:1"}),
	)

	tests := []struct {
		name    string
		filter  Filter
		expect  []string
	}{
		{
			name:   "no-filter-returns-all",
			filter: Filter{},
			expect: []string{"a:1", "b:1", "c:1"},
		},
		{
			name:   "include-prod-only",
			filter: Filter{Include: []string{"prod"}},
			expect: []string{"a:1"},
		},
		{
			name:   "exclude-test",
			filter: Filter{Exclude: []string{"test"}},
			expect: []string{"a:1", "b:1"},
		},
		{
			name:   "include-then-exclude",
			filter: Filter{Include: []string{"prod", "staging", "test"}, Exclude: []string{"test"}},
			expect: []string{"a:1", "b:1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, errs := CollectImages(context.Background(), cs, tc.filter)
			if len(errs) != 0 {
				t.Fatalf("errs: %v", errs)
			}
			sort.Strings(got)
			sort.Strings(tc.expect)
			if len(got) != len(tc.expect) {
				t.Fatalf("got %v, want %v", got, tc.expect)
			}
			for i := range got {
				if got[i] != tc.expect[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], tc.expect[i])
				}
			}
		})
	}
}

func TestExtractImagesFromPodSpec_OrderAndEmpty(t *testing.T) {
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{
			{Image: "init-a"},
			{Image: ""}, // empty image must be dropped
			{Image: "init-b"},
		},
		Containers: []corev1.Container{
			{Image: "main-a"},
		},
	}
	got := extractImagesFromPodSpec(spec)
	want := []string{"init-a", "init-b", "main-a"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExtractImagesFromPodSpec_NilSafe(t *testing.T) {
	if got := extractImagesFromPodSpec(nil); got != nil {
		t.Errorf("extractImagesFromPodSpec(nil) = %v, want nil", got)
	}
}

func TestFilter_Allowed(t *testing.T) {
	tests := []struct {
		name   string
		filter Filter
		ns     string
		want   bool
	}{
		{"empty-allows-all", Filter{}, "any", true},
		{"include-match", Filter{Include: []string{"prod", "staging"}}, "prod", true},
		{"include-miss", Filter{Include: []string{"prod"}}, "test", false},
		{"exclude-match", Filter{Exclude: []string{"test"}}, "test", false},
		{"exclude-miss", Filter{Exclude: []string{"test"}}, "prod", true},
		{"include-then-exclude-wins", Filter{Include: []string{"prod"}, Exclude: []string{"prod"}}, "prod", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.allowed(tc.ns); got != tc.want {
				t.Errorf("allowed(%q) = %v, want %v", tc.ns, got, tc.want)
			}
		})
	}
}
