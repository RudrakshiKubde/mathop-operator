package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	mathv1alpha1 "github.com/RudrakshiKubde/mathop-operator/api/v1alpha1"
)

func taskReferencedByOthers(ctx context.Context, c client.Client, namespace, apiVersion, kind, name string) (bool, error) {
	var tasks mathv1alpha1.HTTPTaskList
	if err := c.List(ctx, &tasks, client.InNamespace(namespace),
		client.MatchingFields{taskSourceRefIndexKey: sourceRefIndexValue(apiVersion, kind, name)}); err != nil {
		return false, fmt.Errorf("listing tasks: %w", err)
	}
	return len(tasks.Items) > 0, nil
}
