package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	syncv1alpha1 "github.com/sj14/sync-operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRemove(t *testing.T) {
	tests := []struct {
		name  string
		slice []string
		elem  string
		want  []string
	}{
		{"remove middle", []string{"a", "b", "c"}, "b", []string{"a", "c"}},
		{"remove first", []string{"a", "b", "c"}, "a", []string{"b", "c"}},
		{"remove last", []string{"a", "b", "c"}, "c", []string{"a", "b"}},
		{"not present", []string{"a", "b", "c"}, "z", []string{"a", "b", "c"}},
		{"empty slice", nil, "a", nil},
		{"single element removed", []string{"a"}, "a", nil},
		{"duplicates removed", []string{"a", "b", "a"}, "a", []string{"b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := remove(tt.slice, tt.elem)
			// nil and an empty, non-nil slice are equivalent results here
			// (both are iterated/appended to identically by callers), so
			// normalize before comparing order-sensitively rather than
			// asserting on which one the implementation happens to produce.
			want := append([]string{}, tt.want...)
			got = append([]string{}, got...)
			require.Equal(t, want, got)

			// the input slice must not be mutated by the call.
			original := append([]string(nil), tt.slice...)
			remove(tt.slice, tt.elem)
			require.Equal(t, original, tt.slice)
		})
	}
}

func TestResyncInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"unset falls back to default", 0, defaultResyncInterval},
		{"explicit value is preserved", 30 * time.Minute, 30 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syncObject := syncv1alpha1.SyncObject{
				Spec: syncv1alpha1.SyncObjectSpec{
					ResyncInterval: metav1.Duration{Duration: tt.interval},
				},
			}
			require.Equal(t, tt.want, resyncInterval(syncObject))
		})
	}
}

func TestReferencedObjectKey(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "group", Version: "v1", Kind: "Kind"}
	base := referencedObjectKey(gvk, "name")
	require.Equal(t, base, referencedObjectKey(gvk, "name"), "same fields must produce the same key")

	variants := []string{
		referencedObjectKey(schema.GroupVersionKind{Group: "other", Version: "v1", Kind: "Kind"}, "name"),
		referencedObjectKey(schema.GroupVersionKind{Group: "group", Version: "v2", Kind: "Kind"}, "name"),
		referencedObjectKey(schema.GroupVersionKind{Group: "group", Version: "v1", Kind: "Other"}, "name"),
		referencedObjectKey(gvk, "other-name"),
	}
	for _, v := range variants {
		require.NotEqual(t, base, v, "changing a single field must change the key")
	}
}

func TestIndexByReference(t *testing.T) {
	ref := syncv1alpha1.Reference{
		Group:     "apps",
		Version:   "v1",
		Kind:      "Deployment",
		Name:      "my-deploy",
		Namespace: "my-ns",
	}
	syncObject := &syncv1alpha1.SyncObject{
		Spec: syncv1alpha1.SyncObjectSpec{Reference: ref},
	}

	want := []string{referencedObjectKey(ref.GroupVersionKind(), ref.Name)}
	require.Equal(t, want, indexByReference(syncObject))

	// A replica differs from the original only by namespace, so it has to
	// land on the same key -- that's what lets an event for a replica find
	// the SyncObject that owns it.
	replica := &unstructured.Unstructured{}
	replica.SetGroupVersionKind(ref.GroupVersionKind())
	replica.SetNamespace("some-other-namespace")
	replica.SetName(ref.Name)
	require.Equal(t, want[0], referencedObjectKey(replica.GroupVersionKind(), replica.GetName()))

	// indexByReference is registered against the SyncObject type; anything
	// else should be ignored rather than panic.
	require.Nil(t, indexByReference(&corev1.ConfigMap{}))
}

func TestGetTargetNamespaces(t *testing.T) {
	const referenceNamespace = "origin-ns"

	tests := []struct {
		name             string
		targetNamespaces []string
		ignoreNamespaces []string
		wantTargets      []string
	}{
		{
			name:        "no targets defined syncs to all but the reference namespace",
			wantTargets: []string{"a-ns", "b-ns"},
		},
		{
			name:             "only the explicit targets are synced to",
			targetNamespaces: []string{"a-ns"},
			wantTargets:      []string{"a-ns"},
		},
		{
			name:             "ignored namespaces are dropped from targets",
			ignoreNamespaces: []string{"b-ns"},
			wantTargets:      []string{"a-ns"},
		},
		{
			name:             "ignoring the reference namespace changes nothing",
			ignoreNamespaces: []string{referenceNamespace},
			wantTargets:      []string{"a-ns", "b-ns"},
		},
		{
			// the reference namespace holds the original, which must not be
			// overwritten by a replica of itself.
			name:             "targeting the reference namespace does not replicate over the original",
			targetNamespaces: []string{"a-ns", referenceNamespace},
			wantTargets:      []string{"a-ns"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithObjects(
					&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: referenceNamespace}},
					&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "a-ns"}},
					&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "b-ns"}},
				).
				Build()

			r := &SyncObjectReconciler{Client: fakeClient}

			syncObject := syncv1alpha1.SyncObject{
				Spec: syncv1alpha1.SyncObjectSpec{
					Reference: syncv1alpha1.Reference{
						Group:     "",
						Version:   "v1",
						Kind:      "ConfigMap",
						Name:      "cm",
						Namespace: referenceNamespace,
					},
					TargetNamespaces: tt.targetNamespaces,
					IgnoreNamespaces: tt.ignoreNamespaces,
				},
			}

			targets, err := r.getTargetNamespaces(context.Background(), syncObject)
			require.NoError(t, err)

			require.ElementsMatch(t, tt.wantTargets, targets)

			// whatever the configuration, the original must never be
			// replicated over.
			require.NotContains(t, targets, referenceNamespace)
		})
	}
}

func TestMarkAsReplica(t *testing.T) {
	syncObject := syncv1alpha1.SyncObject{
		ObjectMeta: metav1.ObjectMeta{Name: "my-syncobject"},
		Spec: syncv1alpha1.SyncObjectSpec{
			Reference: syncv1alpha1.Reference{
				Group:     "",
				Version:   "v1",
				Kind:      "ConfigMap",
				Name:      "source-cm",
				Namespace: "source-ns",
			},
		},
	}

	newReplica := func() *unstructured.Unstructured {
		replica := &unstructured.Unstructured{}
		replica.SetGroupVersionKind(syncObject.Spec.Reference.GroupVersionKind())
		replica.SetName("source-cm")
		replica.SetNamespace("target-ns")
		return replica
	}

	t.Run("marks the replica", func(t *testing.T) {
		replica := newReplica()
		markAsReplica(replica, syncObject)

		require.Equal(t, managedByValue, replica.GetLabels()[managedByLabel])
		require.Equal(t, map[string]string{
			syncObjectAnnotation:      "my-syncobject",
			sourceNamespaceAnnotation: "source-ns",
			sourceNameAnnotation:      "source-cm",
		}, replica.GetAnnotations())
	})

	t.Run("keeps labels and annotations copied from the original", func(t *testing.T) {
		replica := newReplica()
		replica.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm", "team": "platform"})
		replica.SetAnnotations(map[string]string{"example.com/note": "keep me"})

		markAsReplica(replica, syncObject)

		labels := replica.GetLabels()
		require.Equal(t, managedByValue, labels[managedByLabel])
		require.Equal(t, "platform", labels["team"])
		require.Equal(t, "Helm", labels["app.kubernetes.io/managed-by"],
			"the original's own managed-by must not be hijacked")
		require.Equal(t, "keep me", replica.GetAnnotations()["example.com/note"])
	})

	// Anything time- or state-dependent in here would make every reconcile a
	// real write, which would wake the watch and reconcile again, forever.
	t.Run("is deterministic", func(t *testing.T) {
		first := newReplica()
		markAsReplica(first, syncObject)

		second := newReplica()
		markAsReplica(second, syncObject)
		markAsReplica(second, syncObject)

		require.Equal(t, first.Object, second.Object, "marking must be repeatable and idempotent")
	})
}

func TestReferencesToCleanUp(t *testing.T) {
	current := syncv1alpha1.Reference{Group: "", Version: "v1", Kind: "ConfigMap", Name: "current", Namespace: "ns"}
	previous := syncv1alpha1.Reference{Group: "", Version: "v1", Kind: "ConfigMap", Name: "previous", Namespace: "ns"}

	tests := []struct {
		name    string
		applied *syncv1alpha1.Reference
		want    []syncv1alpha1.Reference
	}{
		{"nothing applied yet", nil, []syncv1alpha1.Reference{current}},
		{"applied matches spec", &current, []syncv1alpha1.Reference{current}},
		{"reference changed", &previous, []syncv1alpha1.Reference{current, previous}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syncObject := syncv1alpha1.SyncObject{
				Spec:   syncv1alpha1.SyncObjectSpec{Reference: current},
				Status: syncv1alpha1.SyncObjectStatus{AppliedReference: tt.applied},
			}
			require.Equal(t, tt.want, referencesToCleanUp(syncObject))
		})
	}
}

// testRef is the reference used by the deleteReplicas tests below.
var testRef = syncv1alpha1.Reference{
	Group: "", Version: "v1", Kind: "ConfigMap", Name: "shared-name", Namespace: "origin-ns",
}

// testSyncObject owns the replicas of testRef.
var testSyncObject = syncv1alpha1.SyncObject{
	ObjectMeta: metav1.ObjectMeta{Name: "owner"},
	Spec:       syncv1alpha1.SyncObjectSpec{Reference: testRef},
}

// markedConfigMap builds a ConfigMap carrying the marks replicate would
// have put on a replica of ref created by the named SyncObject.
func markedConfigMap(namespace, syncObjectName string, ref syncv1alpha1.Reference) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ref.Name,
			Namespace: namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
			Annotations: map[string]string{
				syncObjectAnnotation:      syncObjectName,
				sourceNamespaceAnnotation: ref.Namespace,
				sourceNameAnnotation:      ref.Name,
			},
		},
	}
}

// TestGetTargetNamespacesSkipsTerminating covers a namespace on its way
// out: nothing can be created in it, so replicating into it only produces
// failures until it finally disappears.
func TestGetTargetNamespacesSkipsTerminating(t *testing.T) {
	deletionTimestamp := metav1.Now()

	fakeClient := fake.NewClientBuilder().
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "alive-ns"}},
			&corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: "phase-terminating-ns"},
				Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating},
			},
			&corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "being-deleted-ns",
					DeletionTimestamp: &deletionTimestamp,
					Finalizers:        []string{"kubernetes.io/test"},
				},
			},
		).
		Build()

	r := &SyncObjectReconciler{Client: fakeClient}

	syncObject := syncv1alpha1.SyncObject{
		Spec: syncv1alpha1.SyncObjectSpec{Reference: testRef},
	}

	targets, err := r.getTargetNamespaces(context.Background(), syncObject)
	require.NoError(t, err)

	require.Equal(t, []string{"alive-ns"}, targets)
}

// TestReplicateSkipsTerminatingNamespace closes the gap the filtering above
// cannot: an explicitly listed namespace is never checked, and any namespace
// can start terminating between the check and the create.
func TestReplicateSkipsTerminatingNamespace(t *testing.T) {
	terminating := apierrors.NewForbidden(
		schema.GroupResource{Resource: "configmaps"}, testRef.Name,
		errors.New("unable to create new content in namespace doomed-ns because it is being terminated"),
	)
	terminating.ErrStatus.Details.Causes = append(terminating.ErrStatus.Details.Causes, metav1.StatusCause{
		Type:    corev1.NamespaceTerminatingCause,
		Message: "namespace doomed-ns is being terminated",
		Field:   "metadata.namespace",
	})

	fakeClient := fake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				return terminating
			},
		}).
		Build()

	r := &SyncObjectReconciler{Client: fakeClient}

	original := &unstructured.Unstructured{}
	original.SetGroupVersionKind(testRef.GroupVersionKind())
	original.SetName(testRef.Name)
	original.SetNamespace(testRef.Namespace)

	require.NoError(t, r.replicate(context.Background(), testSyncObject, original, "doomed-ns"),
		"a namespace being deleted is not a failure to report and retry")
}

func TestIsReplicaOf(t *testing.T) {
	otherRef := syncv1alpha1.Reference{
		Group: "", Version: "v1", Kind: "ConfigMap", Name: "shared-name", Namespace: "somewhere-else",
	}

	tests := []struct {
		name string
		obj  *corev1.ConfigMap
		want bool
	}{
		{
			name: "a replica of this reference",
			obj:  markedConfigMap("target-ns", testSyncObject.Name, testRef),
			want: true,
		},
		{
			// the original carries no marks, which is what keeps it safe
			// even when a previous reference shared its kind and name
			name: "the original itself",
			obj:  &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: testRef.Name, Namespace: testRef.Namespace}},
			want: false,
		},
		{
			name: "an unrelated object that happens to share the name",
			obj:  &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: testRef.Name, Namespace: "someone-elses-ns"}},
			want: false,
		},
		{
			name: "a replica belonging to a different SyncObject",
			obj:  markedConfigMap("target-ns", "someone-else", testRef),
			want: false,
		},
		{
			name: "a replica of a different source object",
			obj:  markedConfigMap("target-ns", testSyncObject.Name, otherRef),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(testRef.GroupVersionKind())
			obj.SetName(tt.obj.Name)
			obj.SetNamespace(tt.obj.Namespace)
			obj.SetLabels(tt.obj.Labels)
			obj.SetAnnotations(tt.obj.Annotations)

			require.Equal(t, tt.want, isReplicaOf(obj, testSyncObject, testRef))
		})
	}
}

// TestDeleteReplicasOnlyDeletesOwnReplicas covers what used to need special
// casing: the original of a reference sharing its kind and name is now left
// alone simply because it isn't marked as a replica.
func TestDeleteReplicasOnlyDeletesOwnReplicas(t *testing.T) {
	otherRef := syncv1alpha1.Reference{
		Group: "", Version: "v1", Kind: "ConfigMap", Name: "shared-name", Namespace: "new-origin-ns",
	}

	fakeClient := fake.NewClientBuilder().
		WithObjects(
			// the original of this reference
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: testRef.Name, Namespace: testRef.Namespace}},
			// another original, sharing the kind and name
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: otherRef.Name, Namespace: otherRef.Namespace}},
			// a pre-existing object that merely shares the name
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: testRef.Name, Namespace: "innocent-ns"}},
			// a replica of a different SyncObject
			markedConfigMap("other-owner-ns", "someone-else", testRef),
			// our own replicas
			markedConfigMap("target-a", testSyncObject.Name, testRef),
			markedConfigMap("target-b", testSyncObject.Name, testRef),
		).
		Build()

	r := &SyncObjectReconciler{Client: fakeClient}

	require.NoError(t, r.deleteReplicas(context.Background(), testSyncObject, testRef, nil))

	exists := func(namespace string) bool {
		err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: testRef.Name}, &corev1.ConfigMap{})
		return err == nil
	}

	require.True(t, exists(testRef.Namespace), "the original must never be deleted")
	require.True(t, exists(otherRef.Namespace), "another reference's original must not be deleted")
	require.True(t, exists("innocent-ns"), "an unrelated object sharing the name must not be deleted")
	require.True(t, exists("other-owner-ns"), "another SyncObject's replica must not be deleted")

	require.False(t, exists("target-a"), "our own replica should be deleted")
	require.False(t, exists("target-b"), "our own replica should be deleted")
}

func TestDeleteReplicasKeepsGivenNamespaces(t *testing.T) {
	fakeClient := fake.NewClientBuilder().
		WithObjects(
			markedConfigMap("keep-me", testSyncObject.Name, testRef),
			markedConfigMap("drop-me", testSyncObject.Name, testRef),
		).
		Build()

	r := &SyncObjectReconciler{Client: fakeClient}

	require.NoError(t, r.deleteReplicas(context.Background(), testSyncObject, testRef, []string{"keep-me"}))

	exists := func(namespace string) bool {
		err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: testRef.Name}, &corev1.ConfigMap{})
		return err == nil
	}

	require.True(t, exists("keep-me"), "a replica in a namespace that is still a target should survive")
	require.False(t, exists("drop-me"), "a replica outside the target namespaces should be deleted")
}

// TestDeleteReplicasToleratesUnknownKind covers the referenced kind being
// removed from the cluster, e.g. its CRD was uninstalled. The API server
// has already removed the objects of that kind, so there is nothing to
// clean up -- and reporting an error would leave the SyncObject stuck in
// Terminating, because its finalizer could never complete.
func TestDeleteReplicasToleratesUnknownKind(t *testing.T) {
	noMatch := &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: "example.com", Kind: "Gone"},
		SearchedVersions: []string{"v1"},
	}

	fakeClient := fake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				return noMatch
			},
		}).
		Build()

	r := &SyncObjectReconciler{Client: fakeClient}

	require.NoError(t, r.deleteReplicas(context.Background(), testSyncObject, testRef, nil),
		"a kind that no longer exists means there is nothing left to delete")
}

// TestHandleFinalizerCompletesWhenKindIsUnknown is the reason the above
// matters: a SyncObject whose kind has disappeared must still be deletable.
func TestHandleFinalizerCompletesWhenKindIsUnknown(t *testing.T) {
	deletionTimestamp := metav1.Now()
	syncObject := &syncv1alpha1.SyncObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:              testSyncObject.Name,
			Finalizers:        []string{finalizerName},
			DeletionTimestamp: &deletionTimestamp,
		},
		Spec: syncv1alpha1.SyncObjectSpec{Reference: testRef},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, syncv1alpha1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(syncObject).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				return &meta.NoKindMatchError{
					GroupKind:        schema.GroupKind{Group: testRef.Group, Kind: testRef.Kind},
					SearchedVersions: []string{testRef.Version},
				}
			},
		}).
		Build()

	r := &SyncObjectReconciler{Client: fakeClient}

	stop, err := r.handleFinalizer(context.Background(), syncObject)
	require.NoError(t, err)
	require.True(t, stop, "reconciliation should stop, the object is being deleted")
	require.NotContains(t, syncObject.Finalizers, finalizerName,
		"the finalizer must be removed, otherwise the SyncObject is stuck in Terminating forever")
}

func TestDeleteReplicasPropagatesErrors(t *testing.T) {
	const failingNamespace = "failing-ns"

	wantErr := errors.New("boom")
	deletedOK := false

	fakeClient := fake.NewClientBuilder().
		WithObjects(
			markedConfigMap(failingNamespace, testSyncObject.Name, testRef),
			markedConfigMap("ok-ns", testSyncObject.Name, testRef),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetNamespace() == failingNamespace {
					return wantErr
				}
				deletedOK = true
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	r := &SyncObjectReconciler{Client: fakeClient}

	err := r.deleteReplicas(context.Background(), testSyncObject, testRef, nil)
	require.Error(t, err, "an error from a single namespace must not be swallowed")
	require.ErrorContains(t, err, wantErr.Error())
	require.True(t, deletedOK, "deletion in the non-failing namespace should still have been attempted")
}
