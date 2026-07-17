package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/crossplane/function-sdk-go/errors"
	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/resource/composed"
	"github.com/crossplane/function-sdk-go/response"

	"github.com/crossplane/function-httptask/input/v1beta1"
)

type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer
	log logging.Logger
}

type TaskSpec struct {
	Name      string                 `json:"name"`
	Endpoint  string                 `json:"endpoint"`
	Method    string                 `json:"method,omitempty"`
	Headers   map[string]string      `json:"headers,omitempty"`
	Input     map[string]interface{} `json:"input,omitempty"`
	InputFrom []FieldMapping         `json:"inputFrom,omitempty"`
}

type FieldMapping struct {
	SourceTask  string `json:"sourceTask"`
	SourceField string `json:"sourceField"`
	TargetField string `json:"targetField"`
}

type TaskStatus struct {
	Name       string                 `json:"name"`
	Phase      string                 `json:"phase"` // Pending, Running, Succeeded, Failed, Skipped
	StatusCode *int32                 `json:"statusCode,omitempty"`
	Output     map[string]interface{} `json:"output,omitempty"`
	Error      string                 `json:"error,omitempty"`
}

func (f *Function) RunFunction(ctx context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	f.log.Info("Running function", "tag", req.GetMeta().GetTag())
	rsp := response.To(req, response.DefaultTTL)

	in := &v1beta1.Input{}
	if err := request.GetInput(req, in); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input"))
		return rsp, nil
	}
	providerConfigName := in.ProviderConfigName
	if providerConfigName == "" {
		providerConfigName = "default"
	}

	oxr, err := request.GetObservedCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get observed composite resource"))
		return rsp, nil
	}
	content := oxr.Resource.UnstructuredContent()
	uid, _, _ := unstructured.NestedString(content, "metadata", "uid")

	rawTasks, found, err := unstructured.NestedSlice(content, "spec", "tasks")
	if err != nil || !found || len(rawTasks) == 0 {
		response.Fatal(rsp, errors.New("spec.tasks must contain at least one task"))
		return rsp, nil
	}
	tasksJSON, _ := json.Marshal(rawTasks)
	var tasks []TaskSpec
	if err := json.Unmarshal(tasksJSON, &tasks); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "spec.tasks does not match the expected shape"))
		return rsp, nil
	}

	observed, err := request.GetObservedComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get observed composed resources"))
		return rsp, nil
	}

	desired := map[resource.Name]*resource.DesiredComposed{}
	outputs := map[string]map[string]interface{}{}
	failedTasks := map[string]bool{}
	overallFailed := false
	statuses := make([]TaskStatus, 0, len(tasks))

	for _, task := range tasks {
		name := resource.Name(task.Name)

		// If any of THIS task's own dependencies has failed, it can never
		// run — skip it permanently. This does NOT block unrelated tasks
		// with no dependency on the failed one.
		depFailed := false
		for _, m := range task.InputFrom {
			if failedTasks[m.SourceTask] {
				depFailed = true
				break
			}
		}
		if depFailed {
			statuses = append(statuses, TaskStatus{Name: task.Name, Phase: "Skipped"})
			continue
		}

		obs, exists := observed[name]

		if !exists {
			if !depsReady(task, outputs) {
				statuses = append(statuses, TaskStatus{Name: task.Name, Phase: "Pending"})
				continue // NOT blocked — just this task waits; move on to the next
			}
			dr, err := buildDisposableRequest(task, in.DefaultHeaders, providerConfigName, outputs)
			if err != nil {
				statuses = append(statuses, TaskStatus{Name: task.Name, Phase: "Failed", Error: err.Error()})
				failedTasks[task.Name] = true
				overallFailed = true
				continue
			}
			desired[name] = &resource.DesiredComposed{Resource: dr}
			statuses = append(statuses, TaskStatus{Name: task.Name, Phase: "Pending"})
			continue
		}

	// Already exists — rebuild deterministically (never echo obs.Resource).
	dr, err := buildDisposableRequest(task, in.DefaultHeaders, providerConfigName, outputs)
	if err != nil {
		statuses = append(statuses, TaskStatus{Name: task.Name, Phase: "Failed", Error: err.Error()})
		failedTasks[task.Name] = true
		overallFailed = true
		continue
	}
	desired[name] = &resource.DesiredComposed{Resource: dr}

	s := taskStatusFromObserved(task.Name, obs.Resource)
	if s.Phase == "Succeeded" {
		outputs[task.Name] = s.Output
	}
	if s.Phase == "Failed" {
		failedTasks[task.Name] = true
		overallFailed = true
	}
	statuses = append(statuses, s)
}

	phase := "Running"
	if overallFailed {
		phase = "Failed"
	} else if allDone(statuses) {
		phase = "Succeeded"
	}

	const notifyKey = resource.Name("__notify__")
	if phase == "Succeeded" || phase == "Failed" {
		notifyURL, _, _ := unstructured.NestedString(content, "spec", "notifyURL")
		workflowName, _, _ := unstructured.NestedString(content, "metadata", "name")
		if notifyURL != "" {
			nr, err := buildNotifyRequest(workflowName, uid, phase, statuses, notifyURL, providerConfigName)
			if err != nil {
				f.log.Info("cannot build notify request", "error", err.Error())
			} else {
				desired[notifyKey] = &resource.DesiredComposed{Resource: nr}
			}
		}
	}


	if err := response.SetDesiredComposedResources(rsp, desired); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot set desired composed resources"))
		return rsp, nil
	}

	dxr, err := request.GetDesiredCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get desired composite resource"))
		return rsp, nil
	}
	dc := dxr.Resource.UnstructuredContent()
	_ = unstructured.SetNestedField(dc, uid, "status", "transactionID")
	_ = unstructured.SetNestedField(dc, phase, "status", "phase")
	rawFinal, _ := toUnstructuredSlice(statuses)
	_ = unstructured.SetNestedSlice(dc, rawFinal, "status", "tasks")
	if err := response.SetDesiredCompositeResource(rsp, dxr); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot set desired composite resource"))
		return rsp, nil
	}

	switch phase {
	case "Failed":
		response.ConditionFalse(rsp, "FunctionSuccess", "TaskFailed").
			WithMessage("one or more tasks failed, see status.tasks").TargetCompositeAndClaim()
	case "Running":
		response.ConditionFalse(rsp, "FunctionSuccess", "Running").
			WithMessage("workflow still in progress").TargetCompositeAndClaim()
	default:
		response.ConditionTrue(rsp, "FunctionSuccess", "Computed").
			WithMessage(fmt.Sprintf("ran %d task(s) successfully", len(tasks))).TargetCompositeAndClaim()
	}
	return rsp, nil
}

// buildDisposableRequest constructs the desired DisposableRequest for a task
// that hasn't been created yet, resolving inputFrom against already-captured
// outputs of earlier tasks.
func buildDisposableRequest(task TaskSpec, defaultHeaders map[string]string, providerConfigName string, outputs map[string]map[string]interface{}) (*composed.Unstructured, error) {
	body := map[string]interface{}{}
	for k, v := range task.Input {
		body[k] = v
	}
	for _, m := range task.InputFrom {
		src, ok := outputs[m.SourceTask]
		if !ok {
			return nil, fmt.Errorf("task %q has not completed yet (referenced by %q)", m.SourceTask, task.Name)
		}
		val, ok := getJSONField(src, m.SourceField)
		if !ok {
			return nil, fmt.Errorf("field %q not present in task %q's output", m.SourceField, m.SourceTask)
		}
		setJSONField(body, m.TargetField, val)
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	method := task.Method
	if method == "" {
		method = "POST"
	}
	headers := map[string][]string{"Content-Type": {"application/json"}}
	for k, v := range defaultHeaders {
		headers[k] = []string{v}
	}
	for k, v := range task.Headers {
		headers[k] = []string{v}
	}

	dr := composed.New()
	dr.SetAPIVersion("http.crossplane.io/v1alpha2")
	dr.SetKind("DisposableRequest")
	_ = dr.SetValue("spec.providerConfigRef.name", providerConfigName)
	_ = dr.SetValue("spec.forProvider.url", task.Endpoint)
	_ = dr.SetValue("spec.forProvider.method", method)
	_ = dr.SetValue("spec.forProvider.body", string(bodyBytes))
	headersIface := map[string]interface{}{}
	for k, v := range headers {
		vals := make([]interface{}, len(v))
		for i, s := range v {
			vals[i] = s
		}
		headersIface[k] = vals
	}
	_ = dr.SetValue("spec.forProvider.headers", headersIface)
	return dr, nil
}

// creates a DisposableRequest for the provider-http to call the workflow's notifyURL once the workflow has settled (succeeded or failed). This is done via a synthetic "notify" task that is only created once the workflow has completed, so that we don't risk double-firing the webhook from two places.
func buildNotifyRequest(workflowName, txID, phase string, tasks []TaskStatus, notifyURL, providerConfigName string) (*composed.Unstructured, error) {
	payload, err := json.Marshal(map[string]interface{}{
		"workflow":      workflowName,
		"transactionID": txID,
		"phase":         phase,
		"tasks":         tasks,
	})
	if err != nil {
		return nil, err
	}
	//creates new composed resource
	nr := composed.New()
	nr.SetAPIVersion("http.crossplane.io/v1alpha2")
	//sets the resource kind to DisposableRequest, which is a resource type that provider-http understands and can use to make HTTP requests.
	nr.SetKind("DisposableRequest")
	_ = nr.SetValue("spec.providerConfigRef.name", providerConfigName)
	_ = nr.SetValue("spec.forProvider.url", notifyURL)
	_ = nr.SetValue("spec.forProvider.method", "POST")
	_ = nr.SetValue("spec.forProvider.body", string(payload))
	_ = nr.SetValue("spec.forProvider.headers", map[string]interface{}{
		"Content-Type": []interface{}{"application/json"},
	})
	return nr, nil
}


// taskStatusFromObserved reads an already-created DisposableRequest's status
// and translates it into our TaskStatus shape
func taskStatusFromObserved(name string, res *composed.Unstructured) TaskStatus {
	errMsg, _ := res.GetString("status.error")
	if errMsg != "" {
		return TaskStatus{Name: name, Phase: "Failed", Error: errMsg}
	}

	code, _ := res.GetInteger("status.response.statusCode")
	bodyStr, _ := res.GetString("status.response.body")
	synced, _ := res.GetValue("status.synced")

	if code == 0 && bodyStr == "" {
		return TaskStatus{Name: name, Phase: "Running"}
	}

	c := int32(code)
	if code < 200 || code >= 300 {
		return TaskStatus{Name: name, Phase: "Failed", StatusCode: &c,
			Error: fmt.Sprintf("endpoint returned HTTP %d: %s", code, bodyStr)}
	}
	if syncedBool, ok := synced.(bool); ok && !syncedBool {
		return TaskStatus{Name: name, Phase: "Running", StatusCode: &c}
	}

	var out map[string]interface{}
	if err := json.Unmarshal([]byte(bodyStr), &out); err != nil {
		return TaskStatus{Name: name, Phase: "Failed", StatusCode: &c,
			Error: "response body is not a JSON object"}
	}
	return TaskStatus{Name: name, Phase: "Succeeded", StatusCode: &c, Output: out}
}

func depsReady(task TaskSpec, outputs map[string]map[string]interface{}) bool {
	for _, m := range task.InputFrom {
		if _, ok := outputs[m.SourceTask]; !ok {
			return false
		}
	}
	return true
}

func allDone(statuses []TaskStatus) bool {
	for _, s := range statuses {
		if s.Phase != "Succeeded" {
			return false
		}
	}
	return len(statuses) > 0
}

func toUnstructuredSlice(v interface{}) ([]interface{}, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out []interface{}
	err = json.Unmarshal(b, &out)
	return out, err
}

func getJSONField(data map[string]interface{}, path string) (interface{}, bool) {
	parts := strings.Split(path, ".")
	var cur interface{} = data
	for _, p := range parts {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		v, ok := m[p]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

func setJSONField(data map[string]interface{}, path string, value interface{}) {
	parts := strings.Split(path, ".")
	cur := data
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = value
			return
		}
		next, ok := cur[p].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			cur[p] = next
		}
		cur = next
	}
}