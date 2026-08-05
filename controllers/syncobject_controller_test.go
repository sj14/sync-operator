package controllers

import (
	"context"
	"errors"
	"testing"

	syncv1alpha1 "github.com/sj14/sync-operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	syncObject := syncv1alpha1.SyncObject{
		Spec: syncv1alpha1.SyncObjectSpec{
			Reference: syncv1alpha1.Reference{
				Group:     "",
				Version:   "v1",
				Kind:      "ConfigMap",
				Name:      "cm",
				Namespace: referenceNamespace,
			},
		},
	}

	err := r.deleteAllReplicas(context.Background(), syncObject)
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

	syncObject := syncv1alpha1.SyncObject{
		Spec: syncv1alpha1.SyncObjectSpec{
			Reference: syncv1alpha1.Reference{
				Group:     "",
				Version:   "v1",
				Kind:      "ConfigMap",
				Name:      "does-not-exist",
				Namespace: referenceNamespace,
			},
		},
	}

	// deleting a replica that was never created (e.g. a namespace that was
	// never a sync target) must not be reported as an error.
	require.NoError(t, r.deleteReplica(context.Background(), syncObject, emptyNamespace))
}
