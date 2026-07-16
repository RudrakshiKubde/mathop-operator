package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var workflowGVR = schema.GroupVersionResource{
	Group: "example.crossplane.io", Version: "v1", Resource: "workflows",
}

func main() {
	ctx := context.Background()

	dbURL := mustEnv("DATABASE_URL")
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("cannot connect to postgres: %v", err)
	}
	defer pool.Close()
	if err := ensureSchema(ctx, pool); err != nil {
		log.Fatalf("cannot ensure schema: %v", err)
	}

	dyn, err := buildDynamicClient()
	if err != nil {
		log.Fatalf("cannot build kube client: %v", err)
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, 0)
	informer := factory.ForResource(workflowGVR).Informer()

	handle := func(obj interface{}) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return
		}
		if err := handleWorkflow(ctx, pool, u); err != nil {
			log.Printf("error handling workflow %s: %v", u.GetName(), err)
		}
	}
	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { handle(obj) },
		UpdateFunc: func(_, obj interface{}) { handle(obj) },
	})
	if err != nil {
		log.Fatalf("cannot add event handler: %v", err)
	}

	stop := make(chan struct{})
	factory.Start(stop)
	factory.WaitForCacheSync(stop)
	log.Println("watcher running, watching Workflow objects")
	select {}
}

// handleWorkflow persists a Workflow's current progress to Postgres.
// It does NOT fire the completion webhook — that's now handled by
// provider-http via the synthetic "notify" DisposableRequest that fn.go
// creates once the workflow settles, so this stays purely a data-recording
// concern (no risk of double-firing the webhook from two places).
func handleWorkflow(ctx context.Context, pool *pgxpool.Pool, u *unstructured.Unstructured) error {
	name := u.GetName()
	txID, _, _ := unstructured.NestedString(u.Object, "status", "transactionID")
	if txID == "" {
		return nil // function hasn't run yet
	}
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	notifyURL, _, _ := unstructured.NestedString(u.Object, "spec", "notifyURL")
	rawTasks, _, _ := unstructured.NestedSlice(u.Object, "status", "tasks")

	if err := upsertRun(ctx, pool, txID, name, notifyURL, phase); err != nil {
		return err
	}
	for _, rt := range rawTasks {
		t, ok := rt.(map[string]interface{})
		if !ok {
			continue
		}
		if err := upsertTask(ctx, pool, txID, t); err != nil {
			return err
		}
	}
	return nil
}

func buildDynamicClient() (dynamic.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = os.Getenv("HOME") + "/.kube/config"
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, err
		}
	}
	return dynamic.NewForConfig(cfg)
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing required env var %s", k)
	}
	return v
}

func ensureSchema(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS workflow_runs (
    transaction_id TEXT PRIMARY KEY,
    workflow_name  TEXT NOT NULL,
    notify_url     TEXT,
    phase          TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS workflow_tasks (
    transaction_id TEXT NOT NULL REFERENCES workflow_runs(transaction_id) ON DELETE CASCADE,
    task_name      TEXT NOT NULL,
    phase          TEXT NOT NULL,
    status_code    INT,
    output         JSONB,
    error          TEXT,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (transaction_id, task_name)
);`)
	return err
}

func upsertRun(ctx context.Context, pool *pgxpool.Pool, txID, name, notifyURL, phase string) error {
	_, err := pool.Exec(ctx, `
INSERT INTO workflow_runs (transaction_id, workflow_name, notify_url, phase, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (transaction_id) DO UPDATE
SET phase = EXCLUDED.phase, notify_url = EXCLUDED.notify_url, updated_at = now()`,
		txID, name, nullIfEmpty(notifyURL), phase)
	return err
}

func upsertTask(ctx context.Context, pool *pgxpool.Pool, txID string, t map[string]interface{}) error {
	name, _ := t["name"].(string)
	phase, _ := t["phase"].(string)
	var statusCode *int32
	if v, ok := t["statusCode"].(float64); ok {
		c := int32(v)
		statusCode = &c
	}
	var outputJSON []byte
	if out, ok := t["output"]; ok {
		outputJSON, _ = json.Marshal(out)
	}
	errMsg, _ := t["error"].(string)

	_, err := pool.Exec(ctx, `
INSERT INTO workflow_tasks (transaction_id, task_name, phase, status_code, output, error, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (transaction_id, task_name) DO UPDATE
SET phase = EXCLUDED.phase, status_code = EXCLUDED.status_code,
    output = EXCLUDED.output, error = EXCLUDED.error, updated_at = now()`,
		txID, name, phase, statusCode, nullIfEmptyBytes(outputJSON), nullIfEmpty(errMsg))
	return err
}

func nullIfEmpty(s string) interface{} {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func nullIfEmptyBytes(b []byte) interface{} {
	if len(b) == 0 {
		return nil
	}
	return b
}