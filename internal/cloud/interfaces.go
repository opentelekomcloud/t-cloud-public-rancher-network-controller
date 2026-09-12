package cloud

import "context"

type Credentials struct {
	AuthURL      string
	AuthMethod   string
	Username     string
	Password     string
	AccessKey    string
	SecretKey    string
	DomainName   string
	DomainID     string
	ProjectName  string
	ProjectID    string
	Region       string
	EndpointType string
}

type Resource struct {
	ID   string
	Name string
}

type VPCRequest struct {
	ID   string
	Name string
	CIDR string
}

type SubnetRequest struct {
	ID               string
	VPCID            string
	Name             string
	CIDR             string
	GatewayIP        string
	DNSNameservers   []string
	AvailabilityZone string
}

type Rule struct {
	Protocol      string
	FromPort      int
	ToPort        int
	CIDR          string
	RemoteGroupID string
}

type SecurityGroupRequest struct {
	ID          string
	Name        string
	Description string
	Rules       []Rule
	RemoveRules []Rule
}

type Service interface {
	EnsureVPC(context.Context, VPCRequest) (Resource, error)
	EnsureSubnet(context.Context, SubnetRequest) (Resource, error)
	EnsureSecurityGroup(context.Context, SecurityGroupRequest) (Resource, error)
	GetVPC(context.Context, string) (Resource, error)
	GetSubnet(context.Context, string) (Resource, error)
	GetSecurityGroup(context.Context, string) (Resource, error)
	AttachedPorts(context.Context, string) (int, error)
	DeleteSecurityGroup(context.Context, string) error
	DeleteSubnet(context.Context, string, string) error
	DeleteVPC(context.Context, string) error
}

type Factory interface {
	New(context.Context, Credentials) (Service, error)
}
