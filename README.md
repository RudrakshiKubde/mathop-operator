# mathop-operator

A Kubernetes operator built with [Kubebuilder](https://book.kubebuilder.io/) that chains
Custom Resources together at runtime — modeled after Crossplane's composition pattern — so
that **any** "Task" resource's result can be fed into **any** "Consumer" resource, without the
Consumer ever having a compile-time dependency on the Task's Go type.

Concretely, this repo ships two Task kinds (`Add`, `Subtract`) and one Consumer kind (`Square`),
but the point of the design is that a brand-new Task kind can be introduced and wired up **with
zero code changes to the Consumer** — only a new CRD, a new controller for that Task, and a
config change (RBAC).

## What it does

- **`Add`** — `spec.x`, `spec.y` → `status.result = x + y`
- **`Subtract`** — `spec.x`, `spec.y` → `status.result = x - y`
- **`Square`** — `spec.sourceRef {apiVersion, kind, name}` → reads the referenced object's
  `status.result`, squares it, writes its own `status.result`

```
Add (x=6, y=9)  --status.result=15--> Square (sourceRef → that Add)  --status.result=225-->
Subtract (x=50, y=8) --status.result=42--> Square (sourceRef → that Subtract) --status.result=1764-->
```

Both flows use the exact same `Square` controller code — it never imports `Add` or `Subtract`.

## The core idea: a shared convention, not a shared type

Any resource can act as a **Task** as long as its controller writes an integer to
`status.result`. Any resource can act as a **Consumer** as long as it holds a
`spec.sourceRef {apiVersion, kind, name}` pointing at a Task. That's the entire contract —
no shared Go interface, no import between the two sides. This also composes: since `Square`
itself writes `status.result`, a third resource could reference a `Square` as its own source,
making genuine multi-stage pipelines possible, not just two-step chains.

## Architecture

```
api/v1alpha1/
  add_types.go            Add CRD schema
  subtract_types.go        Subtract CRD schema
  square_types.go           Square CRD schema (spec.sourceRef, not a typed reference)
  groupversion_info.go       Registers the math.example.com/v1alpha1 API group

internal/controller/
  add_controller.go          AddReconciler — computes x + y, holds an in-use finalizer
  subtract_controller.go      SubtractReconciler — computes x - y, holds an in-use finalizer
  square_controller.go         SquareReconciler — thin: computes result², built on task_transfer.go
  task_transfer.go              The reusable engine — see below
  refcheck.go                    Shared "is this Task still referenced by a Square?" helper
```

### `task_transfer.go` — the reusable "hand task 1's result to task 2" engine

Two pieces, both fully generic — neither one imports `Add`, `Subtract`, or `Square`:

- **`FetchTaskResult(ctx, client, namespace, apiVersion, kind, name)`** — fetches *any* resource
  as `unstructured.Unstructured` (not a typed Go struct) and reads `status.result` out of it by
  field name. This is the actual data-transfer line; it works identically no matter what kind is
  referenced.
- **`DynamicWatcher`** — a small runtime registry of "which GVKs am I already watching." The
  first time a Consumer references a new Task kind, `DynamicWatcher.Ensure()` confirms that kind
  actually exists on the cluster (via the RESTMapper) and registers a live watch for it on the
  fly — no restart, no redeploy. Watches self-heal after a pod restart too, since every existing
  Consumer object gets re-reconciled on startup, re-registering its own watch.

Any new Consumer (not just `Square`) can be built as a thin wrapper around these two pieces —
plugging in only its own transform (e.g. `result := sum * sum` for Square, or `-sum` for a
future `Negate`) and its own field-indexer/List boilerplate (a few lines, kept per-type since
Go's typed `List` calls need a concrete list type).

### Reference-protection finalizers

`Add` and `Subtract` both carry the finalizer `math.example.com/in-use-protection`. On deletion,
each checks (via `refcheck.go`'s `referencedBySquare`) whether any `Square` still points at it;
if so, deletion is blocked (requeued every 5s) until the referencing `Square` is removed. This
mirrors Kubernetes' own PVC-protection pattern. It's a deliberate, one-directional coupling —
Tasks know about the existence of `Square`'s index, not the reverse — documented here rather than
hidden, since fully generic n-to-n reference protection would need a bigger design (closer to how
core Kubernetes' garbage collector uses `ownerReferences`).

### Status and conditions

Every type reports a `Ready` condition (`metav1.Condition`) alongside its numeric result, with
reasons like `Computed`, `SourceNotFound`, `WaitingOnSource`, `SourceKindUnavailable` — so
`kubectl describe` gives a clear, human-readable explanation of state, not just a raw number.

### Where results actually live (not a cache)

`status.result` is a real, persisted field written to **etcd** via the Kubernetes API server —
not a temporary or in-memory value. When a Consumer's controller reads it via `FetchTaskResult`,
it's reading that same persisted field back out. The informer cache each controller uses
(`mgr.GetCache()`, used by every `Get`/`List`/`Watch`) is only a local, auto-synced mirror kept
for read performance — it's not where data is stored, and losing it (e.g. on pod restart) loses
nothing, since it's rebuilt from etcd automatically.

### Leader election & caching — already provided by controller-runtime

- **Caches/informers**: every `Get`/`List`/`Watch` in this project already goes through
  controller-runtime's shared, watch-based cache by default. Nothing extra was added for this —
  it's inherent to the framework.
- **Leader election**: scaffolded in `cmd/main.go` (`--leader-elect` flag, `LeaderElectionID` on
  the manager). It only matters if the Deployment is scaled to multiple replicas — it ensures
  only one replica actively reconciles at a time.

## Prerequisites

- Go 1.21+
- Docker
- kubectl
- [kind](https://kind.sigs.k8s.io/)
- [kubebuilder](https://book.kubebuilder.io/quick-start.html)

## Getting started

```bash
kind create cluster --name mathop

make manifests generate   # regenerate CRDs/RBAC/deepcopy after any type changes
make install                # install all CRDs into the cluster
make run                     # run the controller locally, logs to stdout
```

In another terminal — note the quoted `"y":` (a bare `y:` key is parsed as the YAML 1.1 boolean
`true`, not the string `"y"`):

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: math.example.com/v1alpha1
kind: Add
metadata:
  name: demo-add
spec:
  x: 6
  "y": 9
EOF

cat <<'EOF' | kubectl apply -f -
apiVersion: math.example.com/v1alpha1
kind: Square
metadata:
  name: demo-square
spec:
  sourceRef:
    apiVersion: math.example.com/v1alpha1
    kind: Add
    name: demo-add
EOF

kubectl get add demo-add        # RESULT: 15
kubectl get square demo-square  # RESULT: 225
```

Prove the chaining reacts live, by editing only the `Add`:

```bash
kubectl patch add demo-add --type=merge -p '{"spec":{"x":20,"y":30}}'
kubectl get square demo-square   # RESULT updates to 2500, without touching Square at all
```

Prove `Square` works against a completely different Task kind, with no code changes:

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: math.example.com/v1alpha1
kind: Subtract
metadata:
  name: demo-subtract
spec:
  x: 50
  "y": 8
EOF

cat <<'EOF' | kubectl apply -f -
apiVersion: math.example.com/v1alpha1
kind: Square
metadata:
  name: square-from-subtract
spec:
  sourceRef:
    apiVersion: math.example.com/v1alpha1
    kind: Subtract
    name: demo-subtract
EOF

kubectl get subtract demo-subtract        # RESULT: 42
kubectl get square square-from-subtract   # RESULT: 1764
```

Prove the reference-protection finalizer:

```bash
kubectl delete add demo-add --timeout=10s   # hangs/times out — demo-square still references it
kubectl delete square demo-square
kubectl delete add demo-add                  # succeeds immediately now
```

## Running the automated test suite

```bash
make test
```

Spins up a real (lightweight) control plane via `envtest` and verifies both the initial
computation and the chained recomputation after a source update.

## Deploying in-cluster (instead of `make run` on your machine)

```bash
make docker-build IMG=mathop-operator:v0.1.0
kind load docker-image mathop-operator:v0.1.0 --name mathop
make deploy IMG=mathop-operator:v0.1.0

kubectl get pods -n mathop-operator-system
kubectl logs -n mathop-operator-system deploy/mathop-operator-controller-manager -c manager -f
```

Tear down with `make undeploy && make uninstall`.

## Extending with a new Task kind — the point of this design

1. `kubebuilder create api --group math --version v1alpha1 --kind <NewKind>`
2. Write its `_types.go` (spec + `status.result` + `status.conditions`) and controller, following
   `add_types.go`/`add_controller.go` as the template.
3. Grant RBAC — already handled by the wildcard rule on `square_controller.go`
   (`+kubebuilder:rbac:groups=math.example.com,resources=*,verbs=get;list;watch`), so no new RBAC
   rule is needed as long as the new kind stays in the `math.example.com` group.
4. `make manifests generate && make deploy`
5. `kubectl apply` a `Square` with `sourceRef.kind: <NewKind>` — done. `square_controller.go` and
   `task_transfer.go` are never touched.

## Failure handling

- `Square` referencing a nonexistent source → `Ready=False, Reason=SourceNotFound`
- `Square` referencing a source that hasn't computed a result yet → `Ready=False,
  Reason=WaitingOnSource` (resolves automatically once the source writes its result, via the
  same watch)
- `Square` referencing an unrecognized/uninstalled kind → `Ready=False,
  Reason=SourceKindUnavailable`
- Deleting a Task still referenced by a `Square` → blocked by the in-use finalizer until the
  reference is removed

## Built with

Scaffolded with `kubebuilder`, using `sigs.k8s.io/controller-runtime` for the reconciliation
loop, manager, dynamic watch registration, and field indexing.