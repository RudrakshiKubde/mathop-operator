package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mathv1alpha1 "github.com/RudrakshiKubde/mathop-operator/api/v1alpha1"
)

const addRefIndexKey = ".spec.addRef.name"

// SquareReconciler reconciles a Square object
type SquareReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=math.example.com,resources=squares,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=math.example.com,resources=squares/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=math.example.com,resources=squares/finalizers,verbs=update
// +kubebuilder:rbac:groups=math.example.com,resources=adds,verbs=get;list;watch

func (r *SquareReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var square mathv1alpha1.Square
	if err := r.Get(ctx, req.NamespacedName, &square); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var add mathv1alpha1.Add
	addKey := types.NamespacedName{Namespace: square.Namespace, Name: square.Spec.AddRef.Name}
	if err := r.Get(ctx, addKey, &add); err != nil {
		meta.SetStatusCondition(&square.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "AddNotFound",
			Message:            fmt.Sprintf("referenced Add %q not found", square.Spec.AddRef.Name),
			ObservedGeneration: square.Generation,
		})
		_ = r.Status().Update(ctx, &square)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if add.Status.Result == nil {
		meta.SetStatusCondition(&square.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "WaitingOnAdd",
			Message:            fmt.Sprintf("Add %q has not produced a result yet", add.Name),
			ObservedGeneration: square.Generation,
		})
		return ctrl.Result{}, r.Status().Update(ctx, &square)
	}

	sum := *add.Status.Result
	result := sum * sum

	if square.Status.Result == nil || *square.Status.Result != result {
		square.Status.Result = &result
		meta.SetStatusCondition(&square.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Computed",
			Message:            fmt.Sprintf("%d^2 = %d", sum, result),
			ObservedGeneration: square.Generation,
		})
		if err := r.Status().Update(ctx, &square); err != nil {
			logger.Error(err, "unable to update Square status")
			return ctrl.Result{}, err
		}
		logger.Info("computed square", "name", square.Name, "addRef", add.Name, "result", result)
	}

	return ctrl.Result{}, nil
}

func (r *SquareReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index Square objects by the name of the Add they reference, so we can
	// cheaply find "which Squares care about this Add" when an Add changes.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &mathv1alpha1.Square{}, addRefIndexKey,
		func(obj client.Object) []string {
			sq := obj.(*mathv1alpha1.Square)
			if sq.Spec.AddRef.Name == "" {
				return nil
			}
			return []string{sq.Spec.AddRef.Name}
		},
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&mathv1alpha1.Square{}).
		Watches(
			&mathv1alpha1.Add{},
			handler.EnqueueRequestsFromMapFunc(r.findSquaresForAdd),
		).
		Named("square").
		Complete(r)
}

// findSquaresForAdd returns reconcile requests for every Square that
// references the given Add, so editing an Add re-triggers its Squares.
func (r *SquareReconciler) findSquaresForAdd(ctx context.Context, add client.Object) []ctrl.Request {
	var squares mathv1alpha1.SquareList
	if err := r.List(ctx, &squares,
		client.InNamespace(add.GetNamespace()),
		client.MatchingFields{addRefIndexKey: add.GetName()},
	); err != nil {
		return nil
	}

	requests := make([]ctrl.Request, 0, len(squares.Items))
	for _, sq := range squares.Items {
		requests = append(requests, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: sq.Namespace, Name: sq.Name},
		})
	}
	return requests
}
