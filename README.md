# sync-operator

A operator for syncing any kind of resources across namespaces.

## Description

Most operators for syncing between namespaces only allow this for configmaps and secrets (which might also be most of the use-cases), but they won't be able to sync any other resources. So, I got curious about the limitations and started to build `sync-operator` which can sync all kind of resources.
The downside is, that I didn't yet find an easy way for triggering a reconcile when the original resource or one of its replicas get adjusted. Probably, this might be the reason why similar solutions only sync on configmaps ans secrests. As a workaround, you have to set the `interval` in the `SyncObject` to match your needs.

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
  # interval: 1h            # How often to sync the reference resource (defaults to 10h)
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
