package networkpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const firewallTestID = "00000000-0000-4000-8000-000000000001"

func firewallPolicy() Policy {
	return Policy{Body: Rules{Firewall: Firewall{
		Enabled: true, DefaultInbound: "drop", DefaultOutbound: "drop",
	}}}
}

func firewallRules(t *testing.T, p Policy, assigned map[string][]int, chain string) [][]any {
	t.Helper()
	doc, err := compileRuleset(firewallTestID, p, assigned, false)
	require.NoError(t, err)
	var rules [][]any
	for _, command := range doc.NFTables {
		object := command["add"].(map[string]any)
		rule, ok := object["rule"].(map[string]any)
		if ok && rule["chain"] == chain {
			rules = append(rules, rule["expr"].([]any))
		}
	}
	return rules
}

func hasExpression(expressions []any, expected any) bool {
	return slices.ContainsFunc(expressions, func(expression any) bool {
		return reflect.DeepEqual(expression, expected)
	})
}

func firewallCT(key string) map[string]any {
	return map[string]any{"ct": map[string]any{"key": key}}
}

func TestFirewallExplicitRulesPrecedeStatefulReplies(t *testing.T) {
	for _, direction := range []string{"inbound", "outbound"} {
		t.Run(direction, func(t *testing.T) {
			p := firewallPolicy()
			p.Body.Firewall.Chains = []Chain{{ID: "first", Enabled: true}}
			p.Body.Firewall.Rules = []Rule{
				{ID: "later", Enabled: true, Direction: direction, Action: "drop", Protocol: "tcp"},
				{ID: "first", Enabled: true, ChainID: pointer("first"), Direction: direction, Action: "allow", Protocol: "tcp"},
				{ID: "disabled", Direction: direction, Action: "reject", Protocol: "tcp"},
			}
			rules := firewallRules(t, p, nil, direction)
			first := -1
			for i, expressions := range rules {
				if reflect.DeepEqual(expressions, []any{match(meta("l4proto"), "tcp"), verdict("return")}) {
					first = i
					break
				}
			}
			require.NotEqual(t, -1, first)
			require.Equal(t, []any{match(meta("l4proto"), "tcp"), verdict("drop")}, rules[first+1])

			replies := 0
			for i, expressions := range rules {
				if hasExpression(expressions, match(firewallCT("state"), "related")) {
					require.Greater(t, i, first+1, "related errors must follow explicit policy decisions")
				}
				if !hasExpression(expressions, match(firewallCT("state"), "established")) {
					continue
				}
				replies++
				require.Greater(t, i, first+1, "explicit denies must also apply to replies")
				require.True(t, hasExpression(expressions, match(firewallCT("direction"), "reply")))
				require.True(t, hasExpression(expressions, match(map[string]any{"&": []any{firewallCT("status"), "confirmed"}}, "confirmed")), "status is a bitset; seen-reply/DNAT must not hide confirmed")
				require.True(t, hasExpression(expressions, match(meta("l4proto"), set("tcp", "udp"))))
				require.Equal(t, map[string]any{"goto": map[string]any{"target": opposite(direction) + "_origin"}}, expressions[len(expressions)-1])
			}
			require.Positive(t, replies, "default-drop must preserve confirmed TCP/UDP replies")
			require.Equal(t, []any{verdict("drop")}, rules[len(rules)-1])
			origin := firewallRules(t, p, nil, direction+"_origin")
			require.Equal(t, []any{match(ct("protocol", "", "original"), "tcp"), verdict("return")}, origin[0])
			require.Equal(t, []any{match(ct("protocol", "", "original"), "tcp"), verdict("drop")}, origin[1])
			require.Equal(t, []any{verdict("drop")}, origin[len(origin)-1], "tracking a denied original must never authorize its reply")
		})
	}
}

func TestFirewallRelatedICMPIsLimitedToErrors(t *testing.T) {
	p := firewallPolicy()
	for _, direction := range []string{"inbound", "outbound"} {
		rules := firewallRules(t, p, nil, direction)
		for _, family := range []struct {
			ether, protocol, payload string
			types                    []any
		}{
			{"ip", "icmp", "icmp", []any{"destination-unreachable", "time-exceeded", "parameter-problem"}},
			{"ip6", "ipv6-icmp", "icmpv6", []any{"destination-unreachable", "packet-too-big", "time-exceeded", "parameter-problem"}},
		} {
			for _, ctDirection := range []string{"original", "reply"} {
				target := direction
				if ctDirection == "reply" {
					target = opposite(direction)
				}
				expected := []any{
					match(payload("ether", "type"), family.ether),
					match(meta("l4proto"), family.protocol),
					match(payload(family.payload, "type"), set(family.types...)),
					match(firewallCT("state"), "related"),
					match(firewallCT("direction"), ctDirection),
					map[string]any{"goto": map[string]any{"target": target + "_origin"}},
				}
				require.Contains(t, rules, expected, "%s %s %s", direction, family.ether, ctDirection)
			}
		}
		for _, expressions := range rules {
			if hasExpression(expressions, match(firewallCT("state"), "related")) {
				require.Len(t, expressions, 6, "first-packet PMTU errors can be related before confirmation")
				require.False(t, hasExpression(expressions, matchCTStatus("confirmed")), "related errors must work before conntrack confirms the original packet")
			}
		}
	}

	doc, err := compileRuleset(firewallTestID, p, nil, true)
	require.NoError(t, err)
	script, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NotContains(t, string(script), "related", "the startup gate must not admit control traffic")
}

func TestFirewallAllocationScopeAcrossPublishedAndDirectPaths(t *testing.T) {
	p := firewallPolicy()
	p.ResolvedGroups = []ResolvedGroup{{ID: "selected", Protocol: "udp", Allocations: []Allocation{{ID: 1, IP: "192.0.2.10", Port: 25000}}}}
	p.Body.Firewall.Rules = []Rule{{ID: "scoped", Enabled: true, Direction: "inbound", Action: "allow", Protocol: "udp", GroupID: pointer("selected")}}
	for _, scenario := range []struct {
		name, otherIP string
		directVerdict string
	}{
		{"one allocation", "", "return"},
		{"same port different IPv4", "192.0.2.11", "drop"},
		{"same port different family", "2001:db8::11", "return"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			assigned := map[string][]int{"192.0.2.10": {25000}}
			if scenario.otherIP != "" {
				assigned[scenario.otherIP] = []int{25000}
			}
			for _, chain := range []string{"inbound", "inbound_origin"} {
				var scoped [][]any
				for _, expressions := range firewallRules(t, p, assigned, chain) {
					protocol := any(meta("l4proto"))
					if chain == "inbound_origin" {
						protocol = ct("protocol", "", "original")
					}
					if hasExpression(expressions, match(protocol, "udp")) {
						scoped = append(scoped, expressions)
					}
				}
				require.NotEmpty(t, scoped)
				require.True(t, hasExpression(scoped[0], match(map[string]any{"&": []any{firewallCT("status"), "dnat"}}, "dnat")))
				require.True(t, hasExpression(scoped[0], match(
					concat(ct("daddr", "ip", "original"), ct("proto-dst", "", "original")),
					tupleSet([]allocationTuple{{address: "192.0.2.10", port: 25000}}),
				)))
				for _, expressions := range scoped[1:] {
					require.Equal(t, verdict(scenario.directVerdict), expressions[len(expressions)-1], "direct/reply ambiguity must fail closed at the scoped rule")
				}
			}
			outbound := p
			outbound.Body.Firewall.Rules = slices.Clone(p.Body.Firewall.Rules)
			outbound.Body.Firewall.Rules[0].Direction = "outbound"
			_, err := compileRuleset(firewallTestID, outbound, assigned, false)
			if scenario.directVerdict == "drop" {
				require.ErrorContains(t, err, "outbound allocation IP scope")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestFirewallNormalizationPreservesOriginalTupleDirection(t *testing.T) {
	require.Equal(t, map[string]any{"ct": map[string]any{"key": "protocol"}}, ct("protocol", "", "original"), "nft serializes protocol without a direction")
	for _, tuple := range []any{
		ct("saddr", "ip", "original"),
		concat(ct("daddr", "ip", "original"), ct("proto-dst", "", "original")),
	} {
		guarded := map[string]any{"expr": []any{match(payload("ether", "type"), "ip"), match(tuple, "192.0.2.1"), verdict("return")}}
		kernel := map[string]any{"expr": []any{match(tuple, "192.0.2.1"), verdict("return")}}
		normalizeNftRule(guarded)
		normalizeNftRule(kernel)
		require.Equal(t, kernel, guarded, "typed ct address/tuple implies its family")
		data, err := json.Marshal(kernel)
		require.NoError(t, err)
		var changed map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.Replace(string(data), `"original"`, `"reply"`, 1)), &changed))
		normalizeNftRule(changed)
		require.NotEqual(t, guarded, changed, "an altered conntrack tuple direction must remain drift")
	}
}

func TestFirewallAtomicReplacementAndOwnership(t *testing.T) {
	p := firewallPolicy()
	_, _, table, err := Names(firewallTestID)
	require.NoError(t, err)
	owned := `{"nftables":[{"table":{"family":"bridge","name":"` + table + `","comment":"` + ownerPrefix + firewallTestID + `"}}]}`
	failure := errors.New("injected nft failure")
	for _, scenario := range []struct {
		name, live string
		failAt     int
		calls      int
	}{
		{"foreign ownership", strings.Replace(owned, ownerPrefix, "administrator:", 1), 0, 1},
		{"malformed lookup", `{`, 0, 1},
		{"lookup failure", owned, 1, 1},
		{"check failure", owned, 2, 2},
		{"commit failure", owned, 3, 3},
		{"successful replacement", owned, 0, 3},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			calls := 0
			var checked string
			backend := Backend{Run: func(_ context.Context, _ *os.File, input, command string, args ...string) ([]byte, error) {
				calls++
				require.Equal(t, "nft", command)
				if calls == 1 {
					require.Empty(t, input)
					require.Equal(t, []string{"-j", "list", "table", "bridge", table}, args)
				} else {
					var transaction nftDocument
					require.NoError(t, json.Unmarshal([]byte(input), &transaction))
					require.Equal(t, map[string]any{"delete": map[string]any{"table": map[string]any{"family": "bridge", "name": table}}}, transaction.NFTables[0])
					require.Contains(t, transaction.NFTables[1], "add")
					if calls == 2 {
						require.Equal(t, []string{"-j", "--check", "-f", "-"}, args)
						checked = input
					} else {
						require.Equal(t, []string{"-j", "-f", "-"}, args)
						require.Equal(t, checked, input, "commit the exact checked atomic replacement")
					}
				}
				if calls == scenario.failAt {
					return nil, failure
				}
				return []byte(scenario.live), nil
			}}
			err := backend.Firewall(context.Background(), firewallTestID, p, nil, false)
			if scenario.name == "successful replacement" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				if scenario.failAt != 0 {
					require.ErrorIs(t, err, failure)
				}
			}
			require.Equal(t, scenario.calls, calls, "stop before subsequent writes after any failure")
		})
	}
}

func TestFirewallReadbackDetectsReplyAndICMPDrift(t *testing.T) {
	p := firewallPolicy()
	doc, err := compileRuleset(firewallTestID, p, nil, false)
	require.NoError(t, err)
	live := nftDocument{}
	for _, command := range doc.NFTables {
		live.NFTables = append(live.NFTables, command["add"].(map[string]any))
	}
	data, err := json.Marshal(live)
	require.NoError(t, err)
	for _, drift := range []struct{ from, to string }{
		{`"reply"`, `"original"`},
		{`"related"`, `"established"`},
		{`"confirmed"`, `"dnat"`},
	} {
		t.Run(drift.from, func(t *testing.T) {
			require.Contains(t, string(data), drift.from)
			readback := strings.Replace(string(data), drift.from, drift.to, 1)
			backend := Backend{Run: func(context.Context, *os.File, string, string, ...string) ([]byte, error) {
				return []byte(readback), nil
			}}
			require.ErrorContains(t, backend.VerifyFirewall(context.Background(), firewallTestID, p, nil, false), "firewall drift")
		})
	}
}
