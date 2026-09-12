package cloud

import "testing"

func TestRKE2RulesAreProtocolAwareAndSelfReferencing(t *testing.T) {
	rules := RKE2Rules("sg-1", []string{"203.0.113.5/32"}, "canal")

	want := []Rule{
		{Protocol: "tcp", FromPort: 22, ToPort: 22, CIDR: "203.0.113.5/32"},
		{Protocol: "tcp", FromPort: 9345, ToPort: 9345, RemoteGroupID: "sg-1"},
		{Protocol: "udp", FromPort: 8472, ToPort: 8472, RemoteGroupID: "sg-1"},
		{Protocol: "tcp", FromPort: 30000, ToPort: 32767, RemoteGroupID: "sg-1"},
	}
	for _, expected := range want {
		if !containsRule(rules, expected) {
			t.Fatalf("missing rule %#v in %#v", expected, rules)
		}
	}
}

func TestRKE2RulesSupportCalico(t *testing.T) {
	rules := RKE2Rules("sg-1", nil, "calico")
	want := []Rule{
		{Protocol: "tcp", FromPort: 179, ToPort: 179, RemoteGroupID: "sg-1"},
		{Protocol: "udp", FromPort: 4789, ToPort: 4789, RemoteGroupID: "sg-1"},
		{Protocol: "tcp", FromPort: 5473, ToPort: 5473, RemoteGroupID: "sg-1"},
		{Protocol: "tcp", FromPort: 9098, ToPort: 9099, RemoteGroupID: "sg-1"},
	}
	for _, expected := range want {
		if !containsRule(rules, expected) {
			t.Fatalf("missing Calico rule %#v in %#v", expected, rules)
		}
	}
}

func TestRKE2RulesUseFlannelVXLANPort(t *testing.T) {
	rules := RKE2Rules("sg-1", nil, "flannel")

	if !containsRule(rules, Rule{Protocol: "udp", FromPort: 4789, ToPort: 4789, RemoteGroupID: "sg-1"}) {
		t.Fatalf("missing Flannel VXLAN rule in %#v", rules)
	}
	if containsRule(rules, Rule{Protocol: "udp", FromPort: 8472, ToPort: 8472, RemoteGroupID: "sg-1"}) {
		t.Fatalf("Flannel must not use the Canal VXLAN port: %#v", rules)
	}
}

func TestRKE2RulesDoNotAssumeVXLANForCilium(t *testing.T) {
	for _, rule := range RKE2Rules("sg-1", nil, "cilium") {
		if rule.Protocol == "udp" && rule.FromPort == 8472 {
			t.Fatalf("unexpected VXLAN rule for cilium: %#v", rule)
		}
	}
}

func TestObsoleteSSHRulesOnlyReturnsRemovedCIDRs(t *testing.T) {
	rules := ObsoleteSSHRules(
		[]string{"203.0.113.5/32", "198.51.100.0/24"},
		[]string{"192.0.2.10/32", "198.51.100.0/24"},
	)
	want := []Rule{{Protocol: "tcp", FromPort: 22, ToPort: 22, CIDR: "203.0.113.5/32"}}
	if len(rules) != len(want) || rules[0] != want[0] {
		t.Fatalf("unexpected obsolete SSH rules: %#v", rules)
	}
}

func containsRule(rules []Rule, expected Rule) bool {
	for _, rule := range rules {
		if rule == expected {
			return true
		}
	}
	return false
}
