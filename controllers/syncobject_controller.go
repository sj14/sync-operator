package controllers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	syncv1alpha1 "github.com/sj14/sync-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// SyncObjectReconciler reconciles a SyncObject object
type SyncObjectReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// cache and dynamicController are used to lazily start a watch on a
	// Reference's GroupVersionKind the first time it's seen, so changes to
	// the referenced object trigger an immediate reconcile instead of only
	// being picked up on the next periodic resync.
	cache             cache.Cache
	dynamicController controller.Controller

	watchedGVKsMu sync.Mutex
	watchedGVKs   map[schema.GroupVersionKind]struct{}
}

const finalizerName = "sync.sj14.github.io/finalizer"

// defaultResyncInterval is used when SyncObjectSpec.ResyncInterval is not set.
const defaultResyncInterval = 1 * time.Hour

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *SyncObjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("reconciling SyncObject")

	var syncObject syncv1alpha1.SyncObject

	err := r.Client.Get(ctx, req.NamespacedName, &syncObject)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed getting SyncObject: %v", err)
	}

	if ref := syncObject.Spec.Reference; ref.Kind != "" {
		if err := r.ensureReferenceWatch(ctx, ref); err != nil {
			// Not fatal: resyncInterval's periodic resync still covers us, and
			// the next Reconcile call (e.g. once the kind's CRD is installed)
			// retries.
			logger.Error(err, "failed to watch referenced resource for changes; falling back to periodic resync", "reference", ref)
		}
	}

	stop, err := r.handleFinalizer(ctx, &syncObject)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed handling finalizer: %v", err)
	}
	if stop {
		return ctrl.Result{}, nil
	}

	// spec.reference was changed to point somewhere else: the replicas of the
	// previous reference are named after it, so nothing below would ever touch
	// them again and they'd be orphaned.
	if applied := syncObject.Status.AppliedReference; applied != nil && *applied != syncObject.Spec.Reference {
		logger.Info("reference changed, removing replicas of the previous reference", "previous", *applied, "current", syncObject.Spec.Reference)
		if err := r.deleteAllReplicas(ctx, *applied, syncObject.Spec.Reference); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed removing replicas of the previous reference: %v", err)
		}
	}

	targetNamespaces, nonTargetNamespaces, err := r.getTargetNamespaces(ctx, syncObject)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed getting target namespaces: %v", err)
	}

	var multiErr error
	// cleanup leftovers, e.g. when the targetNamespaces changed
	for _, namespace := range nonTargetNamespaces {
		if err := r.deleteReplica(ctx, syncObject.Spec.Reference, namespace); err != nil {
			multiErr = errors.Join(multiErr, fmt.Errorf("failed cleaning up replica: %v", err))
		}
	}

	for _, namespace := range targetNamespaces {
		if err := r.replicate(ctx, syncObject.Spec.Reference, namespace); err != nil {
			multiErr = errors.Join(multiErr, fmt.Errorf("failed creating replica: %v", err))
		}
	}

	if multiErr != nil {
		return ctrl.Result{}, multiErr
	}

	// Only recorded once the replicas above actually exist. If replication
	// failed we keep the previous value, so the next attempt still knows
	// which replicas to clean up.
	if err := r.setAppliedReference(ctx, &syncObject); err != nil {
		return ctrl.Result{}, err
	}

	// when there was no error, requeue after the resync interval as a
	// drift-correction fallback -- reference changes are already synced
	// immediately via the watch set up above.
	return ctrl.Result{RequeueAfter: resyncInterval(syncObject)}, nil
}

// resyncInterval returns the configured resync interval, falling back to
// defaultResyncInterval when none was set on the SyncObject.
func resyncInterval(syncObject syncv1alpha1.SyncObject) time.Duration {
	if syncObject.Spec.ResyncInterval.Duration == 0 {
		return defaultResyncInterval
	}
	return syncObject.Spec.ResyncInterval.Duration
}

func (r *SyncObjectReconciler) handleFinalizer(ctx context.Context, syncObject *syncv1alpha1.SyncObject) (stop bool, err error) {
	// examine DeletionTimestamp to determine if object is under deletion
	if syncObject.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !controllerutil.ContainsFinalizer(syncObject, finalizerName) {
			controllerutil.AddFinalizer(syncObject, finalizerName)
			if err := r.Update(ctx, syncObject); err != nil {
				return true, err
			}
		}
		return false, nil
	}

	// The object is being deleted
	if controllerutil.ContainsFinalizer(syncObject, finalizerName) {
		if !syncObject.Spec.DisableFinalizer {
			// our finalizer is present, so lets handle any external dependency.
			// A reference change that was never reconciled leaves replicas of
			// the previous reference behind too, so clean up both.
			refs := referencesToCleanUp(*syncObject)

			var multiErr error
			for _, ref := range refs {
				if err := r.deleteAllReplicas(ctx, ref, refs...); err != nil {
					multiErr = errors.Join(multiErr, err)
				}
			}
			if multiErr != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried
				return true, multiErr
			}
		}

		// remove our finalizer from the list and update it.
		controllerutil.RemoveFinalizer(syncObject, finalizerName)
		if err := r.Update(ctx, syncObject); err != nil {
			return true, err
		}

		// Stop reconciliation as the item is being deleted
		return true, nil
	}

	return false, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SyncObjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &syncv1alpha1.SyncObject{}, referencedObjectIndexKey, indexByReference); err != nil {
		return fmt.Errorf("failed indexing SyncObject by reference: %w", err)
	}

	r.cache = mgr.GetCache()
	r.watchedGVKs = make(map[schema.GroupVersionKind]struct{})

	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&syncv1alpha1.SyncObject{}).
		// a namespace created later may need replicas of its own
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.requestsForNamespace)).
		Build(r)
	if err != nil {
		return err
	}
	r.dynamicController = c

	return nil
}

// referencedObjectIndexKey is the field index used to look up the
// SyncObjects that care about a given object.
const referencedObjectIndexKey = "spec.reference.gvkAndName"

// referencedObjectKey returns a stable key identifying an object a
// SyncObject manages.
//
// The namespace is deliberately left out: a replica differs from its
// original only by namespace, so this key matches both. That's what lets an
// event for a replica find the SyncObject that owns it, and not just events
// for the original.
func referencedObjectKey(gvk schema.GroupVersionKind, name string) string {
	return strings.Join([]string{gvk.Group, gvk.Version, gvk.Kind, name}, "/")
}

func indexByReference(obj client.Object) []string {
	syncObject, ok := obj.(*syncv1alpha1.SyncObject)
	if !ok {
		return nil
	}
	ref := syncObject.Spec.Reference
	return []string{referencedObjectKey(ref.GroupVersionKind(), ref.Name)}
}

// ensureReferenceWatch makes sure changes to objects of ref's
// GroupVersionKind trigger a reconcile, instead of only being picked up on
// the next periodic resync. It's a no-op once a GVK has successfully been
// watched once.
//
// The informer behind this watch is cluster wide, so it covers the replicas
// as well as the original: editing or deleting a replica by hand is undone
// straight away rather than at the next resync.
func (r *SyncObjectReconciler) ensureReferenceWatch(ctx context.Context, ref syncv1alpha1.Reference) error {
	gvk := ref.GroupVersionKind()

	r.watchedGVKsMu.Lock()
	_, ok := r.watchedGVKs[gvk]
	r.watchedGVKsMu.Unlock()
	if ok {
		return nil
	}

	watched := &unstructured.Unstructured{}
	watched.SetGroupVersionKind(gvk)

	mapFn := func(ctx context.Context, obj *unstructured.Unstructured) []reconcile.Request {
		return r.requestsForObject(ctx, obj)
	}

	src := source.Kind(r.cache, watched, handler.TypedEnqueueRequestsFromMapFunc[*unstructured.Unstructured, reconcile.Request](mapFn))
	if err := r.dynamicController.Watch(src); err != nil {
		return fmt.Errorf("failed watching %s: %w", gvk, err)
	}

	r.watchedGVKsMu.Lock()
	r.watchedGVKs[gvk] = struct{}{}
	r.watchedGVKsMu.Unlock()

	return nil
}

// requestsForObject finds the SyncObjects (if any) that manage obj, whether
// it's their original or one of its replicas, so a change to either
// triggers a reconcile immediately instead of waiting for the resync.
func (r *SyncObjectReconciler) requestsForObject(ctx context.Context, obj *unstructured.Unstructured) []reconcile.Request {
	key := referencedObjectKey(obj.GroupVersionKind(), obj.GetName())

	var syncObjects syncv1alpha1.SyncObjectList
	if err := r.Client.List(ctx, &syncObjects, client.MatchingFields{referencedObjectIndexKey: key}); err != nil {
		log.FromContext(ctx).Error(err, "failed listing SyncObjects for object", "gvk", obj.GroupVersionKind(), "namespace", obj.GetNamespace(), "name", obj.GetName())
		return nil
	}

	requests := make([]reconcile.Request, 0, len(syncObjects.Items))
	for _, syncObject := range syncObjects.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&syncObject)})
	}
	return requests
}

// requestsForNamespace enqueues the SyncObjects that would replicate into
// the given namespace, so a namespace created after the fact gets its
// replicas immediately instead of at the next resync.
func (r *SyncObjectReconciler) requestsForNamespace(ctx context.Context, namespace client.Object) []reconcile.Request {
	var syncObjects syncv1alpha1.SyncObjectList
	if err := r.Client.List(ctx, &syncObjects); err != nil {
		log.FromContext(ctx).Error(err, "failed listing SyncObjects for namespace", "namespace", namespace.GetName())
		return nil
	}

	var requests []reconcile.Request
	for _, syncObject := range syncObjects.Items {
		targets := syncObject.Spec.TargetNamespaces
		// an empty target list means "every namespace", so this one counts too
		if len(targets) > 0 && !slices.Contains(targets, namespace.GetName()) {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&syncObject)})
	}
	return requests
}

// getTargetNamespaces returns the namespaces to replicate the reference
// into, and the namespaces to clean up leftover replicas from.
//
// The reference's own namespace appears in neither list: it holds the
// original, which must never be overwritten by a replica nor deleted as if
// it were one -- not even when the user listed it in targetNamespaces or
// ignoreNamespaces.
func (r *SyncObjectReconciler) getTargetNamespaces(ctx context.Context, syncObject syncv1alpha1.SyncObject) ([]string, []string, error) {
	// cloned: appending to a spec field's slice could otherwise write into
	// the SyncObject's own backing array.
	targetNamespaces := slices.Clone(syncObject.Spec.TargetNamespaces)
	nonTargetNamespaces := slices.Clone(syncObject.Spec.IgnoreNamespaces)

	var allNamespaces corev1.NamespaceList

	if err := r.Client.List(ctx, &allNamespaces); err != nil {
		return nil, nil, fmt.Errorf("failed listing namespaces: %v", err)
	}

	// no namespaces defined, sync to all of them
	if len(targetNamespaces) == 0 {
		for _, namespace := range allNamespaces.Items {
			targetNamespaces = append(targetNamespaces, namespace.GetName())
		}
	}

	// we only sync to specified namespaces, check which are nonTarget namespaces
	// so we can delete replicas there if there are some leftovers
	for _, namespace := range allNamespaces.Items {
		if !slices.Contains(targetNamespaces, namespace.GetName()) {
			// namespace is not a target
			nonTargetNamespaces = append(nonTargetNamespaces, namespace.GetName())
		}
	}

	// Remove namespaces we want to ignore
	for _, ignoreNamespace := range syncObject.Spec.IgnoreNamespaces {
		targetNamespaces = remove(targetNamespaces, ignoreNamespace)
	}

	// the original is not a replica: don't replicate over it, and don't
	// delete it.
	referenceNamespace := syncObject.Spec.Reference.Namespace
	targetNamespaces = remove(targetNamespaces, referenceNamespace)
	nonTargetNamespaces = remove(nonTargetNamespaces, referenceNamespace)

	return targetNamespaces, nonTargetNamespaces, nil
}

// remove returns a copy of slice with all elements equal to s removed.
// The input slice is left untouched: slices.DeleteFunc mutates in place, and
// slice here may alias a SyncObject's own spec field.
func remove(slice []string, s string) []string {
	return slices.DeleteFunc(slices.Clone(slice), func(v string) bool {
		return v == s
	})
}

// TODO: Add finalizer, ownerreference/managedby?
func (r *SyncObjectReconciler) replicate(ctx context.Context, ref syncv1alpha1.Reference, namespace string) error {
	var original unstructured.Unstructured
	original.SetGroupVersionKind(ref.GroupVersionKind())

	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &original); err != nil {
		return fmt.Errorf("failed getting original object: %v", err)
	}

	replica := original.DeepCopy()
	replica.SetNamespace(namespace)

	// remove state from the old object
	replica.SetResourceVersion("")
	replica.SetUID(types.UID(""))
	// TODO: add more?

	log.Log.Info("creating/updating", "gvk", replica.GroupVersionKind().String(), "namespace", replica.GetNamespace(), "name", replica.GetName())

	// create new replica if it doesn't already exist
	err := r.Client.Create(ctx, replica)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		// some other error than already exists...
		return fmt.Errorf("failed creating replica in %q: %v", namespace, err)
	}
	if err == nil {
		// we created a new replica, no need to update it now
		return nil
	}

	// replica already exists, just update it
	if err := r.Client.Update(ctx, replica); err != nil {
		return fmt.Errorf("failed updating replica in %q: %v", namespace, err)
	}

	return nil
}

// TODO: check ownerreference or something before deleting
func (r *SyncObjectReconciler) deleteReplica(ctx context.Context, ref syncv1alpha1.Reference, namespace string) error {
	var objectToDelete unstructured.Unstructured
	objectToDelete.SetName(ref.Name)
	objectToDelete.SetNamespace(namespace)
	objectToDelete.SetGroupVersionKind(ref.GroupVersionKind())

	log.Log.Info("deleting", "object", objectToDelete)

	if err := r.Client.Delete(ctx, &objectToDelete); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed deleting replica: %v", err)
	}

	return nil
}

// deleteAllReplicas removes ref's replicas from every namespace.
//
// ref's own object is never deleted, and neither is any object a protected
// reference points at: when spec.reference changes, the old and new
// reference can share a group/version/kind and name while differing only in
// namespace, in which case cleaning up the old reference would otherwise
// delete the new original.
func (r *SyncObjectReconciler) deleteAllReplicas(ctx context.Context, ref syncv1alpha1.Reference, protected ...syncv1alpha1.Reference) error {
	var namespaces corev1.NamespaceList

	if err := r.Client.List(ctx, &namespaces); err != nil {
		return fmt.Errorf("failed listing namespaces: %v", err)
	}

	var multiErr error
	for _, namespace := range namespaces.Items {
		// the object deleteReplica would delete in this namespace
		candidate := ref
		candidate.Namespace = namespace.GetName()

		if candidate == ref {
			// do not delete the original
			continue
		}
		if slices.Contains(protected, candidate) {
			// another reference points at this object, it's not a replica
			continue
		}

		if err := r.deleteReplica(ctx, ref, namespace.GetName()); err != nil {
			multiErr = errors.Join(multiErr, err)
		}
	}

	return multiErr
}

// referencesToCleanUp returns every reference whose replicas belong to this
// SyncObject: the current one, plus the previously applied one when
// spec.reference has changed and its replicas haven't been cleaned up yet.
func referencesToCleanUp(syncObject syncv1alpha1.SyncObject) []syncv1alpha1.Reference {
	refs := []syncv1alpha1.Reference{syncObject.Spec.Reference}
	if applied := syncObject.Status.AppliedReference; applied != nil && *applied != syncObject.Spec.Reference {
		refs = append(refs, *applied)
	}
	return refs
}

// setAppliedReference records the reference we just replicated, so a later
// change to spec.reference can find and clean up these replicas.
func (r *SyncObjectReconciler) setAppliedReference(ctx context.Context, syncObject *syncv1alpha1.SyncObject) error {
	if applied := syncObject.Status.AppliedReference; applied != nil && *applied == syncObject.Spec.Reference {
		return nil
	}

	ref := syncObject.Spec.Reference
	syncObject.Status.AppliedReference = &ref
	if err := r.Status().Update(ctx, syncObject); err != nil {
		return fmt.Errorf("failed recording applied reference: %v", err)
	}

	return nil
}
