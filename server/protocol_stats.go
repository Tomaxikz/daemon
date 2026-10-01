package server

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/docker/docker/client"

	"github.com/pterodactyl/wings/environment/docker"
)

const ProtocolStatsEvent = "protocol stats"

var errProtocolStatsUnavailable = errors.New("container stopped during statistics collection")

type ProtocolStats struct {
	Total struct {
		InPackets  uint64 `json:"in_packets"`
		OutPackets uint64 `json:"out_packets"`
	} `json:"total"`
	TCP struct {
		InPackets  uint64 `json:"in_packets"`
		OutPackets uint64 `json:"out_packets"`
		InErrors   uint64 `json:"in_errors"`
		OutErrors  uint64 `json:"out_errors"`
	} `json:"tcp"`
	UDP struct {
		InDatagrams  uint64 `json:"in_datagrams"`
		OutDatagrams uint64 `json:"out_datagrams"`
		InErrors     uint64 `json:"in_errors"`
		NoPorts      uint64 `json:"no_ports"`
	} `json:"udp"`
	ICMP struct {
		InMsgs    uint64 `json:"in_msgs"`
		OutMsgs   uint64 `json:"out_msgs"`
		InErrors  uint64 `json:"in_errors"`
		OutErrors uint64 `json:"out_errors"`
	} `json:"icmp"`
	Other struct {
		InPackets  uint64 `json:"in_packets"`
		OutPackets uint64 `json:"out_packets"`
	} `json:"other"`
}

func (s *Server) GetProtocolStats(ctx context.Context) (*ProtocolStats, error) {
	stats := &ProtocolStats{}
	env, ok := s.Environment.(*docker.Environment)
	if !ok {
		return stats, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := env.ContainerInspect(ctx)
	if client.IsErrNotFound(err) {
		return stats, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container for protocol stats: %w", err)
	}
	if c.State == nil || !c.State.Running {
		return stats, nil
	}

	output, err := readProtocolFile(ctx, env, c.ID, "/proc/net/dev")
	if errors.Is(err, errProtocolStatsUnavailable) {
		return stats, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get network device stats: %w", err)
	}
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "eth0:") {
			fields := strings.Fields(line)
			if len(fields) >= 11 {
				fmt.Sscanf(fields[2], "%d", &stats.Total.InPackets)
				fmt.Sscanf(fields[10], "%d", &stats.Total.OutPackets)
			}
			break
		}
	}
	output, err = readProtocolFile(ctx, env, c.ID, "/proc/net/snmp")
	if errors.Is(err, errProtocolStatsUnavailable) {
		return &ProtocolStats{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get protocol stats: %w", err)
	}
	lines = strings.Split(string(output), "\n")
	for i := 0; i < len(lines)-1; i += 2 {
		header := strings.Fields(lines[i])
		values := strings.Fields(lines[i+1])

		if len(header) == 0 || len(values) == 0 {
			continue
		}
		if header[0] != values[0] || len(values) < len(header) {
			return nil, fmt.Errorf("malformed protocol stats for %s", header[0])
		}

		switch header[0] {
		case "Tcp:":
			for j, field := range header[1:] {
				switch field {
				case "InSegs":
					fmt.Sscanf(values[j+1], "%d", &stats.TCP.InPackets)
				case "OutSegs":
					fmt.Sscanf(values[j+1], "%d", &stats.TCP.OutPackets)
				case "InErrs":
					fmt.Sscanf(values[j+1], "%d", &stats.TCP.InErrors)
				case "OutRsts":
					fmt.Sscanf(values[j+1], "%d", &stats.TCP.OutErrors)
				}
			}

		case "Udp:":
			for j, field := range header[1:] {
				switch field {
				case "InDatagrams":
					fmt.Sscanf(values[j+1], "%d", &stats.UDP.InDatagrams)
				case "OutDatagrams":
					fmt.Sscanf(values[j+1], "%d", &stats.UDP.OutDatagrams)
				case "InErrors":
					fmt.Sscanf(values[j+1], "%d", &stats.UDP.InErrors)
				case "NoPorts":
					fmt.Sscanf(values[j+1], "%d", &stats.UDP.NoPorts)
				}
			}

		case "Icmp:":
			for j, field := range header[1:] {
				switch field {
				case "InMsgs":
					fmt.Sscanf(values[j+1], "%d", &stats.ICMP.InMsgs)
				case "OutMsgs":
					fmt.Sscanf(values[j+1], "%d", &stats.ICMP.OutMsgs)
				case "InErrors":
					fmt.Sscanf(values[j+1], "%d", &stats.ICMP.InErrors)
				case "OutErrors":
					fmt.Sscanf(values[j+1], "%d", &stats.ICMP.OutErrors)
				}
			}
		}
	}

	protocolInSum := stats.TCP.InPackets + stats.UDP.InDatagrams + stats.ICMP.InMsgs
	if protocolInSum <= stats.Total.InPackets {
		stats.Other.InPackets = stats.Total.InPackets - protocolInSum
	}

	protocolOutSum := stats.TCP.OutPackets + stats.UDP.OutDatagrams + stats.ICMP.OutMsgs
	if protocolOutSum <= stats.Total.OutPackets {
		stats.Other.OutPackets = stats.Total.OutPackets - protocolOutSum
	}

	return stats, nil
}

func readProtocolFile(ctx context.Context, env *docker.Environment, id, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", id, "cat", path)
	cmd.WaitDelay = 250 * time.Millisecond
	output, err := cmd.Output()
	if err == nil {
		return output, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// A stop or recreation between inspection and exec is expected, not a
	// collection failure. Recheck Docker rather than matching error text.
	c, inspectErr := env.ContainerInspect(ctx)
	if client.IsErrNotFound(inspectErr) || (inspectErr == nil &&
		(c.State == nil || !c.State.Running || c.ID != id)) {
		return nil, errProtocolStatsUnavailable
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if detail := strings.TrimSpace(string(exit.Stderr)); detail != "" {
			if len(detail) > 2048 {
				detail = detail[:2048]
			}
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
	}
	return nil, err
}
