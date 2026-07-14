# Crossplane Generic HTTP Workflow

This project runs an arbitrary, ordered chain of HTTP calls — with data
dependencies between steps — as a Crossplane Composite Resource (`Workflow`).
It's implemented as a Crossplane Composition Function (`function-httptask`)
backed by a mock HTTP service (`mock-server`, from `mathop-operator`) for
testing.

## What it does

You define a `Workflow` object with a list of tasks. Each task is an HTTP
call; later tasks can reference fields from earlier tasks' responses. The
function executes the whole chain, stops on the first failure, and reports
per-task results plus an overall status back onto the `Workflow` object.

Example — add two numbers, then square the result:

```yaml
apiVersion: example.crossplane.io/v1
kind: Workflow
metadata:
  name: example-xr
spec:
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

Checking the result:
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

Adding a new task in the future — a different endpoint, a longer chain, tasks
with no dependencies at all — requires **no code changes and no Composition
changes**. It's purely a matter of editing the `Workflow` object's
`spec.tasks`.

## Why it's built this way

This started from an existing Kubebuilder operator (`mathop-operator`) that
had its own `Workflow`/`HTTPTask` CRDs and a reconciler that ran tasks and
tracked dependencies. The goal here was to re-implement that same idea —
declarative, ordered, dependency-aware HTTP task execution — on top of
Crossplane instead of a custom operator:

| Concept | mathop-operator | This project |
|---|---|---|
| The object you create | `Workflow` CR | `Workflow` XR (Crossplane Composite Resource) |
| What executes the tasks | `WorkflowReconciler` (a controller loop) | `function-httptask` (a Crossplane Composition Function) |
| Where results are reported | `Workflow.status` | `Workflow.status` (same idea) |

Crucially, the Composition itself carries **no business logic** — it's a
single, permanent pipeline step. All the "which tasks, in what order, with
what dependencies" logic lives entirely in the `Workflow` instance you apply,
which is what makes it generic.

## Repository layout

```
mathop-operator/
├── function-httptask/              # the Crossplane Composition Function
│   ├── fn.go                       # core logic: reads spec.tasks, executes
│   │                                #   each HTTP call in order, writes
│   │                                #   status.tasks / status.phase /
│   │                                #   status.transactionID
│   ├── input/v1beta1/input.go      # function's own config (currently just
│   │                                #   optional defaultHeaders)
│   ├── package/
│   │   ├── crossplane.yaml         # Function package metadata
│   │   └── input/...yaml           # generated CRD schema for Input
│   ├── Dockerfile                  # builds the function's runtime image
│   └── example/
│       ├── functions.yaml          # registers the Function in-cluster
│       ├── composition.yaml        # the (single-step, generic) Composition
│       └── workflow.yaml           # example Workflow instance (add→square)
│
├── mock_server.py                  # fake downstream API: /add, /square
├── Dockerfile                      # (fixed) includes a named `mockserver`
│                                    #   build stage for containerizing it
│
└── deploy/
    ├── xrd.yaml                    # CompositeResourceDefinition — registers
    │                                #   the Workflow kind and its schema
    └── mock-server.yaml            # Deployment + Service for mock_server.py
```

## How a `Workflow` gets executed

1. `kubectl apply` the `Workflow` object.
2. Crossplane sees it, looks up its `Composition`, and runs the one pipeline
   step, calling `function-httptask`.
3. `fn.go` reads `spec.tasks` off the `Workflow`.
4. For each task, in order:
   - builds the outgoing HTTP request body, merging in any `inputFrom`
     values pulled from an **earlier** task's already-captured response,
   - makes the call,
   - records the result (`Succeeded`/`Failed`, HTTP status code, parsed
     JSON output, or error message).
5. If a task fails, later tasks are marked `Skipped` and are not attempted.
6. The full set of per-task results, the overall `phase`
   (`Succeeded`/`Failed`), and a `transactionID` (the `Workflow` object's own
   Kubernetes UID) are written back to `status`.

## Setup, from scratch

### 1. Prerequisites
- A kind cluster with Crossplane installed
- A local image registry reachable from the kind cluster (this project uses
  the standard "kind + local registry" pattern — a `registry:2` container
  named `kind-registry`, resolvable from inside the cluster as
  `kind-registry.local:5000`)

### 2. Apply the XRD
```bash
kubectl apply -f deploy/xrd.yaml
```

### 3. Build and deploy the mock server
```bash
docker build --target mockserver -t mock-server:dev -f Dockerfile .
kind load docker-image mock-server:dev --name <your-cluster-name>
kubectl apply -f deploy/mock-server.yaml
```

### 4. Build and push the function package
```bash
cd function-httptask

# runtime image (the actual function binary, containerized)
docker build -t localhost:5001/function-httptask-runtime:v0.2.0 .
docker push localhost:5001/function-httptask-runtime:v0.2.0

# the Crossplane package itself (metadata + embedded runtime image)
crossplane xpkg build \
  --package-root=package \
  --embed-runtime-image=localhost:5001/function-httptask-runtime:v0.2.0 \
  -o function-httptask.xpkg
crossplane xpkg push localhost:5001/function-httptask:v0.2.0 -f function-httptask.xpkg
```
Note: pushing uses `localhost:5001` (the registry's host-mapped port, from
your machine); the `Function` object below uses `kind-registry.local:5000`
(the same registry, resolved from inside the cluster) — both addresses point
at the same underlying registry container.

### 5. Register the Function and Composition
```bash
kubectl apply -f example/functions.yaml     # spec.package: kind-registry.local:5000/function-httptask:v0.2.0
kubectl apply -f example/composition.yaml
```

### 6. Run a workflow
```bash
kubectl apply -f example/workflow.yaml
kubectl get workflow example-xr -o jsonpath='{.status}' | python3 -m json.tool
```

## Current status

- ✅ End-to-end verified working: `add` → `square` chain executes correctly,
  with `sum: 12` feeding into `square` to produce `square: 144`.
- ✅ Generic: adding, removing, or reordering tasks only requires editing the
  `Workflow` object — no code or Composition changes.
- ✅ Failure handling: a failed task is recorded with its HTTP status and
  error message; later tasks are marked `Skipped` rather than attempted.
- ✅ Each `Workflow` gets a unique `transactionID` (its Kubernetes object UID).

## Known limitations / not yet built

- Execution is synchronous — all tasks in a `Workflow` run within a single
  function invocation. There's no intermediate "task 2 of 5 is currently
  running" observable state; you see the final result once the whole chain
  completes (or fails).
- No retry logic — a failed task stays `Failed` permanently on that
  `Workflow` object; you have to delete and recreate it to try again.
- No dashboard or HTTP endpoint for checking status — status is currently
  only readable via `kubectl get workflow ... -o jsonpath=...`.