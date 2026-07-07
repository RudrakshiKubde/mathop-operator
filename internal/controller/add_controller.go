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

type AddReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=math.example.com,resources=adds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=math.example.com,resources=adds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=math.example.com,resources=adds/finalizers,verbs=update

func (r *AddReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var add mathv1alpha1.Add
	if err := r.Get(ctx, req.NamespacedName, &add); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// --- Handle deletion ---
	if !add.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&add, inUseFinalizer) {
			inUse, err := referencedBySquare(ctx, r.Client, add.Namespace, mathv1alpha1.SchemeGroupVersion.String(), "Add", add.Name)
			if err != nil {
				return ctrl.Result{}, err
			}
			if inUse {
				logger.Info("blocking deletion: Add is still referenced ", "name", add.Name)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			controllerutil.RemoveFinalizer(&add, inUseFinalizer)
			if err := r.Update(ctx, &add); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// --- Ensure finalizer is present ---
	if !controllerutil.ContainsFinalizer(&add, inUseFinalizer) {
		controllerutil.AddFinalizer(&add, inUseFinalizer)
		if err := r.Update(ctx, &add); err != nil {
			return ctrl.Result{}, err
		}
	}

	// --- Normal computation ---
	result := add.Spec.X + add.Spec.Y

	if add.Status.Result == nil || *add.Status.Result != result {
		add.Status.Result = &result
		meta.SetStatusCondition(&add.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Computed",
			Message:            fmt.Sprintf("%d + %d = %d", add.Spec.X, add.Spec.Y, result),
			ObservedGeneration: add.Generation,
		})
		if err := r.Status().Update(ctx, &add); err != nil {
			logger.Error(err, "unable to update Add status")
			return ctrl.Result{}, err
		}
		logger.Info("computed sum", "name", add.Name, "result", result)
	}

	return ctrl.Result{}, nil
}

func (r *AddReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mathv1alpha1.Add{}).
		Named("add").
		Complete(r)
}