package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mathv1alpha1 "github.com/RudrakshiKubde/mathop-operator/api/v1alpha1"
)

const sourceRefIndexKey = ".spec.sourceRef"

type SquareReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	watcher *DynamicWatcher // shared engine from task_transfer.go
}

// +kubebuilder:rbac:groups=math.example.com,resources=squares,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=math.example.com,resources=squares/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=math.example.com,resources=squares/finalizers,verbs=update
// +kubebuilder:rbac:groups=math.example.com,resources=*,verbs=get;list;watch

func (r *SquareReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var square mathv1alpha1.Square
	if err := r.Get(ctx, req.NamespacedName, &square); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ref := square.Spec.SourceRef
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		meta.SetStatusCondition(&square.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "InvalidSourceRef",
			Message: fmt.Sprintf("invalid apiVersion %q: %v", ref.APIVersion, err), ObservedGeneration: square.Generation,
		})
		return ctrl.Result{}, r.Status().Update(ctx, &square)
	}
	gvk := gv.WithKind(ref.Kind)

	if err := r.watcher.Ensure(gvk, r.findSquaresForSource); err != nil {
		meta.SetStatusCondition(&square.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "SourceKindUnavailable",
			Message: fmt.Sprintf("cannot watch %s: %v", gvk.String(), err), ObservedGeneration: square.Generation,
		})
		return ctrl.Result{}, r.Status().Update(ctx, &square)
	}

	// --- This is the actual transfer, from task_transfer.go's FetchTaskResult, which is generic and reusable for any Task consumer ---
	sum, found, err := FetchTaskResult(ctx, r.Client, square.Namespace, ref.APIVersion, ref.Kind, ref.Name)
	if err != nil {
		meta.SetStatusCondition(&square.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "SourceNotFound",
			Message: fmt.Sprintf("referenced %s %q not found: %v", ref.Kind, ref.Name, err), ObservedGeneration: square.Generation,
		})
		_ = r.Status().Update(ctx, &square)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !found {
		meta.SetStatusCondition(&square.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "WaitingOnSource",
			Message: fmt.Sprintf("%s %q has not produced status.result yet", ref.Kind, ref.Name), ObservedGeneration: square.Generation,
		})
		return ctrl.Result{}, r.Status().Update(ctx, &square)
	}

	// --- This is the ONLY line specific to "Square" as an operation ---
	result := int32(sum * sum)

	if square.Status.Result == nil || *square.Status.Result != result {
		square.Status.Result = &result
		meta.SetStatusCondition(&square.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionTrue, Reason: "Computed",
			Message: fmt.Sprintf("%d^2 = %d", sum, result), ObservedGeneration: square.Generation,
		})
		if err := r.Status().Update(ctx, &square); err != nil {
			logger.Error(err, "unable to update Square status")
			return ctrl.Result{}, err
		}
		logger.Info("computed square", "name", square.Name, "sourceKind", ref.Kind, "sourceName", ref.Name, "result", result)
	}

	return ctrl.Result{}, nil
}

// this is the mapping function that the DynamicWatcher calls to find which Squares reference a given source resource.
func (r *SquareReconciler) findSquaresForSource(ctx context.Context, obj client.Object) []ctrl.Request {
	gvk := obj.GetObjectKind().GroupVersionKind()
	indexVal := sourceRefIndexValue(gvk.GroupVersion().String(), gvk.Kind, obj.GetName())

	var squares mathv1alpha1.SquareList
	if err := r.List(ctx, &squares, client.InNamespace(obj.GetNamespace()), client.MatchingFields{sourceRefIndexKey: indexVal}); err != nil {
		return nil
	}
	requests := make([]ctrl.Request, 0, len(squares.Items))
	for _, sq := range squares.Items {
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: sq.Namespace, Name: sq.Name}})
	}
	return requests
}

// this is called once during controller setup to register the field indexer and the DynamicWatcher.
func (r *SquareReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &mathv1alpha1.Square{}, sourceRefIndexKey,
		func(obj client.Object) []string {
			sq := obj.(*mathv1alpha1.Square)
			if sq.Spec.SourceRef.Name == "" {
				return nil
			}
			return []string{sourceRefIndexValue(sq.Spec.SourceRef.APIVersion, sq.Spec.SourceRef.Kind, sq.Spec.SourceRef.Name)}
		}); err != nil {
		return err
	}

	c, err := ctrl.NewControllerManagedBy(mgr).For(&mathv1alpha1.Square{}).Named("square").Build(r)
	if err != nil {
		return err
	}
	r.watcher = NewDynamicWatcher(mgr, c) // shared engine from task_transfer.go
	return nil
}
