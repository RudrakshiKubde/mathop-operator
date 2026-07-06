# mathop-operator

A Kubernetes operator built with [Kubebuilder](https://book.kubebuilder.io/) that manages two
chained Custom Resources — `Add` and `Square` — modeled after Crossplane's composition pattern,
where one resource derives its state from another and stays in sync when that dependency changes.

## What it does

- **`Add`** takes two integers (`spec.x`, `spec.y`) and computes their sum into `status.result`.
- **`Square`** references an existing `Add` by name (`spec.addRef.name`), reads its `status.result`,
  and squares it into its own `status.result`.
- `Square`'s controller **watches `Add` resources**, not just its own object — so if you edit an
  `Add`'s numbers after the fact, every `Square` referencing it automatically recomputes, with no
  action needed on the `Square` itself.

```
Add (x=6, y=9) --status.result=15--> Square (addRef: that Add) --status.result=225-->
```

Edit the `Add` to `x=20, y=30` and, without touching `Square` at all, its result updates to `2500`.

## Why this matters

Most introductory operator tutorials only recompute a dependent resource when *that resource
itself* changes. That breaks the moment its upstream dependency changes. This project implements
the pattern Crossplane relies on for composition: a controller that watches an *upstream* resource
type and re-triggers reconciliation of every *downstream* resource that depends on it, using a
field indexer so the lookup stays efficient regardless of cluster size.

## Architecture

```
api/v1alpha1/
  add_types.go          Add CRD schema: spec.x, spec.y, status.result, status.conditions
  square_types.go       Square CRD schema: spec.addRef.name, status.result, status.conditions
  groupversion_info.go  Registers the math.example.com/v1alpha1 API group

internal/controller/
  add_controller.go     AddReconciler — computes x + y
  square_controller.go  SquareReconciler — computes result², watches Add, chains via a field indexer
```

### The chaining mechanism

`SquareReconciler.SetupWithManager` does three things:

1. Registers a **field indexer** on `Square.spec.addRef.name`, so "which Squares reference this
   Add" is an indexed lookup, not a full scan.
2. `.For(&Square{})` — reconcile whenever a `Square` itself changes.
3. `.Watches(&Add{}, handler.EnqueueRequestsFromMapFunc(r.findSquaresForAdd))` — also reconcile
   every `Square` that references an `Add` whenever *that Add* changes.

`findSquaresForAdd` is the mapping function: given a changed `Add`, it uses the field index to
find every `Square` pointing at it and returns a reconcile request for each.

### Status and conditions

Both types report a `Ready` condition (`metav1.Condition`) alongside their numeric result, with
reasons like `Computed`, `AddNotFound`, and `WaitingOnAdd` — so `kubectl describe` gives a clear,
human-readable explanation of state, not just a raw number.

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
make install               # install the CRDs into the cluster
make run                   # run the controller locally, logs to stdout
```

In another terminal:

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
  addRef:
    name: demo-add
EOF

kubectl get add demo-add        # RESULT should show 15
kubectl get square demo-square  # RESULT should show 225
```

> **Note:** in YAML, a bare `y:` key is parsed as the boolean `true` (YAML 1.1 quirk) — always
> quote it as `"y":` when writing `Add` manifests.

Prove the chaining works by editing only the `Add`:

```bash
kubectl patch add demo-add --type=merge -p '{"spec":{"x":20,"y":30}}'
kubectl get square demo-square   # RESULT updates to 2500, without touching Square at all
```

## Running the automated test suite

```bash
make test
```

This spins up a real (lightweight) control plane via `envtest` and verifies both the initial
computation and the chained recomputation after an `Add` update.


## Failure handling

- `Square` referencing a nonexistent `Add` → `Ready=False, Reason=AddNotFound`
- `Square` referencing an `Add` that hasn't computed a result yet → `Ready=False, Reason=WaitingOnAdd`
  (resolves automatically once the `Add` controller writes its result, via the same watch)

## Built with

Scaffolded with `kubebuilder`, using `sigs.k8s.io/controller-runtime` for the reconciliation loop,
manager, and watch/indexer machinery.