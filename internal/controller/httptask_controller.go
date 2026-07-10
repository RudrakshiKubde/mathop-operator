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
	//fetch the latest version of the HTTPTask resource
	var task mathv1alpha1.HTTPTask
	if err := r.Get(ctx, req.NamespacedName, &task); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion / finalizer handling (if the task is being deleted, check if it's still referenced by other tasks)
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
	body := map[string]interface{}{} // this creates an empty JSON object if spec.Input is empty
	if len(task.Spec.Input.Raw) > 0 {
		//convert json into go map[string]interface{} for easier manipulation
		if err := json.Unmarshal(task.Spec.Input.Raw, &body); err != nil {
			return r.fail(ctx, &task, "InvalidInput", fmt.Sprintf("spec.input is not valid JSON: %v", err))
		}
	}
	// --- Merge in fields from other tasks' outputs, as specified in spec.InputFrom (if input is taken from 3 tasks, this loop will run 3 times) ---
	for _, m := range task.Spec.InputFrom {
		var source mathv1alpha1.HTTPTask
		sourceKey := types.NamespacedName{Namespace: task.Namespace, Name: m.SourceRef.Name}
		//fetch the source task's latest version from the API server
		if err := r.Get(ctx, sourceKey, &source); err != nil {
			return r.fail(ctx, &task, "SourceNotFound", fmt.Sprintf("referenced task %q not found", m.SourceRef.Name))
		}

		if len(source.Status.Output.Raw) == 0 {
			return r.waiting(ctx, &task, "WaitingOnSource", fmt.Sprintf("task %q has no output yet", m.SourceRef.Name))
		}
		var sourceOutput map[string]interface{}
		//parse the source task's output JSON into a map for easier field access
		if err := json.Unmarshal(source.Status.Output.Raw, &sourceOutput); err != nil {
			return r.fail(ctx, &task, "InvalidSourceOutput", fmt.Sprintf("task %q output is not a JSON object: %v", m.SourceRef.Name, err))
		}
		//extract required field from source task's output using the specified JSON path and set it in the current task's request body
		val, found := getJSONField(sourceOutput, m.SourceField)
		//if the required field is not found in the source task's output, mark the current task as waiting and requeue
		if !found {
			return r.waiting(ctx, &task, "WaitingOnSource", fmt.Sprintf("field %q not present yet in task %q output", m.SourceField, m.SourceRef.Name))
		}
		//insert the extracted value into the current task's request body at the specified target field path
		setJSONField(body, m.TargetField, val)
	}
	//convert the final request body map into JSON bytes for the HTTP request
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

	// Mark as actively running BEFORE making the (possibly slow) call, so
	// this is genuinely visible to anything watching this object while the
	// call is in flight — not just before/after it.
	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: "Running",
		Message: fmt.Sprintf("calling %s", task.Spec.Endpoint), ObservedGeneration: task.Generation,
	})
	if err := r.Status().Update(ctx, &task); err != nil {
		logger.Error(err, "unable to mark HTTPTask as running")
		return ctrl.Result{}, err
	}

	// --- Make the call ---
	//if user has not specified a method, default to POST
	method := task.Spec.Method
	if method == "" {
		method = http.MethodPost
	}
	//build the HTTP request with the specified method, endpoint, and request body
	httpReq, err := http.NewRequestWithContext(ctx, method, task.Spec.Endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return r.fail(ctx, &task, "InvalidRequest", fmt.Sprintf("failed to build request: %v", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range task.Spec.Headers {
		httpReq.Header.Set(k, v)
	}
	//send the HTTP request using the injected HTTP client
	resp, err := r.HTTPClient.Do(httpReq)
	//handle any errors that occurred during the HTTP request
	if err != nil {
		return r.fail(ctx, &task, "RequestFailed", fmt.Sprintf("HTTP call to %s failed: %v", task.Spec.Endpoint, err))
	}
	//close the response body when done to free up resources
	defer resp.Body.Close()

	//read the response body into bytes for further processing
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return r.fail(ctx, &task, "RequestFailed", fmt.Sprintf("failed reading response body: %v", err))
	}

	statusCode := int32(resp.StatusCode)
	//update the task's status with the HTTP response details, including status code, output, and observed input hash
	task.Status.StatusCode = &statusCode
	task.Status.Output = runtime.RawExtension{Raw: respBytes}
	task.Status.ObservedInputHash = hashHex
	//check if response succeded (2xx) or failed (non-2xx) and set the appropriate condition in the task's status
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

// if a task changes, all tasks that depend on it (via spec.InputFrom) must be re-reconciled. This function finds all such dependent tasks and returns a list of reconcile requests for them.
func (r *HTTPTaskReconciler) findDependentTasks(ctx context.Context, obj client.Object) []ctrl.Request {
	t := obj.(*mathv1alpha1.HTTPTask)
	//build the index value for this task, which is used to look up dependent tasks that reference it in their spec.InputFrom
	indexVal := sourceRefIndexValue("math.example.com/v1alpha1", "HTTPTask", t.Name)

	var tasks mathv1alpha1.HTTPTaskList
	//list all tasks in the same namespace that have an InputFrom entry referencing this task, using the index built in SetupWithManager
	if err := r.List(ctx, &tasks, client.InNamespace(t.Namespace), client.MatchingFields{taskSourceRefIndexKey: indexVal}); err != nil {
		return nil
	}
	//create reconcile requests for each dependent task found, so that they will be re-reconciled in response to changes in this task
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
