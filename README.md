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

Apply the manifests:

```console
kubectl apply -Rf deploy/
```

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
  # resyncInterval: 1h      # Safety-net interval on top of the real-time watches (defaults to 1h)
  # targetNamespaces:       # Namespaces to replicate the reference into (defaults to all namespaces)
  #   - kube-public
  # ignoreNamespaces:       # Namespaces to not replicate into
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

A replica cannot be used as the `reference` of another `SyncObject`. Both would end up managing objects of the same kind and name and overwrite each other's copies on every pass.
