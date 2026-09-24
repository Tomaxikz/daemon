package networkpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

const ownerPrefix = "wings-networkstats:"
const maxCompiledAllocationMatches = 16 * 1024

func Names(id string) (network, bridge, table string, err error) {
	u, err := uuid.Parse(id)
	if err != nil || u.String() != id {
		return "", "", "", errors.New("invalid server UUID")
	}

	hex := strings.ReplaceAll(id, "-", "")
	return "wings-nsm-" + id, "nsm" + hex[:12], "wings_nsm_" + hex, nil
}

// Backend uses route Netlink for shaping and fixed commands for nftables/runc.
type Backend struct {
	Run            func(ctx context.Context, namespace *os.File, input, command string, args ...string) ([]byte, error)
	openRoute      func(context.Context, *os.File) (routeClient, error)
	probeNamespace func(context.Context) (*os.File, error)
}

// ValidateEnforcement rejects rates TBF would round and unsupported firewall rules.
func ValidateEnforcement(p Policy, assigned map[string][]int, ceiling Bandwidth) error {
	if err := p.Validate(assigned, ceiling); err != nil {
		return err
	}

	effective := p.Effective(ceiling)
	for _, rate := range []int64{effective.Upload, effective.Download} {
		if rate != 0 && rate%8 != 0 {
			return errors.New("Linux TBF can enforce only whole-byte rates; bandwidth must be divisible by 8 bits per second")
		}
	}

	if !p.Body.Firewall.Enabled {
		return nil
	}

	const validationID = "00000000-0000-4000-8000-000000000000"
	_, err := compileRuleset(validationID, p, assigned, false)
	return err
}

// CheckPolicy validates the requested nft transaction without committing it.
func (b Backend) CheckPolicy(
	ctx context.Context,
	id string,
	p Policy,
	assigned map[string][]int,
	ceiling Bandwidth,
) error {
	if err := ValidateEnforcement(p, assigned, ceiling); err != nil {
		return err
	}
	if !p.Body.Firewall.Enabled {
		_, _, table, err := Names(id)
		if err != nil {
			return err
		}

		_, err = b.tableExists(ctx, id, table)
		return err
	}

	script, err := b.firewallTransaction(ctx, id, p, assigned, false)
	if err != nil {
		return err
	}
	if _, err := b.Run(ctx, nil, script, "nft", "-j", "--check", "-f", "-"); err != nil {
		return fmt.Errorf("unsupported requested nftables policy: %w", err)
	}

	return nil
}

func Linux() Backend {
	return Backend{Run: runCommand}
}

type limitedOutput struct {
	bytes.Buffer
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len()+n > 1024*1024 {
		return 0, errors.New("network command output exceeds 1 MiB")
	}

	return b.Buffer.Write(p)
}

func runCommand(
	ctx context.Context,
	ns *os.File,
	input, name string,
	args ...string,
) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	program, err := exec.LookPath(name)
	if err != nil {
		return nil, err
	}
	if ns != nil {
		args = append([]string{"--net=/proc/self/fd/3", "--", program}, args...)
		program, err = exec.LookPath("nsenter")
		if err != nil {
			return nil, err
		}
	}

	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.WaitDelay = time.Second
	if ns != nil {
		cmd.ExtraFiles = []*os.File{ns}
	}
	cmd.Stdin = strings.NewReader(input)

	var output limitedOutput
	cmd.Stdout = &output
	cmd.Stderr = &output

	err = cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return output.Bytes(), fmt.Errorf("%s failed: %w: %.2048s", name, err, output.String())
	}

	return output.Bytes(), nil
}

type probeCache struct {
	mu      sync.Mutex
	key     string
	expires time.Time
	running chan struct{}
}

var platformProbe probeCache

// Only platform support is cached; callers still verify privileges and live rules.
func (c *probeCache) check(ctx context.Context, key string, probe func(context.Context) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		if c.running != nil {
			done := c.running
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
				continue
			}
		}
		if c.key == key && time.Now().Before(c.expires) {
			c.mu.Unlock()
			return nil
		}
		c.running = make(chan struct{})
		c.mu.Unlock()

		err := probe(ctx)
		if err == nil {
			err = ctx.Err()
		}
		c.mu.Lock()
		c.key, c.expires = "", time.Time{}
		if err == nil {
			c.key, c.expires = key, time.Now().Add(30*time.Second)
		}
		close(c.running)
		c.running = nil
		c.mu.Unlock()
		return err
	}
}

func probeKey() (string, error) {
	var key strings.Builder
	for _, namespace := range []string{"user", "net"} {
		value, err := os.Readlink("/proc/self/ns/" + namespace)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&key, "%s\n", value)
	}
	path, err := exec.LookPath("nft")
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("cannot identify network tool inode")
	}
	fmt.Fprintf(&key, "%s:%d:%d:%d:%d:%v:%d:%d\n", path, stat.Dev, stat.Ino,
		info.Size(), info.ModTime().UnixNano(), info.Mode(), stat.Ctim.Sec, stat.Ctim.Nsec)
	return key.String(), nil
}

// Probe checks privileges on every call and caches expensive platform checks.
func (b Backend) Probe(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("network policies require rootful Wings on the Docker host")
	}
	if err := requireCapability(12, "CAP_NET_ADMIN"); err != nil {
		return err
	}
	if err := requireCapability(21, "CAP_SYS_ADMIN"); err != nil {
		return err
	}
	key, err := probeKey()
	if err != nil {
		return err
	}
	return platformProbe.check(ctx, key, b.probe)
}

func (b Backend) probe(ctx context.Context) error {
	for _, cmd := range [][]string{
		{"nft", "--version"},
		{"nft", "-j", "list", "tables"},
	} {
		if _, err := b.Run(ctx, nil, "", cmd[0], cmd[1:]...); err != nil {
			return err
		}
	}

	probe, assigned := probePolicy()
	script, err := Ruleset(uuid.NewString(), probe, assigned, false)
	if err != nil {
		return err
	}
	if err := b.probeShaping(ctx); err != nil {
		return err
	}

	_, err = b.Run(ctx, nil, script, "nft", "-j", "--check", "-f", "-")
	return err
}

func probePolicy() (Policy, map[string][]int) {
	allocations := []Allocation{
		{
			ID:   1,
			IP:   "192.0.2.1",
			Port: 25565,
		},
		{
			ID:   2,
			IP:   "2001:db8::1",
			Port: 25565,
		},
	}
	groupID := "probe"

	return Policy{
			ResolvedGroups: []ResolvedGroup{
				{
					ID:          groupID,
					Protocol:    "both",
					Allocations: allocations,
				},
			},
			Body: Rules{
				Firewall: Firewall{
					Enabled:         true,
					DefaultInbound:  "drop",
					DefaultOutbound: "drop",
					Chains:          []Chain{},
					Rules: []Rule{
						{
							ID:        "in",
							Name:      "Probe inbound",
							Enabled:   true,
							Direction: "inbound",
							Action:    "allow",
							Protocol:  "both",
							GroupID:   &groupID,
						},
						{
							ID:        "out",
							Name:      "Probe outbound",
							Enabled:   true,
							Direction: "outbound",
							Action:    "drop",
							Protocol:  "both",
							GroupID:   &groupID,
						},
						{
							ID:        "v4",
							Name:      "Probe IPv4",
							Enabled:   true,
							Direction: "inbound",
							Action:    "drop",
							Protocol:  "tcp",
							CIDR:      pointer("198.51.100.0/24"),
						},
						{
							ID:        "v6",
							Name:      "Probe IPv6",
							Enabled:   true,
							Direction: "outbound",
							Action:    "allow",
							Protocol:  "udp",
							CIDR:      pointer("2001:db8:1::/48"),
						},
					},
				},
			},
		}, map[string][]int{
			"192.0.2.1":   {25565},
			"2001:db8::1": {25565},
		}
}

func pointer(value string) *string {
	return &value
}

func requireCapability(bit uint, name string) error {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return fmt.Errorf("cannot verify %s: %w", name, err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}

		value := strings.TrimSpace(strings.TrimPrefix(line, "CapEff:"))
		capabilities, err := strconv.ParseUint(value, 16, 64)
		if err != nil {
			return fmt.Errorf("cannot parse effective Linux capabilities: %w", err)
		}
		if capabilities&(uint64(1)<<bit) == 0 {
			return fmt.Errorf("network policies require effective %s", name)
		}

		return nil
	}

	return errors.New("cannot verify effective Linux capabilities")
}

func (b Backend) tableExists(ctx context.Context, id, table string) (bool, error) {
	out, err := b.Run(ctx, nil, "", "nft", "-j", "list", "table", "bridge", table)
	if err != nil {
		if missingNftTable(out, err, table) {
			return false, nil
		}
		return false, err
	}

	var list struct {
		NFTables []struct {
			Table *struct {
				Family  string `json:"family"`
				Name    string `json:"name"`
				Comment string `json:"comment"`
			} `json:"table"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return false, err
	}

	for _, entry := range list.NFTables {
		if entry.Table == nil || entry.Table.Family != "bridge" || entry.Table.Name != table {
			continue
		}

		if entry.Table.Comment == ownerPrefix+id {
			return true, nil
		}

		return false, errors.New("refusing a firewall table without the NSM ownership marker")
	}

	return false, errors.New("requested firewall table missing from successful nft response")
}

// nft exposes command failures as text, not errno. Recognize only its C-locale
// ENOENT diagnostic for this exact lookup; permission/parse/transport errors fail.
func missingNftTable(output []byte, err error, table string) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return false
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 3 || lines[1] != "list table bridge "+table {
		return false
	}
	missing := lines[0] == "Error: No such file or directory" ||
		strings.HasPrefix(lines[0], "Error: No such file or directory; did you mean ")
	return missing && strings.Contains(lines[2], "^") && strings.Trim(lines[2], " \t^~") == ""
}

type nftDocument struct {
	NFTables []map[string]any `json:"nftables"`
}

// Ruleset emits nft JSON. An NSM allow cannot override Docker or administrator drops.
func Ruleset(id string, p Policy, assigned map[string][]int, gate bool) (string, error) {
	doc, err := compileRuleset(id, p, assigned, gate)
	if err != nil {
		return "", err
	}

	data, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	if len(data) > 1024*1024 {
		return "", errors.New("compiled rules exceed 1 MiB; narrow the policy")
	}

	return string(data), nil
}

func compileRuleset(id string, p Policy, assigned map[string][]int, gate bool) (nftDocument, error) {
	_, bridge, table, err := Names(id)
	if err != nil {
		return nftDocument{}, err
	}

	doc := nftDocument{}
	budget := compileBudget{}
	var rules []Rule
	if !gate && p.Body.Firewall.Enabled {
		rules = p.OrderedRules()
	}
	doc.add("table", map[string]any{
		"family":  "bridge",
		"name":    table,
		"comment": ownerPrefix + id,
	})
	for _, direction := range []string{"outbound", "inbound"} {
		hook, bridgeMeta := "prerouting", "ibrname"
		if direction == "inbound" {
			hook, bridgeMeta = "postrouting", "obrname"
		}

		doc.add("chain", map[string]any{
			"family": "bridge",
			"table":  table,
			"name":   direction,
		})
		doc.add("chain", map[string]any{
			"family": "bridge",
			"table":  table,
			"name":   direction + "_hook",
			"type":   "filter",
			"hook":   hook,
			"prio":   -200,
			"policy": "accept",
		})
		doc.addRule(table, direction+"_hook", []any{
			match(meta(bridgeMeta), bridge),
			map[string]any{"jump": map[string]any{"target": direction}},
		})

		if gate {
			doc.addRule(table, direction, []any{verdict("drop")})
			continue
		}
		if !p.Body.Firewall.Enabled {
			doc.addRule(table, direction, []any{verdict("return")})
			continue
		}

		// ARP and the two local-link NDP messages are the only control-plane
		// exceptions. Existing connections are deliberately re-evaluated.
		doc.addRule(table, direction, []any{match(payload("ether", "type"), "arp"), verdict("return")})

		// Port policy cannot be soundly evaluated on later fragments, so reject
		// them rather than allowing a fragment to bypass an earlier decision.
		doc.addRule(table, direction, []any{
			match(payload("ether", "type"), "ip"),
			match(map[string]any{"&": []any{payload("ip", "frag-off"), 0x3fff}}, 0, "!="),
			verdict("drop"),
		})
		doc.addRule(table, direction, []any{
			match(payload("ether", "type"), "ip6"),
			match(map[string]any{"exthdr": map[string]any{"name": "frag"}}, true),
			verdict("drop"),
		})
		doc.addRule(table, direction, []any{
			match(payload("ether", "type"), "ip6"),
			match(meta("l4proto"), "ipv6-icmp"),
			match(payload("ip6", "hoplimit"), 255),
			match(payload("icmpv6", "type"), set("nd-neighbor-solicit", "nd-neighbor-advert")),
			verdict("return"),
		})

		for _, rule := range rules {
			if rule.Direction != direction {
				continue
			}
			if err := doc.addPolicyRule(table, p, rule, assigned, &budget); err != nil {
				return nftDocument{}, err
			}
		}

		fallback := p.Body.Firewall.DefaultOutbound
		if direction == "inbound" {
			fallback = p.Body.Firewall.DefaultInbound
		}
		if fallback == "allow" {
			doc.addRule(table, direction, []any{
				match(payload("ether", "type"), set("ip", "ip6")),
				verdict("return"),
			})
		}
		doc.addRule(table, direction, []any{verdict("drop")})
	}

	return doc, nil
}

func (d *nftDocument) add(kind string, object map[string]any) {
	d.NFTables = append(d.NFTables, map[string]any{"add": map[string]any{kind: object}})
}

func (d *nftDocument) addRule(table, chain string, expressions []any) {
	d.add("rule", map[string]any{
		"family": "bridge",
		"table":  table,
		"chain":  chain,
		"expr":   expressions,
	})
}

type compileBudget struct {
	allocationMatches int
}

func (d *nftDocument) addPolicyRule(
	table string,
	p Policy,
	rule Rule,
	assigned map[string][]int,
	budget *compileBudget,
) error {
	for _, proto := range []string{"tcp", "udp"} {
		if rule.Protocol != "both" && rule.Protocol != proto {
			continue
		}

		variants := []allocationMatch{{}}
		if rule.GroupID != nil {
			var err error
			variants, err = resolvedMatches(p, *rule.GroupID, proto, assigned)
			if err != nil {
				return fmt.Errorf("rule %q: %w", rule.ID, err)
			}
			if len(variants) == 0 {
				continue
			}

			for _, variant := range variants {
				budget.allocationMatches += len(variant.allocations)
				if budget.allocationMatches > maxCompiledAllocationMatches {
					return fmt.Errorf("compiled allocation matches exceed the %d-entry backend limit", maxCompiledAllocationMatches)
				}
			}

			ports := p.Ports([]string{*rule.GroupID}, proto, assigned)
			if err := ensurePortScope(p, *rule.GroupID, ports, assigned); err != nil {
				return fmt.Errorf("rule %q: %w", rule.ID, err)
			}
		}

		for _, variant := range variants {
			expressions := []any{match(meta("l4proto"), proto)}
			if variant.family != "" {
				expressions = append(expressions, match(payload("ether", "type"), variant.family))
				field := "sport"
				if rule.Direction == "inbound" {
					field = "dport"
				}
				expressions = append(expressions, match(payload(proto, field), intSet(variant.ports)))
				if rule.Direction == "inbound" {
					expressions = append(expressions, match(
						concat(ct("daddr", variant.family, "original"), ct("proto-dst", "", "original")),
						tupleSet(variant.allocations),
					))
				}
			}
			if rule.CIDR != nil {
				cidr, err := Peer(*rule.CIDR)
				if err != nil {
					return err
				}

				family, field := "ip", "daddr"
				if cidr.Addr().Is6() {
					family = "ip6"
				}
				if variant.family != "" && variant.family != family {
					continue
				}
				if rule.Direction == "inbound" {
					field = "saddr"
				}
				if variant.family == "" {
					expressions = append(expressions, match(payload("ether", "type"), family))
				}
				expressions = append(expressions, match(payload(family, field), prefix(cidr.String(), cidr.Bits(), cidr.Addr().BitLen())))
			}

			result := "drop"
			if rule.Action == "allow" {
				result = "return"
			}
			expressions = append(expressions, verdict(result))
			d.addRule(table, rule.Direction, expressions)
		}
	}

	return nil
}

type allocationMatch struct {
	family      string
	ports       []int
	allocations []allocationTuple
}

type allocationTuple struct {
	address string
	port    int
}

func resolvedMatches(
	p Policy,
	groupID, proto string,
	assigned map[string][]int,
) ([]allocationMatch, error) {
	byFamily := map[string]struct {
		ports       map[int]bool
		allocations map[allocationTuple]bool
	}{}
	for _, group := range p.ResolvedGroups {
		if group.ID != groupID || (group.Protocol != "both" && group.Protocol != proto) {
			continue
		}

		for _, allocation := range group.Allocations {
			if !hasAllocation(assigned, allocation) {
				continue
			}

			ip, err := parseIP(allocation.IP)
			if err != nil {
				return nil, err
			}

			family := "ip"
			if ip.Is6() {
				family = "ip6"
			}
			values := byFamily[family]
			if values.ports == nil {
				values.ports = map[int]bool{}
				values.allocations = map[allocationTuple]bool{}
			}
			values.ports[allocation.Port] = true
			values.allocations[allocationTuple{address: ip.String(), port: allocation.Port}] = true
			byFamily[family] = values
		}

		break
	}

	result := make([]allocationMatch, 0, 2)
	for _, family := range []string{"ip", "ip6"} {
		values, found := byFamily[family]
		if !found {
			continue
		}

		item := allocationMatch{family: family}
		for port := range values.ports {
			item.ports = append(item.ports, port)
		}

		for allocation := range values.allocations {
			item.allocations = append(item.allocations, allocation)
		}

		sort.Ints(item.ports)
		sort.Slice(item.allocations, func(i, j int) bool {
			if item.allocations[i].address == item.allocations[j].address {
				return item.allocations[i].port < item.allocations[j].port
			}

			return item.allocations[i].address < item.allocations[j].address
		})
		result = append(result, item)
	}

	return result, nil
}

func ensurePortScope(p Policy, groupID string, ports []int, assigned map[string][]int) error {
	selected := make(map[int]bool, len(ports))
	for _, port := range ports {
		selected[port] = true
	}

	resolved := make(map[string]bool)
	for _, group := range p.ResolvedGroups {
		if group.ID != groupID {
			continue
		}

		for _, allocation := range group.Allocations {
			key, err := allocationKey(allocation.IP, allocation.Port)
			if err != nil {
				return err
			}

			resolved[key] = true
		}

		break
	}

	for host, assignedPorts := range assigned {
		for _, port := range assignedPorts {
			if !selected[port] {
				continue
			}

			key, err := allocationKey(host, port)
			if err != nil {
				return err
			}
			if !resolved[key] {
				return errors.New("cannot preserve allocation IP scope after Docker DNAT; the selected port is also assigned on another IP")
			}
		}
	}

	return nil
}

func match(left, right any, operators ...string) map[string]any {
	op := "=="
	if len(operators) != 0 {
		op = operators[0]
	}
	return map[string]any{"match": map[string]any{
		"op":    op,
		"left":  left,
		"right": right,
	}}
}

func payload(protocol, field string) map[string]any {
	return map[string]any{"payload": map[string]any{"protocol": protocol, "field": field}}
}

func meta(key string) map[string]any {
	return map[string]any{"meta": map[string]any{"key": key}}
}

func ct(key, family, direction string) map[string]any {
	value := map[string]any{"key": key, "dir": direction}
	if family != "" {
		// nft 1.0.x requires "ip daddr"/"ip6 daddr", not a separate family property.
		value["key"] = family + " " + key
	}
	return map[string]any{"ct": value}
}

func concat(values ...any) map[string]any {
	return map[string]any{"concat": values}
}

func verdict(action string) map[string]any {
	return map[string]any{action: nil}
}

func set(values ...any) map[string]any {
	return map[string]any{"set": values}
}

func intSet(values []int) any {
	if len(values) == 1 {
		return values[0]
	}

	items := make([]any, len(values))
	for i, value := range values {
		items[i] = value
	}

	return set(items...)
}

func tupleSet(allocations []allocationTuple) any {
	values := make([]any, len(allocations))
	for i, allocation := range allocations {
		values[i] = concat(allocation.address, allocation.port)
	}
	// nft requires concatenations to be compared through a set even when the
	// requested allocation set contains a single tuple.
	return set(values...)
}

func prefix(cidr string, bits, width int) any {
	if bits == width {
		return strings.SplitN(cidr, "/", 2)[0]
	}

	return map[string]any{
		"prefix": map[string]any{
			"addr": strings.SplitN(cidr, "/", 2)[0],
			"len":  bits,
		},
	}
}

// Firewall checks and atomically replaces only the server's owned nftables table.
func (b Backend) Firewall(
	ctx context.Context,
	id string,
	p Policy,
	assigned map[string][]int,
	gate bool,
) error {
	if !gate && !p.Body.Firewall.Enabled {
		return b.RemoveFirewall(ctx, id)
	}

	script, err := b.firewallTransaction(ctx, id, p, assigned, gate)
	if err != nil {
		return err
	}
	if _, err = b.Run(ctx, nil, script, "nft", "-j", "--check", "-f", "-"); err != nil {
		return fmt.Errorf("unsupported nftables bridge rules: %w", err)
	}

	_, err = b.Run(ctx, nil, script, "nft", "-j", "-f", "-")
	return err
}

func (b Backend) firewallTransaction(
	ctx context.Context,
	id string,
	p Policy,
	assigned map[string][]int,
	gate bool,
) (string, error) {
	_, _, table, err := Names(id)
	if err != nil {
		return "", err
	}

	doc, err := compileRuleset(id, p, assigned, gate)
	if err != nil {
		return "", err
	}

	exists, err := b.tableExists(ctx, id, table)
	if err != nil {
		return "", err
	}
	if exists {
		deleteTable := map[string]any{
			"delete": map[string]any{"table": map[string]any{
				"family": "bridge",
				"name":   table,
			}},
		}
		doc.NFTables = append([]map[string]any{deleteTable}, doc.NFTables...)
	}

	data, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	if len(data) > 1024*1024 {
		return "", errors.New("compiled rules exceed 1 MiB; narrow the policy")
	}

	return string(data), nil
}

// VerifyFirewall checks the entire ordered ruleset, not just ownership comments.
func (b Backend) VerifyFirewall(
	ctx context.Context,
	id string,
	p Policy,
	assigned map[string][]int,
	gate bool,
) error {
	_, _, table, err := Names(id)
	if err != nil {
		return err
	}
	if !gate && !p.Body.Firewall.Enabled {
		exists, err := b.tableExists(ctx, id, table)
		if err != nil {
			return err
		}
		if exists {
			return errors.New("NSM firewall drift: firewall is disabled but its table remains")
		}

		return nil
	}

	expected, err := compileRuleset(id, p, assigned, gate)
	if err != nil {
		return err
	}

	expectedState, err := requestedNftState(expected)
	if err != nil {
		return err
	}

	out, err := b.Run(ctx, nil, "", "nft", "-j", "--stateless", "list", "table", "bridge", table)
	if err != nil {
		return err
	}

	actualState, err := liveNftState(out)
	if err != nil {
		return err
	}

	if !bytes.Equal(expectedState, actualState) {
		return errors.New("NSM firewall drift: live nftables state does not match the requested policy")
	}

	return nil
}

func requestedNftState(doc nftDocument) ([]byte, error) {
	state := make([]map[string]any, 0, len(doc.NFTables))
	for _, command := range doc.NFTables {
		add, ok := command["add"].(map[string]any)
		if !ok {
			continue
		}

		state = append(state, add)
	}

	return canonicalNftState(state)
}

func liveNftState(data []byte) ([]byte, error) {
	var doc nftDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode nftables state: %w", err)
	}

	state := make([]map[string]any, 0, len(doc.NFTables))
	for _, object := range doc.NFTables {
		if _, metadata := object["metainfo"]; metadata {
			continue
		}

		state = append(state, object)
	}

	return canonicalNftState(state)
}

type nftChainState struct {
	Chain map[string]any   `json:"chain"`
	Rules []map[string]any `json:"rules"`
}

type nftTableState struct {
	Tables []map[string]any `json:"tables"`
	Chains []nftChainState  `json:"chains"`
}

func canonicalNftState(state []map[string]any) ([]byte, error) {
	normalizeNft(state)
	grouped := nftTableState{Tables: []map[string]any{}, Chains: []nftChainState{}}
	chains := make(map[string]int)
	for _, object := range state {
		if len(object) != 1 {
			return nil, errors.New("unexpected nftables object")
		}
		if rule, ok := object["rule"].(map[string]any); ok {
			normalizeNftRule(rule)
		}
		if table, ok := object["table"].(map[string]any); ok {
			grouped.Tables = append(grouped.Tables, table)
			continue
		}
		if chain, ok := object["chain"].(map[string]any); ok {
			key, err := nftObjectKey(chain)
			if err != nil {
				return nil, err
			}
			if _, exists := chains[key]; exists {
				return nil, errors.New("duplicate nftables chain")
			}

			chains[key] = len(grouped.Chains)
			grouped.Chains = append(grouped.Chains, nftChainState{Chain: chain, Rules: []map[string]any{}})
			continue
		}
		if rule, ok := object["rule"].(map[string]any); ok {
			key, err := nftObjectKey(rule)
			if err != nil {
				return nil, err
			}

			index, found := chains[key]
			if !found {
				return nil, errors.New("nftables rule references an unknown chain")
			}

			grouped.Chains[index].Rules = append(grouped.Chains[index].Rules, rule)
			continue
		}

		return nil, errors.New("unexpected nftables object kind")
	}

	sort.Slice(grouped.Chains, func(i, j int) bool {
		left, _ := nftObjectKey(grouped.Chains[i].Chain)
		right, _ := nftObjectKey(grouped.Chains[j].Chain)
		return left < right
	})

	return json.Marshal(grouped)
}

func normalizeNftRule(rule map[string]any) {
	expressions, ok := rule["expr"].([]any)
	if !ok {
		return
	}

	transportPayload := map[string]bool{}
	networkPayload := map[string]bool{}
	for _, expression := range expressions {
		if condition, ok := nftMatch(expression); ok && condition["op"] == "==" && condition["right"] == true {
			if left, ok := condition["left"].(map[string]any); ok {
				if header, ok := left["exthdr"].(map[string]any); ok && len(header) == 1 && header["name"] == "frag" {
					networkPayload["ip6"] = true
				}
			}
		}
		protocol, _, ok := nftPayloadMatch(expression)
		if !ok {
			continue
		}
		if protocol == "tcp" || protocol == "udp" {
			transportPayload[protocol] = true
		}
		if protocol == "icmpv6" {
			transportPayload["ipv6-icmp"] = true
		}
		if protocol == "ip" || protocol == "ip6" {
			networkPayload[protocol] = true
		}
	}

	normalized := expressions[:0]
	for _, expression := range expressions {
		if protocol, ok := nftMetaProtocolMatch(expression); ok && transportPayload[protocol] {
			continue
		}
		if protocol, field, ok := nftPayloadMatch(expression); ok && protocol == "ether" && field == "type" {
			match, _ := nftMatch(expression)
			family, familyOK := nftMatchRight(expression).(string)
			if match["op"] == "==" && familyOK && networkPayload[family] {
				continue
			}
		}
		normalized = append(normalized, expression)
	}

	rule["expr"] = normalized
}

func nftMetaProtocolMatch(expression any) (string, bool) {
	match, ok := nftMatch(expression)
	if !ok || match["op"] != "==" {
		return "", false
	}

	left, ok := match["left"].(map[string]any)
	if !ok {
		return "", false
	}

	meta, ok := left["meta"].(map[string]any)
	if !ok || meta["key"] != "l4proto" {
		return "", false
	}

	protocol, ok := match["right"].(string)
	return protocol, ok
}

func nftPayloadMatch(expression any) (protocol, field string, ok bool) {
	match, ok := nftMatch(expression)
	if !ok || match["op"] != "==" && match["op"] != "!=" {
		return "", "", false
	}

	left, ok := match["left"].(map[string]any)
	if !ok {
		return "", "", false
	}
	if operands, masked := left["&"].([]any); masked && len(operands) == 2 {
		left, ok = operands[0].(map[string]any)
		if !ok {
			return "", "", false
		}
	}
	payload, ok := left["payload"].(map[string]any)
	if !ok {
		return "", "", false
	}

	protocol, protocolOK := payload["protocol"].(string)
	field, fieldOK := payload["field"].(string)
	return protocol, field, protocolOK && fieldOK
}

func nftMatch(expression any) (map[string]any, bool) {
	statement, ok := expression.(map[string]any)
	if !ok {
		return nil, false
	}

	match, ok := statement["match"].(map[string]any)
	return match, ok
}

func nftMatchRight(expression any) any {
	match, ok := nftMatch(expression)
	if !ok {
		return nil
	}

	return match["right"]
}

func nftObjectKey(object map[string]any) (string, error) {
	family, familyOK := object["family"].(string)
	table, tableOK := object["table"].(string)
	name, nameOK := object["name"].(string)
	if !nameOK {
		name, nameOK = object["chain"].(string)
	}
	if !familyOK || !tableOK || !nameOK {
		return "", errors.New("incomplete nftables chain identity")
	}

	return family + "\x00" + table + "\x00" + name, nil
}

func normalizeNft(value any) {
	switch current := value.(type) {
	case []map[string]any:
		for _, item := range current {
			normalizeNft(item)
		}
	case []any:
		for _, item := range current {
			normalizeNft(item)
		}
	case map[string]any:
		delete(current, "handle")
		for _, item := range current {
			normalizeNft(item)
		}
		if items, ok := current["set"].([]any); ok && len(current) == 1 && len(items) > 1 {
			type entry struct {
				value any
				key   []byte
			}
			entries := make([]entry, len(items))
			for i, item := range items {
				key, _ := json.Marshal(item)
				entries[i] = entry{value: item, key: key}
			}

			slices.SortStableFunc(entries, func(a, b entry) int {
				return bytes.Compare(a.key, b.key)
			})

			for i, entry := range entries {
				items[i] = entry.value
			}
		}
	}
}

// RemoveFirewall verifies ownership before deleting a server's nftables table.
func (b Backend) RemoveFirewall(ctx context.Context, id string) error {
	_, _, table, err := Names(id)
	if err != nil {
		return err
	}

	exists, err := b.tableExists(ctx, id, table)
	if err != nil || !exists {
		return err
	}

	_, err = b.Run(ctx, nil, "delete table bridge "+table+"\n", "nft", "-f", "-")
	return err
}

type Link struct {
	Index   int    `json:"ifindex"`
	Peer    int    `json:"link_index"`
	Name    string `json:"ifname"`
	Address string `json:"address"`
	MTU     int    `json:"mtu"`
	master  int
}

func (b Backend) Link(ctx context.Context, ns *os.File, device string) (Link, error) {
	if device == "" || len(device) > 15 || strings.ContainsAny(device, "\x00/: \t\r\n") {
		return Link{}, errors.New("invalid interface name")
	}
	client, err := b.route(ctx, ns)
	if err != nil {
		return Link{}, err
	}
	defer client.close()
	return routeLink(ctx, client, device)
}

func (b Backend) Links(ctx context.Context, ns *os.File, bridge string) ([]Link, error) {
	client, err := b.route(ctx, ns)
	if err != nil {
		return nil, err
	}
	defer client.close()
	data := make([]byte, 16)
	master := 0
	if bridge != "" {
		link, err := routeLink(ctx, client, bridge)
		if err != nil {
			return nil, err
		}
		master = link.Index
		attributes, err := routeAttributes([]netlink.Attribute{{Type: unix.IFLA_MASTER, Data: routeU32(uint32(master))}})
		if err != nil {
			return nil, err
		}
		data = append(data, attributes...)
	}
	replies, err := client.request(ctx, unix.RTM_GETLINK, unix.NLM_F_DUMP, data)
	if err != nil {
		return nil, err
	}
	links := make([]Link, 0, len(replies))
	for _, reply := range replies {
		link, err := decodeRouteLink(reply)
		if err != nil {
			return nil, err
		}
		if master == 0 || link.master == master {
			links = append(links, link)
		}
	}
	return links, nil
}

// OpenNamespace pins and verifies the container's Docker-owned network namespace.
func OpenNamespace(pid int, sandbox string) (*os.File, error) {
	if pid <= 1 || !strings.HasPrefix(sandbox, "/var/run/docker/netns/") || strings.Contains(sandbox, "..") {
		return nil, errors.New("unsupported Docker network namespace")
	}

	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/ns/net")
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	local, err := os.Stat("/proc/self/ns/net")
	if err != nil || os.SameFile(info, local) {
		_ = f.Close()
		return nil, errors.New("refusing the host network namespace")
	}

	docker, err := os.Stat(sandbox)
	if err != nil || !os.SameFile(info, docker) {
		_ = f.Close()
		return nil, errors.New("docker PID and sandbox namespace do not match")
	}

	return f, nil
}

var _ io.Writer = (*limitedOutput)(nil)
