package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	syncv1alpha1 "github.com/sj14/sync-operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
		wantNonTargets   []string
	}{
		{
			name:           "no targets defined syncs to all but the reference namespace",
			wantTargets:    []string{"a-ns", "b-ns"},
			wantNonTargets: nil,
		},
		{
			name:             "explicit targets make the rest non-targets",
			targetNamespaces: []string{"a-ns"},
			wantTargets:      []string{"a-ns"},
			wantNonTargets:   []string{"b-ns"},
		},
		{
			name:             "ignored namespaces are dropped from targets and cleaned up",
			ignoreNamespaces: []string{"b-ns"},
			wantTargets:      []string{"a-ns"},
			wantNonTargets:   []string{"b-ns"},
		},
		{
			// the reference namespace holds the original; listing it here
			// must not turn the original into a deletion candidate.
			name:             "ignoring the reference namespace never targets it for deletion",
			ignoreNamespaces: []string{referenceNamespace},
			wantTargets:      []string{"a-ns", "b-ns"},
			wantNonTargets:   nil,
		},
		{
			// likewise, explicitly targeting it must not replicate over it.
			name:             "targeting the reference namespace does not replicate over the original",
			targetNamespaces: []string{"a-ns", referenceNamespace},
			wantTargets:      []string{"a-ns"},
			wantNonTargets:   []string{"b-ns"},
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

			targets, nonTargets, err := r.getTargetNamespaces(context.Background(), syncObject)
			require.NoError(t, err)

			require.ElementsMatch(t, tt.wantTargets, targets)
			require.ElementsMatch(t, tt.wantNonTargets, nonTargets)

			// whatever the configuration, the original must never be a
			// replication target nor a deletion candidate.
			require.NotContains(t, targets, referenceNamespace)
			require.NotContains(t, nonTargets, referenceNamespace)
		})
	}
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

// TestDeleteAllReplicasProtectsReferencedObjects covers the nastiest case of
// a changed reference: the old and new reference share a kind and name and
// differ only in namespace, so a naive cleanup of the old reference's
// replicas would delete the new reference's original object.
func TestDeleteAllReplicasProtectsReferencedObjects(t *testing.T) {
	oldRef := syncv1alpha1.Reference{Group: "", Version: "v1", Kind: "ConfigMap", Name: "shared-name", Namespace: "old-ns"}
	newRef := syncv1alpha1.Reference{Group: "", Version: "v1", Kind: "ConfigMap", Name: "shared-name", Namespace: "new-ns"}

	fakeClient := fake.NewClientBuilder().
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "old-ns"}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "new-ns"}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "replica-ns"}},
			// the two originals plus one actual replica
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "shared-name", Namespace: "old-ns"}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "shared-name", Namespace: "new-ns"}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "shared-name", Namespace: "replica-ns"}},
		).
		Build()

	r := &SyncObjectReconciler{Client: fakeClient}

	require.NoError(t, r.deleteAllReplicas(context.Background(), oldRef, newRef))

	exists := func(namespace string) bool {
		err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "shared-name"}, &corev1.ConfigMap{})
		return err == nil
	}

	require.True(t, exists("old-ns"), "the old reference's own object must never be deleted")
	require.True(t, exists("new-ns"), "the new reference's original must not be deleted as if it were a replica")
	require.False(t, exists("replica-ns"), "an actual replica of the old reference should be deleted")
}

func TestDeleteAllReplicasPropagatesErrors(t *testing.T) {
	referenceNamespace := "reference-ns"
	failingNamespace := "failing-ns"
	okNamespace := "ok-ns"

	namespaces := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: referenceNamespace}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: failingNamespace}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: okNamespace}},
	}

	replicas := []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: failingNamespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: okNamespace}},
	}

	wantErr := errors.New("boom")
	deletedOK := false

	fakeClient := fake.NewClientBuilder().
		WithObjects(append(namespaces, replicas...)...).
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

	ref := syncv1alpha1.Reference{
		Group:     "",
		Version:   "v1",
		Kind:      "ConfigMap",
		Name:      "cm",
		Namespace: referenceNamespace,
	}

	err := r.deleteAllReplicas(context.Background(), ref)
	require.Error(t, err, "an error from a single namespace must not be swallowed")
	require.ErrorContains(t, err, wantErr.Error())
	require.True(t, deletedOK, "deletion in the non-failing namespace should still have been attempted")
}

func TestDeleteReplicaIgnoresNotFound(t *testing.T) {
	referenceNamespace := "reference-ns"
	emptyNamespace := "empty-ns"

	fakeClient := fake.NewClientBuilder().
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: referenceNamespace}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: emptyNamespace}},
		).
		Build()

	r := &SyncObjectReconciler{Client: fakeClient}

	ref := syncv1alpha1.Reference{
		Group:     "",
		Version:   "v1",
		Kind:      "ConfigMap",
		Name:      "does-not-exist",
		Namespace: referenceNamespace,
	}

	// deleting a replica that was never created (e.g. a namespace that was
	// never a sync target) must not be reported as an error.
	require.NoError(t, r.deleteReplica(context.Background(), ref, emptyNamespace))
}
