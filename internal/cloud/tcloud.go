package cloud

import (
	"context"
	"errors"
	"fmt"
	"strings"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack/compute/v2/extensions/secgroups"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack/networking/v1/subnets"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack/networking/v1/vpcs"
	"github.com/opentelekomcloud/gophertelekomcloud/openstack/networking/v2/ports"
)

type TCloudFactory struct{}

type tcloudService struct {
	vpc     *golangsdk.ServiceClient
	compute *golangsdk.ServiceClient
	network *golangsdk.ServiceClient
}

func (TCloudFactory) New(_ context.Context, credentials Credentials) (Service, error) {
	provider, err := openstack.AuthenticatedClient(golangsdk.AuthOptions{
		IdentityEndpoint: credentials.AuthURL,
		Username:         credentials.Username,
		Password:         credentials.Password,
		DomainName:       credentials.DomainName,
		DomainID:         credentials.DomainID,
		TenantName:       credentials.ProjectName,
		TenantID:         credentials.ProjectID,
		AllowReauth:      true,
	})
	if err != nil {
		return nil, fmt.Errorf("authenticate with T-Cloud: %w", err)
	}
	provider.UserAgent.Prepend("t-cloud-rancher-network-controller")

	endpointOpts := golangsdk.EndpointOpts{
		Region:       credentials.Region,
		Availability: endpointAvailability(credentials.EndpointType),
	}
	vpcClient, err := openstack.NewNetworkV1(provider, endpointOpts)
	if err != nil {
		return nil, fmt.Errorf("create VPC client: %w", err)
	}
	computeClient, err := openstack.NewComputeV2(provider, endpointOpts)
	if err != nil {
		return nil, fmt.Errorf("create compute client: %w", err)
	}
	networkClient, err := openstack.NewNetworkV2(provider, endpointOpts)
	if err != nil {
		return nil, fmt.Errorf("create network client: %w", err)
	}

	return &tcloudService{vpc: vpcClient, compute: computeClient, network: networkClient}, nil
}

func endpointAvailability(value string) golangsdk.Availability {
	switch strings.ToLower(value) {
	case "internal", "internalurl":
		return golangsdk.AvailabilityInternal
	case "admin", "adminurl":
		return golangsdk.AvailabilityAdmin
	default:
		return golangsdk.AvailabilityPublic
	}
}

func (s *tcloudService) EnsureVPC(_ context.Context, request VPCRequest) (Resource, error) {
	if request.ID != "" {
		return s.GetVPC(context.Background(), request.ID)
	}
	list, err := vpcs.List(s.vpc, vpcs.ListOpts{Name: request.Name})
	if err != nil {
		return Resource{}, err
	}
	if len(list) > 1 {
		return Resource{}, fmt.Errorf("multiple VPCs found with managed name %q", request.Name)
	}
	if len(list) == 1 {
		return Resource{ID: list[0].ID, Name: list[0].Name}, nil
	}
	created, err := vpcs.Create(s.vpc, vpcs.CreateOpts{Name: request.Name, CIDR: request.CIDR}).Extract()
	if err != nil {
		return Resource{}, err
	}
	return Resource{ID: created.ID, Name: created.Name}, nil
}

func (s *tcloudService) EnsureSubnet(_ context.Context, request SubnetRequest) (Resource, error) {
	if request.ID != "" {
		return s.GetSubnet(context.Background(), request.ID)
	}
	list, err := subnets.List(s.vpc, subnets.ListOpts{VpcID: request.VPCID, Name: request.Name})
	if err != nil {
		return Resource{}, err
	}
	if len(list) > 1 {
		return Resource{}, fmt.Errorf("multiple subnets found with managed name %q in VPC %s", request.Name, request.VPCID)
	}
	if len(list) == 1 {
		return Resource{ID: list[0].ID, Name: list[0].Name}, nil
	}
	dns := request.DNSNameservers
	if len(dns) == 0 {
		dns = []string{"100.125.4.25", "8.8.8.8"}
	}
	enableDHCP := true
	created, err := subnets.Create(s.vpc, subnets.CreateOpts{
		VpcID: request.VPCID, Name: request.Name, CIDR: request.CIDR,
		GatewayIP: request.GatewayIP, DNSList: dns, AvailabilityZone: request.AvailabilityZone,
		EnableDHCP: &enableDHCP,
	}).Extract()
	if err != nil {
		return Resource{}, err
	}
	return Resource{ID: created.ID, Name: created.Name}, nil
}

func (s *tcloudService) EnsureSecurityGroup(_ context.Context, request SecurityGroupRequest) (Resource, error) {
	var group *secgroups.SecurityGroup
	if request.ID != "" {
		found, err := secgroups.Get(s.compute, request.ID).Extract()
		if err != nil {
			return Resource{}, err
		}
		group = found
	} else {
		page, err := secgroups.List(s.compute).AllPages()
		if err != nil {
			return Resource{}, err
		}
		groups, err := secgroups.ExtractSecurityGroups(page)
		if err != nil {
			return Resource{}, err
		}
		for i := range groups {
			if groups[i].Name == request.Name {
				if group != nil {
					return Resource{}, fmt.Errorf("multiple security groups found with managed name %q", request.Name)
				}
				group = &groups[i]
			}
		}
		if group == nil {
			created, err := secgroups.Create(s.compute, secgroups.CreateOpts{Name: request.Name, Description: request.Description}).Extract()
			if err != nil {
				return Resource{}, err
			}
			group = created
		}
	}

	for _, rule := range request.Rules {
		if hasRule(group, rule) {
			continue
		}
		if err := secgroups.CreateRule(s.compute, secgroups.CreateRuleOpts{
			ParentGroupID: group.ID, FromPort: rule.FromPort, ToPort: rule.ToPort,
			IPProtocol: rule.Protocol, CIDR: rule.CIDR, FromGroupID: rule.RemoteGroupID,
		}).Err; err != nil {
			return Resource{}, err
		}
	}

	return Resource{ID: group.ID, Name: group.Name}, nil
}

func hasRule(group *secgroups.SecurityGroup, wanted Rule) bool {
	for _, existing := range group.Rules {
		remoteGroupMatches := wanted.RemoteGroupID == "" || existing.Group.Name == group.Name
		if strings.EqualFold(existing.IPProtocol, wanted.Protocol) && existing.FromPort == wanted.FromPort &&
			existing.ToPort == wanted.ToPort && existing.IPRange.CIDR == wanted.CIDR && remoteGroupMatches {
			return true
		}
	}
	return false
}

func (s *tcloudService) GetVPC(_ context.Context, id string) (Resource, error) {
	value, err := vpcs.Get(s.vpc, id).Extract()
	if err != nil {
		return Resource{}, err
	}
	return Resource{ID: value.ID, Name: value.Name}, nil
}

func (s *tcloudService) GetSubnet(_ context.Context, id string) (Resource, error) {
	value, err := subnets.Get(s.vpc, id).Extract()
	if err != nil {
		return Resource{}, err
	}
	return Resource{ID: value.ID, Name: value.Name}, nil
}

func (s *tcloudService) GetSecurityGroup(_ context.Context, id string) (Resource, error) {
	value, err := secgroups.Get(s.compute, id).Extract()
	if err != nil {
		return Resource{}, err
	}
	return Resource{ID: value.ID, Name: value.Name}, nil
}

func (s *tcloudService) AttachedPorts(_ context.Context, subnetID string) (int, error) {
	subnet, err := subnets.Get(s.vpc, subnetID).Extract()
	if err != nil {
		if IsNotFound(err) {
			return 0, nil
		}
		return 0, err
	}
	page, err := ports.List(s.network, ports.ListOpts{NetworkID: subnet.NetworkID}).AllPages()
	if err != nil {
		return 0, err
	}
	values, err := ports.ExtractPorts(page)
	if err != nil {
		return 0, err
	}
	attached := 0
	for _, port := range values {
		if strings.HasPrefix(port.DeviceOwner, "compute:") {
			attached++
		}
	}
	return attached, nil
}

func (s *tcloudService) DeleteSecurityGroup(_ context.Context, id string) error {
	err := secgroups.Delete(s.compute, id).Err
	if IsNotFound(err) {
		return nil
	}
	return err
}

func (s *tcloudService) DeleteSubnet(_ context.Context, vpcID, subnetID string) error {
	err := subnets.Delete(s.vpc, vpcID, subnetID).Err
	if IsNotFound(err) {
		return nil
	}
	return err
}

func (s *tcloudService) DeleteVPC(_ context.Context, id string) error {
	err := vpcs.Delete(s.vpc, id).Err
	if IsNotFound(err) {
		return nil
	}
	return err
}

func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	var notFound golangsdk.ErrDefault404
	return errors.As(err, &notFound)
}
