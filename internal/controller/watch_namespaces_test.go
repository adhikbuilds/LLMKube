/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/watchscope"
)

// namespacedOperator sets up the shape a --watch-namespaces install has
// (#1600): an identity whose only grants are a Role in the watched namespace,
// and a second namespace it holds nothing in. It returns a rest.Config that
// acts as that identity, the watched namespace and the unwatched one. envtest
// itself runs as cluster-admin, so the restricted identity is what makes a
// list across all namespaces fail here the way it does in a real install.
func namespacedOperator(t *testing.T) (admin client.Client, restricted *rest.Config, watched, unwatched string) {
	t.Helper()
	admin, cfg, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()
	watched = testNS
	other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "llmkube-unwatched-"}}
	if err := admin.Create(ctx, other); err != nil {
		t.Fatalf("create the unwatched namespace: %v", err)
	}
	unwatched = other.Name

	user := "watch-namespaces-" + watched
	readOnly := []string{"get", "list", "watch"}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "namespaced-operator", Namespace: watched},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{inferencev1alpha1.GroupVersion.Group}, Resources: []string{"models", "metalnodes"}, Verbs: readOnly},
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: readOnly},
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: readOnly},
		},
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "namespaced-operator", Namespace: watched},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
	}
	for _, obj := range []client.Object{role, binding} {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	restricted = rest.CopyConfig(cfg)
	restricted.Impersonate = rest.ImpersonationConfig{UserName: user}
	return admin, restricted, watched, unwatched
}

// startAndSync registers an informer for obj, runs c until the test ends, and
// reports whether the informer synced within d. The informer is registered
// before the start so the wait covers it: an informer that cannot list (a
// forbidden list is retried forever) leaves the wait to run out.
func startAndSync(t *testing.T, c cache.Cache, obj client.Object, d time.Duration) bool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := c.GetInformer(ctx, obj); err != nil {
		cancel()
		t.Fatalf("register the %T informer: %v", obj, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	syncCtx, cancelSync := context.WithTimeout(ctx, d)
	defer cancelSync()
	return c.WaitForCacheSync(syncCtx)
}

// TestWatchNamespacesCacheSyncsUnderNamespacedRoles is the feature's
// acceptance case: an operator that holds only a Role in its watched
// namespace gets a synced cache holding that namespace's objects and nothing
// else. The unscoped cache at the end is the control that shows the Role
// really is all the identity has: it is what every install ran before, and
// under the same Role it cannot sync.
//
// Falsification: make watchscope.DefaultNamespaces return nil. The scoped
// cache then lists Models across all namespaces, is forbidden, and the first
// GetInformer times out.
func TestWatchNamespacesCacheSyncsUnderNamespacedRoles(t *testing.T) {
	admin, restricted, watched, unwatched := namespacedOperator(t)
	ctx := context.Background()
	for _, ns := range []string{watched, unwatched} {
		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "scoped", Namespace: ns},
			Spec:       inferencev1alpha1.ModelSpec{Source: "https://example.com/model.gguf"},
		}
		if err := admin.Create(ctx, model); err != nil {
			t.Fatalf("create Model in %s: %v", ns, err)
		}
	}

	scoped, err := cache.New(restricted, cache.Options{
		Scheme:            scheme.Scheme,
		DefaultNamespaces: watchscope.DefaultNamespaces([]string{watched}),
	})
	if err != nil {
		t.Fatalf("new scoped cache: %v", err)
	}
	if !startAndSync(t, scoped, &inferencev1alpha1.Model{}, 30*time.Second) {
		t.Fatalf("the namespace-scoped Model informer did not sync under a Role in %s", watched)
	}
	models := &inferencev1alpha1.ModelList{}
	if err := scoped.List(ctx, models); err != nil {
		t.Fatalf("list Models from the scoped cache: %v", err)
	}
	if len(models.Items) != 1 || models.Items[0].Namespace != watched {
		t.Errorf("scoped cache holds %d Models, want only %s/scoped: %v", len(models.Items), watched, modelKeys(models))
	}
	if err := scoped.Get(ctx, client.ObjectKey{Namespace: unwatched, Name: "scoped"}, &inferencev1alpha1.Model{}); err == nil {
		t.Errorf("the scoped cache served a Model from unwatched namespace %s", unwatched)
	}

	clusterWide, err := cache.New(restricted, cache.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("new cluster-wide cache: %v", err)
	}
	if startAndSync(t, clusterWide, &inferencev1alpha1.Model{}, 5*time.Second) {
		t.Fatalf("a cluster-wide Model informer synced under a Role in one namespace; the fixture is not restricting anything")
	}
}

func modelKeys(list *inferencev1alpha1.ModelList) []string {
	out := make([]string, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, list.Items[i].Namespace+"/"+list.Items[i].Name)
	}
	return out
}

// TestMetalNodeLeaseCacheFollowsWatchNamespaces pins the one cache the
// manager's scope does not reach. The MetalNode controller builds a Lease
// cache of its own, so under per-namespace Roles it must be given the same
// namespaces or it lists Leases across the cluster, is forbidden, and never
// syncs; an unsynced cache holds the manager's start (#1597).
//
// Falsification: drop DefaultNamespaces from the cache.New call in
// MetalNodeReconciler.SetupWithManager. The Lease cache does not sync within
// the deadline.
func TestMetalNodeLeaseCacheFollowsWatchNamespaces(t *testing.T) {
	admin, restricted, watched, unwatched := namespacedOperator(t)
	ctx := context.Background()
	for _, ns := range []string{watched, unwatched} {
		lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
			Name: "mac-heartbeat", Namespace: ns,
			Labels: map[string]string{inferencev1alpha1.LabelMetalNode: "mac"},
		}}
		if err := admin.Create(ctx, lease); err != nil {
			t.Fatalf("create Lease in %s: %v", ns, err)
		}
	}

	namespaces := []string{watched}
	mgr, err := ctrl.NewManager(restricted, ctrl.Options{
		Scheme:  scheme.Scheme,
		Cache:   cache.Options{DefaultNamespaces: watchscope.DefaultNamespaces(namespaces)},
		Metrics: metricsserver.Options{BindAddress: "0"},
		// Another test in this package registers the same controller name.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	r := &MetalNodeReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		WatchNamespaces:    namespaces,
		InitContainerImage: "registry.local/curl:1",
		AllowedCIDRs:       mustAllowedCIDRs(t, DefaultMetalNodeAllowedCIDRs),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("set up the MetalNode controller: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("manager exited with an error: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Errorf("manager did not stop")
		}
	})

	leaseCache, ok := r.leaseReader.(cache.Cache)
	if !ok {
		t.Fatalf("leaseReader is %T, want the controller's Lease cache", r.leaseReader)
	}
	syncCtx, cancelSync := context.WithTimeout(ctx, 30*time.Second)
	defer cancelSync()
	if !leaseCache.WaitForCacheSync(syncCtx) {
		t.Fatalf("the MetalNode Lease cache did not sync under a Role in %s", watched)
	}
	// A read registers the Lease informer if the controller has not yet, and
	// waits for it, so it runs under the same deadline as the sync.
	if err := leaseCache.Get(syncCtx, client.ObjectKey{Namespace: watched, Name: "mac-heartbeat"}, &coordinationv1.Lease{}); err != nil {
		t.Errorf("the Lease in watched namespace %s is not in the Lease cache: %v", watched, err)
	}
	if err := leaseCache.Get(syncCtx, client.ObjectKey{Namespace: unwatched, Name: "mac-heartbeat"}, &coordinationv1.Lease{}); err == nil {
		t.Errorf("the Lease cache served a Lease from unwatched namespace %s", unwatched)
	}
}
