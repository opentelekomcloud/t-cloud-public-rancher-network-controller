package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/opentelekomcloud/t-cloud-public-rancher-network-controller/api/v1alpha1"
	cloudservice "github.com/opentelekomcloud/t-cloud-public-rancher-network-controller/internal/cloud"
)

var (
	provisioningClusterGVK = schema.GroupVersionKind{Group: "provisioning.cattle.io", Version: "v1", Kind: "Cluster"}
	tcloudMachineGVK       = schema.GroupVersionKind{Group: "rke-machine.cattle.io", Version: "v1", Kind: "OpentelekomcloudMachine"}
	tcloudMachineConfigGVK = schema.GroupVersionKind{Group: "rke-machine-config.cattle.io", Version: "v1", Kind: "OpentelekomcloudConfig"}
)

const clusterNameLabel = "cluster.x-k8s.io/cluster-name"

// +kubebuilder:rbac:groups=infrastructure.otc.t-systems.com,resources=tcloudclusternetworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.otc.t-systems.com,resources=tcloudclusternetworks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.otc.t-systems.com,resources=tcloudclusternetworks/finalizers,verbs=update
// +kubebuilder:rbac:groups=provisioning.cattle.io,resources=clusters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=rke-machine.cattle.io,resources=opentelekomcloudmachines,verbs=get;list;watch
// +kubebuilder:rbac:groups=rke-machine-config.cattle.io,resources=opentelekomcloudconfigs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

type TCloudClusterNetworkReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Factory   cloudservice.Factory
	Recorder  record.EventRecorder
	Now       func() time.Time
}

func (r *TCloudClusterNetworkReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	network := &infrav1.TCloudClusterNetwork{}
	if err := r.Get(ctx, request.NamespacedName, network); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !network.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, network)
	}

	if controllerutil.AddFinalizer(network, infrav1.NetworkFinalizer) {
		if err := r.Update(ctx, network); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	bound, result, err := r.bindClusterOwner(ctx, network)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionOwnerBound, "ClusterLookupFailed", err)
	}
	if result != nil {
		return *result, nil
	}
	if !bound && r.orphanExpired(network) {
		r.Recorder.Event(network, corev1.EventTypeWarning, "OrphanExpired", "Deleting an unbound managed network request after its orphan timeout")
		return ctrl.Result{}, r.Delete(ctx, network)
	}

	service, err := r.cloudService(ctx, network)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionCredentials, "CredentialsInvalid", err)
	}
	r.setCondition(ctx, network, infrav1.ConditionCredentials, metav1.ConditionTrue, "CredentialsAccepted", "Cloud credentials are usable")

	policy := network.Spec.ManagementPolicy
	if policy == "" {
		policy = infrav1.ManagementPolicyManaged
	}
	switch policy {
	case infrav1.ManagementPolicyManaged:
		return r.reconcileManaged(ctx, network, service)
	case infrav1.ManagementPolicyObserve:
		return r.reconcileObserve(ctx, network, service)
	case infrav1.ManagementPolicyAdopt:
		return r.reconcileAdopt(ctx, network, service)
	case infrav1.ManagementPolicyAbandon:
		return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionReady, "AbandonOnlyDuringDeletion", fmt.Errorf("Abandon policy is only valid while deleting"))
	default:
		return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionReady, "InvalidPolicy", fmt.Errorf("unsupported management policy %q", policy))
	}
}

func (r *TCloudClusterNetworkReconciler) reconcileAdopt(ctx context.Context, network *infrav1.TCloudClusterNetwork, service cloudservice.Service) (ctrl.Result, error) {
	resources := network.Status.Resources
	if resources.VPC.ID == "" || resources.Subnet.ID == "" || resources.SecurityGroup.ID == "" {
		discovered, err := r.discoverAdoptableResources(ctx, network)
		if err != nil {
			return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionNetwork, "NetworkAdoptionFailed", err)
		}
		vpc, err := service.GetVPC(ctx, discovered.VPC.ID)
		if err != nil {
			return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionNetwork, "VPCAdoptionFailed", err)
		}
		subnet, err := service.GetSubnet(ctx, discovered.Subnet.ID)
		if err != nil {
			return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionNetwork, "SubnetAdoptionFailed", err)
		}
		securityGroup, err := service.GetSecurityGroup(ctx, discovered.SecurityGroup.ID)
		if err != nil {
			return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionNetwork, "SecurityGroupAdoptionFailed", err)
		}
		discovered.VPC.Name = vpc.Name
		discovered.Subnet.Name = subnet.Name
		discovered.SecurityGroup.Name = securityGroup.Name
		if err := r.patchStatus(ctx, network, func() {
			network.Status.OwnershipToken = string(network.UID)
			network.Status.Resources = discovered
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if _, err := service.EnsureSecurityGroup(ctx, cloudservice.SecurityGroupRequest{
		ID: resources.SecurityGroup.ID, Name: resources.SecurityGroup.Name,
		Rules: cloudservice.RKE2Rules(resources.SecurityGroup.ID, network.Spec.Network.SecurityGroup.SSHAllowedCIDRs, network.Spec.Network.SecurityGroup.CNI),
	}); err != nil {
		return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionNetwork, "SecurityGroupRulesFailed", err)
	}

	if err := r.patchStatus(ctx, network, func() {
		network.Status.ObservedGeneration = network.Generation
		meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
			Type: infrav1.ConditionNetwork, Status: metav1.ConditionTrue, Reason: "ResourcesAdopted", Message: "Existing machine-owned network resources are now controller-managed",
			ObservedGeneration: network.Generation,
		})
		meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
			Type: infrav1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Reconciled", Message: "Adopted T-Cloud cluster network is ready",
			ObservedGeneration: network.Generation,
		})
	}); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *TCloudClusterNetworkReconciler) reconcileManaged(ctx context.Context, network *infrav1.TCloudClusterNetwork, service cloudservice.Service) (ctrl.Result, error) {
	if network.Status.OwnershipToken == "" {
		if err := r.patchStatus(ctx, network, func() { network.Status.OwnershipToken = string(network.UID) }); err != nil {
			return ctrl.Result{}, err
		}
	}
	token := shortToken(network.Status.OwnershipToken)

	vpcName := managedName(firstNonEmpty(network.Spec.Network.VPC.Name, network.Spec.ClusterRef.Name), token)
	vpc, err := service.EnsureVPC(ctx, cloudservice.VPCRequest{
		ID: network.Status.Resources.VPC.ID, Name: vpcName, CIDR: network.Spec.Network.VPC.CIDR,
	})
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionNetwork, "VPCCreationFailed", err)
	}
	if network.Status.Resources.VPC.ID != vpc.ID {
		if err := r.patchStatus(ctx, network, func() {
			network.Status.Resources.VPC = infrav1.ResourceStatus{ID: vpc.ID, Name: vpc.Name, ControllerManaged: true}
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	subnetName := managedName(firstNonEmpty(network.Spec.Network.Subnet.Name, network.Spec.ClusterRef.Name+"-subnet"), token)
	subnet, err := service.EnsureSubnet(ctx, cloudservice.SubnetRequest{
		ID: network.Status.Resources.Subnet.ID, VPCID: vpc.ID, Name: subnetName,
		CIDR: network.Spec.Network.Subnet.CIDR, GatewayIP: network.Spec.Network.Subnet.GatewayIP,
		DNSNameservers: network.Spec.Network.Subnet.DNSNameservers, AvailabilityZone: network.Spec.Network.Subnet.AvailabilityZone,
	})
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionNetwork, "SubnetCreationFailed", err)
	}
	if network.Status.Resources.Subnet.ID != subnet.ID {
		if err := r.patchStatus(ctx, network, func() {
			network.Status.Resources.Subnet = infrav1.ResourceStatus{ID: subnet.ID, Name: subnet.Name, ControllerManaged: true}
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	securityGroupName := managedName(firstNonEmpty(network.Spec.Network.SecurityGroup.Name, network.Spec.ClusterRef.Name+"-rke2"), token)
	securityGroup, err := service.EnsureSecurityGroup(ctx, cloudservice.SecurityGroupRequest{
		ID: network.Status.Resources.SecurityGroup.ID, Name: securityGroupName,
		Description: fmt.Sprintf("Managed by T-Cloud Rancher network controller for %s/%s (%s)", network.Namespace, network.Name, token),
	})
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionNetwork, "SecurityGroupCreationFailed", err)
	}
	if network.Status.Resources.SecurityGroup.ID != securityGroup.ID {
		if err := r.patchStatus(ctx, network, func() {
			network.Status.Resources.SecurityGroup = infrav1.ResourceStatus{ID: securityGroup.ID, Name: securityGroup.Name, ControllerManaged: true}
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	_, err = service.EnsureSecurityGroup(ctx, cloudservice.SecurityGroupRequest{
		ID: securityGroup.ID, Name: securityGroupName,
		Rules: cloudservice.RKE2Rules(securityGroup.ID, network.Spec.Network.SecurityGroup.SSHAllowedCIDRs, network.Spec.Network.SecurityGroup.CNI),
	})
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionNetwork, "SecurityGroupRulesFailed", err)
	}

	if err := r.patchStatus(ctx, network, func() {
		network.Status.ObservedGeneration = network.Generation
		meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
			Type: infrav1.ConditionNetwork, Status: metav1.ConditionTrue, Reason: "ResourcesReady", Message: "Managed network resources are ready",
			ObservedGeneration: network.Generation,
		})
		meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
			Type: infrav1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Reconciled", Message: "T-Cloud cluster network is ready",
			ObservedGeneration: network.Generation,
		})
	}); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *TCloudClusterNetworkReconciler) reconcileObserve(ctx context.Context, network *infrav1.TCloudClusterNetwork, service cloudservice.Service) (ctrl.Result, error) {
	vpc, err := service.GetVPC(ctx, network.Spec.Network.VPC.ID)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionNetwork, "VPCObservationFailed", err)
	}
	subnet, err := service.GetSubnet(ctx, network.Spec.Network.Subnet.ID)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionNetwork, "SubnetObservationFailed", err)
	}
	securityGroup, err := service.GetSecurityGroup(ctx, network.Spec.Network.SecurityGroup.ID)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionNetwork, "SecurityGroupObservationFailed", err)
	}

	if err := r.patchStatus(ctx, network, func() {
		network.Status.ObservedGeneration = network.Generation
		network.Status.Resources = infrav1.NetworkResourceStatus{
			VPC:           infrav1.ResourceStatus{ID: vpc.ID, Name: vpc.Name},
			Subnet:        infrav1.ResourceStatus{ID: subnet.ID, Name: subnet.Name},
			SecurityGroup: infrav1.ResourceStatus{ID: securityGroup.ID, Name: securityGroup.Name},
		}
		meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
			Type: infrav1.ConditionNetwork, Status: metav1.ConditionTrue, Reason: "ResourcesObserved", Message: "Existing network resources were found",
			ObservedGeneration: network.Generation,
		})
		meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
			Type: infrav1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Reconciled", Message: "Existing T-Cloud cluster network is ready",
			ObservedGeneration: network.Generation,
		})
	}); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

func (r *TCloudClusterNetworkReconciler) reconcileDelete(ctx context.Context, network *infrav1.TCloudClusterNetwork) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(network, infrav1.NetworkFinalizer) {
		return ctrl.Result{}, nil
	}
	if network.Spec.ManagementPolicy == infrav1.ManagementPolicyObserve || network.Spec.ManagementPolicy == infrav1.ManagementPolicyAbandon {
		return ctrl.Result{}, r.removeFinalizer(ctx, network)
	}

	remaining, err := r.machineCount(ctx, network)
	if err != nil {
		return ctrl.Result{}, err
	}
	if remaining > 0 {
		r.setCondition(ctx, network, infrav1.ConditionDeleting, metav1.ConditionFalse, "WaitingForMachines", fmt.Sprintf("Waiting for %d T-Cloud machines", remaining))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	service, err := r.cloudService(ctx, network)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionDeleting, "CredentialsInvalid", err)
	}
	if network.Status.Resources.Subnet.ID != "" {
		ports, err := service.AttachedPorts(ctx, network.Status.Resources.Subnet.ID)
		if err != nil {
			return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionDeleting, "PortLookupFailed", err)
		}
		if ports > 0 {
			r.setCondition(ctx, network, infrav1.ConditionDeleting, metav1.ConditionFalse, "WaitingForPorts", fmt.Sprintf("Waiting for %d attached cloud ports", ports))
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
	}

	if network.Status.Resources.SecurityGroup.ID != "" {
		if err := r.verifySecurityGroupOwnership(ctx, network, service); err != nil {
			return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionOwnership, "OwnershipMismatch", err)
		}
		if err := service.DeleteSecurityGroup(ctx, network.Status.Resources.SecurityGroup.ID); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, r.patchStatus(ctx, network, func() { network.Status.Resources.SecurityGroup = infrav1.ResourceStatus{} })
	}
	if network.Status.Resources.Subnet.ID != "" {
		if err := r.verifySubnetOwnership(ctx, network, service); err != nil {
			return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionOwnership, "OwnershipMismatch", err)
		}
		if err := service.DeleteSubnet(ctx, network.Status.Resources.VPC.ID, network.Status.Resources.Subnet.ID); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, r.patchStatus(ctx, network, func() { network.Status.Resources.Subnet = infrav1.ResourceStatus{} })
	}
	if network.Status.Resources.VPC.ID != "" {
		if err := r.verifyVPCOwnership(ctx, network, service); err != nil {
			return ctrl.Result{}, r.fail(ctx, network, infrav1.ConditionOwnership, "OwnershipMismatch", err)
		}
		if err := service.DeleteVPC(ctx, network.Status.Resources.VPC.ID); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, r.patchStatus(ctx, network, func() { network.Status.Resources.VPC = infrav1.ResourceStatus{} })
	}

	r.Recorder.Event(network, corev1.EventTypeNormal, "NetworkDeleted", "Deleted all controller-managed T-Cloud network resources")
	return ctrl.Result{}, r.removeFinalizer(ctx, network)
}

func (r *TCloudClusterNetworkReconciler) cloudService(ctx context.Context, network *infrav1.TCloudClusterNetwork) (cloudservice.Service, error) {
	ref := network.Spec.CredentialSecretRef
	secret := &corev1.Secret{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, secret); err != nil {
		return nil, fmt.Errorf("read cloud credential secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	credentials, err := cloudservice.CredentialsFromSecret(secret, network.Spec.Region, network.Spec.ProjectName, network.Spec.EndpointType)
	if err != nil {
		return nil, err
	}
	return r.Factory.New(ctx, credentials)
}

func (r *TCloudClusterNetworkReconciler) bindClusterOwner(ctx context.Context, network *infrav1.TCloudClusterNetwork) (bool, *ctrl.Result, error) {
	ref := network.Spec.ClusterRef
	namespace := firstNonEmpty(ref.Namespace, network.Namespace)
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(provisioningClusterGVK)
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			r.setCondition(ctx, network, infrav1.ConditionOwnerBound, metav1.ConditionFalse, "ClusterNotFound", "Waiting for the Rancher provisioning Cluster")
			return false, nil, nil
		}
		return false, nil, err
	}

	wanted := metav1.OwnerReference{APIVersion: provisioningClusterGVK.GroupVersion().String(), Kind: provisioningClusterGVK.Kind, Name: cluster.GetName(), UID: cluster.GetUID()}
	ownerFound := false
	for _, owner := range network.OwnerReferences {
		if owner.UID == wanted.UID {
			ownerFound = true
			break
		}
	}
	if !ownerFound {
		base := network.DeepCopy()
		network.OwnerReferences = append(network.OwnerReferences, wanted)
		if err := r.Patch(ctx, network, client.MergeFrom(base)); err != nil {
			return false, nil, err
		}
		return false, &ctrl.Result{Requeue: true}, nil
	}
	if network.Status.ClusterUID != string(cluster.GetUID()) {
		if err := r.patchStatus(ctx, network, func() { network.Status.ClusterUID = string(cluster.GetUID()) }); err != nil {
			return false, nil, err
		}
	}
	r.setCondition(ctx, network, infrav1.ConditionOwnerBound, metav1.ConditionTrue, "ClusterBound", "Bound to the Rancher provisioning Cluster")
	return true, nil, nil
}

func (r *TCloudClusterNetworkReconciler) orphanExpired(network *infrav1.TCloudClusterNetwork) bool {
	timeout := time.Hour
	if network.Spec.OrphanCleanupAfter != nil {
		timeout = network.Spec.OrphanCleanupAfter.Duration
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	return timeout > 0 && now().After(network.CreationTimestamp.Add(timeout))
}

func (r *TCloudClusterNetworkReconciler) machineCount(ctx context.Context, network *infrav1.TCloudClusterNetwork) (int, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(tcloudMachineGVK.GroupVersion().WithKind(tcloudMachineGVK.Kind + "List"))
	if err := r.List(ctx, list, client.InNamespace(network.Spec.ClusterRef.Namespace), client.MatchingLabels{clusterNameLabel: network.Spec.ClusterRef.Name}); err != nil {
		if meta.IsNoMatchError(err) {
			return 0, nil
		}
		return 0, err
	}
	return len(list.Items), nil
}

func (r *TCloudClusterNetworkReconciler) verifyVPCOwnership(ctx context.Context, network *infrav1.TCloudClusterNetwork, service cloudservice.Service) error {
	return verifyResource(ctx, network.Status.Resources.VPC, service.GetVPC)
}

func (r *TCloudClusterNetworkReconciler) verifySubnetOwnership(ctx context.Context, network *infrav1.TCloudClusterNetwork, service cloudservice.Service) error {
	return verifyResource(ctx, network.Status.Resources.Subnet, service.GetSubnet)
}

func (r *TCloudClusterNetworkReconciler) verifySecurityGroupOwnership(ctx context.Context, network *infrav1.TCloudClusterNetwork, service cloudservice.Service) error {
	return verifyResource(ctx, network.Status.Resources.SecurityGroup, service.GetSecurityGroup)
}

func verifyResource(ctx context.Context, status infrav1.ResourceStatus, get func(context.Context, string) (cloudservice.Resource, error)) error {
	if !status.ControllerManaged {
		return fmt.Errorf("resource %s is not marked controller-managed", status.ID)
	}
	actual, err := get(ctx, status.ID)
	if err != nil {
		if cloudservice.IsNotFound(err) {
			return nil
		}
		return err
	}
	if actual.Name != status.Name {
		return fmt.Errorf("resource %s name changed from %q to %q", status.ID, status.Name, actual.Name)
	}
	return nil
}

func (r *TCloudClusterNetworkReconciler) removeFinalizer(ctx context.Context, network *infrav1.TCloudClusterNetwork) error {
	base := network.DeepCopy()
	controllerutil.RemoveFinalizer(network, infrav1.NetworkFinalizer)
	return r.Patch(ctx, network, client.MergeFrom(base))
}

func (r *TCloudClusterNetworkReconciler) patchStatus(ctx context.Context, network *infrav1.TCloudClusterNetwork, mutate func()) error {
	base := network.DeepCopy()
	mutate()
	return r.Status().Patch(ctx, network, client.MergeFrom(base))
}

func (r *TCloudClusterNetworkReconciler) setCondition(ctx context.Context, network *infrav1.TCloudClusterNetwork, conditionType string, status metav1.ConditionStatus, reason, message string) {
	_ = r.patchStatus(ctx, network, func() {
		meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
			Type: conditionType, Status: status, Reason: reason, Message: message, ObservedGeneration: network.Generation,
		})
	})
}

func (r *TCloudClusterNetworkReconciler) fail(ctx context.Context, network *infrav1.TCloudClusterNetwork, conditionType, reason string, err error) error {
	statusErr := r.patchStatus(ctx, network, func() {
		condition := metav1.Condition{
			Type: conditionType, Status: metav1.ConditionFalse, Reason: reason, Message: err.Error(), ObservedGeneration: network.Generation,
		}
		meta.SetStatusCondition(&network.Status.Conditions, condition)
		if conditionType != infrav1.ConditionReady {
			condition.Type = infrav1.ConditionReady
			meta.SetStatusCondition(&network.Status.Conditions, condition)
		}
	})
	if statusErr != nil {
		return errors.Join(err, fmt.Errorf("publish failure status: %w", statusErr))
	}
	return err
}

func managedName(base, token string) string {
	name := strings.ToLower(base)
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, name)
	name = strings.Trim(name, "-")
	if name == "" || len(validation.IsDNS1123Label(name)) > 0 {
		name = "tcloud-network"
	}
	suffix := "-" + token
	if len(name)+len(suffix) > 63 {
		name = strings.TrimRight(name[:63-len(suffix)], "-")
	}
	return name + suffix
}

func shortToken(token string) string {
	token = strings.ReplaceAll(token, "-", "")
	if len(token) > 8 {
		return token[:8]
	}
	return token
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (r *TCloudClusterNetworkReconciler) requestsForCluster(ctx context.Context, object client.Object) []reconcile.Request {
	list := &infrav1.TCloudClusterNetworkList{}
	if err := r.List(ctx, list, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range list.Items {
		item := &list.Items[i]
		if item.Spec.ClusterRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(item)})
		}
	}
	return requests
}

func (r *TCloudClusterNetworkReconciler) requestsForMachine(ctx context.Context, object client.Object) []reconcile.Request {
	clusterName := object.GetLabels()[clusterNameLabel]
	if clusterName == "" {
		return nil
	}
	proxy := &unstructured.Unstructured{}
	proxy.SetNamespace(object.GetNamespace())
	proxy.SetName(clusterName)
	return r.requestsForCluster(ctx, proxy)
}

func (r *TCloudClusterNetworkReconciler) SetupWithManager(manager ctrl.Manager) error {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(provisioningClusterGVK)
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(tcloudMachineGVK)

	return ctrl.NewControllerManagedBy(manager).
		For(&infrav1.TCloudClusterNetwork{}, builder.WithPredicates()).
		Watches(cluster, handler.EnqueueRequestsFromMapFunc(r.requestsForCluster)).
		Watches(machine, handler.EnqueueRequestsFromMapFunc(r.requestsForMachine)).
		Complete(r)
}
