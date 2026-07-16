# Crossplane Generic HTTP Workflow

This project runs an arbitrary, ordered chain of HTTP calls — with data
dependencies between steps — as a Crossplane Composite Resource (`Workflow`).
Each task, and the final completion notification, is executed by
`provider-http` as a real, independently-watchable Kubernetes object, giving
live per-task progress. A separate watcher process persists that progress to
PostgreSQL.

## What it does

You define a `Workflow` object with a list of tasks and a notification URL.
Later tasks can reference fields from earlier tasks' responses.

```yaml
apiVersion: example.crossplane.io/v1
kind: Workflow
metadata:
  name: example-xr
spec:
  notifyURL: https://webhook.site/<your-id>
  tasks:
  - name: add
    endpoint: http://mock-server.default.svc.cluster.local:9090/add
    input:
      x: 5
      y: 7
  - name: square
    endpoint: http://mock-server.default.svc.cluster.local:9090/square
    inputFrom:
    - sourceTask: add
      sourceField: sum
      targetField: value
```

Checking status directly from Kubernetes at any time:
```bash
kubectl get workflow example-xr -o jsonpath='{.status}' | python3 -m json.tool
```
```json
{
  "phase": "Succeeded",
  "transactionID": "8f4a9e9f-b156-41a1-b9c0-35c026815ad9",
  "tasks": [
    { "name": "add",    "phase": "Succeeded", "statusCode": 200, "output": { "sum": 12 } },
    { "name": "square", "phase": "Succeeded", "statusCode": 200, "output": { "square": 144 } }
  ]
}
```
The same information is queryable in Postgres **live, while the workflow is
still running** — not just after it finishes. Once every task settles,
`notifyURL` receives exactly one `POST` with that same JSON body.

Adding a new task later — a different endpoint, more steps, tasks with no
dependencies — needs no code or Composition changes: only `spec.tasks`.

## Architecture

```
 ┌─────────────┐   spec.tasks    ┌──────────────────┐
 │  Workflow    │ ───────────────▶│ function-httptask │
 │  (your XR)   │                 │ (Composition Fn)  │
 └──────┬───────┘                 └─────────┬─────────┘
        │ status.tasks / phase / txID       │ creates one
        │ (written back by the function)    │ DisposableRequest
        ▼                                   ▼ per task, plus one
 ┌──────────────┐                 ┌───────────────────┐  synthetic "notify"
 │   watcher    │◀── watches ─────│  DisposableRequest │◀─ once everything
 │ (Go program) │                  │  (real k8s object) │   settles
 └──────┬───────┘                 └─────────┬─────────┘
        │ writes progress                   │ provider-http executes
        ▼ (live, per change)                 ▼ every one of these,
 ┌──────────────┐                    including the notify call
 │  PostgreSQL  │                  ┌──────────────────┐
 └──────────────┘                  │  notifyURL (POST) │
                                    └──────────────────┘
```

- **`function-httptask`** doesn't make HTTP calls itself. Each reconcile, it
  looks at `spec.tasks` and the currently observed `DisposableRequest`
  objects, and decides: create the next task's request (once its
  dependencies have answered), or read back an existing one's result. Once
  every real task has settled, it also creates one more, synthetic
  `DisposableRequest` — the completion webhook call — using the same
  mechanism as everything else. It always rebuilds each `DisposableRequest`'s
  spec deterministically from source data (never echoes back the observed
  object wholesale, which caused an earlier reconcile-storm bug).
- **`provider-http`** (crossplane-contrib) executes every one of these HTTP
  calls, task or notification alike, as a `DisposableRequest` managed
  resource.
- **Real-time compositions** (`--enable-realtime-compositions`) means the
  `Workflow`'s own status reacts the instant a `DisposableRequest` changes —
  event-driven, not polled.
- **The watcher** is a separate Go program watching `Workflow` objects via
  `client-go`. On every change, it writes a row per task and one row per run
  into Postgres — live, as the workflow progresses. It no longer sends the
  completion webhook itself; that moved into `provider-http` (see below), so
  the watcher's only job now is persistence.

## Why the completion webhook moved out of the watcher

Originally the watcher both persisted progress *and* fired the completion
webhook once `phase` settled. That meant a webhook could, in principle, be
sent twice — once from the watcher, and (after this change) once from the
`notify` `DisposableRequest` — if both were left in place. Since firing an
HTTP request is exactly what `provider-http` already does, the notification
was moved entirely into the declarative Crossplane world: `fn.go` creates a
`notify` `DisposableRequest` once every task has settled, with the full task
rollup as its body. The watcher's webhook-sending code (`sendWebhook`,
`isNotified`, `markNotified`) was removed; it now only writes to Postgres.

## Providers considered, and why two were ruled out

- **`function-sequencer`** — a real Crossplane function for delaying
  resource creation until another resource is ready. Ruled out: its
  sequencing rules are static, declared once in `composition.yaml` at
  authoring time. Your task dependencies are dynamic, defined per-instance
  in `spec.tasks`, so this function can't express them — the dependency
  gating in `fn.go` (`depsReady`/`blocked`) stays custom for that reason.
- **`provider-sql`** — manages database infrastructure (databases, roles,
  grants) as declarative resources. Ruled out for persisting task progress:
  writing arbitrary application data rows isn't infrastructure
  provisioning, so this isn't the right tool for what the watcher does.
- **`provider-kubernetes`** — manages arbitrary raw Kubernetes manifests as
  composed resources (e.g., applying a Deployment as part of a
  Composition). Ruled out: nothing in this project's actual problem (HTTP
  orchestration, chaining, persistence, notification) is "apply a
  manifest as a side effect." Deploying the watcher itself via plain
  `kubectl apply` remains the right approach.
- **`provider-http`** — the one that *did* fit, used for both task
  execution and the completion webhook.

## Repository layout

```
mathop-operator/
├── function-httptask/
│   ├── fn.go                       # orchestrates DisposableRequest objects,
│   │                                #   including the synthetic notify call
│   ├── input/v1beta1/input.go      # providerConfigName, defaultHeaders
│   ├── package/                    # Function package metadata + CRD schema
│   ├── Dockerfile                  # builds the function's runtime image
│   ├── watcher/
│   │   ├── main.go                 # watches Workflow objects, persists
│   │   │                           #   progress to Postgres (no webhook)
│   │   ├── go.mod
│   │   └── Dockerfile
│   └── example/
│       ├── functions.yaml
│       ├── composition.yaml        # single, permanent pipeline step
│       └── workflow.yaml           # example Workflow instance
│
├── mock_server.py                  # fake downstream API: /add, /square
├── Dockerfile                      # includes the `mockserver` build stage
│
└── deploy/
    ├── xrd.yaml                    # Workflow schema
    ├── mock-server.yaml
    ├── provider-http.yaml          # Provider + ProviderConfig
    ├── watcher.yaml                # watcher Deployment + RBAC
    └── watcher-db-secret.yaml      # DATABASE_URL for the watcher
```

## Setup, from scratch

### 1. Prerequisites
- A kind cluster with Crossplane installed
- A local image registry (`kind-registry`, resolvable in-cluster as
  `kind-registry.local:5000`, host-pushed via `localhost:5001` — always use
  `:5000` in any manifest the cluster itself pulls from, `:5001` only in
  `docker push` commands run from your host)
- A reachable PostgreSQL instance (this project uses an existing native
  Windows Postgres/PostGIS install, reached from the cluster via
  `host.docker.internal`)

### 2. Enable real-time compositions
```bash
kubectl edit deployment crossplane -n crossplane-system
# add --enable-realtime-compositions under spec.template.spec.containers[0].args
```

### 3. Install provider-http
```bash
kubectl apply -f deploy/provider-http.yaml
kubectl wait --for=condition=Healthy provider.pkg.crossplane.io/provider-http --timeout=120s
```

### 4. Apply the XRD
```bash
kubectl apply -f deploy/xrd.yaml
```

### 5. Build and deploy the mock server
```bash
docker build --target mockserver -t mock-server:dev -f Dockerfile .
kind load docker-image mock-server:dev --name <your-cluster-name>
kubectl apply -f deploy/mock-server.yaml
```

### 6. Build and push the function package
```bash
cd function-httptask
docker build -t localhost:5001/function-httptask-runtime:v0.4.0 .
docker push localhost:5001/function-httptask-runtime:v0.4.0
crossplane xpkg build --package-root=package \
  --embed-runtime-image=localhost:5001/function-httptask-runtime:v0.4.0 \
  -o function-httptask.xpkg
crossplane xpkg push localhost:5001/function-httptask:v0.4.0 -f function-httptask.xpkg
```
```bash
kubectl apply -f example/functions.yaml   # spec.package: kind-registry.local:5000/function-httptask:v0.4.0
kubectl apply -f example/composition.yaml
```

### 7. Create the watcher's database + user
```sql
CREATE USER workflow_watcher WITH LOGIN PASSWORD '<password>';
CREATE DATABASE workflows OWNER workflow_watcher;
```
Ensure `listen_addresses = '*'` and an appropriate `pg_hba.conf` entry allow
the connection, then restart Postgres. The watcher creates its own tables
(`workflow_runs`, `workflow_tasks`) on first startup.

### 8. Build and deploy the watcher
```bash
cd watcher
docker build -t localhost:5001/workflow-watcher:v0.2.0 .
docker push localhost:5001/workflow-watcher:v0.2.0
```
```bash
kubectl apply -f ../../deploy/watcher-db-secret.yaml
kubectl apply -f ../../deploy/watcher.yaml   # image: kind-registry.local:5000/workflow-watcher:v0.2.0
```

### 9. Run a workflow
```bash
kubectl apply -f example/workflow.yaml
kubectl get workflow example-xr -o jsonpath='{.status}' | python3 -m json.tool
```

## Verifying it's working

- `kubectl get disposablerequests` — expect three objects over a full run:
  one per real task, plus one synthetic `notify` request once everything
  settles.
- In Postgres, mid-run (not just at the end):
  ```sql
  SELECT task_name, phase, updated_at FROM workflow_tasks
  WHERE transaction_id = '<transaction_id>' ORDER BY updated_at;
  ```
  Some tasks should already show `Succeeded` with real timestamps while
  others are still `Pending` — confirms live, incremental persistence.
- Your `notifyURL` endpoint receives exactly one `POST`, sent by
  `provider-http`, once `phase` settles.

## Current status

- ✅ End-to-end verified: `add` → `square` chain executes, with per-task
  results visible live as real Kubernetes objects, persisted to Postgres as
  they happen, and a completion webhook fired via `provider-http`.
- ✅ Generic: new tasks require only editing the `Workflow` object.
- ✅ Failure handling: a failed task is recorded with its error; later
  tasks are marked `Skipped`.
- ✅ Real-time, event-driven progress via `--enable-realtime-compositions`.
- ✅ Notification is declarative (a `DisposableRequest`), not custom code.

## Known limitations

- **Sequential task creation**: `fn.go` creates at most one new task's
  `DisposableRequest` per reconcile; independent tasks aren't run in
  parallel yet.
- **No retries**: a failed task stays `Failed` permanently; delete and
  recreate the `Workflow` to retry.
- **Postgres access**: a single manually provisioned instance, plaintext
  connection string in a Secret — no TLS, pooling tuning, or HA.
- **Short bursts of reconciles are expected**: a `DisposableRequest` moving
  through several real status transitions in quick succession produces a
  short burst of `RunFunction` calls — this is real-time compositions
  working correctly, distinct from the earlier reconcile-storm bug (which
  produced continuous, gapless invocations with no settling).