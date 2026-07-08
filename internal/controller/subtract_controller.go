package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mathv1alpha1 "github.com/RudrakshiKubde/mathop-operator/api/v1alpha1"
)

type SubtractReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=math.example.com,resources=subtracts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=math.example.com,resources=subtracts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=math.example.com,resources=subtracts/finalizers,verbs=update

func (r *SubtractReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sub mathv1alpha1.Subtract
	if err := r.Get(ctx, req.NamespacedName, &sub); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// --- Handle deletion ---
	if !sub.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&sub, inUseFinalizer) {
			inUse, err := referencedBySquare(ctx, r.Client, sub.Namespace, mathv1alpha1.SchemeGroupVersion.String(), "Subtract", sub.Name)
			if err != nil {
				return ctrl.Result{}, err
			}
			if inUse {
				logger.Info("blocking deletion: Subtract is still referenced by a Square", "name", sub.Name)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			controllerutil.RemoveFinalizer(&sub, inUseFinalizer)
			if err := r.Update(ctx, &sub); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// --- Ensure finalizer is present ---
	if !controllerutil.ContainsFinalizer(&sub, inUseFinalizer) {
		controllerutil.AddFinalizer(&sub, inUseFinalizer)
		if err := r.Update(ctx, &sub); err != nil {
			return ctrl.Result{}, err
		}
	}

	// --- Normal computation ---
	result := sub.Spec.X - sub.Spec.Y

	if sub.Status.Result == nil || *sub.Status.Result != result {
		sub.Status.Result = &result
		meta.SetStatusCondition(&sub.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Computed",
			Message:            fmt.Sprintf("%d - %d = %d", sub.Spec.X, sub.Spec.Y, result),
			ObservedGeneration: sub.Generation,
		})
		if err := r.Status().Update(ctx, &sub); err != nil {
			logger.Error(err, "unable to update Subtract status")
			return ctrl.Result{}, err
		}
		logger.Info("computed difference", "name", sub.Name, "result", result)
	}

	return ctrl.Result{}, nil
}

func (r *SubtractReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mathv1alpha1.Subtract{}).
		Named("subtract").
		Complete(r)
}
