package server

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/pterodactyl/wings/environment/docker"
)

const ProtocolStatsEvent = "protocol stats"

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
	cmd := exec.Command("docker", "exec", env.Id, "cat", "/proc/net/dev")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get network device stats: %v", err)
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
	cmd = exec.Command("docker", "exec", env.Id, "cat", "/proc/net/snmp")
	output, err = cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get protocol stats: %v", err)
	}
	lines = strings.Split(string(output), "\n")
	for i := 0; i < len(lines)-1; i += 2 {
		header := strings.Fields(lines[i])
		values := strings.Fields(lines[i+1])

		if len(header) == 0 || len(values) == 0 {
			continue
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
