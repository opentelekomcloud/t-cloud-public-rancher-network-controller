package cloud

import "testing"

func TestRKE2RulesAreProtocolAwareAndSelfReferencing(t *testing.T) {
	rules := RKE2Rules("sg-1", []string{"203.0.113.5/32"}, "canal")

	want := []Rule{
		{Protocol: "tcp", FromPort: 22, ToPort: 22, CIDR: "203.0.113.5/32"},
		{Protocol: "tcp", FromPort: 9345, ToPort: 9345, RemoteGroupID: "sg-1"},
		{Protocol: "udp", FromPort: 8472, ToPort: 8472, RemoteGroupID: "sg-1"},
	}
	for _, expected := range want {
		if !containsRule(rules, expected) {
			t.Fatalf("missing rule %#v in %#v", expected, rules)
		}
	}
}

func TestRKE2RulesDoNotAssumeVXLANForCilium(t *testing.T) {
	for _, rule := range RKE2Rules("sg-1", nil, "cilium") {
		if rule.Protocol == "udp" && rule.FromPort == 8472 {
			t.Fatalf("unexpected VXLAN rule for cilium: %#v", rule)
		}
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
