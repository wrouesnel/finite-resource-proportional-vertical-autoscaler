# finite-resource-proportional-vertical-autoscaler

A cluster-wide Kubernetes operator which treats the resource requests of every pod as *proportional
weights*, rather than absolute amounts, and scales them so that together they exactly fill the
finite resources of the cluster.

A pod submitted asking for `cpu: 100m` doesn't get 100 millicores. It gets 100 parts of the cluster's
CPU, where the other parts are the requests of every other managed pod. If the cluster has 90 cores
to share and the managed pods ask for 3 cores in total, every request is multiplied by 30. When
another pod joins, everyone's share shrinks; when one leaves, everyone grows into the space.

## How it works

The operator is a single binary which runs two things:

* **A mutating admission webhook** on every replica. When a pod is created (from any source:
  Deployments, Jobs, operators, bare pods), it records the pod's submitted requests and limits in
  the managed `frpva.wrouesnel.github.com/original-resources` annotation, and rewrites the requests
  to the pod's proportional share of the cluster.
* **A rebalancing controller** on the elected leader. It watches pods, nodes and the policy, and
  resizes running pods in place (through the `pods/resize` subresource) whenever their share drifts
  from what they hold: because pods arrived or left, nodes were added, removed or cordoned, or the
  policy changed.

### The calculation

For each managed resource (`cpu`, `memory`, `ephemeral-storage`):

```
capacity  = configured capacity, or (sum of allocatable of counted nodes) * (100 - headroomPercent)%
available = capacity - requests of unmanaged pods on counted nodes - pod overhead of managed pods
weight    = sum over managed pods of the pod's original effective request
factor    = available / weight

scaled request of each container = original request * factor
```

The default headroom is 10%, so the auto-detected capacity is 90% of what the nodes report.

The pod's *effective* request is computed the way the scheduler does (`max(init containers,
containers + sidecars)`, or pod-level resources when set). Every container of every pod is scaled by
the same factor, so effective requests scale by that factor too, and the managed pods together
request exactly `available`.

A few refinements:

* **No pod is scaled beyond what fits on a node.** A pod's factor is capped so its effective request
  doesn't exceed the largest counted node's allocatable (less headroom). Capacity freed by the cap
  isn't redistributed.
* **Limits** follow the `limitPolicy`. With `Proportional` (the default) limits are scaled by the same
  factor, which keeps every container's limit:request ratio and so its QoS class. Guaranteed pods stay
  Guaranteed, which matters because in-place resizes may not change the QoS class. With `Unchanged`
  the submitted limits are kept and only raised where the scaled request would exceed them.
* **Rounding.** CPU is rounded to the millicore and memory and storage to the KiB. Nothing is scaled
  below one unit or the pool's `minRequest`. Resources a container doesn't request (or requests as
  `0`) are left alone, so BestEffort pods stay BestEffort.
* **Rebalancing is damped.** A running pod is only resized when a request is more than
  `tolerancePercent` (default 5%) away from its proportional value, and resize passes run at most every
  `minInterval` (default 30s). Each pass applies all decreases before any increases, so growing pods
  have room to grow into.

### What is managed

A pod is managed unless:

* its namespace matches one of `excludedNamespaces` (exact names or globs, e.g. `kube-*`). The
  operator's own namespace is always excluded.
* it is annotated `frpva.wrouesnel.github.com/exclude: "true"`.
* it is a static (mirror) pod.

Unmanaged pods keep their requests, and the requests they hold are subtracted from the capacity shared
among managed pods (set `subtractUnmanaged: false` to stop this).

Nodes count towards the detected capacity unless they are cordoned, being deleted, have a
`NoSchedule`/`NoExecute` taint (e.g. control-plane or not-ready nodes; set `includeTaintedNodes` to
count them anyway), or don't match `nodeSelector`.

### Limitations

* `ephemeral-storage` can't be resized in place, so it is only scaled at admission. Running pods
  keep the storage request they were admitted with until they are recreated.
* Pods using pod-level resources (`spec.resources`) are rebalanced only on nodes whose kubelet
  declares the `InPlacePodLevelResourcesVerticalScaling` feature in `status.declaredFeatures`. The API
  server refuses pod-level resizes on other nodes, so pods there keep the share they were admitted
  with (they are still counted at their proportional weight). Unscheduled pods are not checked
  against a node.
* Running pods can only be resized if the cluster supports in-place pod resize (GA since Kubernetes
  1.35). Resizes the kubelet can't satisfy right away are deferred by the kubelet. Resizes the API
  rejects are recorded as `ProportionalResizeFailed` events on the pod.
* The operator owns the requests of managed pods. If someone resizes a managed pod by hand, the next
  rebalance puts it back to its proportional share. To change a pod's weight, change the request in
  its controller's pod template so new pods are admitted with the new weight.
* Pods created while the webhook is unavailable are admitted as submitted (the webhook fails open) and
  treated as unmanaged, unless `rebalance.adoptExistingPods` is set.

## Configuration

The operator acts on the cluster-scoped `ProportionalResourcePolicy` named `default` (set with
`--policy-name`). Until that policy exists, the operator does nothing.

```yaml
apiVersion: frpva.wrouesnel.github.com/v1alpha1
kind: ProportionalResourcePolicy
metadata:
  name: default
spec:
  # Stop scaling and rebalancing without uninstalling.
  suspend: false
  resources:
    - name: cpu
      headroomPercent: 10   # held back from detected capacity
      minRequest: 1m
    - name: memory
      # An explicit capacity replaces detection.
      capacity: 512Gi
      minRequest: 16Mi
    - name: ephemeral-storage
  excludedNamespaces:
    - kube-*
    - monitoring
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/worker: ""
  includeTaintedNodes: false
  subtractUnmanaged: true
  limitPolicy: Proportional   # or Unchanged
  rebalance:
    mode: InPlace             # or Disabled, to only scale at admission
    tolerancePercent: 5
    minInterval: 30s
    # Record the current requests of pods which predate the operator as their weights, and manage them.
    adoptExistingPods: false
```

The status reports what the operator sees:

```console
$ kubectl get prp
NAME      SUSPENDED   NODES   MANAGED   READY   AGE
default   false       12      340       True    3d
$ kubectl get prp default -o jsonpath='{.status.resources[0]}'
{"available":"86","capacity":"97200m","factor":"27.5641","name":"cpu","nodeAllocatable":"108","totalWeight":"3120m","unmanaged":"11200m"}
```

## Installing

```sh
kubectl apply -k deploy/
kubectl apply -f deploy/policy.yaml
```

This installs the CRD, the operator (two replicas in `frpva-system`), its RBAC, and the
`MutatingWebhookConfiguration`. Apply the policy once the CRD exists.

By default the operator generates and rotates its own webhook certificate, storing it in the
`frpva-webhook-cert` secret and injecting the CA into the webhook configuration. To use cert-manager
instead, run with `--webhook-cert-mode=external`, mount the certificate at `--webhook-cert-dir`, and
let cert-manager's CA injector set the `caBundle`.

The webhook configuration's `namespaceSelector` keeps the API server from calling the webhook for
`kube-system` and `frpva-system`. All other exclusions are applied by the operator from the policy.

## Building

```sh
go run mage.go binary   # builds bin/ and symlinks ./frpva
go run mage.go test     # unit tests, plus integration tests against an envtest control plane
go run mage.go lint
go run mage.go style
```

`go run mage.go test` downloads the envtest `kube-apiserver` and `etcd` binaries into `.bin/envtest`.
The integration tests run the operator through its entrypoint against a real API server, and check
admission scaling, in-place rebalancing, exclusions, adoption and certificate management.

The CRD and deepcopy code are generated by `controller-gen` from `pkg/apis/v1alpha1`, which
`go generate ./...` runs (and every mage build target runs).

## Layout

| Path | Purpose |
|---|---|
| `cmd/frpva` | Minimal `main` which calls the entrypoint. |
| `pkg/entrypoints/frpva` | Command line, logging, and wiring of the manager, webhook and certificates. |
| `pkg/apis/v1alpha1` | The `ProportionalResourcePolicy` API. |
| `pkg/proportional` | The calculation: original resources annotation, classification, plan and scaling. |
| `pkg/snapshot` | Reads the cluster state the webhook and controller share. |
| `pkg/webhook` | The pod mutating admission webhook. |
| `pkg/controller` | The rebalancing controller. |
| `deploy/` | Kustomize manifests. `deploy/crd` is generated. |

## History

This repository started as a fork of
[cluster-proportional-vertical-autoscaler](https://github.com/kubernetes-sigs/cluster-proportional-vertical-autoscaler),
which scales a single workload with the size of the cluster. It has since been rewritten around a
different idea: every workload shares out the cluster in proportion to what it asks for.
