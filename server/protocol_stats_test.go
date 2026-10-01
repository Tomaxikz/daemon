package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/stretchr/testify/require"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/environment/docker"
)

type protocolStatsTransport func(*http.Request) (*http.Response, error)

func (f protocolStatsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestProtocolStatsContainerLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Docker statistics collector targets Linux containers")
	}

	const running = `{"Id":"original-container","State":{"Running":true}}`
	const stopped = `{"Id":"original-container","State":{"Running":false,"Status":"exited"}}`
	for _, test := range []struct {
		name, mode, wantError string
		responses             []string
		status                int
		wantStats             bool
		deadline              bool
	}{
		{name: "created", responses: []string{`{"Id":"original-container","State":{"Running":false,"Status":"created"}}`}},
		{name: "exited", responses: []string{stopped}},
		{name: "missing", responses: []string{`{"message":"No such container"}`}, status: http.StatusNotFound},
		{name: "inspection failure", responses: []string{`{"message":"inspection denied"}`}, status: http.StatusInternalServerError, wantError: "inspection denied"},
		{name: "running", responses: []string{running}, wantStats: true},
		{name: "stopped before first read", mode: "dev-failure", responses: []string{running, stopped}},
		{name: "stopped before second read", mode: "snmp-failure", responses: []string{running, stopped}},
		{name: "removed during read", mode: "dev-failure", responses: []string{running, `{"message":"No such container"}`}, status: http.StatusNotFound},
		{name: "recreated during read", mode: "dev-failure", responses: []string{running, `{"Id":"replacement-container","State":{"Running":true}}`}},
		{name: "real read error", mode: "dev-failure", responses: []string{running}, wantError: "fixture permission denied"},
		{name: "malformed counters", mode: "malformed", responses: []string{running}, wantError: "malformed protocol stats"},
		{name: "collection deadline", mode: "hang", responses: []string{running}, deadline: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config.Set(&config.Configuration{AuthenticationToken: "protocol-test"})
			env, err := docker.New("stats-server", &docker.Metadata{}, environment.NewConfiguration(environment.Settings{}, nil))
			require.NoError(t, err)
			cli, err := environment.Docker()
			require.NoError(t, err)
			httpClient := cli.HTTPClient()
			t.Cleanup(func() { require.NoError(t, client.WithHTTPClient(httpClient)(cli)) })
			inspections := 0
			transport := protocolStatsTransport(func(r *http.Request) (*http.Response, error) {
				if err := r.Context().Err(); err != nil {
					return nil, err
				}
				headers := make(http.Header)
				headers.Set("API-Version", "1.50")
				body, status := "OK", http.StatusOK
				if strings.HasSuffix(r.URL.Path, "/json") {
					index := min(inspections, len(test.responses)-1)
					body = test.responses[index]
					inspections++
					if test.status != 0 && index == len(test.responses)-1 {
						status = test.status
					}
					headers.Set("Content-Type", "application/json")
				} else {
					require.Equal(t, "/_ping", r.URL.Path)
				}
				return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			require.NoError(t, client.WithHTTPClient(&http.Client{Transport: transport})(cli))

			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("PROTOCOL_TEST_CALLS", calls)
			t.Setenv("PROTOCOL_TEST_MODE", test.mode)
			script := `#!/bin/sh
printf '%s\n' "$*" >> "$PROTOCOL_TEST_CALLS"
if [ "$PROTOCOL_TEST_MODE" = hang ]; then
    while :; do :; done
fi
if { [ "$PROTOCOL_TEST_MODE" = dev-failure ] && [ "$4" = /proc/net/dev ]; } ||
   { [ "$PROTOCOL_TEST_MODE" = snmp-failure ] && [ "$4" = /proc/net/snmp ]; }; then
    echo 'fixture permission denied' >&2
    exit 1
fi
if [ "$4" = /proc/net/dev ]; then
    echo 'eth0: 1000 40 0 0 0 0 0 0 2000 50 0 0 0 0 0 0'
elif [ "$PROTOCOL_TEST_MODE" = malformed ]; then
    printf 'Tcp: InSegs OutSegs InErrs\nTcp: 10\n'
else
    printf 'Tcp: InSegs OutSegs InErrs OutRsts\nTcp: 10 20 1 2\nUdp: InDatagrams OutDatagrams InErrors NoPorts\nUdp: 5 6 3 4\nIcmp: InMsgs OutMsgs InErrors OutErrors\nIcmp: 1 2 0 0\n'
fi
`
			require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700))
			ctx := context.Background()
			if test.deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
			}
			stats, err := (&Server{Environment: env}).GetProtocolStats(ctx)
			if test.deadline {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			} else if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)
			} else {
				require.NoError(t, err)
				if test.wantStats {
					require.Equal(t, uint64(40), stats.Total.InPackets)
					require.Equal(t, uint64(50), stats.Total.OutPackets)
					require.Equal(t, uint64(10), stats.TCP.InPackets)
					require.Equal(t, uint64(6), stats.UDP.OutDatagrams)
					require.Equal(t, uint64(24), stats.Other.InPackets)
					require.Equal(t, uint64(22), stats.Other.OutPackets)
				} else {
					require.Equal(t, &ProtocolStats{}, stats, "never return a partial snapshot after the container stops")
					data, err := json.Marshal(stats)
					require.NoError(t, err)
					require.JSONEq(t, `{
						"total":{"in_packets":0,"out_packets":0},
						"tcp":{"in_packets":0,"out_packets":0,"in_errors":0,"out_errors":0},
						"udp":{"in_datagrams":0,"out_datagrams":0,"in_errors":0,"no_ports":0},
						"icmp":{"in_msgs":0,"out_msgs":0,"in_errors":0,"out_errors":0},
						"other":{"in_packets":0,"out_packets":0}
					}`, string(data))
				}
			}
			data, readErr := os.ReadFile(calls)
			if test.responses[0] != running {
				require.ErrorIs(t, readErr, os.ErrNotExist, "offline containers must not execute collection commands")
			} else {
				require.NoError(t, readErr)
				require.Contains(t, string(data), "exec original-container cat /proc/net/dev")
				require.NotContains(t, string(data), "stats-server", "pin both reads to the inspected container ID")
				if test.wantStats {
					require.Contains(t, string(data), "exec original-container cat /proc/net/snmp")
				}
			}
		})
	}
}
