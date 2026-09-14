package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/opentelekomcloud/t-cloud-public-rancher-network-controller/api/v1alpha1"
	cloudservice "github.com/opentelekomcloud/t-cloud-public-rancher-network-controller/internal/cloud"
)

type fakeFactory struct{ service *fakeCloud }

func (f fakeFactory) New(context.Context, cloudservice.Credentials) (cloudservice.Service, error) {
	return f.service, nil
}

type fakeCloud struct {
	resources             map[string]cloudservice.Resource
	deleted               []string
	securityGroupRequests []cloudservice.SecurityGroupRequest
	ports                 int
	failSubnet            int
}

func TestManagedSSHAllowedCIDRsAreReconciled(t *testing.T) {
	ctx := context.Background()
	reconciler, kubeClient, cloud := testReconciler(t, infrav1.ManagementPolicyManaged)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-de-network", Namespace: "fleet-default"}}
	reconcileUntilReady(t, ctx, reconciler, kubeClient, request)

	network := &infrav1.TCloudClusterNetwork{}
	if err := kubeClient.Get(ctx, request.NamespacedName, network); err != nil {
		t.Fatal(err)
	}
	if len(network.Status.AppliedSSHAllowedCIDRs) != 1 || network.Status.AppliedSSHAllowedCIDRs[0] != "203.0.113.10/32" {
		t.Fatalf("initial applied CIDRs were not recorded: %#v", network.Status.AppliedSSHAllowedCIDRs)
	}
	network.Spec.Network.SecurityGroup.SSHAllowedCIDRs = []string{"198.51.100.0/24"}
	if err := kubeClient.Update(ctx, network); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}

	last := cloud.securityGroupRequests[len(cloud.securityGroupRequests)-1]
	wantedRemoval := cloudservice.Rule{Protocol: "tcp", FromPort: 22, ToPort: 22, CIDR: "203.0.113.10/32"}
	if len(last.RemoveRules) != 1 || last.RemoveRules[0] != wantedRemoval {
		t.Fatalf("unexpected obsolete SSH rules: %#v", last.RemoveRules)
	}
	if !containsCloudRule(last.Rules, cloudservice.Rule{Protocol: "tcp", FromPort: 22, ToPort: 22, CIDR: "198.51.100.0/24"}) {
		t.Fatalf("new SSH rule was not requested: %#v", last.Rules)
	}
	if err := kubeClient.Get(ctx, request.NamespacedName, network); err != nil {
		t.Fatal(err)
	}
	if len(network.Status.AppliedSSHAllowedCIDRs) != 1 || network.Status.AppliedSSHAllowedCIDRs[0] != "198.51.100.0/24" {
		t.Fatalf("updated applied CIDRs were not recorded: %#v", network.Status.AppliedSSHAllowedCIDRs)
	}
}

func containsCloudRule(rules []cloudservice.Rule, wanted cloudservice.Rule) bool {
	for _, rule := range rules {
		if rule == wanted {
			return true
		}
	}
	return false
}

func newFakeCloud() *fakeCloud { return &fakeCloud{resources: map[string]cloudservice.Resource{}} }

func (f *fakeCloud) EnsureVPC(_ context.Context, request cloudservice.VPCRequest) (cloudservice.Resource, error) {
	return f.ensure("vpc", request.ID, request.Name), nil
}
func (f *fakeCloud) EnsureSubnet(_ context.Context, request cloudservice.SubnetRequest) (cloudservice.Resource, error) {
	if f.failSubnet > 0 {
		f.failSubnet--
		return cloudservice.Resource{}, fmt.Errorf("temporary subnet failure")
	}
	return f.ensure("subnet", request.ID, request.Name), nil
}
func (f *fakeCloud) EnsureSecurityGroup(_ context.Context, request cloudservice.SecurityGroupRequest) (cloudservice.Resource, error) {
	f.securityGroupRequests = append(f.securityGroupRequests, request)
	return f.ensure("sg", request.ID, request.Name), nil
}
func (f *fakeCloud) ensure(kind, id, name string) cloudservice.Resource {
	if id != "" {
		return f.resources[id]
	}
	id = kind + "-id"
	resource := cloudservice.Resource{ID: id, Name: name}
	f.resources[id] = resource
	return resource
}
func (f *fakeCloud) GetVPC(_ context.Context, id string) (cloudservice.Resource, error) {
	return f.get(id)
}
func (f *fakeCloud) GetSubnet(_ context.Context, id string) (cloudservice.Resource, error) {
	return f.get(id)
}
func (f *fakeCloud) GetSecurityGroup(_ context.Context, id string) (cloudservice.Resource, error) {
	return f.get(id)
}
func (f *fakeCloud) get(id string) (cloudservice.Resource, error) {
	resource, found := f.resources[id]
	if !found {
		return cloudservice.Resource{}, fmt.Errorf("resource %s not found", id)
	}
	return resource, nil
}
func (f *fakeCloud) AttachedPorts(context.Context, string) (int, error)     { return f.ports, nil }
func (f *fakeCloud) DeleteSecurityGroup(_ context.Context, id string) error { return f.delete(id) }
func (f *fakeCloud) DeleteSubnet(_ context.Context, _, id string) error     { return f.delete(id) }
func (f *fakeCloud) DeleteVPC(_ context.Context, id string) error           { return f.delete(id) }
func (f *fakeCloud) delete(id string) error {
	f.deleted = append(f.deleted, id)
	delete(f.resources, id)
	return nil
}

func TestManagedNetworkLifecycle(t *testing.T) {
	ctx := context.Background()
	reconciler, kubeClient, cloud := testReconciler(t, infrav1.ManagementPolicyManaged)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-de-network", Namespace: "fleet-default"}}

	for i := 0; i < 5; i++ {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("reconcile create: %v", err)
		}
	}
	created := &infrav1.TCloudClusterNetwork{}
	if err := kubeClient.Get(ctx, request.NamespacedName, created); err != nil {
		t.Fatal(err)
	}
	if !conditionTrue(created, infrav1.ConditionReady) {
		t.Fatalf("network not ready: %#v", created.Status.Conditions)
	}
	if created.Status.Resources.VPC.ID == "" || created.Status.Resources.Subnet.ID == "" || created.Status.Resources.SecurityGroup.ID == "" {
		t.Fatalf("resource IDs were not persisted: %#v", created.Status.Resources)
	}
	if len(created.OwnerReferences) != 1 {
		t.Fatalf("cluster owner was not bound: %#v", created.OwnerReferences)
	}

	if err := kubeClient.Delete(ctx, created); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_, err := reconciler.Reconcile(ctx, request)
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("reconcile delete: %v", err)
		}
	}
	if len(cloud.deleted) != 3 || cloud.deleted[0] != "sg-id" || cloud.deleted[1] != "subnet-id" || cloud.deleted[2] != "vpc-id" {
		t.Fatalf("unexpected deletion order: %#v", cloud.deleted)
	}
	if err := kubeClient.Get(ctx, request.NamespacedName, &infrav1.TCloudClusterNetwork{}); !apierrors.IsNotFound(err) {
		t.Fatalf("network object still exists, error: %v", err)
	}
}

func TestManagedNetworkRecoversAfterPartialCreation(t *testing.T) {
	ctx := context.Background()
	reconciler, kubeClient, cloud := testReconciler(t, infrav1.ManagementPolicyManaged)
	cloud.failSubnet = 1
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-de-network", Namespace: "fleet-default"}}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err == nil {
		t.Fatal("expected transient subnet error")
	}
	partial := &infrav1.TCloudClusterNetwork{}
	if err := kubeClient.Get(ctx, request.NamespacedName, partial); err != nil {
		t.Fatal(err)
	}
	if partial.Status.Resources.VPC.ID == "" || partial.Status.Resources.Subnet.ID != "" {
		t.Fatalf("partial progress was not persisted: %#v", partial.Status.Resources)
	}
	if !conditionStatus(partial, infrav1.ConditionReady, metav1.ConditionFalse) {
		t.Fatalf("failure was not exposed through Ready=False: %#v", partial.Status.Conditions)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	ready := &infrav1.TCloudClusterNetwork{}
	if err := kubeClient.Get(ctx, request.NamespacedName, ready); err != nil {
		t.Fatal(err)
	}
	if !conditionTrue(ready, infrav1.ConditionReady) {
		t.Fatalf("network did not recover: %#v", ready.Status.Conditions)
	}
}

func TestDeletionWaitsForAttachedPorts(t *testing.T) {
	ctx := context.Background()
	reconciler, kubeClient, cloud := testReconciler(t, infrav1.ManagementPolicyManaged)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-de-network", Namespace: "fleet-default"}}
	reconcileUntilReady(t, ctx, reconciler, kubeClient, request)
	cloud.ports = 1
	value := &infrav1.TCloudClusterNetwork{}
	if err := kubeClient.Get(ctx, request.NamespacedName, value); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Delete(ctx, value); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if len(cloud.deleted) != 0 {
		t.Fatalf("deleted resources while a port remained: %#v", cloud.deleted)
	}

	cloud.ports = 0
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if len(cloud.deleted) != 1 || cloud.deleted[0] != "sg-id" {
		t.Fatalf("cleanup did not resume: %#v", cloud.deleted)
	}
}

func TestDeletionWaitsForRancherMachines(t *testing.T) {
	ctx := context.Background()
	reconciler, kubeClient, cloud := testReconciler(t, infrav1.ManagementPolicyManaged)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-de-network", Namespace: "fleet-default"}}
	reconcileUntilReady(t, ctx, reconciler, kubeClient, request)
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(tcloudMachineGVK)
	machine.SetNamespace("fleet-default")
	machine.SetName("test-de-worker")
	machine.SetLabels(map[string]string{clusterNameLabel: "test-de"})
	if err := kubeClient.Create(ctx, machine); err != nil {
		t.Fatal(err)
	}
	value := &infrav1.TCloudClusterNetwork{}
	if err := kubeClient.Get(ctx, request.NamespacedName, value); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Delete(ctx, value); err != nil {
		t.Fatal(err)
	}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if len(cloud.deleted) != 0 {
		t.Fatalf("deleted resources while a machine remained: %#v", cloud.deleted)
	}
	if err := kubeClient.Delete(ctx, machine); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if len(cloud.deleted) != 1 || cloud.deleted[0] != "sg-id" {
		t.Fatalf("cleanup did not resume: %#v", cloud.deleted)
	}
}

func TestDeletionRefusesOwnershipMismatch(t *testing.T) {
	ctx := context.Background()
	reconciler, kubeClient, cloud := testReconciler(t, infrav1.ManagementPolicyManaged)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-de-network", Namespace: "fleet-default"}}
	reconcileUntilReady(t, ctx, reconciler, kubeClient, request)
	cloud.resources["sg-id"] = cloudservice.Resource{ID: "sg-id", Name: "somebody-elses-group"}
	value := &infrav1.TCloudClusterNetwork{}
	if err := kubeClient.Get(ctx, request.NamespacedName, value); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Delete(ctx, value); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err == nil {
		t.Fatal("expected ownership error")
	}
	if len(cloud.deleted) != 0 {
		t.Fatalf("ownership mismatch still deleted a resource: %#v", cloud.deleted)
	}
}

func TestObservePolicyNeverDeletesResources(t *testing.T) {
	ctx := context.Background()
	reconciler, kubeClient, cloud := testReconciler(t, infrav1.ManagementPolicyObserve)
	cloud.resources["existing-vpc"] = cloudservice.Resource{ID: "existing-vpc", Name: "existing"}
	cloud.resources["existing-subnet"] = cloudservice.Resource{ID: "existing-subnet", Name: "existing"}
	cloud.resources["existing-sg"] = cloudservice.Resource{ID: "existing-sg", Name: "existing"}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-de-network", Namespace: "fleet-default"}}

	for i := 0; i < 5; i++ {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("reconcile observe: %v", err)
		}
	}
	value := &infrav1.TCloudClusterNetwork{}
	if err := kubeClient.Get(ctx, request.NamespacedName, value); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Delete(ctx, value); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}
	if len(cloud.deleted) != 0 {
		t.Fatalf("Observe policy deleted resources: %#v", cloud.deleted)
	}
}

func TestObserveNetworkCreatesAndDeletesOnlyManagedSecurityGroup(t *testing.T) {
	ctx := context.Background()
	reconciler, kubeClient, cloud := testReconciler(t, infrav1.ManagementPolicyObserve)
	cloud.resources["existing-vpc"] = cloudservice.Resource{ID: "existing-vpc", Name: "existing-vpc"}
	cloud.resources["existing-subnet"] = cloudservice.Resource{ID: "existing-subnet", Name: "existing-subnet"}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-de-network", Namespace: "fleet-default"}}
	network := &infrav1.TCloudClusterNetwork{}
	if err := kubeClient.Get(ctx, request.NamespacedName, network); err != nil {
		t.Fatal(err)
	}
	network.Spec.Network.SecurityGroup = infrav1.SecurityGroupSpec{
		Name: "test-de-rke2", ManagementPolicy: infrav1.ManagementPolicyManaged,
		CNI: "calico", SSHAllowedCIDRs: []string{"203.0.113.10/32"},
	}
	if err := kubeClient.Update(ctx, network); err != nil {
		t.Fatal(err)
	}
	reconcileUntilReady(t, ctx, reconciler, kubeClient, request)

	if err := kubeClient.Get(ctx, request.NamespacedName, network); err != nil {
		t.Fatal(err)
	}
	if network.Status.Resources.VPC.ControllerManaged || network.Status.Resources.Subnet.ControllerManaged || !network.Status.Resources.SecurityGroup.ControllerManaged {
		t.Fatalf("unexpected hybrid ownership: %#v", network.Status.Resources)
	}
	managedSecurityGroupID := network.Status.Resources.SecurityGroup.ID
	if managedSecurityGroupID == "" {
		t.Fatal("managed security group was not created")
	}
	// Unrelated ports in the observed subnet must not block deletion of the
	// controller-owned security group.
	cloud.ports = 5
	if err := kubeClient.Delete(ctx, network); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		_, err := reconciler.Reconcile(ctx, request)
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
	}
	if len(cloud.deleted) != 1 || cloud.deleted[0] != managedSecurityGroupID {
		t.Fatalf("hybrid cleanup touched resources other than the managed security group: %#v", cloud.deleted)
	}
	if _, found := cloud.resources["existing-vpc"]; !found {
		t.Fatal("existing VPC was deleted")
	}
	if _, found := cloud.resources["existing-subnet"]; !found {
		t.Fatal("existing subnet was deleted")
	}
}

func TestAdoptMachineManagedNetwork(t *testing.T) {
	ctx := context.Background()
	reconciler, kubeClient, cloud := testReconciler(t, infrav1.ManagementPolicyAdopt)
	cloud.resources["adopted-vpc"] = cloudservice.Resource{ID: "adopted-vpc", Name: "vpc-docker-machine"}
	cloud.resources["adopted-subnet"] = cloudservice.Resource{ID: "adopted-subnet", Name: "subnet-docker-machine"}
	cloud.resources["adopted-sg"] = cloudservice.Resource{ID: "adopted-sg", Name: "docker-machine"}

	machine := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": tcloudMachineGVK.GroupVersion().String(),
		"kind":       tcloudMachineGVK.Kind,
		"metadata": map[string]interface{}{
			"name":              "test-de-worker",
			"namespace":         "fleet-default",
			"creationTimestamp": "2026-01-01T00:00:00Z",
			"labels":            map[string]interface{}{clusterNameLabel: "test-de"},
		},
		"status": map[string]interface{}{"ready": true},
	}}
	if err := kubeClient.Create(ctx, machine); err != nil {
		t.Fatal(err)
	}
	stateSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-de-worker-machine-state", Namespace: "fleet-default"},
		Data:       map[string][]byte{"extractedConfig": machineStateArchive(t, "adopted-vpc", "adopted-subnet", "adopted-sg", true)},
	}
	if err := kubeClient.Create(ctx, stateSecret); err != nil {
		t.Fatal(err)
	}

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-de-network", Namespace: "fleet-default"}}
	for i := 0; i < 6; i++ {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("reconcile adopt: %v", err)
		}
	}
	value := &infrav1.TCloudClusterNetwork{}
	if err := kubeClient.Get(ctx, request.NamespacedName, value); err != nil {
		t.Fatal(err)
	}
	if !conditionTrue(value, infrav1.ConditionReady) {
		t.Fatalf("adopted network not ready: %#v", value.Status.Conditions)
	}
	if value.Status.Resources.VPC.ID != "adopted-vpc" || value.Status.Resources.Subnet.ID != "adopted-subnet" || value.Status.Resources.SecurityGroup.ID != "adopted-sg" {
		t.Fatalf("unexpected adopted resources: %#v", value.Status.Resources)
	}
	if !value.Status.Resources.VPC.ControllerManaged || !value.Status.Resources.Subnet.ControllerManaged || !value.Status.Resources.SecurityGroup.ControllerManaged {
		t.Fatalf("adopted resources are not controller-managed: %#v", value.Status.Resources)
	}
	if len(cloud.securityGroupRequests) == 0 || len(cloud.securityGroupRequests[len(cloud.securityGroupRequests)-1].Rules) == 0 {
		t.Fatal("adoption did not reconcile security-group rules")
	}
	if err := kubeClient.Delete(ctx, machine); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Delete(ctx, value); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_, err := reconciler.Reconcile(ctx, request)
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("reconcile adopted cleanup: %v", err)
		}
	}
	if len(cloud.deleted) != 3 || cloud.deleted[0] != "adopted-sg" || cloud.deleted[1] != "adopted-subnet" || cloud.deleted[2] != "adopted-vpc" {
		t.Fatalf("unexpected adopted resource deletion order: %#v", cloud.deleted)
	}
}

func TestDecodeMachineStatePreservesUserManagedOwnership(t *testing.T) {
	state, err := decodeMachineDriverState(machineStateArchive(t, "existing-vpc", "existing-subnet", "", false))
	if err != nil {
		t.Fatal(err)
	}
	if state.Driver.VPCID.Managed || state.Driver.SubnetID.Managed || state.Driver.ManagedSecurityGroup != "" {
		t.Fatalf("unexpected decoded ownership: %#v", state.Driver)
	}
	if _, err := adoptableResourcesFromState("test-worker", state); err == nil {
		t.Fatal("expected adoption to reject a user-managed network")
	}
}

func TestOrphanExpiryDeletesRequest(t *testing.T) {
	ctx := context.Background()
	reconciler, kubeClient, _ := testReconciler(t, infrav1.ManagementPolicyManaged)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-de-network", Namespace: "fleet-default"}}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(provisioningClusterGVK)
	cluster.SetNamespace("fleet-default")
	cluster.SetName("test-de")
	if err := kubeClient.Delete(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	reconciler.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	value := &infrav1.TCloudClusterNetwork{}
	if err := kubeClient.Get(ctx, request.NamespacedName, value); err != nil {
		t.Fatal(err)
	}
	if value.DeletionTimestamp.IsZero() {
		t.Fatal("orphan request was not marked for deletion")
	}
}

func testReconciler(t *testing.T, policy infrav1.ManagementPolicy) (*TCloudClusterNetworkReconciler, client.Client, *fakeCloud) {
	t.Helper()
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

	network := &infrav1.TCloudClusterNetwork{
		TypeMeta:   metav1.TypeMeta{APIVersion: infrav1.GroupVersion.String(), Kind: "TCloudClusterNetwork"},
		ObjectMeta: metav1.ObjectMeta{Name: "test-de-network", Namespace: "fleet-default", UID: types.UID("12345678-1234-1234-1234-123456789abc"), CreationTimestamp: metav1.Now()},
		Spec: infrav1.TCloudClusterNetworkSpec{
			ClusterRef:          infrav1.NamespacedReference{Name: "test-de", Namespace: "fleet-default"},
			CredentialSecretRef: infrav1.NamespacedReference{Name: "cc-test", Namespace: "cattle-global-data"},
			ManagementPolicy:    policy, Region: "eu-de", ProjectName: "project",
			Network: infrav1.NetworkSpec{
				VPC:           infrav1.VPCSpec{ID: observeID(policy, "existing-vpc"), Name: "test-de", CIDR: "192.168.0.0/16"},
				Subnet:        infrav1.SubnetSpec{ID: observeID(policy, "existing-subnet"), Name: "test-de", CIDR: "192.168.0.0/24", GatewayIP: "192.168.0.1"},
				SecurityGroup: infrav1.SecurityGroupSpec{ID: observeID(policy, "existing-sg"), Name: "test-de-rke2", CNI: "canal", SSHAllowedCIDRs: []string{"203.0.113.10/32"}},
			},
		},
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(provisioningClusterGVK)
	cluster.SetName("test-de")
	cluster.SetNamespace("fleet-default")
	cluster.SetUID(types.UID("cluster-uid"))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cc-test", Namespace: "cattle-global-data"},
		Data: map[string][]byte{
			"opentelekomcloudcredentialConfig-authUrl":    []byte("https://iam.example/v3"),
			"opentelekomcloudcredentialConfig-username":   []byte("user"),
			"opentelekomcloudcredentialConfig-password":   []byte("password"),
			"opentelekomcloudcredentialConfig-domainName": []byte("domain"),
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&infrav1.TCloudClusterNetwork{}).WithObjects(network, cluster, secret).Build()
	cloud := newFakeCloud()
	reconciler := &TCloudClusterNetworkReconciler{
		Client: kubeClient, APIReader: kubeClient, Scheme: scheme, Factory: fakeFactory{service: cloud}, Recorder: record.NewFakeRecorder(20),
	}
	return reconciler, kubeClient, cloud
}

func machineStateArchive(t *testing.T, vpcID, subnetID, securityGroupID string, managed bool) []byte {
	t.Helper()
	config := map[string]interface{}{
		"DriverName": "opentelekomcloud",
		"Driver": map[string]interface{}{
			"vpc_id":                 map[string]interface{}{"value": vpcID, "managed": managed},
			"subnet_id":              map[string]interface{}{"value": subnetID, "managed": managed},
			"managed_security_group": securityGroupID,
		},
	}
	value, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	buffer := &bytes.Buffer{}
	gzipWriter := gzip.NewWriter(buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "/home/machine/.docker/machine/machines/test/config.json", Mode: 0600, Size: int64(len(value))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(value); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func observeID(policy infrav1.ManagementPolicy, id string) string {
	if policy == infrav1.ManagementPolicyObserve {
		return id
	}
	return ""
}

func reconcileUntilReady(t *testing.T, ctx context.Context, reconciler *TCloudClusterNetworkReconciler, kubeClient client.Client, request ctrl.Request) {
	t.Helper()
	for i := 0; i < 5; i++ {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	value := &infrav1.TCloudClusterNetwork{}
	if err := kubeClient.Get(ctx, request.NamespacedName, value); err != nil {
		t.Fatal(err)
	}
	if !conditionTrue(value, infrav1.ConditionReady) {
		t.Fatalf("network not ready: %#v", value.Status.Conditions)
	}
}

func conditionTrue(network *infrav1.TCloudClusterNetwork, conditionType string) bool {
	return conditionStatus(network, conditionType, metav1.ConditionTrue)
}

func conditionStatus(network *infrav1.TCloudClusterNetwork, conditionType string, status metav1.ConditionStatus) bool {
	for _, condition := range network.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Status == status
		}
	}
	return false
}
