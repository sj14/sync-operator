# sync-operator

A operator for syncing any kind of resources across namespaces.

## Description

Most operators for syncing between namespaces only allow this for configmaps and secrets (which might also be most of the use-cases), but they won't be able to sync any other resources. So, I got curious about the limitations and started to build `sync-operator` which can sync all kind of resources.

Since the reference resource can be of any kind, `sync-operator` dynamically starts watching whatever kind is referenced, the first time it sees a `SyncObject` pointing at it. That watch is cluster wide, so it covers both directions in real time:

- the reference changes, and the replicas are updated to match
- a replica is edited or deleted by hand, and it gets restored from the reference

Namespaces are watched as well, so a namespace created later gets its replicas straight away instead of waiting for a periodic pass.

`resyncInterval` (default `1h`) is what's left over: a safety net for what a watch can't catch, such as the referenced kind's CRD not being installed yet when the `SyncObject` was created, or a missed event. It is not how changes are normally picked up.

## Deploy

- Adjust the `sync-operator-object-role` [ClusterRole](deploy/clusterrole.yaml) according to your needs. By default, it has permissions for all resources. You may want to adjust it to the resources you want to sync.
- Pin the image version of the operator in the [Deployment](deploy/deployment.yaml).
- Adjust the [sample](deploy/samples/syncobject.yaml) according to the resource you want to sync.

Apply the manifests, CRDs first:

```console
kubectl apply -f deploy/crds/
kubectl apply -f deploy/
kubectl apply -f deploy/samples/
```

Anything under [deploy/optional](deploy/optional) is deliberately left out and applied separately, see [below](#preventing-edits-to-replicas-optional).

## Example

Lets imagine we have the following `ConfigMap` we want to sync:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-sync
  namespace: default
data:
    key1: value1
```

When you have installed the CRDs and the Operator succesfully, you can create a `SyncObject` and reference the `ConfigMap` from above:

```yaml
apiVersion: sync.sj14.github.io/v1alpha1
kind: SyncObject
metadata:
  name: syncobject-sample
spec:
  # resyncInterval: 1h      # Safety-net interval on top of the real-time watches (defaults to 1h, minimum 1s)
  # targetNamespaces:       # Namespaces to replicate the reference into (defaults to all namespaces)
  #   - kube-public
  # ignoreNamespaces:       # Namespaces to not replicate into (cannot overlap targetNamespaces)
  #   - kube-system
  # disableFinalizer: true  # Do not remove replicas when the reference gets removed
  reference:                # Reference which will get replicated into other namespaces
    group: ""               # empty for core group
    version: v1 
    kind: ConfigMap         # case-sensitive!
    name: test-sync
    namespace: default
```

After applying the manifests, the `ConfigMap` should get synced across the namespaces.

## Status

Each `SyncObject` reports whether its last sync worked, so a broken one can be spotted without reading the operator's logs:

```console
kubectl get syncobjects
```

```
NAME                KIND        SOURCE      READY   REASON       AGE
syncobject-sample   ConfigMap   test-sync   True    Synced       5m
broken-sample       Secret      missing     False   SyncFailed   2m
```

`kubectl describe syncobject broken-sample` then shows the `Ready` condition with the reason it failed. `status.observedGeneration` tells you whether the most recent change to the spec has been acted on yet.

## Replicas

Replicas keep the labels and annotations of the resource they were copied from, and get these added on top:

| Key | Type | Description |
| --- | --- | --- |
| `sync.sj14.github.io/managed-by` | label | Always `sync-operator`. Marks the object as a replica. |
| `sync.sj14.github.io/sync-object` | annotation | Name of the `SyncObject` that created it. |
| `sync.sj14.github.io/source-namespace` | annotation | Namespace of the resource it was copied from. |
| `sync.sj14.github.io/source-name` | annotation | Name of the resource it was copied from. |

The reference itself is never marked, only its replicas. So to list every replica in the cluster:

```console
kubectl get configmaps -A -l sync.sj14.github.io/managed-by=sync-operator
```

These marks are also how the operator decides what it may delete. It only ever removes objects it created itself, matched by the marks above rather than by name, so an existing resource that happens to share a name with a replica is left alone.

### Preventing edits to replicas (optional)

Editing a replica appears to work and is then reverted from its source moments later, which is confusing to run into. An optional [ValidatingAdmissionPolicy](deploy/optional/protect-replicas.yaml) refuses the edit instead, and says where to make the change:

```console
kubectl apply -f deploy/optional/protect-replicas.yaml
```

It is not part of the install above, and needs Kubernetes 1.30 or newer.

Two things to know before applying it:

- It assumes the operator runs as `system:serviceaccount:sync-operator:sync-operator`. If you deployed it elsewhere, adjust `matchConditions` first, or the operator itself is denied and syncing stops.
- It denies every update to a replica, for all kinds. Subresources are not affected, so a controller writing a replica's `status` or `scale` still works. A controller writing the *main* object does not — the `Deployment` controller setting its revision annotation, for example. If you sync such a kind, narrow `resourceRules` to exclude it.

It covers updates only, not deletions. Denying deletions would leave any namespace holding a replica stuck in `Terminating`, and a deleted replica is restored from its source within moments anyway. It is a guardrail rather than a guarantee: a cluster admin can always remove the policy.

Since it is applied separately, it is also removed separately — worth doing when uninstalling the operator, or the policy carries on refusing edits to whatever replicas are left behind:

```console
kubectl delete -f deploy/optional/protect-replicas.yaml
```

A replica cannot be used as the `reference` of another `SyncObject`. Both would end up managing objects of the same kind and name and overwrite each other's copies on every pass.
