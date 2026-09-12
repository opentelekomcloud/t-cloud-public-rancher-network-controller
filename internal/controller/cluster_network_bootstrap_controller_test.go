package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/opentelekomcloud/t-cloud-public-rancher-network-controller/api/v1alpha1"
)

func TestBootstrapAdoptsAndPatchesConfigsForUIAndManifestScaling(t *testing.T) {
	ctx := context.Background()
	scheme := bootstrapTestScheme(t)
	cluster := bootstrapCluster("test-scale", "pool-one")
	config := bootstrapMachineConfig("test-scale", "pool-one")
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&infrav1.TCloudClusterNetwork{}).
		WithObjects(cluster, config).Build()
	reconciler := &TCloudClusterNetworkBootstrapReconciler{Client: kubeClient, APIReader: kubeClient, Scheme: scheme}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	network := &infrav1.TCloudClusterNetwork{}
	networkKey := types.NamespacedName{Namespace: "fleet-default", Name: "test-scale-network"}
	if err := kubeClient.Get(ctx, networkKey, network); err != nil {
		t.Fatal(err)
	}
	if network.Spec.ManagementPolicy != infrav1.ManagementPolicyAdopt || network.Spec.Network.SecurityGroup.CNI != "calico" {
		t.Fatalf("unexpected adoption request: %#v", network.Spec)
	}
	currentCluster := &unstructured.Unstructured{}
	currentCluster.SetGroupVersionKind(provisioningClusterGVK)
	if err := kubeClient.Get(ctx, request.NamespacedName, currentCluster); err != nil {
		t.Fatal(err)
	}
	annotations := currentCluster.GetAnnotations()
	if annotations[infrav1.ClusterAnnotation] != network.Name || annotations[infrav1.NetworkPolicyAnnotation] != string(infrav1.ManagementPolicyAdopt) || annotations[infrav1.UIProviderAnnotation] != infrav1.TCloudProviderID {
		t.Fatalf("cluster annotations were not preserved: %#v", annotations)
	}

	network.Status.ObservedGeneration = network.Generation
	network.Status.Resources = infrav1.NetworkResourceStatus{
		VPC:           infrav1.ResourceStatus{ID: "vpc-id", Name: "vpc-name", ControllerManaged: true},
		Subnet:        infrav1.ResourceStatus{ID: "subnet-id", Name: "subnet-name", ControllerManaged: true},
		SecurityGroup: infrav1.ResourceStatus{ID: "sg-id", Name: "sg-name", ControllerManaged: true},
	}
	network.Status.Conditions = []metav1.Condition{{Type: infrav1.ConditionReady, Status: metav1.ConditionTrue}}
	if err := kubeClient.Status().Update(ctx, network); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	assertSharedMachineConfig(t, ctx, kubeClient, "pool-one")

	// Rancher's quick +1 and a direct manifest quantity patch both leave the
	// already-shared machine config unchanged and require no extension hook.
	if err := kubeClient.Get(ctx, request.NamespacedName, currentCluster); err != nil {
		t.Fatal(err)
	}
	pools, _, _ := unstructured.NestedSlice(currentCluster.Object, "spec", "rkeConfig", "machinePools")
	pools[0].(map[string]interface{})["quantity"] = int64(2)
	if err := unstructured.SetNestedSlice(currentCluster.Object, pools, "spec", "rkeConfig", "machinePools"); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Update(ctx, currentCluster); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	assertSharedMachineConfig(t, ctx, kubeClient, "pool-one")

	// A pool added through a manifest is converged to the same shared network.
	second := bootstrapMachineConfig("test-scale", "pool-two")
	if err := kubeClient.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	assertSharedMachineConfig(t, ctx, kubeClient, "pool-two")

	// Rancher can now reference and activate the already-converged pool config.
	if err := kubeClient.Get(ctx, request.NamespacedName, currentCluster); err != nil {
		t.Fatal(err)
	}
	pools, _, _ = unstructured.NestedSlice(currentCluster.Object, "spec", "rkeConfig", "machinePools")
	pools = append(pools, machinePool("pool-two", 0))
	if err := unstructured.SetNestedSlice(currentCluster.Object, pools, "spec", "rkeConfig", "machinePools"); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Update(ctx, currentCluster); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	assertSharedMachineConfig(t, ctx, kubeClient, "pool-two")
}

func TestBootstrapDoesNotClaimExplicitExistingNetwork(t *testing.T) {
	ctx := context.Background()
	scheme := bootstrapTestScheme(t)
	cluster := bootstrapCluster("existing", "existing-pool")
	config := bootstrapMachineConfig("existing", "existing-pool")
	config.Object["vpcId"] = "existing-vpc"
	config.Object["subnetId"] = "existing-subnet"
	config.Object["secGroups"] = "existing-sg"
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, config).Build()
	reconciler := &TCloudClusterNetworkBootstrapReconciler{Client: kubeClient, APIReader: kubeClient, Scheme: scheme}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}); err != nil {
		t.Fatal(err)
	}
	err := kubeClient.Get(ctx, types.NamespacedName{Namespace: "fleet-default", Name: "existing-network"}, &infrav1.TCloudClusterNetwork{})
	if err == nil {
		t.Fatal("explicit existing network was incorrectly adopted")
	}
}

func TestBootstrapDoesNotClaimUncoordinatedMultiNodeCluster(t *testing.T) {
	ctx := context.Background()
	scheme := bootstrapTestScheme(t)
	cluster := bootstrapCluster("multi", "multi-pool")
	pools, _, _ := unstructured.NestedSlice(cluster.Object, "spec", "rkeConfig", "machinePools")
	pools[0].(map[string]interface{})["quantity"] = int64(3)
	_ = unstructured.SetNestedSlice(cluster.Object, pools, "spec", "rkeConfig", "machinePools")
	config := bootstrapMachineConfig("multi", "multi-pool")
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, config).Build()
	reconciler := &TCloudClusterNetworkBootstrapReconciler{Client: kubeClient, APIReader: kubeClient, Scheme: scheme}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: "fleet-default", Name: "multi-network"}, &infrav1.TCloudClusterNetwork{}); err == nil {
		t.Fatal("uncoordinated multi-node cluster was incorrectly adopted")
	}
}

func TestNetworkObjectNameMatchesExtensionNormalization(t *testing.T) {
	if actual := networkObjectName("My__Cluster"); actual != "my-cluster-network" {
		t.Fatalf("unexpected network object name %q", actual)
	}
	if actual := networkObjectName(strings.Repeat("a", 70)); len(actual) != 63 {
		t.Fatalf("network object name has length %d, want 63", len(actual))
	}
}

func bootstrapTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := infrav1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for gvk, listKind := range map[schema.GroupVersionKind]string{
		provisioningClusterGVK: "ClusterList", tcloudMachineGVK: "OpentelekomcloudMachineList", tcloudMachineConfigGVK: "OpentelekomcloudConfigList",
	} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(listKind), &unstructured.UnstructuredList{})
	}
	return scheme
}

func bootstrapCluster(name, configName string) *unstructured.Unstructured {
	cluster := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": provisioningClusterGVK.GroupVersion().String(), "kind": provisioningClusterGVK.Kind,
		"metadata": map[string]interface{}{"name": name, "namespace": "fleet-default", "uid": "cluster-uid"},
		"spec": map[string]interface{}{
			"cloudCredentialSecretName": "cattle-global-data:cc-test",
			"rkeConfig": map[string]interface{}{
				"machineGlobalConfig": map[string]interface{}{"cni": "calico"},
				"machinePools":        []interface{}{machinePool(configName, 1)},
			},
		},
	}}
	return cluster
}

func machinePool(configName string, quantity int64) map[string]interface{} {
	return map[string]interface{}{
		"name": configName, "quantity": quantity,
		"machineConfigRef": map[string]interface{}{"kind": tcloudMachineConfigGVK.Kind, "name": configName},
	}
}

func bootstrapMachineConfig(clusterName, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": tcloudMachineConfigGVK.GroupVersion().String(), "kind": tcloudMachineConfigGVK.Kind,
		"metadata": map[string]interface{}{
			"name": name, "namespace": "fleet-default",
			"ownerReferences": []interface{}{map[string]interface{}{
				"apiVersion": provisioningClusterGVK.GroupVersion().String(), "kind": provisioningClusterGVK.Kind, "name": clusterName, "uid": "cluster-uid",
			}},
		},
		"networkScope": "machine", "vpcId": "", "subnetId": "", "secGroups": "",
		"region": "eu-ch2", "projectName": "project", "endpointType": "publicURL", "sshAllowCidr": "203.0.113.10/32",
	}}
}

func assertSharedMachineConfig(t *testing.T, ctx context.Context, kubeClient client.Client, name string) {
	t.Helper()
	config := &unstructured.Unstructured{}
	config.SetGroupVersionKind(tcloudMachineConfigGVK)
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: "fleet-default", Name: name}, config); err != nil {
		t.Fatal(err)
	}
	if config.Object["networkScope"] != "shared" || config.Object["vpcId"] != "vpc-id" || config.Object["subnetId"] != "subnet-id" || config.Object["secGroups"] != "sg-name" || config.Object["skipDefaultSg"] != true {
		t.Fatalf("machine config was not patched to shared networking: %#v", config.Object)
	}
}
