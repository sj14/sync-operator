package controllers

import (
	"context"
	"errors"
	"fmt"

	syncv1alpha1 "github.com/sj14/sync-operator/api/v1alpha1"
	"golang.org/x/exp/slices"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// SyncObjectReconciler reconciles a SyncObject object
type SyncObjectReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const finalizerName = "sync.sj14.github.io/finalizer"

//+kubebuilder:rbac:groups=sync.sj14.github.io,resources=syncobjects,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=sync.sj14.github.io,resources=syncobjects/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=sync.sj14.github.io,resources=syncobjects/finalizers,verbs=update

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

	logger.Info("reference", "reference", syncObject.Spec.Reference)

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

	// cleanup leftovers, e.g. when the targetNamespaces changed
	for _, namespace := range nonTargetNamespaces {
		r.deleteReplica(ctx, syncObject, namespace)
	}

	for _, namespace := range targetNamespaces {
		r.replicate(ctx, syncObject, namespace)
	}

	return ctrl.Result{}, nil
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&syncv1alpha1.SyncObject{}).
		Complete(r)
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

func remove(slice []string, s string) []string {
	var result []string
	for idx := range slice {
		if slice[idx] == s {
			continue
		}
		result = append(slice[:idx], slice[idx+1:]...)
	}
	return result
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

	// create new replica if it doesn't already exist
	err := r.Client.Create(ctx, replica)
	if !apierrors.IsAlreadyExists(err) {
		// we create a new replica, no need for updating it
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed creating replica in %q: %v", namespace, err)
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

	log.Log.Info("going to delete", "object", objectToDelete, "ns", namespace)

	if err := r.Client.Delete(ctx, &objectToDelete); err != nil {
		return fmt.Errorf("failed deleting replica: %v", err)
	}

	return nil
}

func (r *SyncObjectReconciler) deleteAllReplicas(ctx context.Context, syncObject syncv1alpha1.SyncObject) error {
	var namespaces corev1.NamespaceList

	if err := r.Client.List(ctx, &namespaces); err != nil {
		return fmt.Errorf("failed listing namespaces: %v", err)
	}

	var err error
	for _, namespace := range namespaces.Items {
		if namespace.GetName() == syncObject.Spec.Reference.Namespace {
			// do not delete the original
			continue
		}

		// log.Log.Info("going to delete", "object", objectToDelete, "ns", namespace.GetName())
		if err := r.deleteReplica(ctx, syncObject, namespace.GetName()); err != nil {
			errors.Join(err)
		}
	}

	return err
}
