package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/crossplane/function-sdk-go/errors"
	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/response"

	"github.com/crossplane/function-httptask/input/v1beta1"
)

type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer

	log        logging.Logger
	httpClient *http.Client
}

// TaskSpec is one step in the XR's spec.tasks list.
type TaskSpec struct {
	Name      string                 `json:"name"`
	Endpoint  string                 `json:"endpoint"`
	Method    string                 `json:"method,omitempty"`
	Headers   map[string]string      `json:"headers,omitempty"`
	Input     map[string]interface{} `json:"input,omitempty"`
	InputFrom []FieldMapping         `json:"inputFrom,omitempty"`
}

// FieldMapping pulls one field out of an EARLIER task's output (in this
// same run) and places it into THIS task's outgoing request body.
type FieldMapping struct {
	SourceTask  string `json:"sourceTask"`
	SourceField string `json:"sourceField"`
	TargetField string `json:"targetField"`
}

// TaskStatus is written back to the XR's status.tasks for observability.
type TaskStatus struct {
	Name       string                 `json:"name"`
	Phase      string                 `json:"phase"`
	StatusCode *int32                 `json:"statusCode,omitempty"`
	Output     map[string]interface{} `json:"output,omitempty"`
	Error      string                 `json:"error,omitempty"`
}

func (f *Function) RunFunction(ctx context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	f.log.Info("Running function", "tag", req.GetMeta().GetTag())
	rsp := response.To(req, response.DefaultTTL)

	in := &v1beta1.Input{}
	if err := request.GetInput(req, in); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input from %T", req))
		return rsp, nil
	}

	oxr, err := request.GetObservedCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get observed composite resource"))
		return rsp, nil
	}

	rawTasks, found, err := unstructured.NestedSlice(oxr.Resource.UnstructuredContent(), "spec", "tasks")
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot read spec.tasks"))
		return rsp, nil
	}
	if !found || len(rawTasks) == 0 {
		response.Fatal(rsp, errors.New("spec.tasks must contain at least one task"))
		return rsp, nil
	}

	tasksJSON, err := json.Marshal(rawTasks)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot marshal spec.tasks"))
		return rsp, nil
	}
	var tasks []TaskSpec
	if err := json.Unmarshal(tasksJSON, &tasks); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "spec.tasks does not match the expected shape"))
		return rsp, nil
	}

	if f.httpClient == nil {
		f.httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	outputs := map[string]map[string]interface{}{} // task name -> parsed response
	statuses := make([]TaskStatus, 0, len(tasks))
	failed := false

	for _, task := range tasks {
		if failed {
			statuses = append(statuses, TaskStatus{Name: task.Name, Phase: "Pending"})
			continue
		}

		body := map[string]interface{}{}
		for k, v := range task.Input {
			body[k] = v
		}

		mappingErr := ""
		for _, m := range task.InputFrom {
			src, ok := outputs[m.SourceTask]
			if !ok {
				mappingErr = fmt.Sprintf("task %q has not run yet (referenced by %q)", m.SourceTask, task.Name)
				break
			}
			val, ok := getJSONField(src, m.SourceField)
			if !ok {
				mappingErr = fmt.Sprintf("field %q not present in task %q's output", m.SourceField, m.SourceTask)
				break
			}
			setJSONField(body, m.TargetField, val)
		}
		if mappingErr != "" {
			statuses = append(statuses, TaskStatus{Name: task.Name, Phase: "Failed", Error: mappingErr})
			failed = true
			continue
		}

		bodyBytes, err := json.Marshal(body)
		if err != nil {
			statuses = append(statuses, TaskStatus{Name: task.Name, Phase: "Failed", Error: err.Error()})
			failed = true
			continue
		}

		method := task.Method
		if method == "" {
			method = http.MethodPost
		}
		httpReq, err := http.NewRequestWithContext(ctx, method, task.Endpoint, bytes.NewReader(bodyBytes))
		if err != nil {
			statuses = append(statuses, TaskStatus{Name: task.Name, Phase: "Failed", Error: err.Error()})
			failed = true
			continue
		}
		httpReq.Header.Set("Content-Type", "application/json")
		for k, v := range in.DefaultHeaders {
			httpReq.Header.Set(k, v)
		}
		for k, v := range task.Headers {
			httpReq.Header.Set(k, v)
		}

		httpRsp, err := f.httpClient.Do(httpReq)
		if err != nil {
			statuses = append(statuses, TaskStatus{Name: task.Name, Phase: "Failed",
				Error: fmt.Sprintf("HTTP call to %s failed: %v", task.Endpoint, err)})
			failed = true
			continue
		}
		respBytes, readErr := io.ReadAll(httpRsp.Body)
		httpRsp.Body.Close()
		if readErr != nil {
			statuses = append(statuses, TaskStatus{Name: task.Name, Phase: "Failed", Error: readErr.Error()})
			failed = true
			continue
		}

		code := int32(httpRsp.StatusCode)
		if httpRsp.StatusCode < 200 || httpRsp.StatusCode >= 300 {
			statuses = append(statuses, TaskStatus{Name: task.Name, Phase: "Failed", StatusCode: &code,
				Error: fmt.Sprintf("%s returned HTTP %d: %s", task.Endpoint, httpRsp.StatusCode, string(respBytes))})
			failed = true
			continue
		}

		var outputMap map[string]interface{}
		if err := json.Unmarshal(respBytes, &outputMap); err != nil {
			statuses = append(statuses, TaskStatus{Name: task.Name, Phase: "Failed", StatusCode: &code,
				Error: fmt.Sprintf("response from %s is not a JSON object", task.Endpoint)})
			failed = true
			continue
		}

		outputs[task.Name] = outputMap
		statuses = append(statuses, TaskStatus{Name: task.Name, Phase: "Succeeded", StatusCode: &code, Output: outputMap})
		f.log.Info("called endpoint", "task", task.Name, "endpoint", task.Endpoint, "statusCode", httpRsp.StatusCode)
	}

	// --- write status.tasks / status.phase back onto the XR ---
	dxr, err := request.GetDesiredCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get desired composite resource"))
		return rsp, nil
	}
	statusesRaw, err := toUnstructuredSlice(statuses)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot convert task statuses"))
		return rsp, nil
	}
	phase := "Succeeded"
	if failed {
		phase = "Failed"
	}
	content := dxr.Resource.UnstructuredContent()
	_ = unstructured.SetNestedField(content, phase, "status", "phase")
	_ = unstructured.SetNestedSlice(content, statusesRaw, "status", "tasks")
	if err := response.SetDesiredCompositeResource(rsp, dxr); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot set desired composite resource"))
		return rsp, nil
	}

	if failed {
		response.ConditionFalse(rsp, "FunctionSuccess", "TaskFailed").
			WithMessage("one or more tasks failed, see status.tasks for details").
			TargetCompositeAndClaim()
		response.Fatal(rsp, errors.New("one or more tasks failed"))
		return rsp, nil
	}

	response.ConditionTrue(rsp, "FunctionSuccess", "Computed").
		WithMessage(fmt.Sprintf("ran %d task(s) successfully", len(tasks))).
		TargetCompositeAndClaim()

	return rsp, nil
}

func toUnstructuredSlice(v interface{}) ([]interface{}, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out []interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// --- dot-path helpers, unchanged ---

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