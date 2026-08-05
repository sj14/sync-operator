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

func TestControllersCreateDelete(t *testing.T) {
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

// TestCRDDefaultsInterval creates a SyncObject as raw, unstructured JSON that
// omits the "interval" field entirely, the way a hand-written YAML manifest
// would. This is deliberately not done via the typed client: metav1.Duration
// is a struct, and Go's encoding/json never treats "omitempty" struct fields
// as empty, so the typed client always sends an explicit "interval":"0s" and
// the CRD default would never get a chance to apply.
func TestCRDDefaultsInterval(t *testing.T) {
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
	// the controller's own fallback (syncInterval) must agree on the default.
	require.Equal(t, defaultInterval, syncObject.Spec.Interval.Duration)
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
