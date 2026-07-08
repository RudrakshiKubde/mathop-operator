package controller

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// ============================================================================
// FetchTaskResult — the actual "take Task 1's output" logic.
// This is the reusable engine: it works identically regardless of whether
// the referenced resource is Add, Subtract, or anything built later, because
// it never touches a concrete Go type — only the shared status.result
// convention every Task is expected to follow.
// ============================================================================

// FetchTaskResult fetches any resource by apiVersion+kind+name and reads its
// status.result field. Returns (value, found, error). found=false means the
// resource exists but hasn't produced a result yet (not an error condition).

// This function's job: given "some resource, identified only by strings," go read its status.result field
func FetchTaskResult(ctx context.Context, c client.Client, namespace, apiVersion, kind, name string) (int64, bool, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return 0, false, fmt.Errorf("invalid apiVersion %q: %w", apiVersion, err)
	}

	src := &unstructured.Unstructured{}
	src.SetGroupVersionKind(gv.WithKind(kind))

	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, src); err != nil {
		return 0, false, err // caller checks client.IgnoreNotFound if it wants to distinguish NotFound
	}

	result, found, err := unstructured.NestedInt64(src.Object, "status", "result")
	if err != nil {
		return 0, false, fmt.Errorf("status.result on %s/%s is not an integer: %w", kind, name, err)
	}
	return result, found, nil
}

// ============================================================================
// DynamicWatcher — the reusable "discover and watch new Task kinds at
// runtime" engine. Any consumer controller (Square, or a future one) embeds
// one of these and calls Ensure() from inside Reconcile, the same way
// SquareReconciler.ensureWatch already did.
// ============================================================================

type DynamicWatcher struct {
	mgr ctrl.Manager
	ctl controller.Controller

	mu   sync.Mutex
	seen map[schema.GroupVersionKind]bool
}

func NewDynamicWatcher(mgr ctrl.Manager, ctl controller.Controller) *DynamicWatcher {
	return &DynamicWatcher{mgr: mgr, ctl: ctl, seen: make(map[schema.GroupVersionKind]bool)}
}

// Ensure registers a watch for gvk the first time it's seen; subsequent
// calls for an already-watched gvk are no-ops.
func (w *DynamicWatcher) Ensure(gvk schema.GroupVersionKind, mapFunc handler.MapFunc) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.seen[gvk] {
		return nil
	}

	if _, err := w.mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		return fmt.Errorf("kind not found on cluster: %w", err)
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)

	src := source.Kind[client.Object](w.mgr.GetCache(), u, handler.EnqueueRequestsFromMapFunc(mapFunc))
	if err := w.ctl.Watch(src); err != nil {
		return err
	}

	w.seen[gvk] = true
	return nil
}
func sourceRefIndexValue(apiVersion, kind, name string) string {
	return fmt.Sprintf("%s/%s/%s", apiVersion, kind, name)
}
