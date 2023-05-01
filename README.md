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
