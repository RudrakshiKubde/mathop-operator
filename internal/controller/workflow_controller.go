// workflow_controller.go
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mathv1alpha1 "github.com/RudrakshiKubde/mathop-operator/api/v1alpha1"
)

const TransactionIndexKey = ".status.transactionID"

type WorkflowReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	DashboardBaseURL string // e.g. "http://localhost:8081"
}

// +kubebuilder:rbac:groups=math.example.com,resources=workflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=math.example.com,resources=workflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=math.example.com,resources=httptasks,verbs=get;list;watch;create;update;patch;delete

func (r *WorkflowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Workflow instance
	var wf mathv1alpha1.Workflow
	if err := r.Get(ctx, req.NamespacedName, &wf); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Ensure TransactionID and DashboardURL are set
	if wf.Status.TransactionID == "" {
		wf.Status.TransactionID = string(wf.UID)
	}
	wf.Status.DashboardURL = fmt.Sprintf("%s/workflows/%s", r.DashboardBaseURL, wf.Status.TransactionID)
	taskStatuses := make([]mathv1alpha1.WorkflowTaskStatus, 0, len(wf.Spec.Tasks))
	hasFailed := false
	hasPending := false

	// Reconcile each WorkflowTaskSpec into a corresponding HTTPTask
	for _, step := range wf.Spec.Tasks {
		childName := fmt.Sprintf("%s-%s", wf.Name, step.Name)

		mappedInputFrom := make([]mathv1alpha1.FieldMapping, 0, len(step.InputFrom))
		for _, m := range step.InputFrom {
			mappedInputFrom = append(mappedInputFrom, mathv1alpha1.FieldMapping{
				SourceRef: mathv1alpha1.SourceReference{
					APIVersion: mathv1alpha1.SchemeGroupVersion.String(),
					Kind:       "HTTPTask",
					Name:       fmt.Sprintf("%s-%s", wf.Name, m.SourceStep),
				},
				SourceField: m.SourceField,
				TargetField: m.TargetField,
			})
		}

		child := &mathv1alpha1.HTTPTask{ObjectMeta: metav1.ObjectMeta{Name: childName, Namespace: wf.Namespace}}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, child, func() error {
			child.Spec.Endpoint = step.Endpoint
			child.Spec.Method = step.Method
			child.Spec.Headers = step.Headers
			child.Spec.Input = step.Input
			child.Spec.InputFrom = mappedInputFrom
			return ctrl.SetControllerReference(&wf, child, r.Scheme)
		})
		if err != nil {
			logger.Error(err, "unable to reconcile child HTTPTask", "step", step.Name)
			return ctrl.Result{}, err
		}

		ts := mathv1alpha1.WorkflowTaskStatus{
			Name:       step.Name,
			StatusCode: child.Status.StatusCode,
			Output:     child.Status.Output,
			Phase:      "Pending",
		}

		// Determine the phase of the child HTTPTask and update the WorkflowTaskStatus accordingly
		ready := findReadyCondition(child.Status.Conditions)
		switch {
		case ready == nil:
			ts.Phase = "Pending"
			hasPending = true
		case ready.Status == metav1.ConditionTrue:
			ts.Phase = "Succeeded"
		case ready.Reason == "WaitingOnSource":
			ts.Phase = "Pending"
			hasPending = true
		case ready.Reason == "Running":
			ts.Phase = "Running"
			hasPending = true
		default:
			ts.Phase = "Failed"
			ts.Error = fmt.Sprintf("%s: %s", ready.Reason, ready.Message)
			hasFailed = true
		}

		taskStatuses = append(taskStatuses, ts)
	}
	overallPhase := "Succeeded"
	if hasFailed {
		overallPhase = "Failed"
	} else if hasPending {
		overallPhase = "Running"
	}

	// Update the Workflow status with the aggregated task statuses and overall phase
	wf.Status.Tasks = taskStatuses
	wf.Status.Phase = overallPhase

	if err := r.Status().Update(ctx, &wf); err != nil {
		logger.Error(err, "unable to update Workflow status")
		return ctrl.Result{}, err
	}

	if isTerminal(overallPhase) && wf.Spec.NotifyURL != "" && wf.Status.NotifiedPhase != overallPhase {
		if err := r.notify(ctx, &wf); err != nil {
			logger.Error(err, "failed to send notification", "url", wf.Spec.NotifyURL)
			// retry the notify without re-running the whole task loop
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		wf.Status.NotifiedPhase = overallPhase
		if err := r.Status().Update(ctx, &wf); err != nil {
			logger.Error(err, "unable to record notified phase")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func isTerminal(phase string) bool {
	return phase == "Succeeded" || phase == "Failed"
}

func (r *WorkflowReconciler) notify(ctx context.Context, wf *mathv1alpha1.Workflow) error {
	body, err := json.Marshal(wf.Status)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wf.Spec.NotifyURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("notify endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func findReadyCondition(conditions []metav1.Condition) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == "Ready" {
			return &conditions[i]
		}
	}
	return nil
}

func (r *WorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &mathv1alpha1.Workflow{}, TransactionIndexKey,
		func(obj client.Object) []string {
			wf := obj.(*mathv1alpha1.Workflow)
			if wf.Status.TransactionID == "" {
				return nil
			}
			return []string{wf.Status.TransactionID}
		}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&mathv1alpha1.Workflow{}).
		Owns(&mathv1alpha1.HTTPTask{}). // re-reconcile the Workflow whenever any child HTTPTask's status changes
		Named("workflow").
		Complete(r)
}

var _ = types.NamespacedName{}      // silence unused import if you trim things
var _ = unstructured.Unstructured{} // remove this line if not otherwise needed
