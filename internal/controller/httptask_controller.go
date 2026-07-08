package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mathv1alpha1 "github.com/RudrakshiKubde/mathop-operator/api/v1alpha1"
)

const (
	taskSourceRefIndexKey = ".spec.inputFrom.sourceRef"
	taskInUseFinalizer    = "math.example.com/task-in-use-protection"
)

type HTTPTaskReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	HTTPClient *http.Client // inject in main.go; defaults handled there too
}

// +kubebuilder:rbac:groups=math.example.com,resources=httptasks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=math.example.com,resources=httptasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=math.example.com,resources=httptasks/finalizers,verbs=update

func (r *HTTPTaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var task mathv1alpha1.HTTPTask
	if err := r.Get(ctx, req.NamespacedName, &task); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// --- Deletion / finalizer handling ---
	if !task.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&task, taskInUseFinalizer) {
			inUse, err := taskReferencedByOthers(ctx, r.Client, task.Namespace, "math.example.com/v1alpha1", "HTTPTask", task.Name)
			if err != nil {
				return ctrl.Result{}, err
			}
			if inUse {
				logger.Info("blocking deletion: task is still referenced by another task", "name", task.Name)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			controllerutil.RemoveFinalizer(&task, taskInUseFinalizer)
			if err := r.Update(ctx, &task); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(&task, taskInUseFinalizer) {
		controllerutil.AddFinalizer(&task, taskInUseFinalizer)
		if err := r.Update(ctx, &task); err != nil {
			return ctrl.Result{}, err
		}
	}

	// --- Build the request body: start from spec.Input, merge in mapped fields ---
	body := map[string]interface{}{}
	if len(task.Spec.Input.Raw) > 0 {
		if err := json.Unmarshal(task.Spec.Input.Raw, &body); err != nil {
			return r.fail(ctx, &task, "InvalidInput", fmt.Sprintf("spec.input is not valid JSON: %v", err))
		}
	}

	for _, m := range task.Spec.InputFrom {
		var source mathv1alpha1.HTTPTask
		sourceKey := types.NamespacedName{Namespace: task.Namespace, Name: m.SourceRef.Name}
		if err := r.Get(ctx, sourceKey, &source); err != nil {
			return r.fail(ctx, &task, "SourceNotFound", fmt.Sprintf("referenced task %q not found", m.SourceRef.Name))
		}
		if len(source.Status.Output.Raw) == 0 {
			return r.waiting(ctx, &task, "WaitingOnSource", fmt.Sprintf("task %q has no output yet", m.SourceRef.Name))
		}
		var sourceOutput map[string]interface{}
		if err := json.Unmarshal(source.Status.Output.Raw, &sourceOutput); err != nil {
			return r.fail(ctx, &task, "InvalidSourceOutput", fmt.Sprintf("task %q output is not a JSON object: %v", m.SourceRef.Name, err))
		}
		val, found := getJSONField(sourceOutput, m.SourceField)
		if !found {
			return r.waiting(ctx, &task, "WaitingOnSource", fmt.Sprintf("field %q not present yet in task %q output", m.SourceField, m.SourceRef.Name))
		}
		setJSONField(body, m.TargetField, val)
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return r.fail(ctx, &task, "InvalidInput", fmt.Sprintf("failed to marshal request body: %v", err))
	}
	hash := sha256.Sum256(bodyBytes)
	hashHex := hex.EncodeToString(hash[:])

	// --- Skip the HTTP call if nothing has actually changed since the last successful one ---
	if task.Status.ObservedInputHash == hashHex && len(task.Status.Output.Raw) > 0 {
		return ctrl.Result{}, nil
	}

	// --- Make the call ---
	method := task.Spec.Method
	if method == "" {
		method = http.MethodPost
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, task.Spec.Endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return r.fail(ctx, &task, "InvalidRequest", fmt.Sprintf("failed to build request: %v", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range task.Spec.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := r.HTTPClient.Do(httpReq)
	if err != nil {
		return r.fail(ctx, &task, "RequestFailed", fmt.Sprintf("HTTP call to %s failed: %v", task.Spec.Endpoint, err))
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return r.fail(ctx, &task, "RequestFailed", fmt.Sprintf("failed reading response body: %v", err))
	}

	statusCode := int32(resp.StatusCode)
	task.Status.StatusCode = &statusCode
	task.Status.Output = runtime.RawExtension{Raw: respBytes}
	task.Status.ObservedInputHash = hashHex

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "NonSuccessStatus",
			Message: fmt.Sprintf("endpoint returned HTTP %d", resp.StatusCode), ObservedGeneration: task.Generation,
		})
	} else {
		meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionTrue, Reason: "Computed",
			Message: fmt.Sprintf("called %s, got HTTP %d", task.Spec.Endpoint, resp.StatusCode), ObservedGeneration: task.Generation,
		})
	}

	if err := r.Status().Update(ctx, &task); err != nil {
		logger.Error(err, "unable to update HTTPTask status")
		return ctrl.Result{}, err
	}
	logger.Info("called task endpoint", "name", task.Name, "endpoint", task.Spec.Endpoint, "statusCode", resp.StatusCode)

	return ctrl.Result{}, nil
}

func (r *HTTPTaskReconciler) fail(ctx context.Context, task *mathv1alpha1.HTTPTask, reason, msg string) (ctrl.Result, error) {
	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: reason, Message: msg, ObservedGeneration: task.Generation,
	})
	_ = r.Status().Update(ctx, task)
	return ctrl.Result{}, nil
}

func (r *HTTPTaskReconciler) waiting(ctx context.Context, task *mathv1alpha1.HTTPTask, reason, msg string) (ctrl.Result, error) {
	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: reason, Message: msg, ObservedGeneration: task.Generation,
	})
	return ctrl.Result{}, r.Status().Update(ctx, task)
}

func (r *HTTPTaskReconciler) findDependentTasks(ctx context.Context, obj client.Object) []ctrl.Request {
	t := obj.(*mathv1alpha1.HTTPTask)
	indexVal := sourceRefIndexValue("math.example.com/v1alpha1", "HTTPTask", t.Name)

	var tasks mathv1alpha1.HTTPTaskList
	if err := r.List(ctx, &tasks, client.InNamespace(t.Namespace), client.MatchingFields{taskSourceRefIndexKey: indexVal}); err != nil {
		return nil
	}
	requests := make([]ctrl.Request, 0, len(tasks.Items))
	for _, dt := range tasks.Items {
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: dt.Namespace, Name: dt.Name}})
	}
	return requests
}

func (r *HTTPTaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.HTTPClient == nil {
		r.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}

	// Multi-valued index: a Task can appear under several keys, one per
	// inputFrom entry, since it may depend on more than one other Task.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &mathv1alpha1.HTTPTask{}, taskSourceRefIndexKey,
		func(obj client.Object) []string {
			t := obj.(*mathv1alpha1.HTTPTask)
			vals := make([]string, 0, len(t.Spec.InputFrom))
			for _, m := range t.Spec.InputFrom {
				vals = append(vals, sourceRefIndexValue(m.SourceRef.APIVersion, m.SourceRef.Kind, m.SourceRef.Name))
			}
			return vals
		}); err != nil {
		return err
	}

	// Same Kind on both sides — a plain self-referencing watch is enough.
	// No dynamic GVK discovery needed here, unlike the earlier multi-kind design.
	return ctrl.NewControllerManagedBy(mgr).
		For(&mathv1alpha1.HTTPTask{}).
		Watches(&mathv1alpha1.HTTPTask{}, handler.EnqueueRequestsFromMapFunc(r.findDependentTasks)).
		Named("httptask").
		Complete(r)
}
