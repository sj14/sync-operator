package controllers

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	syncv1alpha1 "github.com/sj14/sync-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// SyncObjectReconciler reconciles a SyncObject object
type SyncObjectReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=sync.sj14.github.io,resources=syncobjects,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=sync.sj14.github.io,resources=syncobjects/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=sync.sj14.github.io,resources=syncobjects/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the SyncObject object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *SyncObjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("reconciling SyncObject")

	var syncObject syncv1alpha1.SyncObject

	if err := r.Client.Get(ctx, req.NamespacedName, &syncObject); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed getting SyncObject: %v", err)
	}

	logger.Info("reference", "reference", syncObject.Spec.Reference)

	var original unstructured.Unstructured
	original.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   syncObject.Spec.Reference.Group,
		Version: syncObject.Spec.Reference.Version,
		Kind:    syncObject.Spec.Reference.Kind,
	})
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: syncObject.Spec.Reference.Namespace, Name: syncObject.Spec.Reference.Name}, &original); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed getting original object: %v", err)
	}

	logger.Info("original", "original", original)

	if len(syncObject.Spec.TargetNamespaces) > 0 {

	} else {
		var namespaces corev1.NamespaceList

		if err := r.Client.List(ctx, &namespaces); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed listing namespaces: %v", err)
		}

		for _, namespace := range namespaces.Items {
			replica := original.DeepCopy()
			replica.SetNamespace(namespace.GetName())

			// remove state from the old object
			replica.SetResourceVersion("")
			replica.SetUID(types.UID(""))
			// TODO: add more?

			// create new replica if it doesn't already exist
			var err error
			if err = r.Client.Create(ctx, replica); client.IgnoreAlreadyExists(err) != nil {
				logger.Error(err, "failed creating replica", "namespace", namespace.GetName())
				continue
			}

			if !apierrors.IsAlreadyExists(err) {
				// we create a new replica, no need for updating it
				continue
			}

			// replica already exists, just update it
			if err := r.Client.Update(ctx, replica); err != nil {
				logger.Error(err, "failed updating replica", "namespace", namespace.GetName())
			}
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SyncObjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&syncv1alpha1.SyncObject{}).
		Complete(r)
}
