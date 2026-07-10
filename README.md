# mathop-operator

A Kubernetes operator built with [Kubebuilder](https://book.kubebuilder.io/), evolved from a
fixed two-CRD demo into a generic, HTTP-driven workflow engine with live progress tracking —
modeled after Crossplane's composition pattern, where one resource's output becomes another's
input, resolved entirely at runtime.

## The four iterations, in order

1. **Typed, in-cluster computation** — `Add`/`Subtract` compute directly in Go; `Square` reads a
   referenced resource's result and squares it, chained via a field indexer + watch.
2. **Generic, runtime-discovered chaining** — `Square` reworked to reference *any* resource kind
   generically (`unstructured.Unstructured` + a `DynamicWatcher` that registers watches for new
   kinds the first time they're referenced) — no code change needed for a new Task kind.
3. **HTTP-driven tasks** — `HTTPTask`: a single generic kind representing "call this URL with
   this JSON, optionally built from other tasks' outputs." The operator stops computing anything
   itself; the actual operation lives behind a real HTTP endpoint.
4. **Workflows with live status (current)** — `Workflow`: groups multiple `HTTPTask` steps under
   one object, with a transaction ID, a live-updating dashboard URL, and an optional one-shot
   webhook notification on completion.

This README focuses on iteration 4; the earlier layers remain in the repo and are functional.

## What `Workflow` does

- **`spec.tasks`** — an embedded, ordered list of steps (endpoint, method, headers, input, and
  `inputFrom` mappings referencing *other steps in the same workflow* by name)
- **`spec.notifyURL`** — optional; if set, the operator `POST`s the final status here exactly
  once, the moment the workflow reaches `Succeeded` or `Failed`
- **`status.transactionID`** — a stable, unique ID (the object's own UID) for external correlation
- **`status.dashboardURL`** — a live, human-visitable URL showing real-time per-task progress
- **`status.phase`** / **`status.tasks[].phase`** — `Pending` → `Running` → `Succeeded`/`Failed`,
  per step and overall, with the specific error message attached to whichever step failed

```
Workflow "demo"
  ├─ step "add"     → creates/owns HTTPTask "demo-add"
  └─ step "square"  → creates/owns HTTPTask "demo-square", inputFrom: add.sum → value
```

`WorkflowReconciler` doesn't compute anything itself — it materializes a real, owned `HTTPTask`
child object per embedded step (reusing all of `HTTPTask`'s existing logic unchanged: hashing,
skip-if-unchanged, chaining, finalizers), then aggregates each child's live status back into
`workflow.status`.

## Architecture

```
api/v1alpha1/
  workflow_types.go       Workflow CRD: spec.tasks[]/notifyURL, status.transactionID/
                            dashboardURL/phase/tasks[]/notifiedPhase/conditions

internal/controller/
  workflow_controller.go    WorkflowReconciler — creates/owns child HTTPTasks, aggregates
                              status, sends the one-shot notifyURL callback
  httptask_controller.go     (updated) now writes a "Running" status BEFORE making the HTTP
                               call, not just after — makes in-flight state actually observable

internal/statusserver/
  server.go                  Embedded HTTP server (runs inside the operator process):
                               GET /workflows/{id}            → live HTML dashboard (SSE-driven)
                               GET /api/workflows/{id}         → JSON snapshot
                               GET /api/workflows/stream/{id}  → Server-Sent Events, push-based

watch_workflow.py             Terminal equivalent of the browser dashboard — connects to the
                                same SSE stream, prints live task-by-task progress
notify_listener.py             Mock webhook receiver for testing spec.notifyURL
```

### `WorkflowReconciler.Reconcile`, step by step

1. Sets `status.transactionID` (= object UID) and `status.dashboardURL` if not already set.
2. For each embedded step: builds a `FieldMapping` translating `sourceStep` (a name within this
   workflow) into a full `HTTPTask` `SourceRef`, then `controllerutil.CreateOrUpdate`s a child
   `HTTPTask` named `<workflow>-<step>`, with `ctrl.SetControllerReference` so Kubernetes
   garbage-collects children automatically when the `Workflow` is deleted.
3. Reads each child's `Ready` condition and maps it to a per-step phase:
   - no condition yet → `Pending`
   - `Reason: Running` → `Running` (see below)
   - `Reason: WaitingOnSource` → `Pending`
   - `Status: True` → `Succeeded`
   - anything else → `Failed`, with `error` set to `"<Reason>: <Message>"`
4. Computes the **overall** phase only *after* the full loop, from two booleans (`hasFailed`,
   `hasPending`) collected across every step — not by letting the last-processed step overwrite
   an earlier one. `Failed` beats `Running` beats `Succeeded`, regardless of step order.
5. If the overall phase is now terminal (`Succeeded`/`Failed`) and hasn't already been notified
   for that exact phase (`status.notifiedPhase`), `POST`s the full `status` to `spec.notifyURL`
   once. A failed notify attempt retries in 10s without re-running the task loop.

`.Owns(&HTTPTask{})` re-triggers the `Workflow`'s reconcile whenever any child's status changes —
this is what keeps the aggregated view current as each step progresses.

### Making "Running" real

A synchronous HTTP call has no natural "in progress" state to observe from outside — it's either
not started or already done. `httptask_controller.go` now writes a `Ready=False, Reason=Running`
condition via its own `Status().Update()` **before** calling `r.HTTPClient.Do(...)`, so that write
lands in etcd and becomes visible before the (possibly slow) call even begins — not a cosmetic
label applied after the fact.

### The live dashboard

`/workflows/{id}` serves a small HTML page that: fetches the current status once immediately
(`fetch('/api/workflows/...')`), then opens a `Server-Sent Events` connection to
`/api/workflows/stream/{id}` and re-renders on every pushed update. The server-side stream
handler polls the workflow object every second, only pushes a frame when the JSON actually
changed, and closes the connection once the workflow reaches a terminal phase — the page never
needs a manual refresh.

`watch_workflow.py` is the same idea for a terminal: it opens the same SSE endpoint directly and
prints the same task-by-task breakdown as it arrives.

## Prerequisites

Same as before — Go 1.21+, Docker, kubectl, kind, kubebuilder, Python 3.

## Getting started

```bash
kind create cluster --name mathop
make manifests generate
make install
make run   # leave running
```

Start the demo HTTP endpoint (from the `HTTPTask` iteration — `mock_server.py`, with the
missing-field 400 check) and, separately, the mock notification receiver:

```bash
python3 mock_server.py        # port 9090, leave running
python3 notify_listener.py    # port 9091, leave running
```

Create a workflow with a deliberately broken first step, so you can watch the full
Pending→Running→Failed sequence live:

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: math.example.com/v1alpha1
kind: Workflow
metadata:
  name: notify-fail-demo
spec:
  notifyURL: http://127.0.0.1:9091/webhook
  tasks:
  - name: add
    endpoint: http://127.0.0.1:9090/add
    input:
      x: 5
  - name: square
    endpoint: http://127.0.0.1:9090/square
    inputFrom:
    - sourceStep: add
      sourceField: sum
      targetField: value
EOF
```

Get the transaction ID and watch it live, two ways:

```bash
TXID=$(kubectl get workflow notify-fail-demo -o jsonpath='{.status.transactionID}')
echo "$TXID"

# in a browser:
echo "http://localhost:8090/workflows/$TXID"

# or in a terminal:
python3 watch_workflow.py "$TXID"
```

Watch `notify_listener.py`'s terminal too — once the workflow reaches `Failed`, it prints the
same breakdown, delivered via the one-shot `notifyURL` callback, independent of whether you were
watching the dashboard or not.

Fix the input and reapply (delete first, to reset `notifiedPhase` for a clean re-test) to see the
success path end to end, including a fresh notification for the new outcome.

## A note on the health-probe port

`main.go`'s health/readiness probe (`healthz`/`readyz`) defaults to `:8081`, separate from the
dashboard's `:8090`. They're easy to confuse when testing — if a `curl` to the dashboard port
returns a bare `404 page not found`, double-check you're hitting `8090`, not `8081`.

## Running the automated test suite / deploying in-cluster

Unchanged from before — see `make test`, `make docker-build` / `make deploy`. As before, an
in-cluster pod can't reach a `127.0.0.1` process on your WSL host; this workflow demo assumes
`make run` (operator as a local process) alongside locally-run mock servers.

## Built with

`kubebuilder`, `sigs.k8s.io/controller-runtime`, Go's `net/http` (both for outbound task calls and
the embedded dashboard/SSE server), `html/template` for the dashboard page.