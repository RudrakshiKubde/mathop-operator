package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	mathv1alpha1 "github.com/RudrakshiKubde/mathop-operator/api/v1alpha1"
)

const inUseFinalizer = "math.example.com/in-use-protection"

// referencedBySquare reports whether any Square in the given namespace
// still points at the resource identified by (apiVersion, kind, name).
func referencedBySquare(ctx context.Context, c client.Client, namespace, apiVersion, kind, name string) (bool, error) {
	var squares mathv1alpha1.SquareList
	if err := c.List(ctx, &squares,
		client.InNamespace(namespace),
		client.MatchingFields{sourceRefIndexKey: sourceRefIndexValue(apiVersion, kind, name)},
	); err != nil {
		return false, fmt.Errorf("listing squares: %w", err)
	}
	return len(squares.Items) > 0, nil
}
