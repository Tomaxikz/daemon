package networkpolicy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/mdlayher/netlink"
	"github.com/mdlayher/socket"
	"golang.org/x/sys/unix"
)

const routeReadLimit = 1 << 20

// One connection belongs to one operation and one pinned namespace.
type routeClient interface {
	request(context.Context, uint16, uint16, []byte) ([]netlink.Message, error)
	close() error
}

type routeSocket struct {
	conn *socket.Conn
	pid  uint32
	seq  uint32
}

func (b Backend) route(ctx context.Context, ns *os.File) (routeClient, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.openRoute != nil {
		return b.openRoute(ctx, ns)
	}

	configuration := &socket.Config{}
	if ns != nil {
		if ns.Fd() == ^uintptr(0) {
			return nil, os.ErrClosed
		}
		fd, err := unix.FcntlInt(ns.Fd(), unix.F_DUPFD_CLOEXEC, 3)
		if err != nil {
			return nil, err
		}
		defer unix.Close(fd)
		configuration.NetNS = fd
	}

	conn, err := socket.Socket(unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_ROUTE, "wings-rtnetlink", configuration)
	if err != nil {
		return nil, err
	}

	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()

	if err := conn.Bind(&unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, err
	}
	if err := conn.SetsockoptInt(unix.SOL_NETLINK, unix.NETLINK_GET_STRICT_CHK, 1); err != nil {
		return nil, fmt.Errorf("strict Netlink readback is required: %w", err)
	}

	address, err := conn.Getsockname()
	if err != nil {
		return nil, err
	}
	local, okAddress := address.(*unix.SockaddrNetlink)
	if !okAddress {
		return nil, errors.New("unexpected Netlink socket address")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ok = true
	return &routeSocket{conn: conn, pid: local.Pid}, nil
}

func (r *routeSocket) close() error { return r.conn.Close() }

func (r *routeSocket) request(ctx context.Context, kind, flags uint16, data []byte) ([]netlink.Message, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.seq++
	request := netlink.Message{
		Header: netlink.Header{
			Length:   uint32(16 + len(data)),
			Type:     netlink.HeaderType(kind),
			Flags:    netlink.HeaderFlags(flags | unix.NLM_F_REQUEST),
			Sequence: r.seq,
			PID:      r.pid,
		},
		Data: data,
	}
	raw, err := request.MarshalBinary()
	if err != nil {
		return nil, err
	}
	if err := r.conn.Sendto(ctx, raw, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, routeError(ctx, err)
	}

	var replies []netlink.Message
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var peek [1]byte
		n, _, _, from, err := r.conn.Recvmsg(ctx, peek[:], nil, unix.MSG_PEEK|unix.MSG_TRUNC)
		if err != nil {
			return nil, routeError(ctx, err)
		}
		peer, valid := from.(*unix.SockaddrNetlink)
		if !valid || peer.Pid != 0 {
			return nil, errors.New("Netlink reply is not from the kernel")
		}
		if n < 16 || n > routeReadLimit {
			return nil, errors.New("Netlink readback exceeds bounds")
		}
		buffer := make([]byte, n)
		n, _, receivedFlags, from, err := r.conn.Recvmsg(ctx, buffer, nil, 0)
		if err != nil {
			return nil, routeError(ctx, err)
		}
		peer, valid = from.(*unix.SockaddrNetlink)
		if !valid || peer.Pid != 0 || receivedFlags&unix.MSG_TRUNC != 0 {
			return nil, errors.New("invalid or truncated Netlink datagram")
		}
		messages, done, err := decodeRouteReply(request, buffer[:n])
		if err != nil {
			return nil, err
		}
		for _, message := range messages {
			// Some kernels ignore the requested index; don't retain other devices.
			if kind == unix.RTM_GETQDISC && len(data) >= 8 && len(message.Data) >= 8 &&
				binary.NativeEndian.Uint32(message.Data[4:8]) != binary.NativeEndian.Uint32(data[4:8]) {
				continue
			}
			total += len(message.Data) + 16
			if total > routeReadLimit {
				return nil, errors.New("Netlink readback exceeds bounds")
			}
			// Copy the payload so filtered-out datagram data isn't kept alive.
			message.Data = bytes.Clone(message.Data)
			replies = append(replies, message)
		}
		if len(replies) > 4096 {
			return nil, errors.New("too many Netlink messages")
		}
		if done {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return replies, nil
		}
	}
}

func routeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("route Netlink: %w", err)
}

// Keep DONE visible: an interrupted dump must not hide an administrator's qdisc.
func decodeRouteReply(request netlink.Message, data []byte) ([]netlink.Message, bool, error) {
	var messages []netlink.Message
	done := false
	for len(data) > 0 {
		if done {
			return nil, false, errors.New("Netlink data after completion")
		}
		if len(data) < 16 {
			return nil, false, errors.New("short Netlink header")
		}
		length := int(binary.NativeEndian.Uint32(data[:4]))
		if length < 16 || length > len(data) {
			return nil, false, errors.New("invalid Netlink message length")
		}
		var message netlink.Message
		if err := message.UnmarshalBinary(data[:length]); err != nil {
			return nil, false, err
		}
		if err := netlink.Validate(request, []netlink.Message{message}); err != nil {
			return nil, false, err
		}
		if message.Header.Flags&netlink.HeaderFlags(unix.NLM_F_DUMP_INTR) != 0 {
			return nil, false, errors.New("interrupted Netlink dump")
		}
		switch message.Header.Type {
		case netlink.Overrun:
			return nil, false, errors.New("Netlink receive overrun")
		case netlink.Error, netlink.Done:
			if message.Header.Type == netlink.Done && request.Header.Flags&netlink.Dump != netlink.Dump {
				return nil, false, errors.New("unexpected Netlink dump completion")
			}
			if len(message.Data) < 4 {
				return nil, false, errors.New("short Netlink completion")
			}
			code := int32(binary.NativeEndian.Uint32(message.Data[:4]))
			if code < 0 {
				return nil, false, fmt.Errorf("kernel rejected Netlink request: %w", unix.Errno(-int64(code)))
			}
			if code > 0 {
				return nil, false, errors.New("invalid Netlink error code")
			}
			if message.Header.Type == netlink.Done || request.Header.Flags&netlink.Dump != netlink.Dump {
				done = true
			}
		default:
			if request.Header.Flags&netlink.Acknowledge != 0 && request.Header.Flags&netlink.Dump != netlink.Dump {
				return nil, false, errors.New("Netlink mutation was not acknowledged")
			}
			if request.Header.Flags&netlink.Dump == netlink.Dump && message.Header.Flags&netlink.Multi == 0 {
				return nil, false, errors.New("incomplete Netlink multipart reply")
			}
			messages = append(messages, message)
			if message.Header.Flags&netlink.Multi == 0 {
				done = true
			}
		}
		aligned := (length + 3) &^ 3
		if aligned > len(data) {
			if length != len(data) {
				return nil, false, errors.New("truncated Netlink alignment")
			}
			aligned = length
		}
		data = data[aligned:]
	}
	return messages, done, nil
}

func routeAttributes(attributes []netlink.Attribute) ([]byte, error) {
	return netlink.MarshalAttributes(attributes)
}

func routeU32(value uint32) []byte {
	data := make([]byte, 4)
	binary.NativeEndian.PutUint32(data, value)
	return data
}

func routeU64(value uint64) []byte {
	data := make([]byte, 8)
	binary.NativeEndian.PutUint64(data, value)
	return data
}

func routeLink(ctx context.Context, client routeClient, name string) (Link, error) {
	if name == "" || len(name) > 15 || strings.ContainsAny(name, "\x00/: \t\r\n") {
		return Link{}, errors.New("invalid interface name")
	}
	attributes, err := routeAttributes([]netlink.Attribute{{Type: unix.IFLA_IFNAME, Data: append([]byte(name), 0)}})
	if err != nil {
		return Link{}, err
	}
	replies, err := client.request(ctx, unix.RTM_GETLINK, 0, append(make([]byte, 16), attributes...))
	if err != nil {
		return Link{}, err
	}
	if len(replies) != 1 {
		return Link{}, errors.New("interface lookup did not return one link")
	}
	link, err := decodeRouteLink(replies[0])
	if err != nil {
		return Link{}, err
	}
	if link.Name != name {
		return Link{}, errors.New("interface lookup returned another link")
	}
	return link, nil
}

func decodeRouteLink(message netlink.Message) (Link, error) {
	if message.Header.Type != unix.RTM_NEWLINK || len(message.Data) < 16 {
		return Link{}, errors.New("invalid interface Netlink response")
	}
	link := Link{Index: int(int32(binary.NativeEndian.Uint32(message.Data[4:8])))}
	decoder, err := netlink.NewAttributeDecoder(message.Data[16:])
	if err != nil {
		return link, err
	}
	for decoder.Next() {
		switch decoder.Type() {
		case unix.IFLA_IFNAME:
			link.Name = decoder.String()
		case unix.IFLA_MTU:
			link.MTU = int(decoder.Uint32())
		case unix.IFLA_LINK:
			link.Peer = int(decoder.Uint32())
		case unix.IFLA_MASTER:
			link.master = int(decoder.Uint32())
		case unix.IFLA_ADDRESS:
			link.Address = net.HardwareAddr(decoder.Bytes()).String()
		}
	}
	if err := decoder.Err(); err != nil {
		return link, err
	}
	if link.Index <= 0 || link.Name == "" || link.MTU <= 0 {
		return link, errors.New("incomplete interface readback")
	}
	return link, nil
}

// A failed restoration leaves the dedicated thread locked, so Go discards it.
func disposableNetworkNamespace(ctx context.Context) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		file *os.File
		err  error
	}
	ready := make(chan result)
	go func() {
		runtime.LockOSThread()
		current := fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid())
		original, err := os.Open(current)
		var created *os.File
		if err == nil {
			err = unix.Unshare(unix.CLONE_NEWNET)
			if err == nil {
				created, err = os.Open(current)
				if restoreErr := unix.Setns(int(original.Fd()), unix.CLONE_NEWNET); restoreErr != nil {
					if created != nil {
						_ = created.Close()
						created = nil
					}
					err = fmt.Errorf("restore network namespace: %w", restoreErr)
				} else {
					runtime.UnlockOSThread()
				}
			} else {
				runtime.UnlockOSThread()
			}
			_ = original.Close()
		} else {
			runtime.UnlockOSThread()
		}
		select {
		case ready <- result{created, err}:
		case <-ctx.Done():
			if created != nil {
				_ = created.Close()
			}
		}
	}()
	select {
	case value := <-ready:
		return value.file, value.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
