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
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

const (
	// managedByLabel marks an object as a replica created by this operator.
	//
	// It's a label rather than an annotation so it can be selected on, by
	// kubectl or by the objectSelector of a ValidatingAdmissionPolicyBinding.
	//
	// Deliberately not app.kubernetes.io/managed-by: the referenced object
	// may already carry that (set by Helm, say), and overwriting it on the
	// replica would both lose that information and misreport the replica to
	// whatever tool set it.
	managedByLabel = "sync.sj14.github.io/managed-by"
	managedByValue = "sync-operator"

	// Provenance of a replica, for anyone wondering where it came from.
	syncObjectAnnotation      = "sync.sj14.github.io/sync-object"
	sourceNamespaceAnnotation = "sync.sj14.github.io/source-namespace"
	sourceNameAnnotation      = "sync.sj14.github.io/source-name"
)

// stripOriginalState removes the parts of the copied object that describe the
// original rather than the replica. What is left is the desired state: the
// spec, data, labels and annotations the two are meant to share.
func stripOriginalState(replica *unstructured.Unstructured) {
	// identity of the original
	replica.SetResourceVersion("")
	replica.SetUID(types.UID(""))
	replica.SetCreationTimestamp(metav1.Time{})
	replica.SetManagedFields(nil)

	// An owner reference to a namespaced owner only means anything within
	// that owner's own namespace. Kubernetes treats a reference to an owner
	// that isn't in the namespace as absent, and garbage collects the
	// dependent once all its owners are absent. A copied owner reference
	// therefore gets the replica deleted shortly after it is created -- and
	// since we watch replicas, recreated, and deleted again.
	replica.SetOwnerReferences(nil)

	// Whatever would remove these finalizers is watching the original, not a
	// copy of it in some other namespace, so on a replica they are never
	// removed. The replica could then never be deleted, which would also
	// block deleting the SyncObject and the namespace holding it.
	replica.SetFinalizers(nil)

	// Records an apply against the original. Leaving it on the replica would
	// make a later `kubectl apply` there diff against the wrong base.
	annotations := replica.GetAnnotations()
	delete(annotations, corev1.LastAppliedConfigAnnotation)
	replica.SetAnnotations(annotations)
}

// markAsReplica records that this object is a replica, and which SyncObject
// and source object it came from. Existing labels and annotations copied
// from the original are kept.
//
// Everything written here is derived from the SyncObject, never from the
// current time or the cluster's state: the values have to come out
// identical on every reconcile, otherwise each pass would be a real write,
// which would wake the watch, which would reconcile again.
func markAsReplica(replica *unstructured.Unstructured, syncObject syncv1alpha1.SyncObject) {
	labels := replica.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[managedByLabel] = managedByValue
	replica.SetLabels(labels)

	annotations := replica.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[syncObjectAnnotation] = syncObject.Name
	annotations[sourceNamespaceAnnotation] = syncObject.Spec.Reference.Namespace
	annotations[sourceNameAnnotation] = syncObject.Spec.Reference.Name
	replica.SetAnnotations(annotations)
}

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

	syncErr := r.sync(ctx, syncObject)

	// Recorded whatever happened, so a failure is visible in the object
	// rather than only in the operator's logs.
	if err := r.updateStatus(ctx, &syncObject, syncErr); err != nil {
		return ctrl.Result{}, errors.Join(syncErr, err)
	}
	if syncErr != nil {
		return ctrl.Result{}, syncErr
	}

	// when there was no error, requeue after the resync interval as a
	// drift-correction fallback -- reference changes are already synced
	// immediately via the watch set up above.
	return ctrl.Result{RequeueAfter: resyncInterval(syncObject)}, nil
}

// sync brings the replicas in line with the reference.
func (r *SyncObjectReconciler) sync(ctx context.Context, syncObject syncv1alpha1.SyncObject) error {
	logger := log.FromContext(ctx)

	// spec.reference was changed to point somewhere else: the replicas of the
	// previous reference are named after it, so nothing below would ever touch
	// them again and they'd be orphaned.
	if applied := syncObject.Status.AppliedReference; applied != nil && *applied != syncObject.Spec.Reference {
		logger.Info("reference changed, removing replicas of the previous reference", "previous", *applied, "current", syncObject.Spec.Reference)
		if err := r.deleteReplicas(ctx, syncObject, *applied, nil); err != nil {
			return fmt.Errorf("failed removing replicas of the previous reference: %v", err)
		}
	}

	targetNamespaces, err := r.getTargetNamespaces(ctx, syncObject)
	if err != nil {
		return fmt.Errorf("failed getting target namespaces: %v", err)
	}

	var multiErr error
	// cleanup leftovers, e.g. when the targetNamespaces changed
	if err := r.deleteReplicas(ctx, syncObject, syncObject.Spec.Reference, targetNamespaces); err != nil {
		multiErr = errors.Join(multiErr, fmt.Errorf("failed cleaning up replicas: %v", err))
	}

	// fetched once rather than per namespace: every replica is a copy of the
	// same object anyway
	original, err := r.getOriginal(ctx, syncObject.Spec.Reference)
	if err != nil {
		multiErr = errors.Join(multiErr, err)
	} else {
		for _, namespace := range targetNamespaces {
			if err := r.replicate(ctx, syncObject, original, namespace); err != nil {
				multiErr = errors.Join(multiErr, fmt.Errorf("failed creating replica: %v", err))
			}
		}
	}

	return multiErr
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
			var multiErr error
			for _, ref := range referencesToCleanUp(*syncObject) {
				if err := r.deleteReplicas(ctx, *syncObject, ref, nil); err != nil {
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
// into. Replicas anywhere else are cleaned up by deleteReplicas, which
// finds them by their marks rather than by namespace.
//
// The reference's own namespace is never a target: it holds the original,
// which must not be overwritten by a replica of itself, not even when the
// user listed that namespace explicitly.
func (r *SyncObjectReconciler) getTargetNamespaces(ctx context.Context, syncObject syncv1alpha1.SyncObject) ([]string, error) {
	// cloned: appending to a spec field's slice could otherwise write into
	// the SyncObject's own backing array.
	targetNamespaces := slices.Clone(syncObject.Spec.TargetNamespaces)

	// no namespaces defined, sync to all of them
	if len(targetNamespaces) == 0 {
		var allNamespaces corev1.NamespaceList
		if err := r.Client.List(ctx, &allNamespaces); err != nil {
			return nil, fmt.Errorf("failed listing namespaces: %v", err)
		}
		for _, namespace := range allNamespaces.Items {
			if isTerminating(namespace) {
				continue
			}
			targetNamespaces = append(targetNamespaces, namespace.GetName())
		}
	}

	// Remove namespaces we want to ignore
	for _, ignoreNamespace := range syncObject.Spec.IgnoreNamespaces {
		targetNamespaces = remove(targetNamespaces, ignoreNamespace)
	}

	return remove(targetNamespaces, syncObject.Spec.Reference.Namespace), nil
}

// isTerminating reports whether the namespace is on its way out. The API
// server refuses to create anything in one, so it is pointless as a
// replication target.
func isTerminating(namespace corev1.Namespace) bool {
	return namespace.Status.Phase == corev1.NamespaceTerminating || !namespace.DeletionTimestamp.IsZero()
}

// remove returns a copy of slice with all elements equal to s removed.
// The input slice is left untouched: slices.DeleteFunc mutates in place, and
// slice here may alias a SyncObject's own spec field.
func remove(slice []string, s string) []string {
	return slices.DeleteFunc(slices.Clone(slice), func(v string) bool {
		return v == s
	})
}

// getOriginal fetches the object the reference points at.
//
// A replica is refused as a source. Replicating a replica means two
// SyncObjects managing objects of the same kind and name: they would
// overwrite each other's copies on every pass, and because the chain keeps
// the original's name, the second one would eventually overwrite the
// original itself.
func (r *SyncObjectReconciler) getOriginal(ctx context.Context, ref syncv1alpha1.Reference) (*unstructured.Unstructured, error) {
	var original unstructured.Unstructured
	original.SetGroupVersionKind(ref.GroupVersionKind())

	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &original); err != nil {
		return nil, fmt.Errorf("failed getting original object: %v", err)
	}

	if original.GetLabels()[managedByLabel] == managedByValue {
		return nil, fmt.Errorf("refusing to replicate %s %s/%s: it is itself a replica, created by the SyncObject %q; reference that SyncObject's own source instead",
			ref.Kind, ref.Namespace, ref.Name, original.GetAnnotations()[syncObjectAnnotation])
	}

	return &original, nil
}

// TODO: Add finalizer, ownerreference?
func (r *SyncObjectReconciler) replicate(ctx context.Context, syncObject syncv1alpha1.SyncObject, original *unstructured.Unstructured, namespace string) error {
	replica := original.DeepCopy()
	replica.SetNamespace(namespace)

	stripOriginalState(replica)
	markAsReplica(replica, syncObject)

	log.Log.Info("creating/updating", "gvk", replica.GroupVersionKind().String(), "namespace", replica.GetNamespace(), "name", replica.GetName())

	// create new replica if it doesn't already exist
	err := r.Client.Create(ctx, replica)
	if err != nil && apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
		// The namespace is being deleted, so nothing can be created in it and
		// whatever is in there is about to go away regardless. Retrying until
		// the namespace finally disappears would only produce noise.
		//
		// getTargetNamespaces already skips namespaces it saw terminating;
		// this covers the ones that started while we were working, and the
		// ones a user listed in targetNamespaces explicitly.
		log.Log.Info("skipping terminating namespace", "namespace", namespace)
		return nil
	}
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

// isReplicaOf reports whether obj is a replica this SyncObject created from
// ref, going by the marks replicate put on it.
func isReplicaOf(obj *unstructured.Unstructured, syncObject syncv1alpha1.SyncObject, ref syncv1alpha1.Reference) bool {
	if obj.GetLabels()[managedByLabel] != managedByValue {
		return false
	}

	annotations := obj.GetAnnotations()
	return annotations[syncObjectAnnotation] == syncObject.Name &&
		annotations[sourceNamespaceAnnotation] == ref.Namespace &&
		annotations[sourceNameAnnotation] == ref.Name
}

// deleteReplicas removes the replicas this SyncObject created from ref,
// apart from those in the keep namespaces. Pass no keep namespaces to
// remove all of them.
//
// Replicas are identified by the marks replicate leaves on them rather than
// by name, so an unrelated object that merely happens to share a name is
// never touched. Neither is the original, which carries no such marks --
// this is what makes cleaning up a previous reference safe even when it
// shares a kind and name with the current one.
func (r *SyncObjectReconciler) deleteReplicas(ctx context.Context, syncObject syncv1alpha1.SyncObject, ref syncv1alpha1.Reference, keep []string) error {
	listGVK := ref.GroupVersionKind()
	listGVK.Kind += "List"

	var candidates unstructured.UnstructuredList
	candidates.SetGroupVersionKind(listGVK)

	// the label narrows this down server side; the annotations below then
	// pin it to this SyncObject and this particular reference.
	if err := r.Client.List(ctx, &candidates, client.MatchingLabels{managedByLabel: managedByValue}); err != nil {
		if meta.IsNoMatchError(err) {
			// The kind itself is gone from the cluster, so the API server
			// has already removed everything of that kind, replicas
			// included. Treating this as an error instead would leave the
			// SyncObject stuck in Terminating, since the finalizer could
			// never complete.
			return nil
		}
		return fmt.Errorf("failed listing replicas: %v", err)
	}

	var multiErr error
	for _, replica := range candidates.Items {
		if !isReplicaOf(&replica, syncObject, ref) {
			continue
		}
		if slices.Contains(keep, replica.GetNamespace()) {
			continue
		}

		log.Log.Info("deleting replica", "gvk", replica.GroupVersionKind().String(), "namespace", replica.GetNamespace(), "name", replica.GetName())

		if err := r.Client.Delete(ctx, &replica); err != nil && !apierrors.IsNotFound(err) {
			multiErr = errors.Join(multiErr, fmt.Errorf("failed deleting replica in %q: %v", replica.GetNamespace(), err))
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

// maxConditionMessage keeps the message inside the 32768 characters the API
// allows. An error joined across a few hundred namespaces can get long.
const maxConditionMessage = 2048

// updateStatus records the outcome of a sync on the SyncObject itself, so a
// failure is visible to whoever created it rather than only in the
// operator's logs.
//
// It writes nothing when the status is unchanged. A write here would
// otherwise wake the SyncObject watch and reconcile again, forever.
func (r *SyncObjectReconciler) updateStatus(ctx context.Context, syncObject *syncv1alpha1.SyncObject, syncErr error) error {
	previous := syncObject.Status.DeepCopy()

	condition := metav1.Condition{
		Type:               syncv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Synced",
		Message:            "The reference is replicated to its target namespaces",
		ObservedGeneration: syncObject.Generation,
	}
	if syncErr != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "SyncFailed"
		condition.Message = truncate(syncErr.Error(), maxConditionMessage)
	} else {
		// Only recorded once the replicas actually exist. If replication
		// failed we keep the previous value, so the next attempt still knows
		// which replicas to clean up.
		ref := syncObject.Spec.Reference
		syncObject.Status.AppliedReference = &ref
	}

	meta.SetStatusCondition(&syncObject.Status.Conditions, condition)
	syncObject.Status.ObservedGeneration = syncObject.Generation

	if equality.Semantic.DeepEqual(previous, &syncObject.Status) {
		return nil
	}

	if err := r.Status().Update(ctx, syncObject); err != nil {
		return fmt.Errorf("failed updating status: %v", err)
	}

	return nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
