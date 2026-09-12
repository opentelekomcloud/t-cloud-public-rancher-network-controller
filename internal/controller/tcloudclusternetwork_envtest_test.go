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
	cloudservice "github.com/opentelekomcloud/t-cloud-public-rancher-network-controller/internal/cloud"
)

func TestEnvtestManagedAndAdoptLifecycle(t *testing.T) {
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
	bootstrapReconciler := &TCloudClusterNetworkBootstrapReconciler{
		Client: manager.GetClient(), APIReader: manager.GetAPIReader(), Scheme: scheme,
	}
	if err := bootstrapReconciler.SetupWithManager(manager); err != nil {
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

	cloud.deleted = nil
	cloud.resources["adopted-vpc"] = cloudservice.Resource{ID: "adopted-vpc", Name: "vpc-docker-machine"}
	cloud.resources["adopted-subnet"] = cloudservice.Resource{ID: "adopted-subnet", Name: "subnet-docker-machine"}
	cloud.resources["adopted-sg"] = cloudservice.Resource{ID: "adopted-sg", Name: "docker-machine"}
	machine := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": tcloudMachineGVK.GroupVersion().String(),
		"kind":       tcloudMachineGVK.Kind,
		"metadata": map[string]interface{}{
			"name":      "test-de-worker",
			"namespace": "fleet-default",
			"labels":    map[string]interface{}{clusterNameLabel: "test-de"},
		},
		"status": map[string]interface{}{"ready": true},
	}}
	if err := apiClient.Create(ctx, machine); err != nil {
		t.Fatal(err)
	}
	if err := apiClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-de-worker-machine-state", Namespace: "fleet-default"},
		Data:       map[string][]byte{"extractedConfig": machineStateArchive(t, "adopted-vpc", "adopted-subnet", "adopted-sg", true)},
	}); err != nil {
		t.Fatal(err)
	}
	adopted := &infrav1.TCloudClusterNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "test-de-adopted-network", Namespace: "fleet-default"},
		Spec: infrav1.TCloudClusterNetworkSpec{
			ClusterRef:          infrav1.NamespacedReference{Name: "test-de", Namespace: "fleet-default"},
			CredentialSecretRef: infrav1.NamespacedReference{Name: "cc-test", Namespace: "cattle-global-data"},
			ManagementPolicy:    infrav1.ManagementPolicyAdopt,
			Region:              "eu-de",
			ProjectName:         "project",
			Network: infrav1.NetworkSpec{
				VPC:    infrav1.VPCSpec{},
				Subnet: infrav1.SubnetSpec{},
				SecurityGroup: infrav1.SecurityGroupSpec{
					CNI: "calico", SSHAllowedCIDRs: []string{"203.0.113.5/32"},
				},
			},
		},
	}
	if err := apiClient.Create(ctx, adopted); err != nil {
		t.Fatal(err)
	}
	adoptedKey := types.NamespacedName{Name: adopted.Name, Namespace: adopted.Namespace}
	eventually(t, 15*time.Second, func() bool {
		value := &infrav1.TCloudClusterNetwork{}
		return apiClient.Get(ctx, adoptedKey, value) == nil && conditionTrue(value, infrav1.ConditionReady) && value.Status.Resources.VPC.ID == "adopted-vpc"
	})
	if err := apiClient.Delete(ctx, machine); err != nil {
		t.Fatal(err)
	}
	if err := apiClient.Get(ctx, adoptedKey, adopted); err != nil {
		t.Fatal(err)
	}
	if err := apiClient.Delete(ctx, adopted); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, func() bool {
		err := apiClient.Get(ctx, adoptedKey, &infrav1.TCloudClusterNetwork{})
		return apierrors.IsNotFound(err)
	})
	if len(cloud.deleted) != 3 {
		t.Fatalf("expected all adopted resources deleted, got %#v", cloud.deleted)
	}

	cloud.resources["bootstrap-vpc"] = cloudservice.Resource{ID: "bootstrap-vpc", Name: "bootstrap-vpc"}
	cloud.resources["bootstrap-subnet"] = cloudservice.Resource{ID: "bootstrap-subnet", Name: "bootstrap-subnet"}
	cloud.resources["bootstrap-sg"] = cloudservice.Resource{ID: "bootstrap-sg", Name: "bootstrap-sg"}
	bootstrapCluster := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": provisioningClusterGVK.GroupVersion().String(), "kind": provisioningClusterGVK.Kind,
		"metadata": map[string]interface{}{"name": "manifest-scale", "namespace": "fleet-default"},
		"spec": map[string]interface{}{
			"cloudCredentialSecretName": "cattle-global-data:cc-test",
			"rkeConfig": map[string]interface{}{
				"machineGlobalConfig": map[string]interface{}{"cni": "calico"},
				"machinePools": []interface{}{map[string]interface{}{
					"name": "pool", "quantity": int64(1),
					"machineConfigRef": map[string]interface{}{"kind": tcloudMachineConfigGVK.Kind, "name": "manifest-scale-config"},
				}},
			},
		},
	}}
	if err := apiClient.Create(ctx, bootstrapCluster); err != nil {
		t.Fatal(err)
	}
	bootstrapConfig := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": tcloudMachineConfigGVK.GroupVersion().String(), "kind": tcloudMachineConfigGVK.Kind,
		"metadata": map[string]interface{}{
			"name": "manifest-scale-config", "namespace": "fleet-default",
			"ownerReferences": []interface{}{map[string]interface{}{
				"apiVersion": provisioningClusterGVK.GroupVersion().String(), "kind": provisioningClusterGVK.Kind,
				"name": bootstrapCluster.GetName(), "uid": string(bootstrapCluster.GetUID()),
			}},
		},
		"networkScope": "machine", "vpcId": "", "subnetId": "", "secGroups": "",
		"region": "eu-de", "projectName": "project", "endpointType": "publicURL",
	}}
	if err := apiClient.Create(ctx, bootstrapConfig); err != nil {
		t.Fatal(err)
	}
	bootstrapMachine := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": tcloudMachineGVK.GroupVersion().String(), "kind": tcloudMachineGVK.Kind,
		"metadata": map[string]interface{}{
			"name": "manifest-scale-worker", "namespace": "fleet-default",
			"labels": map[string]interface{}{clusterNameLabel: "manifest-scale"},
		},
	}}
	if err := apiClient.Create(ctx, bootstrapMachine); err != nil {
		t.Fatal(err)
	}
	if err := apiClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "manifest-scale-worker-machine-state", Namespace: "fleet-default"},
		Data:       map[string][]byte{"extractedConfig": machineStateArchive(t, "bootstrap-vpc", "bootstrap-subnet", "bootstrap-sg", true)},
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, func() bool {
		currentConfig := &unstructured.Unstructured{}
		currentConfig.SetGroupVersionKind(tcloudMachineConfigGVK)
		if apiClient.Get(ctx, types.NamespacedName{Namespace: "fleet-default", Name: "manifest-scale-config"}, currentConfig) != nil {
			return false
		}
		currentCluster := &unstructured.Unstructured{}
		currentCluster.SetGroupVersionKind(provisioningClusterGVK)
		if apiClient.Get(ctx, types.NamespacedName{Namespace: "fleet-default", Name: "manifest-scale"}, currentCluster) != nil {
			return false
		}
		return currentConfig.Object["networkScope"] == "shared" && currentConfig.Object["vpcId"] == "bootstrap-vpc" && currentCluster.GetAnnotations()[infrav1.UIProviderAnnotation] == infrav1.TCloudProviderID
	})
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
