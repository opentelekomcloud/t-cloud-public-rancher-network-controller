//go:build integration

package controller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	infrav1 "github.com/opentelekomcloud/t-cloud-public-rancher-network-controller/api/v1alpha1"
)

func TestEnvtestManagedLifecycle(t *testing.T) {
	testEnvironment := &envtest.Environment{CRDDirectoryPaths: []string{
		filepath.Join("..", "..", "config", "crd", "bases"),
		filepath.Join("testdata", "crds"),
	}}
	config, err := testEnvironment.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnvironment.Stop() })

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := infrav1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(provisioningClusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(provisioningClusterGVK.GroupVersion().WithKind("ClusterList"), &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(tcloudMachineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(tcloudMachineGVK.GroupVersion().WithKind("OpentelekomcloudMachineList"), &unstructured.UnstructuredList{})

	manager, err := ctrl.NewManager(config, ctrl.Options{Scheme: scheme, LeaderElection: false})
	if err != nil {
		t.Fatal(err)
	}
	cloud := newFakeCloud()
	reconciler := &TCloudClusterNetworkReconciler{
		Client: manager.GetClient(), APIReader: manager.GetAPIReader(), Scheme: scheme,
		Factory: fakeFactory{service: cloud}, Recorder: manager.GetEventRecorderFor("envtest"),
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if err := manager.Start(ctx); err != nil {
			panic(err)
		}
	}()

	apiClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, namespace := range []string{"fleet-default", "cattle-global-data"} {
		if err := apiClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			t.Fatal(err)
		}
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cc-test", Namespace: "cattle-global-data"}, Data: map[string][]byte{
		"opentelekomcloudcredentialConfig-authUrl":    []byte("https://iam.example/v3"),
		"opentelekomcloudcredentialConfig-username":   []byte("user"),
		"opentelekomcloudcredentialConfig-password":   []byte("password"),
		"opentelekomcloudcredentialConfig-domainName": []byte("domain"),
	}}
	if err := apiClient.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(provisioningClusterGVK)
	cluster.SetNamespace("fleet-default")
	cluster.SetName("test-de")
	if err := apiClient.Create(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	network := &infrav1.TCloudClusterNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "test-de-network", Namespace: "fleet-default"},
		Spec: infrav1.TCloudClusterNetworkSpec{
			ClusterRef:          infrav1.NamespacedReference{Name: "test-de", Namespace: "fleet-default"},
			CredentialSecretRef: infrav1.NamespacedReference{Name: "cc-test", Namespace: "cattle-global-data"},
			ManagementPolicy:    infrav1.ManagementPolicyManaged, Region: "eu-de", ProjectName: "project",
			Network: infrav1.NetworkSpec{
				VPC:           infrav1.VPCSpec{Name: "test-de", CIDR: "192.168.0.0/16"},
				Subnet:        infrav1.SubnetSpec{Name: "test-de", CIDR: "192.168.0.0/24", GatewayIP: "192.168.0.1"},
				SecurityGroup: infrav1.SecurityGroupSpec{Name: "test-de-rke2", CNI: "canal", SSHAllowedCIDRs: []string{"203.0.113.5/32"}},
			},
		},
	}
	if err := apiClient.Create(ctx, network); err != nil {
		t.Fatal(err)
	}

	key := types.NamespacedName{Name: network.Name, Namespace: network.Namespace}
	eventually(t, 15*time.Second, func() bool {
		current := &infrav1.TCloudClusterNetwork{}
		return apiClient.Get(ctx, key, current) == nil && conditionTrue(current, infrav1.ConditionReady) && len(current.OwnerReferences) == 1
	})
	current := &infrav1.TCloudClusterNetwork{}
	if err := apiClient.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if err := apiClient.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, func() bool {
		err := apiClient.Get(ctx, key, &infrav1.TCloudClusterNetwork{})
		return apierrors.IsNotFound(err)
	})
	if len(cloud.deleted) != 3 {
		t.Fatalf("expected all managed resources deleted, got %#v", cloud.deleted)
	}
}

func eventually(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}
