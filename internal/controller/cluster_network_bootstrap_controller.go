package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/opentelekomcloud/t-cloud-public-rancher-network-controller/api/v1alpha1"
)

const bootstrapRetryInterval = 5 * time.Second

type TCloudClusterNetworkBootstrapReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
}

func (r *TCloudClusterNetworkBootstrapReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(provisioningClusterGVK)
	if err := r.Get(ctx, request.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if cluster.GetDeletionTimestamp() != nil {
		return ctrl.Result{}, nil
	}

	refs, err := tcloudMachineConfigRefs(cluster)
	if err != nil || len(refs) == 0 {
		return ctrl.Result{}, err
	}
	if err := r.ensureProviderAnnotation(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	annotations := cluster.GetAnnotations()
	name := annotations[infrav1.ClusterAnnotation]
	if name == "" {
		name = networkObjectName(cluster.GetName())
	}
	network := &infrav1.TCloudClusterNetwork{}
	key := types.NamespacedName{Namespace: cluster.GetNamespace(), Name: name}
	if err := r.Get(ctx, key, network); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if annotations[infrav1.ClusterAnnotation] != "" || annotations[infrav1.NetworkPolicyAnnotation] == string(infrav1.ManagementPolicyManaged) || annotations[infrav1.NetworkPolicyAnnotation] == string(infrav1.ManagementPolicyObserve) {
			return ctrl.Result{RequeueAfter: bootstrapRetryInterval}, nil
		}
		if annotations[infrav1.NetworkPolicyAnnotation] != string(infrav1.ManagementPolicyAdopt) && desiredNodeCount(cluster) != 1 {
			return ctrl.Result{}, nil
		}
		config, err := r.machineConfig(ctx, cluster.GetNamespace(), refs[0])
		if err != nil {
			return ctrl.Result{}, err
		}
		if !machineScopedWithoutExistingNetwork(config) {
			return ctrl.Result{}, nil
		}
		network, err = r.newAdoptionNetwork(cluster, config, name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, network); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return ctrl.Result{}, err
			}
		} else if err := r.patchClusterNetworkAnnotations(ctx, cluster, name, infrav1.ManagementPolicyAdopt); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: bootstrapRetryInterval}, nil
	}

	if !meta.IsStatusConditionTrue(network.Status.Conditions, infrav1.ConditionReady) || network.Status.ObservedGeneration != network.Generation {
		return ctrl.Result{RequeueAfter: bootstrapRetryInterval}, nil
	}
	if err := completeNetworkStatus(network); err != nil {
		return ctrl.Result{}, err
	}
	configNames, err := r.machineConfigNames(ctx, cluster, refs)
	if err != nil {
		return ctrl.Result{}, err
	}
	for _, ref := range configNames {
		if err := r.patchMachineConfig(ctx, cluster.GetNamespace(), ref, network); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.patchClusterNetworkAnnotations(ctx, cluster, name, network.Spec.ManagementPolicy); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *TCloudClusterNetworkBootstrapReconciler) machineConfigNames(ctx context.Context, cluster *unstructured.Unstructured, refs []string) ([]string, error) {
	seen := make(map[string]bool, len(refs))
	names := make([]string, 0, len(refs))
	for _, name := range refs {
		seen[name] = true
		names = append(names, name)
	}
	configs := &unstructured.UnstructuredList{}
	configs.SetGroupVersionKind(tcloudMachineConfigGVK.GroupVersion().WithKind(tcloudMachineConfigGVK.Kind + "List"))
	if err := r.List(ctx, configs, client.InNamespace(cluster.GetNamespace())); err != nil {
		return nil, fmt.Errorf("list T-Cloud machine configs in %s: %w", cluster.GetNamespace(), err)
	}
	for i := range configs.Items {
		config := &configs.Items[i]
		if seen[config.GetName()] || !ownedByCluster(config, cluster) {
			continue
		}
		seen[config.GetName()] = true
		names = append(names, config.GetName())
	}
	return names, nil
}

func ownedByCluster(config, cluster *unstructured.Unstructured) bool {
	for _, owner := range config.GetOwnerReferences() {
		if owner.APIVersion == provisioningClusterGVK.GroupVersion().String() && owner.Kind == provisioningClusterGVK.Kind && owner.Name == cluster.GetName() && (cluster.GetUID() == "" || owner.UID == cluster.GetUID()) {
			return true
		}
	}
	return false
}

func desiredNodeCount(cluster *unstructured.Unstructured) int64 {
	pools, found, _ := unstructured.NestedSlice(cluster.Object, "spec", "rkeConfig", "machinePools")
	if !found {
		return 0
	}
	var total int64
	for _, raw := range pools {
		pool, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		quantity, _, _ := unstructured.NestedInt64(pool, "quantity")
		total += quantity
	}
	return total
}

func tcloudMachineConfigRefs(cluster *unstructured.Unstructured) ([]string, error) {
	pools, found, err := unstructured.NestedSlice(cluster.Object, "spec", "rkeConfig", "machinePools")
	if err != nil || !found {
		return nil, err
	}
	refs := make([]string, 0, len(pools))
	seen := map[string]bool{}
	for _, raw := range pools {
		pool, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		ref, _, err := unstructured.NestedMap(pool, "machineConfigRef")
		if err != nil {
			return nil, err
		}
		kind, _, _ := unstructured.NestedString(ref, "kind")
		name, _, _ := unstructured.NestedString(ref, "name")
		if !strings.EqualFold(kind, tcloudMachineConfigGVK.Kind) || name == "" || seen[name] {
			continue
		}
		seen[name] = true
		refs = append(refs, name)
	}
	return refs, nil
}

func (r *TCloudClusterNetworkBootstrapReconciler) machineConfig(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	config := &unstructured.Unstructured{}
	config.SetGroupVersionKind(tcloudMachineConfigGVK)
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, config); err != nil {
		return nil, fmt.Errorf("read T-Cloud machine config %s/%s: %w", namespace, name, err)
	}
	return config, nil
}

func machineScopedWithoutExistingNetwork(config *unstructured.Unstructured) bool {
	scope, _, _ := unstructured.NestedString(config.Object, "networkScope")
	vpcID, _, _ := unstructured.NestedString(config.Object, "vpcId")
	subnetID, _, _ := unstructured.NestedString(config.Object, "subnetId")
	securityGroups, _, _ := unstructured.NestedString(config.Object, "secGroups")
	return (scope == "" || scope == "machine") && vpcID == "" && subnetID == "" && securityGroups == ""
}

func (r *TCloudClusterNetworkBootstrapReconciler) newAdoptionNetwork(cluster, config *unstructured.Unstructured, name string) (*infrav1.TCloudClusterNetwork, error) {
	credential, _, _ := unstructured.NestedString(cluster.Object, "spec", "cloudCredentialSecretName")
	secretRef, err := namespacedReference(credential)
	if err != nil {
		return nil, err
	}
	region, _, _ := unstructured.NestedString(config.Object, "region")
	if region == "" {
		return nil, fmt.Errorf("T-Cloud machine config %s/%s has no region", config.GetNamespace(), config.GetName())
	}
	projectName, _, _ := unstructured.NestedString(config.Object, "projectName")
	endpointType, _, _ := unstructured.NestedString(config.Object, "endpointType")
	if endpointType == "" {
		endpointType = "public"
	}
	sshCIDR, _, _ := unstructured.NestedString(config.Object, "sshAllowCidr")
	sshCIDRs := splitNonEmpty(sshCIDR)
	if len(sshCIDRs) == 0 {
		sshCIDRs = []string{"0.0.0.0/0"}
	}
	cni, _, _ := unstructured.NestedString(cluster.Object, "spec", "rkeConfig", "machineGlobalConfig", "cni")
	if cni == "" {
		cni = "canal"
	}
	return &infrav1.TCloudClusterNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.GetNamespace()},
		Spec: infrav1.TCloudClusterNetworkSpec{
			ClusterRef:          infrav1.NamespacedReference{Name: cluster.GetName(), Namespace: cluster.GetNamespace()},
			CredentialSecretRef: secretRef,
			ManagementPolicy:    infrav1.ManagementPolicyAdopt,
			Region:              region,
			ProjectName:         projectName,
			EndpointType:        endpointType,
			Network: infrav1.NetworkSpec{
				VPC:           infrav1.VPCSpec{},
				Subnet:        infrav1.SubnetSpec{},
				SecurityGroup: infrav1.SecurityGroupSpec{CNI: cni, SSHAllowedCIDRs: sshCIDRs},
			},
		},
	}, nil
}

func namespacedReference(value string) (infrav1.NamespacedReference, error) {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return infrav1.NamespacedReference{}, fmt.Errorf("cloudCredentialSecretName %q is not a namespaced Secret reference", value)
	}
	return infrav1.NamespacedReference{Namespace: parts[0], Name: parts[1]}, nil
}

func splitNonEmpty(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func completeNetworkStatus(network *infrav1.TCloudClusterNetwork) error {
	resources := network.Status.Resources
	if resources.VPC.ID == "" || resources.Subnet.ID == "" || resources.SecurityGroup.ID == "" || resources.SecurityGroup.Name == "" {
		return fmt.Errorf("T-Cloud cluster network %s/%s is Ready without complete resource status", network.Namespace, network.Name)
	}
	return nil
}

func (r *TCloudClusterNetworkBootstrapReconciler) patchMachineConfig(ctx context.Context, namespace, name string, network *infrav1.TCloudClusterNetwork) error {
	config, err := r.machineConfig(ctx, namespace, name)
	if err != nil {
		return err
	}
	resources := network.Status.Resources
	desired := map[string]interface{}{
		"vpcId": resources.VPC.ID, "vpcName": resources.VPC.Name,
		"subnetId": resources.Subnet.ID, "subnetName": resources.Subnet.Name,
		"secGroups": resources.SecurityGroup.Name, "networkScope": "shared", "skipDefaultSg": true,
	}
	base := config.DeepCopy()
	changed := false
	for field, value := range desired {
		if current, found, _ := unstructured.NestedFieldNoCopy(config.Object, field); !found || current != value {
			if err := unstructured.SetNestedField(config.Object, value, field); err != nil {
				return err
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := r.Patch(ctx, config, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch T-Cloud machine config %s/%s: %w", namespace, name, err)
	}
	return nil
}

func (r *TCloudClusterNetworkBootstrapReconciler) ensureProviderAnnotation(ctx context.Context, cluster *unstructured.Unstructured) error {
	annotations := cluster.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if annotations[infrav1.UIProviderAnnotation] == infrav1.TCloudProviderID {
		return nil
	}
	base := cluster.DeepCopy()
	annotations[infrav1.UIProviderAnnotation] = infrav1.TCloudProviderID
	cluster.SetAnnotations(annotations)
	return r.Patch(ctx, cluster, client.MergeFrom(base))
}

func (r *TCloudClusterNetworkBootstrapReconciler) patchClusterNetworkAnnotations(ctx context.Context, cluster *unstructured.Unstructured, name string, policy infrav1.ManagementPolicy) error {
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(provisioningClusterGVK)
	if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), current); err != nil {
		return err
	}
	annotations := current.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if annotations[infrav1.ClusterAnnotation] == name && annotations[infrav1.NetworkPolicyAnnotation] == string(policy) && annotations[infrav1.UIProviderAnnotation] == infrav1.TCloudProviderID {
		return nil
	}
	base := current.DeepCopy()
	annotations[infrav1.ClusterAnnotation] = name
	annotations[infrav1.NetworkPolicyAnnotation] = string(policy)
	annotations[infrav1.UIProviderAnnotation] = infrav1.TCloudProviderID
	current.SetAnnotations(annotations)
	return r.Patch(ctx, current, client.MergeFrom(base))
}

func networkObjectName(clusterName string) string {
	runes := make([]rune, 0, len(clusterName)+len("-network"))
	lastWasDash := false
	for _, value := range strings.ToLower(clusterName + "-network") {
		valid := value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
		if valid {
			runes = append(runes, value)
			lastWasDash = false
		} else if len(runes) > 0 && !lastWasDash {
			runes = append(runes, '-')
			lastWasDash = true
		}
	}
	name := strings.Trim(string(runes), "-")
	if name == "" {
		name = "tcloud-cluster-network"
	}
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
}

func (r *TCloudClusterNetworkBootstrapReconciler) requestsForMachine(_ context.Context, object client.Object) []reconcile.Request {
	name := object.GetLabels()[clusterNameLabel]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: object.GetNamespace(), Name: name}}}
}

func (r *TCloudClusterNetworkBootstrapReconciler) requestForNetwork(_ context.Context, object client.Object) []reconcile.Request {
	network, ok := object.(*infrav1.TCloudClusterNetwork)
	if !ok || network.Spec.ClusterRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: network.Spec.ClusterRef.Namespace, Name: network.Spec.ClusterRef.Name}}}
}

func (r *TCloudClusterNetworkBootstrapReconciler) requestForMachineConfig(_ context.Context, object client.Object) []reconcile.Request {
	for _, owner := range object.GetOwnerReferences() {
		if owner.APIVersion == provisioningClusterGVK.GroupVersion().String() && owner.Kind == provisioningClusterGVK.Kind {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: object.GetNamespace(), Name: owner.Name}}}
		}
	}
	return nil
}

func (r *TCloudClusterNetworkBootstrapReconciler) SetupWithManager(manager ctrl.Manager) error {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(provisioningClusterGVK)
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(tcloudMachineGVK)
	config := &unstructured.Unstructured{}
	config.SetGroupVersionKind(tcloudMachineConfigGVK)
	return ctrl.NewControllerManagedBy(manager).
		Named("tcloud-cluster-network-bootstrap").
		For(cluster).
		Watches(machine, handler.EnqueueRequestsFromMapFunc(r.requestsForMachine)).
		Watches(config, handler.EnqueueRequestsFromMapFunc(r.requestForMachineConfig)).
		Watches(&infrav1.TCloudClusterNetwork{}, handler.EnqueueRequestsFromMapFunc(r.requestForNetwork)).
		Complete(r)
}
