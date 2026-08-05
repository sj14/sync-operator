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

	targetNamespaces, nonTargetNamespaces, err := r.getTargetNamespaces(ctx, syncObject)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed getting target namespaces: %v", err)
	}

	var multiErr error
	// cleanup leftovers, e.g. when the targetNamespaces changed
	for _, namespace := range nonTargetNamespaces {
		if err := r.deleteReplica(ctx, syncObject, namespace); err != nil {
			multiErr = errors.Join(multiErr, fmt.Errorf("failed cleaning up replica: %v", err))
		}
	}

	for _, namespace := range targetNamespaces {
		if err := r.replicate(ctx, syncObject, namespace); err != nil {
			multiErr = errors.Join(multiErr, fmt.Errorf("failed creating replica: %v", err))
		}
	}

	if multiErr != nil {
		return ctrl.Result{}, multiErr
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
			// our finalizer is present, so lets handle any external dependency
			if err := r.deleteAllReplicas(ctx, *syncObject); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried
				return true, err
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
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &syncv1alpha1.SyncObject{}, referenceIndexKey, indexByReference); err != nil {
		return fmt.Errorf("failed indexing SyncObject by reference: %w", err)
	}

	r.cache = mgr.GetCache()
	r.watchedGVKs = make(map[schema.GroupVersionKind]struct{})

	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&syncv1alpha1.SyncObject{}).
		Build(r)
	if err != nil {
		return err
	}
	r.dynamicController = c

	return nil
}

// referenceIndexKey is the field index used to look up SyncObjects by the
// object their Reference points at.
const referenceIndexKey = "spec.reference"

// referenceKey returns a stable key identifying the object a Reference
// points at. It's used both to index SyncObjects by their Reference and to
// look them back up from a watched object's own GVK/namespace/name.
func referenceKey(group, version, kind, namespace, name string) string {
	return strings.Join([]string{group, version, kind, namespace, name}, "/")
}

func indexByReference(obj client.Object) []string {
	syncObject, ok := obj.(*syncv1alpha1.SyncObject)
	if !ok {
		return nil
	}
	ref := syncObject.Spec.Reference
	return []string{referenceKey(ref.Group, ref.Version, ref.Kind, ref.Namespace, ref.Name)}
}

// ensureReferenceWatch makes sure changes to objects of ref's
// GroupVersionKind trigger a reconcile, instead of only being picked up on
// the next periodic resync. It's a no-op once a GVK has successfully been
// watched once.
func (r *SyncObjectReconciler) ensureReferenceWatch(ctx context.Context, ref syncv1alpha1.Reference) error {
	gvk := schema.GroupVersionKind{Group: ref.Group, Version: ref.Version, Kind: ref.Kind}

	r.watchedGVKsMu.Lock()
	_, ok := r.watchedGVKs[gvk]
	r.watchedGVKsMu.Unlock()
	if ok {
		return nil
	}

	watched := &unstructured.Unstructured{}
	watched.SetGroupVersionKind(gvk)

	mapFn := func(ctx context.Context, obj *unstructured.Unstructured) []reconcile.Request {
		return r.requestsForReferenced(ctx, obj)
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

// requestsForReferenced finds the SyncObjects (if any) whose Reference
// points at obj, so a change to the referenced object can trigger their
// reconcile immediately instead of waiting for the periodic resync.
func (r *SyncObjectReconciler) requestsForReferenced(ctx context.Context, obj *unstructured.Unstructured) []reconcile.Request {
	gvk := obj.GroupVersionKind()
	key := referenceKey(gvk.Group, gvk.Version, gvk.Kind, obj.GetNamespace(), obj.GetName())

	var syncObjects syncv1alpha1.SyncObjectList
	if err := r.Client.List(ctx, &syncObjects, client.MatchingFields{referenceIndexKey: key}); err != nil {
		log.FromContext(ctx).Error(err, "failed listing SyncObjects for referenced object", "gvk", gvk, "namespace", obj.GetNamespace(), "name", obj.GetName())
		return nil
	}

	requests := make([]reconcile.Request, 0, len(syncObjects.Items))
	for _, syncObject := range syncObjects.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&syncObject)})
	}
	return requests
}

// TODO: add unit test
// returns target and non-target namespaces
func (r *SyncObjectReconciler) getTargetNamespaces(ctx context.Context, syncObject syncv1alpha1.SyncObject) ([]string, []string, error) {
	targetNamespaces := syncObject.Spec.TargetNamespaces
	nonTargetNamespaces := syncObject.Spec.IgnoreNamespaces

	var allNamespaces corev1.NamespaceList

	if err := r.Client.List(ctx, &allNamespaces); err != nil {
		return nil, nil, fmt.Errorf("failed listing namespaces: %v", err)
	}

	// no namespaces defined, sync to all of them
	if len(targetNamespaces) == 0 {
		for _, namespace := range allNamespaces.Items {
			if namespace.GetName() == syncObject.Spec.Reference.Namespace {
				// don't create a replica in the reference namespace
				continue
			}
			targetNamespaces = append(targetNamespaces, namespace.GetName())
		}
	}

	// we only sync to specified namespaces, check which are nonTarget namespaces
	// so we can delete replicas there if there are some leftovers
	if len(targetNamespaces) > 0 {
		for _, namespace := range allNamespaces.Items {
			if namespace.GetName() == syncObject.Spec.Reference.Namespace {
				// don't remove reference
				continue
			}
			if !slices.Contains(targetNamespaces, namespace.GetName()) {
				// namespace is not a target
				nonTargetNamespaces = append(nonTargetNamespaces, namespace.GetName())
			}
		}
	}

	// Remove namespaces we want to ignore
	for _, ignoreNamespace := range syncObject.Spec.IgnoreNamespaces {
		targetNamespaces = remove(targetNamespaces, ignoreNamespace)
	}

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
func (r *SyncObjectReconciler) replicate(ctx context.Context, syncObject syncv1alpha1.SyncObject, namespace string) error {
	var original unstructured.Unstructured
	original.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   syncObject.Spec.Reference.Group,
		Version: syncObject.Spec.Reference.Version,
		Kind:    syncObject.Spec.Reference.Kind,
	})

	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: syncObject.Spec.Reference.Namespace, Name: syncObject.Spec.Reference.Name}, &original); err != nil {
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
func (r *SyncObjectReconciler) deleteReplica(ctx context.Context, syncObject syncv1alpha1.SyncObject, namespace string) error {
	var objectToDelete unstructured.Unstructured
	objectToDelete.SetName(syncObject.Spec.Reference.Name)
	objectToDelete.SetNamespace(namespace)
	objectToDelete.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   syncObject.Spec.Reference.Group,
		Version: syncObject.Spec.Reference.Version,
		Kind:    syncObject.Spec.Reference.Kind,
	})

	log.Log.Info("deleting", "object", objectToDelete)

	if err := r.Client.Delete(ctx, &objectToDelete); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed deleting replica: %v", err)
	}

	return nil
}

func (r *SyncObjectReconciler) deleteAllReplicas(ctx context.Context, syncObject syncv1alpha1.SyncObject) error {
	var namespaces corev1.NamespaceList

	if err := r.Client.List(ctx, &namespaces); err != nil {
		return fmt.Errorf("failed listing namespaces: %v", err)
	}

	var multiErr error
	for _, namespace := range namespaces.Items {
		if namespace.GetName() == syncObject.Spec.Reference.Namespace {
			// do not delete the original
			continue
		}

		if err := r.deleteReplica(ctx, syncObject, namespace.GetName()); err != nil {
			multiErr = errors.Join(multiErr, err)
		}
	}

	return multiErr
}
