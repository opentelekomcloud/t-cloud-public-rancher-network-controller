package cloud

func RKE2Rules(securityGroupID string, sshAllowedCIDRs []string, cni string) []Rule {
	rules := make([]Rule, 0, len(sshAllowedCIDRs)+12)
	for _, cidr := range sshAllowedCIDRs {
		rules = append(rules, Rule{Protocol: "tcp", FromPort: 22, ToPort: 22, CIDR: cidr})
	}

	for _, ports := range [][2]int{{2379, 2381}, {6443, 6443}, {9345, 9345}, {10250, 10250}, {30000, 32767}} {
		rules = append(rules, Rule{
			Protocol:      "tcp",
			FromPort:      ports[0],
			ToPort:        ports[1],
			RemoteGroupID: securityGroupID,
		})
	}

	switch cni {
	case "", "canal":
		rules = append(rules,
			selfRule(securityGroupID, "udp", 8472, 8472),
			selfRule(securityGroupID, "tcp", 9099, 9099),
		)
	case "flannel":
		rules = append(rules, selfRule(securityGroupID, "udp", 4789, 4789))
	case "calico":
		rules = append(rules,
			selfRule(securityGroupID, "tcp", 179, 179),
			selfRule(securityGroupID, "udp", 4789, 4789),
			selfRule(securityGroupID, "tcp", 5473, 5473),
			selfRule(securityGroupID, "tcp", 9098, 9099),
		)
	}

	return rules
}

func selfRule(securityGroupID, protocol string, fromPort, toPort int) Rule {
	return Rule{Protocol: protocol, FromPort: fromPort, ToPort: toPort, RemoteGroupID: securityGroupID}
}
