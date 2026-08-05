# sync-operator

A operator for syncing any kind of resources across namespaces.

## Description

Most operators for syncing between namespaces only allow this for configmaps and secrets (which might also be most of the use-cases), but they won't be able to sync any other resources. So, I got curious about the limitations and started to build `sync-operator` which can sync all kind of resources.

Since the reference resource can be of any kind, `sync-operator` dynamically starts watching whatever kind is referenced (the first time it sees a `SyncObject` pointing at it) and syncs immediately when that resource changes. `resyncInterval` exists as a fallback for what the watch doesn't cover: a replica edited or deleted directly, a new target namespace appearing, or the watch not being established yet (e.g. the referenced kind's CRD wasn't installed at the time). It defaults to `1h`.

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
  # resyncInterval: 1h      # Fallback drift-correction interval, on top of the real-time watch (defaults to 1h)
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
