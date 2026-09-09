package cloud

func RKE2Rules(securityGroupID string, sshAllowedCIDRs []string, cni string) []Rule {
	rules := make([]Rule, 0, len(sshAllowedCIDRs)+6)
	for _, cidr := range sshAllowedCIDRs {
		rules = append(rules, Rule{Protocol: "tcp", FromPort: 22, ToPort: 22, CIDR: cidr})
	}

	for _, ports := range [][2]int{{2379, 2380}, {6443, 6443}, {9345, 9345}, {10250, 10250}} {
		rules = append(rules, Rule{
			Protocol:      "tcp",
			FromPort:      ports[0],
			ToPort:        ports[1],
			RemoteGroupID: securityGroupID,
		})
	}

	if cni == "" || cni == "canal" || cni == "flannel" {
		rules = append(rules, Rule{
			Protocol:      "udp",
			FromPort:      8472,
			ToPort:        8472,
			RemoteGroupID: securityGroupID,
		})
	}

	return rules
}
