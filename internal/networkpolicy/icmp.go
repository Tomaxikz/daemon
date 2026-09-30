package networkpolicy

// addRelatedICMPRules preserves network errors such as PMTU notifications only
// when conntrack validates the quoted packet and its original flow passes the
// current policy. The outer protocol/type checks exclude echo, redirects, and
// helper-created TCP/UDP connections in the related state.
func (d *nftDocument) addRelatedICMPRules(table, chain string) {
	for _, direction := range []string{"original", "reply"} {
		origin := chain
		if direction == "reply" {
			origin = opposite(chain)
		}
		target := map[string]any{"goto": map[string]any{"target": origin + "_origin"}}
		d.addRule(table, chain, []any{
			match(payload("ether", "type"), "ip"),
			match(meta("l4proto"), "icmp"),
			match(payload("icmp", "type"), set("destination-unreachable", "time-exceeded", "parameter-problem")),
			match(map[string]any{"ct": map[string]any{"key": "state"}}, "related"),
			match(map[string]any{"ct": map[string]any{"key": "direction"}}, direction),
			target,
		})
		d.addRule(table, chain, []any{
			match(payload("ether", "type"), "ip6"),
			match(meta("l4proto"), "ipv6-icmp"),
			match(payload("icmpv6", "type"), set("destination-unreachable", "packet-too-big", "time-exceeded", "parameter-problem")),
			match(map[string]any{"ct": map[string]any{"key": "state"}}, "related"),
			match(map[string]any{"ct": map[string]any{"key": "direction"}}, direction),
			target,
		})
	}
}
