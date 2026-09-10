package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	NetworkFinalizer     = "infrastructure.otc.t-systems.com/network-cleanup"
	ClusterAnnotation    = "infrastructure.otc.t-systems.com/cluster-network"
	ConditionReady       = "Ready"
	ConditionCredentials = "CredentialsReady"
	ConditionNetwork     = "NetworkReady"
	ConditionDeleting    = "Deleting"
	ConditionOwnerBound  = "OwnerBound"
	ConditionOwnership   = "OwnershipVerified"
	DefaultOrphanTimeout = "1h"
)

type ManagementPolicy string

const (
	ManagementPolicyManaged ManagementPolicy = "Managed"
	ManagementPolicyObserve ManagementPolicy = "Observe"
	ManagementPolicyAbandon ManagementPolicy = "Abandon"
)

type NamespacedReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
}

type VPCSpec struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	// +kubebuilder:validation:Format=cidr
	CIDR string `json:"cidr,omitempty"`
}

type SubnetSpec struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	// +kubebuilder:validation:Format=cidr
	CIDR string `json:"cidr,omitempty"`
	// +kubebuilder:validation:Format=ipv4
	GatewayIP string `json:"gatewayIP,omitempty"`
	// +kubebuilder:validation:items:Format=ipv4
	DNSNameservers   []string `json:"dnsNameservers,omitempty"`
	AvailabilityZone string   `json:"availabilityZone,omitempty"`
}

type SecurityGroupSpec struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	// +kubebuilder:validation:items:Format=cidr
	// +kubebuilder:validation:MinItems=1
	SSHAllowedCIDRs []string `json:"sshAllowedCIDRs,omitempty"`
	// +kubebuilder:validation:Enum=canal;flannel;calico
	CNI string `json:"cni,omitempty"`
}

type NetworkSpec struct {
	VPC           VPCSpec           `json:"vpc"`
	Subnet        SubnetSpec        `json:"subnet"`
	SecurityGroup SecurityGroupSpec `json:"securityGroup"`
}

// +kubebuilder:validation:XValidation:rule="self.managementPolicy != 'Managed' || (has(self.network.vpc.cidr) && has(self.network.subnet.cidr) && has(self.network.subnet.gatewayIP) && size(self.network.securityGroup.sshAllowedCIDRs) > 0)",message="Managed policy requires VPC CIDR, subnet CIDR, gatewayIP, and at least one SSH allowed CIDR"
// +kubebuilder:validation:XValidation:rule="self.managementPolicy != 'Observe' || (has(self.network.vpc.id) && has(self.network.subnet.id) && has(self.network.securityGroup.id))",message="Observe policy requires VPC, subnet, and security-group IDs"
// +kubebuilder:validation:XValidation:rule="self.clusterRef == oldSelf.clusterRef",message="clusterRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.region == oldSelf.region",message="region is immutable"
// +kubebuilder:validation:XValidation:rule="self.projectName == oldSelf.projectName",message="projectName is immutable"
type TCloudClusterNetworkSpec struct {
	ClusterRef          NamespacedReference `json:"clusterRef"`
	CredentialSecretRef NamespacedReference `json:"credentialSecretRef"`
	// +kubebuilder:default=Managed
	// +kubebuilder:validation:Enum=Managed;Observe;Abandon
	ManagementPolicy ManagementPolicy `json:"managementPolicy,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Region      string `json:"region"`
	ProjectName string `json:"projectName,omitempty"`
	// +kubebuilder:default=public
	// +kubebuilder:validation:Enum=public;internal;admin;publicURL;internalURL;adminURL
	EndpointType string      `json:"endpointType,omitempty"`
	Network      NetworkSpec `json:"network"`
	// +kubebuilder:default="1h"
	OrphanCleanupAfter *metav1.Duration `json:"orphanCleanupAfter,omitempty"`
}

type ResourceStatus struct {
	ID                string `json:"id,omitempty"`
	Name              string `json:"name,omitempty"`
	ControllerManaged bool   `json:"controllerManaged,omitempty"`
}

type NetworkResourceStatus struct {
	VPC           ResourceStatus `json:"vpc,omitempty"`
	Subnet        ResourceStatus `json:"subnet,omitempty"`
	SecurityGroup ResourceStatus `json:"securityGroup,omitempty"`
}

type TCloudClusterNetworkStatus struct {
	ObservedGeneration int64                 `json:"observedGeneration,omitempty"`
	ClusterUID         string                `json:"clusterUID,omitempty"`
	OwnershipToken     string                `json:"ownershipToken,omitempty"`
	Resources          NetworkResourceStatus `json:"resources,omitempty"`
	Conditions         []metav1.Condition    `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=tcn
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=`.spec.managementPolicy`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="VPC",type=string,JSONPath=`.status.resources.vpc.id`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type TCloudClusterNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TCloudClusterNetworkSpec   `json:"spec,omitempty"`
	Status TCloudClusterNetworkStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type TCloudClusterNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TCloudClusterNetwork `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TCloudClusterNetwork{}, &TCloudClusterNetworkList{})
}
