package controllers

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	syncv1alpha1 "github.com/sj14/sync-operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	k8sClient client.Client
)

func TestMain(m *testing.M) {
	// setup
	ctx, cancel := context.WithCancel(context.TODO())
	testEnv, err := setup(ctx)
	if err != nil {
		shutdown(cancel, testEnv)
		log.Fatalf("failed test setup: %v", err)
	}

	// run tests
	code := m.Run()

	// cleanup
	shutdown(cancel, testEnv)
	os.Exit(code)
}

func setup(ctx context.Context) (*envtest.Environment, error) {
	logf.SetLogger(zap.New(zap.WriteTo(os.Stdout), zap.UseDevMode(true)))

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "deploy", "crds")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		return testEnv, fmt.Errorf("failed starting the test environment: %s", err)
	}
	if err := syncv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return testEnv, fmt.Errorf("failed adding scheme: %s", err)
	}

	// setup global k8sClient used in tests
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return testEnv, fmt.Errorf("failed creating new controller client: %s", err)
	}

	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
	})
	if err != nil {
		return testEnv, fmt.Errorf("failed creating new manager: %s", err)
	}

	err = (&SyncObjectReconciler{
		Client: k8sManager.GetClient(),
		Scheme: k8sManager.GetScheme(),
	}).SetupWithManager(k8sManager)
	if err != nil {
		return testEnv, fmt.Errorf("SyncObjectReconciler setup failed: %s", err)
	}

	go func() {
		err = k8sManager.Start(ctx)
		if err != nil {
			log.Fatalf("failed starting k8s manager: %s\n", err)
		}
	}()

	return testEnv, nil
}

func shutdown(cancel context.CancelFunc, testEnv *envtest.Environment) {
	log.Println("tearing down the test environment")
	cancel()
	if testEnv == nil {
		return
	}
	if err := testEnv.Stop(); err != nil {
		log.Printf("failed stopping test environment: %s\n", err)
	}
}

const (
	timeout  = 10 * time.Second
	interval = 250 * time.Millisecond
)

// updateWithRetry re-reads obj, applies mutate and writes it back, retrying
// on conflict.
//
// The operator writes to these same objects -- the SyncObject's status, and
// the replicas themselves -- so a plain read-modify-write races with it and
// intermittently loses with "the object has been modified".
func updateWithRetry(ctx context.Context, t *testing.T, obj client.Object, mutate func()) {
	t.Helper()

	key := client.ObjectKeyFromObject(obj)
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, key, obj); err != nil {
			return err
		}
		mutate()
		return k8sClient.Update(ctx, obj)
	}))
}

func TestControllersCreatesReplica(t *testing.T) {
	ctx := context.Background()

	targetNamespace := &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "target-namespace",
		},
	}

	configMapPayload := map[string]string{
		"key1": "value1",
		"key2": "value2",
		"key3": "value3",
	}

	require.NoError(t, k8sClient.Create(ctx, getOriginNamespace()))
	require.NoError(t, k8sClient.Create(ctx, getOriginConfigMap(configMapPayload)))
	require.NoError(t, k8sClient.Create(ctx, targetNamespace))

	// just for comparison, do not create target configmap
	targetConfigMap := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      getOriginConfigMap(nil).Name, // we keep the name from the origin
			Namespace: targetNamespace.Name,
		},
	}

	t.Run("Check if target namespace does not contain replica", func(t *testing.T) {
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(targetConfigMap), targetConfigMap)
		require.True(t, apierrors.IsNotFound(err))
	})

	t.Run("Create SyncObject", func(t *testing.T) {
		syncObject := &syncv1alpha1.SyncObject{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "sync.sj14.github.io/v1alpha1",
				Kind:       "SyncObject",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "sync-test",
			},
			Spec: syncv1alpha1.SyncObjectSpec{
				Reference: syncv1alpha1.Reference{
					Group:     "",
					Version:   "v1",
					Kind:      getOriginConfigMap(nil).Kind,
					Name:      getOriginConfigMap(nil).Name,
					Namespace: getOriginConfigMap(nil).Namespace,
				},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, syncObject))
	})

	t.Run("Check replica", func(t *testing.T) {
		// be sure that we didn't already get the replica by accident by checking the data
		require.Equal(t, map[string]string(map[string]string(nil)), targetConfigMap.Data)

		// get the replica
		require.Eventually(t, func() bool {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(targetConfigMap), targetConfigMap); err != nil {
				log.Println(err)
				return false
			}
			return true
		}, timeout, interval)

		// check that the data was synced succesfully
		require.Equal(t, configMapPayload, targetConfigMap.Data)
	})
}

// TestControllersDeletingSyncObjectRemovesReplicas covers the finalizer,
// which is the most destructive path in the operator: it deletes replicas
// across every namespace. It also pins down that the finalizer actually
// completes, since a finalizer that errors leaves the SyncObject stuck in
// Terminating forever.
func TestControllersDeletingSyncObjectRemovesReplicas(t *testing.T) {
	ctx := context.Background()

	originNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "delete-origin-namespace"},
	}
	firstTarget := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "delete-target-a"},
	}
	secondTarget := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "delete-target-b"},
	}
	originConfigMap := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "delete-configmap", Namespace: originNamespace.Name},
		Data:       map[string]string{"key": "value"},
	}

	require.NoError(t, k8sClient.Create(ctx, originNamespace))
	require.NoError(t, k8sClient.Create(ctx, firstTarget))
	require.NoError(t, k8sClient.Create(ctx, secondTarget))
	require.NoError(t, k8sClient.Create(ctx, originConfigMap))

	syncObject := &syncv1alpha1.SyncObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: "sync.sj14.github.io/v1alpha1", Kind: "SyncObject"},
		ObjectMeta: metav1.ObjectMeta{Name: "sync-delete"},
		Spec: syncv1alpha1.SyncObjectSpec{
			Reference: syncv1alpha1.Reference{
				Group:     "",
				Version:   "v1",
				Kind:      "ConfigMap",
				Name:      originConfigMap.Name,
				Namespace: originNamespace.Name,
			},
			TargetNamespaces: []string{firstTarget.Name, secondTarget.Name},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, syncObject))

	replicaKeys := []client.ObjectKey{
		{Namespace: firstTarget.Name, Name: originConfigMap.Name},
		{Namespace: secondTarget.Name, Name: originConfigMap.Name},
	}
	for _, key := range replicaKeys {
		require.Eventually(t, func() bool {
			return k8sClient.Get(ctx, key, &corev1.ConfigMap{}) == nil
		}, timeout, interval, "the replica should have been created first")
	}

	require.NoError(t, k8sClient.Delete(ctx, syncObject))

	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(syncObject), &syncv1alpha1.SyncObject{}))
	}, timeout, interval, "the SyncObject should be gone, i.e. the finalizer completed")

	for _, key := range replicaKeys {
		require.Eventually(t, func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &corev1.ConfigMap{}))
		}, timeout, interval, "the replicas should have been cleaned up with the SyncObject")
	}

	// the original is not a replica and stays put
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(originConfigMap), &corev1.ConfigMap{}),
		"deleting the SyncObject must not delete the resource it was replicating")
}

// TestControllersDisableFinalizerKeepsReplicas covers the documented
// opt out: with disableFinalizer the replicas are deliberately left behind
// when the SyncObject goes away.
func TestControllersDisableFinalizerKeepsReplicas(t *testing.T) {
	ctx := context.Background()

	originNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "nofinalizer-origin-namespace"},
	}
	targetNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "nofinalizer-target-namespace"},
	}
	originConfigMap := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "nofinalizer-configmap", Namespace: originNamespace.Name},
		Data:       map[string]string{"key": "value"},
	}

	require.NoError(t, k8sClient.Create(ctx, originNamespace))
	require.NoError(t, k8sClient.Create(ctx, targetNamespace))
	require.NoError(t, k8sClient.Create(ctx, originConfigMap))

	syncObject := &syncv1alpha1.SyncObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: "sync.sj14.github.io/v1alpha1", Kind: "SyncObject"},
		ObjectMeta: metav1.ObjectMeta{Name: "sync-nofinalizer"},
		Spec: syncv1alpha1.SyncObjectSpec{
			Reference: syncv1alpha1.Reference{
				Group:     "",
				Version:   "v1",
				Kind:      "ConfigMap",
				Name:      originConfigMap.Name,
				Namespace: originNamespace.Name,
			},
			TargetNamespaces: []string{targetNamespace.Name},
			DisableFinalizer: true,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, syncObject))

	replicaKey := client.ObjectKey{Namespace: targetNamespace.Name, Name: originConfigMap.Name}
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, replicaKey, &corev1.ConfigMap{}) == nil
	}, timeout, interval)

	require.NoError(t, k8sClient.Delete(ctx, syncObject))

	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(syncObject), &syncv1alpha1.SyncObject{}))
	}, timeout, interval, "the SyncObject should still be deletable")

	// nothing is going to remove the replica now, which is the point
	require.Never(t, func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, replicaKey, &corev1.ConfigMap{}))
	}, 2*time.Second, interval, "with disableFinalizer the replica should be left behind")
}

func TestControllersIgnoreNamespaces(t *testing.T) {
	ctx := context.Background()

	originNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "ignoretest-origin-namespace"},
	}
	targetNamespaceA := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "ignoretest-target-a"},
	}
	targetNamespaceB := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "ignoretest-target-b"},
	}
	ignoredNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "ignoretest-ignore-c"},
	}

	configMapPayload := map[string]string{"key": "value"}
	originConfigMap := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "ignoretest-origin-configmap", Namespace: originNamespace.Name},
		Data:       configMapPayload,
	}

	require.NoError(t, k8sClient.Create(ctx, originNamespace))
	require.NoError(t, k8sClient.Create(ctx, targetNamespaceA))
	require.NoError(t, k8sClient.Create(ctx, targetNamespaceB))
	require.NoError(t, k8sClient.Create(ctx, ignoredNamespace))
	require.NoError(t, k8sClient.Create(ctx, originConfigMap))

	syncObject := &syncv1alpha1.SyncObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: "sync.sj14.github.io/v1alpha1", Kind: "SyncObject"},
		ObjectMeta: metav1.ObjectMeta{Name: "sync-ignoretest"},
		Spec: syncv1alpha1.SyncObjectSpec{
			Reference: syncv1alpha1.Reference{
				Group:   "",
				Version: "v1",
				// originConfigMap.Kind is empty by this point: Create() clears
				// TypeMeta on the object it was called with, so we use the
				// literal Kind instead (see getOriginConfigMap's doc comment
				// for the same gotcha).
				Kind:      "ConfigMap",
				Name:      originConfigMap.Name,
				Namespace: originConfigMap.Namespace,
			},
			TargetNamespaces: []string{targetNamespaceA.Name, targetNamespaceB.Name, ignoredNamespace.Name},
			IgnoreNamespaces: []string{ignoredNamespace.Name},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, syncObject))

	t.Run("replica is created in target namespaces", func(t *testing.T) {
		for _, ns := range []string{targetNamespaceA.Name, targetNamespaceB.Name} {
			replica := &corev1.ConfigMap{}
			key := client.ObjectKey{Namespace: ns, Name: originConfigMap.Name}
			require.Eventually(t, func() bool {
				if err := k8sClient.Get(ctx, key, replica); err != nil {
					return false
				}
				return true
			}, timeout, interval)
			require.Equal(t, configMapPayload, replica.Data)
		}
	})

	t.Run("replica is not created in an ignored namespace", func(t *testing.T) {
		replica := &corev1.ConfigMap{}
		key := client.ObjectKey{Namespace: ignoredNamespace.Name, Name: originConfigMap.Name}
		err := k8sClient.Get(ctx, key, replica)
		require.True(t, apierrors.IsNotFound(err), "expected no replica in an ignored namespace, got: %v", err)
	})
}

// TestControllersStripsOriginalStateFromReplicas covers copying an object
// that is owned by something, or carries a finalizer. Both belong to the
// original: a copied owner reference points at nothing in the replica's
// namespace and gets the replica garbage collected, and a copied finalizer
// is never removed by anyone, so the replica could never be deleted.
func TestControllersStripsOriginalStateFromReplicas(t *testing.T) {
	ctx := context.Background()

	originNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "strip-origin-namespace"},
	}
	targetNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "strip-target-namespace"},
	}

	// something for the original to be owned by, in its own namespace
	owner := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "strip-owner", Namespace: originNamespace.Name},
	}

	require.NoError(t, k8sClient.Create(ctx, originNamespace))
	require.NoError(t, k8sClient.Create(ctx, targetNamespace))
	require.NoError(t, k8sClient.Create(ctx, owner))
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(owner), owner))

	originConfigMap := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:       "strip-configmap",
			Namespace:  originNamespace.Name,
			Finalizers: []string{"example.com/keep-around"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Name:       owner.Name,
				UID:        owner.UID,
			}},
			Annotations: map[string]string{
				corev1.LastAppliedConfigAnnotation: `{"apiVersion":"v1","kind":"ConfigMap"}`,
				"example.com/note":                 "keep me",
			},
		},
		Data: map[string]string{"key": "value"},
	}
	require.NoError(t, k8sClient.Create(ctx, originConfigMap))

	syncObject := &syncv1alpha1.SyncObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: "sync.sj14.github.io/v1alpha1", Kind: "SyncObject"},
		ObjectMeta: metav1.ObjectMeta{Name: "sync-strip"},
		Spec: syncv1alpha1.SyncObjectSpec{
			Reference: syncv1alpha1.Reference{
				Group:     "",
				Version:   "v1",
				Kind:      "ConfigMap",
				Name:      originConfigMap.Name,
				Namespace: originNamespace.Name,
			},
			TargetNamespaces: []string{targetNamespace.Name},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, syncObject))

	replicaKey := client.ObjectKey{Namespace: targetNamespace.Name, Name: originConfigMap.Name}
	replica := &corev1.ConfigMap{}
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, replicaKey, replica) == nil
	}, timeout, interval)

	require.Empty(t, replica.OwnerReferences,
		"a copied owner reference would get the replica garbage collected")
	require.Empty(t, replica.Finalizers,
		"a copied finalizer would make the replica impossible to delete")
	require.NotContains(t, replica.Annotations, corev1.LastAppliedConfigAnnotation)

	// the content and the original's own annotations still come across
	require.Equal(t, map[string]string{"key": "value"}, replica.Data)
	require.Equal(t, "keep me", replica.Annotations["example.com/note"])

	// the original keeps everything it had
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(originConfigMap), originConfigMap))
	require.NotEmpty(t, originConfigMap.OwnerReferences)
	require.NotEmpty(t, originConfigMap.Finalizers)
}

// TestControllersMarksReplicas checks that replicas are identifiable as
// such in the cluster, which is what a ValidatingAdmissionPolicy would need
// to select them, and that the original is left alone.
func TestControllersMarksReplicas(t *testing.T) {
	ctx := context.Background()

	originNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "marks-origin-namespace"},
	}
	targetNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "marks-target-namespace"},
	}
	originConfigMap := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "marks-configmap",
			Namespace:   originNamespace.Name,
			Labels:      map[string]string{"team": "platform"},
			Annotations: map[string]string{"example.com/note": "keep me"},
		},
		Data: map[string]string{"key": "value"},
	}

	require.NoError(t, k8sClient.Create(ctx, originNamespace))
	require.NoError(t, k8sClient.Create(ctx, targetNamespace))
	require.NoError(t, k8sClient.Create(ctx, originConfigMap))

	syncObject := &syncv1alpha1.SyncObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: "sync.sj14.github.io/v1alpha1", Kind: "SyncObject"},
		ObjectMeta: metav1.ObjectMeta{Name: "sync-marks"},
		Spec: syncv1alpha1.SyncObjectSpec{
			Reference: syncv1alpha1.Reference{
				Group:     "",
				Version:   "v1",
				Kind:      "ConfigMap",
				Name:      originConfigMap.Name,
				Namespace: originNamespace.Name,
			},
			TargetNamespaces: []string{targetNamespace.Name},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, syncObject))

	replicaKey := client.ObjectKey{Namespace: targetNamespace.Name, Name: originConfigMap.Name}
	replica := &corev1.ConfigMap{}
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, replicaKey, replica) == nil
	}, timeout, interval)

	require.Equal(t, managedByValue, replica.Labels[managedByLabel])
	require.Equal(t, "platform", replica.Labels["team"], "labels from the original should survive")
	require.Equal(t, syncObject.Name, replica.Annotations[syncObjectAnnotation])
	require.Equal(t, originNamespace.Name, replica.Annotations[sourceNamespaceAnnotation])
	require.Equal(t, originConfigMap.Name, replica.Annotations[sourceNameAnnotation])
	require.Equal(t, "keep me", replica.Annotations["example.com/note"], "annotations from the original should survive")

	// the original is not a replica and must not be marked as one
	original := &corev1.ConfigMap{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(originConfigMap), original))
	require.NotContains(t, original.Labels, managedByLabel)
	require.NotContains(t, original.Annotations, syncObjectAnnotation)

	// the label has to actually be selectable, that's the point of it.
	// Note other SyncObjects in this suite replicate into every namespace,
	// so the selector legitimately returns their replicas too -- only this
	// test's own objects can be asserted on.
	var selected corev1.ConfigMapList
	require.NoError(t, k8sClient.List(ctx, &selected, client.MatchingLabels{managedByLabel: managedByValue}))
	var found bool
	for _, item := range selected.Items {
		if item.Namespace == replicaKey.Namespace && item.Name == replicaKey.Name {
			found = true
		}
		require.False(t, item.Namespace == originNamespace.Name && item.Name == originConfigMap.Name,
			"the original must never match the replica selector")
	}
	require.True(t, found, "the replica should be findable by the managed-by label")
}

// TestControllersRefusesToReplicateAReplica covers pointing a SyncObject at
// something that is already a replica. Both SyncObjects would manage objects
// of the same kind and name, overwriting each other's copies on every pass,
// and since the chain keeps the original's name the second one would
// eventually overwrite the original too.
func TestControllersRefusesToReplicateAReplica(t *testing.T) {
	ctx := context.Background()

	originNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "chain-origin-namespace"},
	}
	firstTarget := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "chain-first-target"},
	}
	secondTarget := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "chain-second-target"},
	}
	originConfigMap := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "chain-configmap", Namespace: originNamespace.Name},
		Data:       map[string]string{"key": "value"},
	}

	require.NoError(t, k8sClient.Create(ctx, originNamespace))
	require.NoError(t, k8sClient.Create(ctx, firstTarget))
	require.NoError(t, k8sClient.Create(ctx, secondTarget))
	require.NoError(t, k8sClient.Create(ctx, originConfigMap))

	first := &syncv1alpha1.SyncObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: "sync.sj14.github.io/v1alpha1", Kind: "SyncObject"},
		ObjectMeta: metav1.ObjectMeta{Name: "sync-chain-first"},
		Spec: syncv1alpha1.SyncObjectSpec{
			Reference: syncv1alpha1.Reference{
				Group:     "",
				Version:   "v1",
				Kind:      "ConfigMap",
				Name:      originConfigMap.Name,
				Namespace: originNamespace.Name,
			},
			TargetNamespaces: []string{firstTarget.Name},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, first))

	replicaKey := client.ObjectKey{Namespace: firstTarget.Name, Name: originConfigMap.Name}
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, replicaKey, &corev1.ConfigMap{}) == nil
	}, timeout, interval, "the first SyncObject should have made a replica")

	// now point a second SyncObject at that replica
	second := &syncv1alpha1.SyncObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: "sync.sj14.github.io/v1alpha1", Kind: "SyncObject"},
		ObjectMeta: metav1.ObjectMeta{Name: "sync-chain-second"},
		Spec: syncv1alpha1.SyncObjectSpec{
			Reference: syncv1alpha1.Reference{
				Group:     "",
				Version:   "v1",
				Kind:      "ConfigMap",
				Name:      originConfigMap.Name,
				Namespace: firstTarget.Name,
			},
			TargetNamespaces: []string{secondTarget.Name},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, second))

	// it must never get anywhere
	chainedKey := client.ObjectKey{Namespace: secondTarget.Name, Name: originConfigMap.Name}
	require.Never(t, func() bool {
		return k8sClient.Get(ctx, chainedKey, &corev1.ConfigMap{}) == nil
	}, 3*time.Second, interval, "a replica of a replica should never be created")

	// and the first SyncObject's replica is left exactly as it was
	replica := &corev1.ConfigMap{}
	require.NoError(t, k8sClient.Get(ctx, replicaKey, replica))
	require.Equal(t, first.Name, replica.Annotations[syncObjectAnnotation],
		"the original owner of the replica should not have been taken over")
}

// TestControllersRepairsReplicaDrift covers a replica being tampered with
// directly. The SyncObject keeps its default 1h resync interval, so a
// repair inside the short Eventually window can only come from the watch
// on the reference's kind, which is cluster wide and therefore sees the
// replicas too.
func TestControllersRepairsReplicaDrift(t *testing.T) {
	ctx := context.Background()

	originNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "drift-origin-namespace"},
	}
	targetNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "drift-target-namespace"},
	}
	originConfigMap := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "drift-configmap", Namespace: originNamespace.Name},
		Data:       map[string]string{"key": "original"},
	}

	require.NoError(t, k8sClient.Create(ctx, originNamespace))
	require.NoError(t, k8sClient.Create(ctx, targetNamespace))
	require.NoError(t, k8sClient.Create(ctx, originConfigMap))

	syncObject := &syncv1alpha1.SyncObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: "sync.sj14.github.io/v1alpha1", Kind: "SyncObject"},
		ObjectMeta: metav1.ObjectMeta{Name: "sync-drift"},
		Spec: syncv1alpha1.SyncObjectSpec{
			Reference: syncv1alpha1.Reference{
				Group:     "",
				Version:   "v1",
				Kind:      "ConfigMap",
				Name:      originConfigMap.Name,
				Namespace: originNamespace.Name,
			},
			TargetNamespaces: []string{targetNamespace.Name},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, syncObject))

	replicaKey := client.ObjectKey{Namespace: targetNamespace.Name, Name: originConfigMap.Name}
	replica := &corev1.ConfigMap{}
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, replicaKey, replica) == nil
	}, timeout, interval)

	t.Run("an edited replica is restored", func(t *testing.T) {
		updateWithRetry(ctx, t, replica, func() {
			replica.Data = map[string]string{"key": "tampered"}
		})

		require.Eventually(t, func() bool {
			if err := k8sClient.Get(ctx, replicaKey, replica); err != nil {
				return false
			}
			return replica.Data["key"] == "original"
		}, timeout, interval, "the replica should have been restored from the original")
	})

	t.Run("a deleted replica is recreated", func(t *testing.T) {
		require.NoError(t, k8sClient.Get(ctx, replicaKey, replica))
		require.NoError(t, k8sClient.Delete(ctx, replica))

		require.Eventually(t, func() bool {
			return k8sClient.Get(ctx, replicaKey, &corev1.ConfigMap{}) == nil
		}, timeout, interval, "the replica should have been recreated")
	})

	// The operator reacting to its own writes must not turn into a hot
	// loop: once settled, nothing should keep rewriting the replica.
	t.Run("reconciling settles instead of looping", func(t *testing.T) {
		require.NoError(t, k8sClient.Get(ctx, replicaKey, replica))
		settled := replica.ResourceVersion

		require.Never(t, func() bool {
			current := &corev1.ConfigMap{}
			if err := k8sClient.Get(ctx, replicaKey, current); err != nil {
				return false
			}
			return current.ResourceVersion != settled
		}, 3*time.Second, 250*time.Millisecond, "the replica keeps being rewritten, the operator is reacting to its own updates")
	})
}

// TestControllersReplicatesIntoNewNamespace covers a namespace created
// after the SyncObject. With targetNamespaces empty every namespace is a
// target, and nothing about the referenced object changes here, so only the
// namespace watch can trigger this before the 1h resync.
func TestControllersReplicatesIntoNewNamespace(t *testing.T) {
	ctx := context.Background()

	originNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "nswatch-origin-namespace"},
	}
	originConfigMap := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "nswatch-configmap", Namespace: originNamespace.Name},
		Data:       map[string]string{"key": "value"},
	}

	require.NoError(t, k8sClient.Create(ctx, originNamespace))
	require.NoError(t, k8sClient.Create(ctx, originConfigMap))

	syncObject := &syncv1alpha1.SyncObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: "sync.sj14.github.io/v1alpha1", Kind: "SyncObject"},
		ObjectMeta: metav1.ObjectMeta{Name: "sync-nswatch"},
		Spec: syncv1alpha1.SyncObjectSpec{
			Reference: syncv1alpha1.Reference{
				Group:     "",
				Version:   "v1",
				Kind:      "ConfigMap",
				Name:      originConfigMap.Name,
				Namespace: originNamespace.Name,
			},
			// no TargetNamespaces: every namespace is a target
		},
	}
	require.NoError(t, k8sClient.Create(ctx, syncObject))

	// let the initial pass finish before adding the namespace
	require.Eventually(t, func() bool {
		key := client.ObjectKey{Namespace: "default", Name: originConfigMap.Name}
		return k8sClient.Get(ctx, key, &corev1.ConfigMap{}) == nil
	}, timeout, interval, "the initial replication should have happened")

	lateNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "nswatch-late-namespace"},
	}
	require.NoError(t, k8sClient.Create(ctx, lateNamespace))

	require.Eventually(t, func() bool {
		key := client.ObjectKey{Namespace: lateNamespace.Name, Name: originConfigMap.Name}
		return k8sClient.Get(ctx, key, &corev1.ConfigMap{}) == nil
	}, timeout, interval, "a namespace created later should get its replica without waiting for the resync")
}

// TestControllersKeepsOriginalWhenItsNamespaceIsIgnored guards against
// destroying the very object being synced: listing the reference's own
// namespace under ignoreNamespaces used to make it a deletion candidate,
// so the operator deleted the original it was supposed to replicate.
func TestControllersKeepsOriginalWhenItsNamespaceIsIgnored(t *testing.T) {
	ctx := context.Background()

	originNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "ignoreorigin-origin-namespace"},
	}
	targetNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "ignoreorigin-target-namespace"},
	}
	originConfigMap := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "ignoreorigin-configmap", Namespace: originNamespace.Name},
		Data:       map[string]string{"key": "value"},
	}

	require.NoError(t, k8sClient.Create(ctx, originNamespace))
	require.NoError(t, k8sClient.Create(ctx, targetNamespace))
	require.NoError(t, k8sClient.Create(ctx, originConfigMap))

	syncObject := &syncv1alpha1.SyncObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: "sync.sj14.github.io/v1alpha1", Kind: "SyncObject"},
		ObjectMeta: metav1.ObjectMeta{Name: "sync-ignoreorigin"},
		Spec: syncv1alpha1.SyncObjectSpec{
			Reference: syncv1alpha1.Reference{
				Group:     "",
				Version:   "v1",
				Kind:      "ConfigMap",
				Name:      originConfigMap.Name,
				Namespace: originNamespace.Name,
			},
			// the reference's own namespace, which must never be treated
			// as a namespace to clean replicas out of
			IgnoreNamespaces: []string{originNamespace.Name},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, syncObject))

	// wait until the operator has done a full pass
	replicaKey := client.ObjectKey{Namespace: targetNamespace.Name, Name: originConfigMap.Name}
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, replicaKey, &corev1.ConfigMap{}) == nil
	}, timeout, interval, "the reference should still be replicated into other namespaces")

	// the original must have survived that pass
	fetched := &corev1.ConfigMap{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(originConfigMap), fetched),
		"the original must not be deleted just because its namespace is ignored")
	require.Equal(t, map[string]string{"key": "value"}, fetched.Data)

	// and it must stay that way across further reconciles
	require.Never(t, func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(originConfigMap), &corev1.ConfigMap{}))
	}, 2*time.Second, interval, "the original must not be deleted by a later reconcile either")
}

// TestControllersReplacesReplicasWhenReferenceChanges covers issue #9:
// pointing spec.reference at a different object must not leave the previous
// reference's replicas behind, since they're named after the old reference
// and nothing would ever touch them again.
func TestControllersReplacesReplicasWhenReferenceChanges(t *testing.T) {
	ctx := context.Background()

	originNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "refchange-origin-namespace"},
	}
	targetNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "refchange-target-namespace"},
	}
	firstOrigin := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "refchange-first", Namespace: originNamespace.Name},
		Data:       map[string]string{"which": "first"},
	}
	secondOrigin := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "refchange-second", Namespace: originNamespace.Name},
		Data:       map[string]string{"which": "second"},
	}

	require.NoError(t, k8sClient.Create(ctx, originNamespace))
	require.NoError(t, k8sClient.Create(ctx, targetNamespace))
	require.NoError(t, k8sClient.Create(ctx, firstOrigin))
	require.NoError(t, k8sClient.Create(ctx, secondOrigin))

	syncObject := &syncv1alpha1.SyncObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: "sync.sj14.github.io/v1alpha1", Kind: "SyncObject"},
		ObjectMeta: metav1.ObjectMeta{Name: "sync-refchange"},
		Spec: syncv1alpha1.SyncObjectSpec{
			Reference: syncv1alpha1.Reference{
				Group:     "",
				Version:   "v1",
				Kind:      "ConfigMap",
				Name:      firstOrigin.Name,
				Namespace: originNamespace.Name,
			},
			TargetNamespaces: []string{targetNamespace.Name},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, syncObject))

	firstReplica := client.ObjectKey{Namespace: targetNamespace.Name, Name: firstOrigin.Name}
	secondReplica := client.ObjectKey{Namespace: targetNamespace.Name, Name: secondOrigin.Name}

	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, firstReplica, &corev1.ConfigMap{}) == nil
	}, timeout, interval, "the first reference should have been replicated")

	// repoint the SyncObject at the other ConfigMap
	updateWithRetry(ctx, t, syncObject, func() {
		syncObject.Spec.Reference.Name = secondOrigin.Name
	})

	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, secondReplica, &corev1.ConfigMap{}) == nil
	}, timeout, interval, "the new reference should have been replicated")

	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, firstReplica, &corev1.ConfigMap{}))
	}, timeout, interval, "the previous reference's replica should have been cleaned up, not orphaned")

	// neither original may be touched by the cleanup
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(firstOrigin), &corev1.ConfigMap{}))
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(secondOrigin), &corev1.ConfigMap{}))
}

// TestControllersSyncsOnReferenceChange proves the dynamic reference watch
// works: the SyncObject below is deliberately left at its default 1h
// resync interval, so if the replica picks up a change to the *origin*
// object within this test's short Eventually timeout, it can only be
// because ensureReferenceWatch's watch fired -- not the periodic resync.
func TestControllersSyncsOnReferenceChange(t *testing.T) {
	ctx := context.Background()

	originNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "watchtest-origin-namespace"},
	}
	targetNamespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "watchtest-target-namespace"},
	}
	originConfigMap := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "watchtest-origin-configmap", Namespace: originNamespace.Name},
		Data:       map[string]string{"key": "before"},
	}

	require.NoError(t, k8sClient.Create(ctx, originNamespace))
	require.NoError(t, k8sClient.Create(ctx, targetNamespace))
	require.NoError(t, k8sClient.Create(ctx, originConfigMap))

	syncObject := &syncv1alpha1.SyncObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: "sync.sj14.github.io/v1alpha1", Kind: "SyncObject"},
		ObjectMeta: metav1.ObjectMeta{Name: "sync-watchtest"},
		Spec: syncv1alpha1.SyncObjectSpec{
			Reference: syncv1alpha1.Reference{
				Group:     "",
				Version:   "v1",
				Kind:      "ConfigMap",
				Name:      originConfigMap.Name,
				Namespace: originConfigMap.Namespace,
			},
			TargetNamespaces: []string{targetNamespace.Name},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, syncObject))

	replicaKey := client.ObjectKey{Namespace: targetNamespace.Name, Name: originConfigMap.Name}
	replica := &corev1.ConfigMap{}
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, replicaKey, replica) == nil
	}, timeout, interval)
	require.Equal(t, map[string]string{"key": "before"}, replica.Data)

	// update the ORIGIN object directly -- the SyncObject itself is never touched.
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(originConfigMap), originConfigMap))
	originConfigMap.Data = map[string]string{"key": "after"}
	require.NoError(t, k8sClient.Update(ctx, originConfigMap))

	require.Eventually(t, func() bool {
		if err := k8sClient.Get(ctx, replicaKey, replica); err != nil {
			return false
		}
		return replica.Data["key"] == "after"
	}, timeout, interval, "replica should pick up the origin change via the reference watch, well before the 1h default resync interval")
}

// TestCRDDefaultsResyncInterval creates a SyncObject as raw, unstructured
// JSON that omits the "resyncInterval" field entirely, the way a
// hand-written YAML manifest would. This is deliberately not done via the
// typed client: metav1.Duration is a struct, and Go's encoding/json never
// treats "omitempty" struct fields as empty, so the typed client always
// sends an explicit "resyncInterval":"0s" and the CRD default would never
// get a chance to apply.
func TestCRDDefaultsResyncInterval(t *testing.T) {
	ctx := context.Background()

	u := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "sync.sj14.github.io/v1alpha1",
			"kind":       "SyncObject",
			"metadata": map[string]interface{}{
				"name": "sync-defaultinterval-test",
			},
			"spec": map[string]interface{}{
				"reference": map[string]interface{}{
					"group":     "",
					"version":   "v1",
					"kind":      "ConfigMap",
					"name":      "does-not-need-to-exist",
					"namespace": "default",
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, u))

	var syncObject syncv1alpha1.SyncObject
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(u), &syncObject))

	// the generated CRD (deploy/crds/sync.sj14.github.io_syncobjects.yaml) and
	// the controller's own fallback (resyncInterval) must agree on the default.
	require.Equal(t, defaultResyncInterval, syncObject.Spec.ResyncInterval.Duration)
}

// helper as gvk would be missing after creation
func getOriginNamespace() *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "origin-namespace",
		},
	}
}

// helper as gvk would be missing after creation
func getOriginConfigMap(payload map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "origin-configmap",
			Namespace: getOriginNamespace().Name,
		},
		Data: payload,
	}
}
