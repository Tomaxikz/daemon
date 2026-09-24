// Package networkpolicy contains NSM's allocation-bound policy and Linux backend.
package networkpolicy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"unicode/utf8"
)

// MaxBody is the maximum size of a policy request or persisted document.
const MaxBody = 1 << 20

const maxRate int64 = 1_000_000_000_000

var (
	idPattern   = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	groupName   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,47}$`)
)

type Capabilities struct {
	APIVersion int  `json:"api_version"`
	Firewall   bool `json:"firewall"`
	Bandwidth  bool `json:"bandwidth"`
}

type Allocation struct {
	ID   int64  `json:"id"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

type ResolvedGroup struct {
	ID          string       `json:"id"`
	Protocol    string       `json:"protocol"`
	Allocations []Allocation `json:"allocations"`
}

// Group selects allocations without creating or publishing ports.
type Group struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Mode          string  `json:"mode"`
	Protocol      string  `json:"protocol"`
	AllocationIDs []int64 `json:"allocation_ids"`
	PortStart     int     `json:"port_start"`
	PortEnd       int     `json:"port_end"`
	IP            *string `json:"ip"`
}

// Chain is an ordered rule group. It does not represent an executable jump.
type Chain struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// Rule filters traffic in one direction; its evaluated position determines precedence.
type Rule struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Enabled   bool    `json:"enabled"`
	ChainID   *string `json:"chain_id"`
	Direction string  `json:"direction"`
	Action    string  `json:"action"`
	Protocol  string  `json:"protocol"`
	GroupID   *string `json:"group_id"`
	CIDR      *string `json:"cidr"`
}

type Firewall struct {
	Enabled         bool    `json:"enabled"`
	DefaultInbound  string  `json:"default_inbound"`
	DefaultOutbound string  `json:"default_outbound"`
	Chains          []Chain `json:"chains"`
	Rules           []Rule  `json:"rules"`
}

// Bandwidth holds independent upload and download limits in decimal bits per second.
type Bandwidth struct {
	Upload   int64 `json:"upload_bps"`
	Download int64 `json:"download_bps"`
}

type Rules struct {
	Groups    []Group   `json:"groups"`
	Firewall  Firewall  `json:"firewall"`
	Bandwidth Bandwidth `json:"bandwidth"`
}

type Policy struct {
	APIVersion     int             `json:"api_version"`
	Revision       uint64          `json:"revision"`
	PolicyHash     string          `json:"policy_hash"`
	Allocations    []Allocation    `json:"allocations"`
	ResolvedGroups []ResolvedGroup `json:"resolved_groups"`
	Body           Rules           `json:"policy"`
}

// Status distinguishes desired intent from the last confirmed application.
type Status struct {
	APIVersion      int     `json:"api_version"`
	State           string  `json:"state"`
	AppliedRevision uint64  `json:"applied_revision"`
	AppliedHash     *string `json:"applied_hash"`

	Desired            Policy    `json:"-"`
	EffectiveBandwidth Bandwidth `json:"-"`
	Error              string    `json:"-"`
}

func Empty() Policy {
	return Policy{}
}

func (p Policy) Required(ceiling Bandwidth) bool {
	return p.Body.Firewall.Enabled ||
		p.Body.Bandwidth.Upload > 0 ||
		p.Body.Bandwidth.Download > 0 ||
		ceiling.Upload > 0 ||
		ceiling.Download > 0
}

// Effective applies administrator ceilings independently of firewall state.
func (p Policy) Effective(ceiling Bandwidth) Bandwidth {
	b := p.Body.Bandwidth
	if ceiling.Upload > 0 && (b.Upload == 0 || b.Upload > ceiling.Upload) {
		b.Upload = ceiling.Upload
	}
	if ceiling.Download > 0 && (b.Download == 0 || b.Download > ceiling.Download) {
		b.Download = ceiling.Download
	}

	return b
}

func (p Policy) SameRules(other Policy) bool {
	return reflect.DeepEqual(p.Body, other.Body)
}

func (p Policy) OrderedRules() []Rule {
	ordered := make([]Rule, 0, len(p.Body.Firewall.Rules))
	for _, chain := range p.Body.Firewall.Chains {
		if !chain.Enabled {
			continue
		}

		for _, rule := range p.Body.Firewall.Rules {
			if rule.Enabled && rule.ChainID != nil && *rule.ChainID == chain.ID {
				ordered = append(ordered, rule)
			}
		}
	}

	for _, rule := range p.Body.Firewall.Rules {
		if rule.Enabled && rule.ChainID == nil {
			ordered = append(ordered, rule)
		}
	}

	return ordered
}

func validProtocol(value string) bool {
	return value == "tcp" || value == "udp" || value == "both"
}

func validAction(value string) bool {
	return value == "allow" || value == "drop"
}

func validRate(rate int64) bool {
	return rate >= 0 && rate <= maxRate
}

func validateBandwidth(rate, ceiling int64) error {
	if !validRate(rate) || !validRate(ceiling) {
		return errors.New("bandwidth must be an integer from 0 through 1000000000000 bits per second")
	}
	if ceiling > 0 && rate > ceiling {
		return errors.New("bandwidth exceeds administrator ceiling")
	}

	return nil
}

func validName(value string) bool {
	return value != "" && utf8.RuneCountInString(value) <= 80
}

func parseIP(value string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(value)
	if err != nil || ip.Zone() != "" || ip.Is4In6() {
		return netip.Addr{}, errors.New("address must be an IPv4 or IPv6 address without a zone")
	}

	return ip, nil
}

// Validate binds submitted allocations to the authoritative server address/port set.
func (p *Policy) Validate(assigned map[string][]int, ceiling Bandwidth) error {
	if p == nil {
		return errors.New("network policy is null")
	}
	if reflect.DeepEqual(*p, Policy{}) {
		if err := validateBandwidth(0, ceiling.Upload); err != nil {
			return err
		}
		if err := validateBandwidth(0, ceiling.Download); err != nil {
			return err
		}

		return nil
	}
	if err := p.validateStatic(); err != nil {
		return err
	}
	if err := validateBandwidth(p.Body.Bandwidth.Upload, ceiling.Upload); err != nil {
		return err
	}
	if err := validateBandwidth(p.Body.Bandwidth.Download, ceiling.Download); err != nil {
		return err
	}

	owned, err := assignedAllocations(assigned)
	if err != nil {
		return err
	}
	if len(owned) != len(p.Allocations) {
		return errors.New("policy allocations do not exactly match the server's assigned allocations")
	}

	for _, allocation := range p.Allocations {
		key, err := allocationKey(allocation.IP, allocation.Port)
		if err != nil {
			return err
		}
		if !owned[key] {
			return fmt.Errorf("allocation %s:%d is not assigned to this server", allocation.IP, allocation.Port)
		}
	}

	return nil
}

func (p Policy) validateStatic() error {
	if p.APIVersion != 1 {
		return errors.New("api_version must be 1")
	}
	if p.Revision == 0 {
		return errors.New("revision must be a positive integer")
	}
	if !hashPattern.MatchString(p.PolicyHash) {
		return errors.New("policy_hash must be 64 lowercase hexadecimal characters")
	}
	if p.Allocations == nil ||
		p.ResolvedGroups == nil ||
		p.Body.Groups == nil ||
		p.Body.Firewall.Chains == nil ||
		p.Body.Firewall.Rules == nil {
		return errors.New("policy arrays must not be null")
	}
	if len(p.Body.Groups) > 50 {
		return errors.New("maximum 50 groups")
	}
	if len(p.Body.Firewall.Chains) > 25 {
		return errors.New("maximum 25 chains")
	}
	if len(p.Body.Firewall.Rules) > 100 {
		return errors.New("maximum 100 rules")
	}

	allocations := make(map[int64]Allocation, len(p.Allocations))
	tupleIDs := make(map[string]int64, len(p.Allocations))
	for _, allocation := range p.Allocations {
		if allocation.ID <= 0 || allocation.Port < 1 || allocation.Port > 65535 {
			return errors.New("allocation IDs and ports must be positive and ports must not exceed 65535")
		}
		if _, found := allocations[allocation.ID]; found {
			return fmt.Errorf("duplicate allocation ID %d", allocation.ID)
		}

		key, err := allocationKey(allocation.IP, allocation.Port)
		if err != nil {
			return err
		}
		if id, found := tupleIDs[key]; found {
			return fmt.Errorf("allocation IDs %d and %d ambiguously identify the same address and port", id, allocation.ID)
		}

		allocations[allocation.ID] = allocation
		tupleIDs[key] = allocation.ID
	}

	groups := make(map[string]Group, len(p.Body.Groups))
	for _, group := range p.Body.Groups {
		if !idPattern.MatchString(group.ID) {
			return fmt.Errorf("invalid group ID %q", group.ID)
		}
		if _, found := groups[group.ID]; found {
			return fmt.Errorf("duplicate group ID %q", group.ID)
		}
		if !validName(group.Name) {
			return fmt.Errorf("invalid group name for %q", group.ID)
		}
		if group.Mode != "allocations" && group.Mode != "range" {
			return fmt.Errorf("invalid group mode for %q", group.ID)
		}
		if !validProtocol(group.Protocol) {
			return fmt.Errorf("invalid group protocol for %q", group.ID)
		}
		if group.AllocationIDs == nil || len(group.AllocationIDs) > 1000 {
			return fmt.Errorf("group %q allocation_ids must be an array with at most 1000 entries", group.ID)
		}
		if group.PortStart < 1 || group.PortEnd > 65535 || group.PortStart > group.PortEnd {
			return fmt.Errorf("invalid port range for group %q", group.ID)
		}
		if group.IP != nil {
			if _, err := parseIP(*group.IP); err != nil {
				return fmt.Errorf("invalid IP for group %q: %w", group.ID, err)
			}
		}

		for _, id := range group.AllocationIDs {
			if id <= 0 {
				return fmt.Errorf("group %q contains an invalid allocation ID", group.ID)
			}
		}

		groups[group.ID] = group
	}

	if err := p.validateResolvedGroups(groups, allocations); err != nil {
		return err
	}
	if err := p.validateFirewall(groups); err != nil {
		return err
	}
	if err := validateBandwidth(p.Body.Bandwidth.Upload, 0); err != nil {
		return err
	}
	if err := validateBandwidth(p.Body.Bandwidth.Download, 0); err != nil {
		return err
	}

	hash, err := p.CalculatedHash()
	if err != nil {
		return err
	}
	if p.PolicyHash != hash {
		return errors.New("policy_hash does not match the complete policy envelope")
	}

	return nil
}

func (p Policy) validateResolvedGroups(groups map[string]Group, allocations map[int64]Allocation) error {
	if len(p.ResolvedGroups) != len(groups) {
		return errors.New("resolved_groups must contain exactly one entry for every group")
	}

	seen := make(map[string]bool, len(p.ResolvedGroups))
	for _, resolved := range p.ResolvedGroups {
		group, found := groups[resolved.ID]
		if !found || seen[resolved.ID] {
			return fmt.Errorf("unknown or duplicate resolved group %q", resolved.ID)
		}

		seen[resolved.ID] = true
		if resolved.Protocol != group.Protocol {
			return fmt.Errorf("resolved group %q protocol does not match its group", resolved.ID)
		}
		if resolved.Allocations == nil {
			return fmt.Errorf("resolved group %q allocations must be an array", resolved.ID)
		}

		expected, err := selectedAllocations(group, p.Allocations)
		if err != nil {
			return err
		}
		if len(resolved.Allocations) != len(expected) {
			return fmt.Errorf("resolved group %q does not contain the exact selected allocation set", resolved.ID)
		}

		for _, allocation := range resolved.Allocations {
			original, found := allocations[allocation.ID]
			if !found || original != allocation || !expected[allocation.ID] {
				return fmt.Errorf("resolved group %q contains an allocation outside its selection", resolved.ID)
			}

			delete(expected, allocation.ID)
		}

		if len(expected) != 0 {
			return fmt.Errorf("resolved group %q omits a selected allocation", resolved.ID)
		}
	}

	return nil
}

func selectedAllocations(group Group, allocations []Allocation) (map[int64]bool, error) {
	selected := make(map[int64]bool)
	if group.Mode == "allocations" {
		available := make(map[int64]bool, len(allocations))
		for _, allocation := range allocations {
			available[allocation.ID] = true
		}

		for _, id := range group.AllocationIDs {
			if available[id] {
				selected[id] = true
			}
		}

		return selected, nil
	}

	var matchIP netip.Addr
	if group.IP != nil {
		var err error
		matchIP, err = parseIP(*group.IP)
		if err != nil {
			return nil, err
		}
	}
	for _, allocation := range allocations {
		if allocation.Port < group.PortStart || allocation.Port > group.PortEnd {
			continue
		}
		if group.IP != nil {
			ip, err := parseIP(allocation.IP)
			if err != nil {
				return nil, err
			}
			if ip != matchIP {
				continue
			}
		}
		selected[allocation.ID] = true
	}

	return selected, nil
}

func (p Policy) validateFirewall(groups map[string]Group) error {
	firewall := p.Body.Firewall
	if !validAction(firewall.DefaultInbound) || !validAction(firewall.DefaultOutbound) {
		return errors.New("firewall defaults must be allow or drop")
	}

	chains := make(map[string]bool, len(firewall.Chains))
	for _, chain := range firewall.Chains {
		if !idPattern.MatchString(chain.ID) || !validName(chain.Name) {
			return errors.New("invalid chain ID or name")
		}
		if _, found := chains[chain.ID]; found {
			return fmt.Errorf("duplicate chain ID %q", chain.ID)
		}

		chains[chain.ID] = true
	}

	rules := make(map[string]bool, len(firewall.Rules))
	for _, rule := range firewall.Rules {
		if !idPattern.MatchString(rule.ID) || !validName(rule.Name) {
			return errors.New("invalid rule ID or name")
		}
		if rules[rule.ID] {
			return fmt.Errorf("duplicate rule ID %q", rule.ID)
		}

		rules[rule.ID] = true
		if rule.Direction != "inbound" && rule.Direction != "outbound" {
			return fmt.Errorf("invalid direction for rule %q", rule.ID)
		}
		if !validAction(rule.Action) || !validProtocol(rule.Protocol) {
			return fmt.Errorf("invalid action or protocol for rule %q", rule.ID)
		}
		if rule.ChainID != nil && !chains[*rule.ChainID] {
			return fmt.Errorf("rule %q references unknown chain %q", rule.ID, *rule.ChainID)
		}
		if rule.GroupID != nil {
			group, found := groups[*rule.GroupID]
			if !found {
				return fmt.Errorf("rule %q references unknown group %q", rule.ID, *rule.GroupID)
			}
			if rule.Protocol != "both" && group.Protocol != "both" && rule.Protocol != group.Protocol {
				return fmt.Errorf("rule %q and group %q protocols do not overlap", rule.ID, group.ID)
			}
		}
		if rule.CIDR != nil {
			if _, err := Peer(*rule.CIDR); err != nil {
				return fmt.Errorf("invalid CIDR for rule %q: %w", rule.ID, err)
			}
		}
	}

	return nil
}

func assignedAllocations(assigned map[string][]int) (map[string]bool, error) {
	owned := make(map[string]bool)
	for host, ports := range assigned {
		for _, port := range ports {
			if port < 1 || port > 65535 {
				return nil, fmt.Errorf("server configuration contains invalid assigned port %d", port)
			}

			key, err := allocationKey(host, port)
			if err != nil {
				return nil, fmt.Errorf("server configuration contains invalid assigned IP %q: %w", host, err)
			}
			if owned[key] {
				return nil, fmt.Errorf("server configuration contains ambiguous duplicate allocation %s:%d", host, port)
			}

			owned[key] = true
		}
	}

	return owned, nil
}

func allocationKey(value string, port int) (string, error) {
	ip, err := parseIP(value)
	if err != nil {
		return "", err
	}

	return netip.AddrPortFrom(ip, uint16(port)).String(), nil
}

func hasAllocation(assigned map[string][]int, allocation Allocation) bool {
	wanted, err := allocationKey(allocation.IP, allocation.Port)
	if err != nil {
		return false
	}

	for host, ports := range assigned {
		if !slices.Contains(ports, allocation.Port) {
			continue
		}

		key, err := allocationKey(host, allocation.Port)
		if err == nil && key == wanted {
			return true
		}
	}

	return false
}

// Peer parses an address or CIDR, rejecting zones and IPv4-mapped IPv6 addresses.
func Peer(value string) (netip.Prefix, error) {
	if ip, err := parseIP(value); err == nil {
		return netip.PrefixFrom(ip, ip.BitLen()), nil
	}

	prefix, err := netip.ParsePrefix(value)
	if err != nil || prefix.Addr().Zone() != "" || prefix.Addr().Is4In6() {
		return netip.Prefix{}, errors.New("CIDR must be an IPv4/IPv6 address or prefix without a zone")
	}

	return prefix.Masked(), nil
}

// Ports returns the sorted owned port union for resolved groups whose protocol overlaps proto.
func (p Policy) Ports(names []string, proto string, assigned map[string][]int) []int {
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}

	ports := make(map[int]bool)
	for _, group := range p.ResolvedGroups {
		if !wanted[group.ID] || (group.Protocol != "both" && group.Protocol != proto) {
			continue
		}

		for _, allocation := range group.Allocations {
			if hasAllocation(assigned, allocation) {
				ports[allocation.Port] = true
			}
		}
	}

	result := make([]int, 0, len(ports))
	for port := range ports {
		result = append(result, port)
	}

	sort.Ints(result)

	return result
}

// CalculatedHash returns the canonical SHA-256 identity of the envelope without policy_hash.
func (p Policy) CalculatedHash() (string, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	if len(data) > MaxBody {
		return "", errors.New("policy exceeds 1 MiB")
	}

	// Typed output needs no duplicate-key validation.
	return hashCanonicalObject(data)
}

// CanonicalHash hashes recursively sorted compact JSON while preserving array order.
func CanonicalHash(data []byte) (string, error) {
	if len(data) > MaxBody {
		return "", errors.New("policy exceeds 1 MiB")
	}
	if err := validateUniqueJSON(data); err != nil {
		return "", err
	}
	return hashCanonicalObject(data)
}

func hashCanonicalObject(data []byte) (string, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return "", errors.New("policy envelope must be a JSON object")
	}
	if _, found := object["policy_hash"]; !found {
		return "", errors.New("policy envelope is missing policy_hash")
	}

	delete(object, "policy_hash")

	canonical, err := appendCanonicalObject(nil, object)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func appendCanonical(out []byte, raw json.RawMessage) ([]byte, error) {
	data := bytes.TrimSpace(raw)
	if len(data) == 0 {
		return nil, errors.New("invalid empty JSON value")
	}

	switch data[0] {
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(data, &object); err != nil {
			return nil, err
		}
		return appendCanonicalObject(out, object)
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, err
		}
		out = append(out, '[')
		for i, item := range items {
			if i > 0 {
				out = append(out, ',')
			}
			var err error
			out, err = appendCanonical(out, item)
			if err != nil {
				return nil, err
			}
		}
		return append(out, ']'), nil
	case '"':
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, err
		}
		return appendJSONString(out, value), nil
	default:
		return append(out, data...), nil
	}
}

func appendCanonicalObject(out []byte, object map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	out = append(out, '{')
	for i, key := range keys {
		if i > 0 {
			out = append(out, ',')
		}
		out = appendJSONString(out, key)
		out = append(out, ':')
		var err error
		out, err = appendCanonical(out, object[key])
		if err != nil {
			return nil, err
		}
	}

	return append(out, '}'), nil
}

func appendJSONString(out []byte, value string) []byte {
	out = append(out, '"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			out = append(out, '\\', byte(r))
		case '\b':
			out = append(out, `\b`...)
		case '\f':
			out = append(out, `\f`...)
		case '\n':
			out = append(out, `\n`...)
		case '\r':
			out = append(out, `\r`...)
		case '\t':
			out = append(out, `\t`...)
		default:
			if r < 0x20 {
				out = append(out, `\u00`...)
				out = strconv.AppendInt(out, int64(r>>4), 16)
				out = strconv.AppendInt(out, int64(r&0x0f), 16)
			} else {
				out = utf8.AppendRune(out, r)
			}
		}
	}

	return append(out, '"')
}

func (p *Policy) UnmarshalJSON(data []byte) error {
	if len(data) > MaxBody {
		return errors.New("policy exceeds 1 MiB")
	}
	if err := validatePolicyJSON(data); err != nil {
		return err
	}

	type plain Policy
	var decoded plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}

	policy := Policy(decoded)
	if err := policy.validateStatic(); err != nil {
		return err
	}

	*p = policy
	return nil
}

func validatePolicyJSON(data []byte) error {
	if err := validateUniqueJSON(data); err != nil {
		return err
	}

	top, err := exactObject(data, "api_version", "revision", "policy_hash", "allocations", "resolved_groups", "policy")
	if err != nil {
		return err
	}
	if err := requireNonNull(top, "api_version", "revision", "policy_hash", "allocations", "resolved_groups", "policy"); err != nil {
		return err
	}

	allocations, err := arrayItems(top["allocations"])
	if err != nil {
		return fmt.Errorf("allocations: %w", err)
	}

	for _, allocation := range allocations {
		if err := validateAllocationJSON(allocation); err != nil {
			return err
		}
	}

	resolved, err := arrayItems(top["resolved_groups"])
	if err != nil {
		return fmt.Errorf("resolved_groups: %w", err)
	}

	for _, raw := range resolved {
		object, err := exactObject(raw, "id", "protocol", "allocations")
		if err != nil {
			return fmt.Errorf("resolved_groups: %w", err)
		}
		if err := requireNonNull(object, "id", "protocol", "allocations"); err != nil {
			return err
		}

		items, err := arrayItems(object["allocations"])
		if err != nil {
			return fmt.Errorf("resolved group allocations: %w", err)
		}

		for _, allocation := range items {
			if err := validateAllocationJSON(allocation); err != nil {
				return err
			}
		}
	}

	return validateRulesJSON(top["policy"])
}

func validateAllocationJSON(raw json.RawMessage) error {
	object, err := exactObject(raw, "id", "ip", "port")
	if err != nil {
		return fmt.Errorf("allocation: %w", err)
	}

	return requireNonNull(object, "id", "ip", "port")
}

func validateRulesJSON(raw json.RawMessage) error {
	body, err := exactObject(raw, "groups", "firewall", "bandwidth")
	if err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	if err := requireNonNull(body, "groups", "firewall", "bandwidth"); err != nil {
		return err
	}

	groups, err := arrayItems(body["groups"])
	if err != nil {
		return fmt.Errorf("groups: %w", err)
	}

	for _, rawGroup := range groups {
		group, err := exactObject(rawGroup, "id", "name", "mode", "protocol", "allocation_ids", "port_start", "port_end", "ip")
		if err != nil {
			return fmt.Errorf("group: %w", err)
		}
		if err := requireNonNull(group, "id", "name", "mode", "protocol", "allocation_ids", "port_start", "port_end"); err != nil {
			return err
		}
		if _, err := arrayItems(group["allocation_ids"]); err != nil {
			return fmt.Errorf("allocation_ids: %w", err)
		}
	}

	firewall, err := exactObject(body["firewall"], "enabled", "default_inbound", "default_outbound", "chains", "rules")
	if err != nil {
		return fmt.Errorf("firewall: %w", err)
	}
	if err := requireNonNull(firewall, "enabled", "default_inbound", "default_outbound", "chains", "rules"); err != nil {
		return err
	}

	chains, err := arrayItems(firewall["chains"])
	if err != nil {
		return fmt.Errorf("chains: %w", err)
	}

	for _, rawChain := range chains {
		chain, err := exactObject(rawChain, "id", "name", "enabled")
		if err != nil {
			return fmt.Errorf("chain: %w", err)
		}
		if err := requireNonNull(chain, "id", "name", "enabled"); err != nil {
			return err
		}
	}

	rules, err := arrayItems(firewall["rules"])
	if err != nil {
		return fmt.Errorf("rules: %w", err)
	}

	for _, rawRule := range rules {
		rule, err := exactObject(rawRule, "id", "name", "enabled", "chain_id", "direction", "action", "protocol", "group_id", "cidr")
		if err != nil {
			return fmt.Errorf("rule: %w", err)
		}
		if err := requireNonNull(rule, "id", "name", "enabled", "direction", "action", "protocol"); err != nil {
			return err
		}
	}

	bandwidth, err := exactObject(body["bandwidth"], "download_bps", "upload_bps")
	if err != nil {
		return fmt.Errorf("bandwidth: %w", err)
	}

	return requireNonNull(bandwidth, "download_bps", "upload_bps")
}

func exactObject(raw json.RawMessage, fields ...string) (map[string]json.RawMessage, error) {
	data := bytes.TrimSpace(raw)
	if len(data) == 0 || data[0] != '{' {
		return nil, errors.New("expected a JSON object")
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}

	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field] = true
		if _, found := object[field]; !found {
			return nil, fmt.Errorf("missing required field %q", field)
		}
	}

	for field := range object {
		if !allowed[field] {
			return nil, fmt.Errorf("unknown field %q", field)
		}
	}

	return object, nil
}

func requireNonNull(object map[string]json.RawMessage, fields ...string) error {
	for _, field := range fields {
		if bytes.Equal(bytes.TrimSpace(object[field]), []byte("null")) {
			return fmt.Errorf("field %q must not be null", field)
		}
	}

	return nil
}

func arrayItems(raw json.RawMessage) ([]json.RawMessage, error) {
	data := bytes.TrimSpace(raw)
	if len(data) == 0 || data[0] != '[' {
		return nil, errors.New("expected a JSON array")
	}

	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}

	return items, nil
}
