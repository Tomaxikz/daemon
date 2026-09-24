package networkpolicy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"

	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

const (
	shapingHandle   = "4e53:"
	fairQueueHandle = "4e54:"
	shapingParent   = "4e53:1"
)

type shapingProfile struct {
	rate    int64
	burst   int64
	limit   int64
	packets int64
	memory  int64
	quantum int64
}

func newShapingProfile(device string, rate int64, mtu int) (shapingProfile, error) {
	if !idPattern.MatchString(device) || len(device) > 15 || !validRate(rate) {
		return shapingProfile{}, errors.New("invalid shaping device or rate")
	}
	if rate == 0 {
		return shapingProfile{}, nil
	}
	if rate%8 != 0 {
		return shapingProfile{}, errors.New("TBF can enforce only whole-byte rates; bandwidth must be divisible by 8 bits per second")
	}
	if mtu < 576 || mtu > 65536 {
		return shapingProfile{}, fmt.Errorf("unsupported shaping MTU %d", mtu)
	}

	frame := int64(mtu)
	burst := min(max(rate/800, 2*frame), 1<<20)
	// Avoid overflowing the kernel's finite-width bucket timer.
	if burst > (rate/8)*60 {
		return shapingProfile{}, fmt.Errorf("bandwidth %d is too low for MTU %d: a two-packet burst must fit within 60 seconds", rate, mtu)
	}

	queue := min(max(rate/160, 2*frame), 2<<20)
	return shapingProfile{
		rate:    rate,
		burst:   burst,
		limit:   burst + queue,
		packets: min(max((queue+frame-1)/frame, 32), 2048),
		memory:  min(max(queue*2, 64<<10), 4<<20),
		quantum: frame + 14,
	}, nil
}

type qdisc struct {
	Kind, Handle, Parent string
	Root                 bool
	TBF                  *tbfState
	Fair                 *fairQueueState
}

type tbfState struct {
	Rate          uint64
	Buffer, Limit uint32
	Unsupported   bool
}

type fairQueueState struct {
	Limit, Flows, Quantum, Target, Interval, Memory, ECN uint32
}

func (b Backend) readShapers(ctx context.Context, ns *os.File, device string) ([]qdisc, error) {
	client, err := b.route(ctx, ns)
	if err != nil {
		return nil, err
	}
	defer client.close()
	link, err := routeLink(ctx, client, device)
	if err != nil {
		return nil, err
	}
	return readRouteShapers(ctx, client, link.Index)
}

// Deleting a root also deletes its children, so refuse foreign descendants.
func ownedShapers(qdiscs []qdisc, active bool) (root, leaf *qdisc, err error) {
	for i := range qdiscs {
		q := &qdiscs[i]
		if q.Root {
			if q.Handle == shapingHandle {
				if q.Kind != "tbf" || root != nil {
					return nil, nil, errors.New("refusing unexpected or duplicate NSM root qdisc")
				}
				root = q
			} else if active && (q.Kind != "noqueue" || q.Handle != "0:") {
				return nil, nil, errors.New("refusing administrator or duplicate root qdisc")
			}
		}
	}
	if root == nil {
		return nil, nil, nil
	}

	for i := range qdiscs {
		q := &qdiscs[i]
		if q.Root || q.Kind == "clsact" || q.Kind == "ingress" {
			continue
		}
		if q.Parent == shapingParent && q.Handle == fairQueueHandle && q.Kind == "fq_codel" && leaf == nil {
			leaf = q
			continue
		}
		return nil, nil, errors.New("refusing administrator or unexpected child qdisc")
	}
	return root, leaf, nil
}

func (p shapingProfile) matchesRoot(q *qdisc) bool {
	if q == nil || q.TBF == nil || q.TBF.Unsupported {
		return false
	}
	// Linux exports TBF buffer time in 64 ns ticks. Preserve the one-microsecond
	// rounding allowance for queues created by older tc-based Wings binaries.
	want := burstTicks(uint64(p.rate/8), uint64(p.burst))
	return q.TBF.Rate == uint64(p.rate/8) && q.TBF.Limit == uint32(p.limit) &&
		q.TBF.Buffer > 0 && q.TBF.Buffer <= want && want-q.TBF.Buffer <= 16
}

func (p shapingProfile) matchesLeaf(q *qdisc) bool {
	if q == nil || q.Fair == nil {
		return false
	}
	qf := q.Fair
	return qf.Limit == uint32(p.packets) && qf.Flows == 1024 && qf.Quantum == uint32(p.quantum) &&
		qf.Target >= 4999 && qf.Target <= 5000 && qf.Interval >= 99999 && qf.Interval <= 100000 &&
		qf.Memory == uint32(p.memory) && qf.ECN == 1
}

// Shape leaves matching queues untouched to preserve packets and token state.
func (b Backend) Shape(ctx context.Context, ns *os.File, device string, rate int64, mtu int) error {
	profile, err := newShapingProfile(device, rate, mtu)
	if err != nil {
		return err
	}
	client, err := b.route(ctx, ns)
	if err != nil {
		return err
	}
	defer client.close()
	link, err := routeLink(ctx, client, device)
	if err != nil {
		return err
	}
	qdiscs, err := readRouteShapers(ctx, client, link.Index)
	if err != nil {
		return err
	}
	root, leaf, err := ownedShapers(qdiscs, rate != 0)
	if err != nil {
		return fmt.Errorf("shaping %s: %w", device, err)
	}

	if rate == 0 {
		if root == nil {
			return nil
		}
		return changeRouteShaper(ctx, client, link.Index, unix.RTM_DELQDISC, 0, rootHandle, rootParent, "", nil)
	}

	changed := !profile.matchesRoot(root)
	if leaf != nil && (changed || leaf.Fair == nil || leaf.Fair.Flows != 1024) {
		// Purge old packets that could stall a smaller bucket; keep the root cap.
		err = changeRouteShaper(ctx, client, link.Index, unix.RTM_DELQDISC, 0, leafHandle, leafParent, "", nil)
		if err != nil {
			return fmt.Errorf("remove outdated fair queue on %s: %w", device, err)
		}
		leaf = nil
	}

	if changed {
		flags := uint16(0)
		if root == nil {
			flags = unix.NLM_F_CREATE | unix.NLM_F_EXCL
		}
		err = changeRouteShaper(ctx, client, link.Index, unix.RTM_NEWQDISC, flags, rootHandle, rootParent, "tbf", profile.tbfOptions())
		if err != nil {
			return fmt.Errorf("configure rate limiter on %s: %w", device, err)
		}
	}

	if !profile.matchesLeaf(leaf) {
		flags := uint16(0)
		if leaf == nil {
			flags = unix.NLM_F_CREATE | unix.NLM_F_EXCL
		}
		err = changeRouteShaper(ctx, client, link.Index, unix.RTM_NEWQDISC, flags, leafHandle, leafParent, "fq_codel", profile.fairOptions(leaf == nil))
		if err != nil {
			return fmt.Errorf("configure fair queue on %s: %w", device, err)
		}
	}
	return nil
}

// VerifyShape reads both qdiscs before the caller publishes applied status.
func (b Backend) VerifyShape(ctx context.Context, ns *os.File, device string, rate int64, mtu int) error {
	profile, err := newShapingProfile(device, rate, mtu)
	if err != nil {
		return err
	}
	qdiscs, err := b.readShapers(ctx, ns, device)
	if err != nil {
		return err
	}
	return profile.verify(qdiscs, device)
}

func (p shapingProfile) verify(qdiscs []qdisc, device string) error {
	root, leaf, err := ownedShapers(qdiscs, p.rate != 0)
	if err != nil {
		return fmt.Errorf("shaping %s: %w", device, err)
	}
	if p.rate == 0 {
		if root != nil {
			return fmt.Errorf("NSM shaping remains on %s after clearing the cap", device)
		}
		return nil
	}
	if root == nil {
		return fmt.Errorf("NSM shaping is missing on %s", device)
	}
	if !p.matchesRoot(root) {
		return fmt.Errorf("NSM shaping drift on %s: rate, burst or queue limit changed", device)
	}
	if !p.matchesLeaf(leaf) {
		return fmt.Errorf("NSM fair queue drift on %s: fq_codel is missing or its settings changed", device)
	}
	return nil
}

func (b Backend) probeShaping(ctx context.Context) error {
	create := disposableNetworkNamespace
	if b.probeNamespace != nil {
		create = b.probeNamespace
	}
	ns, err := create(ctx)
	if err != nil {
		return fmt.Errorf("create shaping probe namespace: %w", err)
	}
	defer ns.Close()
	if err := b.Shape(ctx, ns, "lo", 1_000_000, 65536); err != nil {
		return err
	}
	return b.VerifyShape(ctx, ns, "lo", 1_000_000, 65536)
}

const (
	rootHandle = 0x4e530000
	leafHandle = 0x4e540000
	rootParent = 0xffffffff
	leafParent = 0x4e530001

	// Qdisc attributes from Linux pkt_sched.h.
	tcaKind       = 1
	tcaOptions    = 2
	tbfParameters = 1
	tbfRate64     = 4
	tbfPeakRate64 = 5
	tbfBurst      = 6
	fqTarget      = 1
	fqLimit       = 2
	fqInterval    = 3
	fqECN         = 4
	fqFlows       = 5
	fqQuantum     = 6
	fqMemory      = 9
)

func burstTicks(rate, burst uint64) uint32 {
	if rate == 0 {
		return 0
	}
	return uint32((burst * 1_000_000_000 / rate) >> 6)
}

func (p shapingProfile) tbfOptions() []netlink.Attribute {
	parameters := make([]byte, 36)
	parameters[1] = 1 // Ethernet rate accounting.
	rate := uint64(p.rate / 8)
	binary.NativeEndian.PutUint32(parameters[8:12], uint32(min(rate, uint64(0xffffffff))))
	binary.NativeEndian.PutUint32(parameters[24:28], uint32(p.limit))
	binary.NativeEndian.PutUint32(parameters[28:32], burstTicks(rate, uint64(p.burst)))
	return []netlink.Attribute{
		{Type: tbfParameters, Data: parameters},
		{Type: tbfRate64, Data: routeU64(rate)},
		{Type: tbfBurst, Data: routeU32(uint32(p.burst))},
	}
}

func (p shapingProfile) fairOptions(create bool) []netlink.Attribute {
	attributes := []netlink.Attribute{
		{Type: fqTarget, Data: routeU32(5000)},
		{Type: fqLimit, Data: routeU32(uint32(p.packets))},
		{Type: fqInterval, Data: routeU32(100000)},
		{Type: fqECN, Data: routeU32(1)},
		{Type: fqQuantum, Data: routeU32(uint32(p.quantum))},
		{Type: fqMemory, Data: routeU32(uint32(p.memory))},
	}
	if create {
		attributes = append(attributes, netlink.Attribute{Type: fqFlows, Data: routeU32(1024)})
	}
	return attributes
}

func shaperMessage(index int, handle, parent uint32) []byte {
	data := make([]byte, 20)
	binary.NativeEndian.PutUint32(data[4:8], uint32(index))
	binary.NativeEndian.PutUint32(data[8:12], handle)
	binary.NativeEndian.PutUint32(data[12:16], parent)
	return data
}

func changeRouteShaper(
	ctx context.Context, client routeClient, index int,
	command, flags uint16, handle, parent uint32,
	kind string, options []netlink.Attribute,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data := shaperMessage(index, handle, parent)
	if command != unix.RTM_DELQDISC {
		nested, err := routeAttributes(options)
		if err != nil {
			return err
		}
		attributes, err := routeAttributes([]netlink.Attribute{{Type: tcaKind, Data: append([]byte(kind), 0)}, {Type: tcaOptions, Data: nested}})
		if err != nil {
			return err
		}
		data = append(data, attributes...)
	}
	_, err := client.request(ctx, command, flags|unix.NLM_F_ACK, data)
	return err
}

func handleString(handle uint32) string {
	if handle&0xffff == 0 {
		return fmt.Sprintf("%x:", handle>>16)
	}
	return fmt.Sprintf("%x:%x", handle>>16, handle&0xffff)
}

func readRouteShapers(ctx context.Context, client routeClient, index int) ([]qdisc, error) {
	replies, err := client.request(ctx, unix.RTM_GETQDISC, unix.NLM_F_DUMP, shaperMessage(index, 0, 0))
	if err != nil {
		return nil, err
	}
	qdiscs := make([]qdisc, 0, len(replies))
	for _, message := range replies {
		if message.Header.Type != unix.RTM_NEWQDISC || len(message.Data) < 20 {
			return nil, errors.New("invalid qdisc Netlink response")
		}
		if int(int32(binary.NativeEndian.Uint32(message.Data[4:8]))) != index {
			continue
		}
		q, err := decodeRouteShaper(message.Data)
		if err != nil {
			return nil, err
		}
		qdiscs = append(qdiscs, q)
	}
	return qdiscs, nil
}

func decodeRouteShaper(data []byte) (qdisc, error) {
	if len(data) < 20 {
		return qdisc{}, errors.New("short qdisc message")
	}
	parent := binary.NativeEndian.Uint32(data[12:16])
	q := qdisc{Handle: handleString(binary.NativeEndian.Uint32(data[8:12])), Parent: handleString(parent), Root: parent == rootParent}
	decoder, err := netlink.NewAttributeDecoder(data[20:])
	if err != nil {
		return q, err
	}
	var options []byte
	for decoder.Next() {
		switch decoder.Type() {
		case tcaKind:
			q.Kind = decoder.String()
		case tcaOptions:
			options = decoder.Bytes()
		}
	}
	if err := decoder.Err(); err != nil {
		return q, err
	}
	if q.Kind == "" {
		return q, errors.New("qdisc kind is missing")
	}
	if q.Kind != "tbf" && q.Kind != "fq_codel" {
		return q, nil
	}
	decoder, err = netlink.NewAttributeDecoder(options)
	if err != nil {
		return q, err
	}
	if q.Kind == "tbf" {
		state := &tbfState{}
		var rate64 *uint64
		found := false
		for decoder.Next() {
			switch decoder.Type() {
			case tbfParameters:
				parameters := decoder.Bytes()
				if len(parameters) != 36 {
					return q, errors.New("invalid TBF parameters")
				}
				state.Rate = uint64(binary.NativeEndian.Uint32(parameters[8:12]))
				state.Limit = binary.NativeEndian.Uint32(parameters[24:28])
				state.Buffer = binary.NativeEndian.Uint32(parameters[28:32])
				state.Unsupported = state.Unsupported || parameters[1] > 1 ||
					binary.NativeEndian.Uint16(parameters[2:4]) != 0 || // Overhead
					binary.NativeEndian.Uint16(parameters[6:8]) != 0 || // Minimum packet size
					binary.NativeEndian.Uint32(parameters[20:24]) != 0 || // Peak rate
					binary.NativeEndian.Uint32(parameters[32:36]) != 0 // Peak bucket
				found = true
			case tbfRate64:
				value := decoder.Uint64()
				rate64 = &value
			case tbfPeakRate64:
				if decoder.Uint64() != 0 {
					state.Unsupported = true
				}
			}
		}
		if rate64 != nil {
			state.Rate = *rate64
		}
		if !found {
			return q, errors.New("TBF parameters are missing")
		}
		q.TBF = state
	} else {
		state := &fairQueueState{}
		for decoder.Next() {
			switch decoder.Type() {
			case fqTarget:
				state.Target = decoder.Uint32()
			case fqLimit:
				state.Limit = decoder.Uint32()
			case fqInterval:
				state.Interval = decoder.Uint32()
			case fqECN:
				state.ECN = decoder.Uint32()
			case fqFlows:
				state.Flows = decoder.Uint32()
			case fqQuantum:
				state.Quantum = decoder.Uint32()
			case fqMemory:
				state.Memory = decoder.Uint32()
			}
		}
		q.Fair = state
	}
	return q, decoder.Err()
}
