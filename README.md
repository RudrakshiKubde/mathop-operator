# mathop-operator

A Kubernetes operator built with [Kubebuilder](https://book.kubebuilder.io/), evolved through
three iterations from a fixed two-CRD demo into a generic, HTTP-driven workflow engine — modeled
after Crossplane's composition pattern, where one resource's output becomes another's input,
resolved entirely at runtime.

## The three iterations, in order

1. **Typed, in-cluster computation** — `Add` and `Subtract` compute directly in Go
   (`status.result = x + y`), and `Square` reads a specific referenced resource's result and
   squares it. Chaining works via a field indexer + watch, so editing `Add` automatically
   recomputes any `Square` that depends on it.
2. **Generic, runtime-discovered chaining** — `Square` was reworked to reference *any* resource
   kind (`spec.sourceRef {apiVersion, kind, name}`), fetched as `unstructured.Unstructured`
   rather than a typed Go struct, with a `DynamicWatcher` that registers watches for new kinds
   the first time they're referenced — no code change or redeploy needed to support a brand-new
   Task kind.
3. **HTTP-driven workflow (current)** — the operator no longer computes anything itself. A
   single generic `HTTPTask` kind represents "call this URL with this JSON, optionally built
   from other tasks' outputs." The actual operation (add, square, or anything a vendor exposes)
   lives outside the cluster, behind an HTTP endpoint.

All three layers are present in this repo; the sections below cover `HTTPTask`, the current
focus, in depth, with the earlier layers summarized after.

## What `HTTPTask` does

- **`spec.endpoint`** — the URL to call
- **`spec.method`** — HTTP method, defaults to `POST`
- **`spec.headers`** — custom headers (e.g. API keys, auth tokens)
- **`spec.input`** — a raw, schema-less JSON object — the base request body
- **`spec.inputFrom`** — a list of field mappings, each pulling one field out of another
  `HTTPTask`'s `status.output` and inserting it into this task's request body
- **`status.output`** — the raw JSON response from the last successful call
- **`status.statusCode`**, **`status.conditions`** — call result and human-readable state

```
task-add   (input: {x:5,y:7})        --POST /add-->     {"sum":12}
task-square (inputFrom: task-add.sum --> value) --POST /square--> {"square":144}
```

A task with no `inputFrom` is a "root" task — any number of these can exist side by side, each
starting its own chain. A task's `inputFrom` can reference *any* prior task by name, not
necessarily the one immediately before it, and can pull from more than one source at once.

## Architecture

```
api/v1alpha1/
  httptask_types.go     HTTPTask CRD: spec.endpoint/method/headers/input/inputFrom,
                          status.output/statusCode/observedInputHash/conditions,
                          plus FieldMapping and SourceReference

internal/controller/
  httptask_controller.go   HTTPTaskReconciler — the orchestration logic (see below)
  jsonpath.go                Tiny dot-path get/set helpers for arbitrary JSON (no arrays)
  task_refcheck.go            "Is this task still referenced by another?" — for its finalizer
  task_transfer.go             Shared sourceRefIndexValue() helper (also used by Square below);
                                 also still holds FetchTaskResult/DynamicWatcher from iteration 2,
                                 unused by HTTPTask itself since there's only one Kind now
```

### `HTTPTaskReconciler.Reconcile`, step by step

1. **Finalizer bookkeeping** — attaches `math.example.com/task-in-use-protection` on creation;
   on deletion, blocks (requeues every 5s) while any other task's `inputFrom` still references
   this one.
2. **Build the request body** — starts from `spec.input`, then for each `inputFrom` entry:
   fetches the referenced task (plain typed `Get`, since every task shares one Kind), reads the
   named field out of its `status.output` via `jsonpath.go`, writes it into this task's body. If
   a dependency has no output yet, reports `WaitingOnSource` and stops — the watch below
   re-triggers this once that dependency produces output.
3. **Hashes the resolved body** and compares to `status.observedInputHash` — skips the actual
   HTTP call if nothing has changed since the last successful one, since re-calling a real
   external endpoint isn't free or necessarily idempotent the way in-cluster math was.
4. **Sends the request** — method defaults to POST, custom headers applied, 10s timeout.
5. **Records the response** — `status.output`, `status.statusCode`, and a `Ready` condition
   based on the status code range (`Computed` for 2xx, `NonSuccessStatus` otherwise).

### Chaining mechanism

`SetupWithManager` registers a **multi-valued** field indexer on `.spec.inputFrom.sourceRef`
(multi-valued because one task can depend on several others at once), then sets up a
**self-referencing watch**: `.For(&HTTPTask{})` and `.Watches(&HTTPTask{}, ...)` both target the
same type. `findDependentTasks` maps a changed task to every other task whose `inputFrom` points
at it, re-queuing those — so editing `task-add` automatically re-triggers `task-square`.

### Where results live

`status.output` is a real field persisted to **etcd** via the API server — not a cache, not
temporary. The informer cache each controller uses is only a local, auto-synced read mirror; it's
rebuilt from etcd on restart and never the source of truth.

### A known gotcha, worth remembering

`spec.input` and `spec.inputFrom`'s field paths are free-form JSON/YAML with no CRD schema
validation on their contents. A bare `y:` key in YAML is parsed as the boolean `true` (YAML 1.1),
not the string `"y"` — always quote it as `"y":`. Unlike the earlier typed `Add`/`Subtract` CRDs
(where a malformed field was rejected outright by schema validation), a mistake here just
silently produces a wrong request body — a real tradeoff of going schema-less for generality.

## Prerequisites

- Go 1.21+, Docker, kubectl, [kind](https://kind.sigs.k8s.io/),
  [kubebuilder](https://book.kubebuilder.io/quick-start.html), Python 3 (for the demo endpoint)

## Getting started

```bash
kind create cluster --name mathop
make manifests generate
make install
make run          # leave running
```

In another terminal, start a demo HTTP endpoint (plain Python process, nothing Kubernetes-aware
— stands in for a real vendor API). Save as `mock_server.py`:

```python
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get('Content-Length', 0))
        body = json.loads(self.rfile.read(length) or b'{}')

        if self.path == '/add':
            result = {"sum": body.get("x", 0) + body.get("y", 0)}
        elif self.path == '/square':
            v = body.get("value", 0)
            result = {"square": v * v}
        else:
            self.send_response(404)
            self.end_headers()
            return

        payload = json.dumps(result).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, format, *args):
        print(f"[mock_server] {self.path} <- {args}")

ThreadingHTTPServer(("0.0.0.0", 9090), Handler).serve_forever()
```

```bash
python3 mock_server.py   # leave running; ThreadingHTTPServer avoids one slow request blocking all others
```

Create a root task and a dependent task, using `127.0.0.1` rather than `localhost` (avoids a
Go/IPv6-resolution stall some environments hit):

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: math.example.com/v1alpha1
kind: HTTPTask
metadata:
  name: task-add
spec:
  endpoint: http://127.0.0.1:9090/add
  input:
    x: 5
    "y": 7
EOF

kubectl get httptask task-add -o jsonpath='{.status.output}'; echo   # {"sum":12}

cat <<'EOF' | kubectl apply -f -
apiVersion: math.example.com/v1alpha1
kind: HTTPTask
metadata:
  name: task-square
spec:
  endpoint: http://127.0.0.1:9090/square
  inputFrom:
  - sourceRef:
      apiVersion: math.example.com/v1alpha1
      kind: HTTPTask
      name: task-add
    sourceField: sum
    targetField: value
EOF

kubectl get httptask task-square -o jsonpath='{.status.output}'; echo   # {"square":144}
```

Prove chaining reacts live:

```bash
kubectl patch httptask task-add --type=merge -p '{"spec":{"input":{"x":20,"y":30}}}'
kubectl get httptask task-square -o jsonpath='{.status.output}'; echo   # {"square":2500}
```

Prove the reference-protection finalizer:

```bash
kubectl delete httptask task-add --timeout=10s   # hangs — task-square still references it
kubectl delete httptask task-square
kubectl delete httptask task-add                  # succeeds immediately
```

## The earlier layers (still present, still functional)

- **`api/v1alpha1/add_types.go` / `internal/controller/add_controller.go`** — `Add`: computes
  `x + y` natively, carries the `in-use-protection` finalizer.
- **`api/v1alpha1/subtract_types.go` / `internal/controller/subtract_controller.go`** — same
  pattern, `x - y`.
- **`api/v1alpha1/square_types.go` / `internal/controller/square_controller.go`** — reads any
  referenced resource's `status.result` generically (via `task_transfer.go`'s
  `FetchTaskResult`/`DynamicWatcher`) and squares it. Wildcard RBAC
  (`resources=*,verbs=get;list;watch` on `math.example.com`) lets it read any future typed kind
  in that group without new config.
- **`internal/controller/refcheck.go`** — `Add`/`Subtract`'s reference-protection check against
  `Square`.

These demonstrate the progression from fixed, typed CRDs to generic runtime discovery, ahead of
the fully external, HTTP-driven `HTTPTask` design above.

## Running the automated test suite

```bash
make test
```

## Deploying in-cluster

```bash
make docker-build IMG=mathop-operator:v0.1.0
kind load docker-image mathop-operator:v0.1.0 --name mathop
make deploy IMG=mathop-operator:v0.1.0
kubectl logs -n mathop-operator-system deploy/mathop-operator-controller-manager -c manager -f
```

Note: an in-cluster pod can't reach a `127.0.0.1` process running directly on your WSL host —
that only works with `make run`. Reaching a host-machine endpoint from inside `kind` needs the
node's host-gateway address instead; ask if you need this wired up for a deployed demo.

## Built with

`kubebuilder`, `sigs.k8s.io/controller-runtime` (reconciliation loop, manager, dynamic watch
registration, field indexing), Go's standard `net/http` for the HTTP orchestration layer.