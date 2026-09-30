package networkpolicy

import "slices"

type policyAllocationMatch struct {
	allocationMatch
	path string
	drop bool
}

func policyAllocationMatches(
	rule Rule,
	variants []allocationMatch,
	assigned map[string][]int,
	original bool,
) ([]policyAllocationMatch, error) {
	var matches []policyAllocationMatch
	for _, variant := range variants {
		if rule.Direction != "inbound" || rule.GroupID == nil && (rule.CIDR == nil || original) {
			matches = append(matches, policyAllocationMatch{allocationMatch: variant})
			continue
		}

		matches = append(matches, policyAllocationMatch{allocationMatch: variant, path: "dnat"})
		if rule.GroupID == nil {
			matches = append(matches, policyAllocationMatch{allocationMatch: variant, path: "direct"})
			matches = append(matches, policyAllocationMatch{allocationMatch: variant, path: "reply"})
			continue
		}

		ports, ambiguous, err := directAllocationPorts(variant, assigned)
		if err != nil {
			return nil, err
		}
		paths := []string{"direct"}
		if !original {
			paths = append(paths, "reply")
		}
		for _, path := range paths {
			if len(ports) != 0 {
				direct := variant
				direct.ports = ports
				matches = append(matches, policyAllocationMatch{allocationMatch: direct, path: path})
			}
			if len(ambiguous) != 0 {
				// A private destination or reply cannot identify which public IP
				// owns a shared port. Fail closed at this rule's ordered position.
				direct := variant
				direct.ports = ambiguous
				matches = append(matches, policyAllocationMatch{allocationMatch: direct, path: path, drop: true})
			}
		}
	}
	return matches, nil
}

func directAllocationPorts(variant allocationMatch, assigned map[string][]int) (ports, ambiguous []int, err error) {
	selected := make(map[allocationTuple]bool, len(variant.allocations))
	for _, allocation := range variant.allocations {
		selected[allocation] = true
	}
	shared := make(map[int]bool)
	for host, assignedPorts := range assigned {
		ip, err := parseIP(host)
		if err != nil {
			return nil, nil, err
		}
		family := "ip"
		if ip.Is6() {
			family = "ip6"
		}
		if family != variant.family {
			continue
		}
		for _, port := range assignedPorts {
			if slices.Contains(variant.ports, port) && !selected[allocationTuple{address: ip.String(), port: port}] {
				shared[port] = true
			}
		}
	}
	for _, port := range variant.ports {
		if shared[port] {
			ambiguous = append(ambiguous, port)
		} else {
			ports = append(ports, port)
		}
	}
	return ports, ambiguous, nil
}
