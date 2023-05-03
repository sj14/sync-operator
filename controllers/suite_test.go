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
