package networkpolicy

func (d *nftDocument) addEstablishedReplies(table, direction string) {
	// Conntrack may confirm routed traffic before bridge policy evaluates it.
	// Recheck the original flow against its current policy instead of treating
	// tracking alone as proof that NSM admitted the initial packet.
	d.addRule(table, direction, []any{
		match(payload("ether", "type"), set("ip", "ip6")),
		match(meta("l4proto"), set("tcp", "udp")),
		match(map[string]any{"ct": map[string]any{"key": "state"}}, "established"),
		match(map[string]any{"ct": map[string]any{"key": "direction"}}, "reply"),
		matchCTStatus("confirmed"),
		map[string]any{"goto": map[string]any{"target": opposite(direction) + "_origin"}},
	})
}

func opposite(direction string) string {
	if direction == "inbound" {
		return "outbound"
	}
	return "inbound"
}
