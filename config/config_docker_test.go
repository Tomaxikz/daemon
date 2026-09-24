package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

func TestDockerRegistryCredentialsForImage(t *testing.T) {
	cfg := DockerConfiguration{
		Registries: map[string]RegistryConfiguration{
			"registry.example.com": {
				Username: "registry",
				Password: "secret",
			},
			"registry.example.com/team": {
				Username: "team",
				Password: "secret",
			},
			"registry.example.com:5000": {
				Username: "port",
				Password: "secret",
			},
			"https://index.docker.io/v1/": {
				Username: "docker",
				Password: "secret",
			},
		},
	}

	tests := []struct {
		name     string
		image    string
		username string
	}{
		{
			name:     "registry domain",
			image:    "registry.example.com/project/image:latest",
			username: "registry",
		},
		{
			name:     "registry with port",
			image:    "registry.example.com:5000/project/image:latest",
			username: "port",
		},
		{
			name:     "registry path",
			image:    "registry.example.com/team/image:latest",
			username: "team",
		},
		{
			name:  "registry prefix is not domain",
			image: "registry.example.com.evil/project/image:latest",
		},
		{
			name:     "registry path prefix falls back to domain",
			image:    "registry.example.com/team-evil/image:latest",
			username: "registry",
		},
		{
			name:     "legacy docker hub registry",
			image:    "docker.io/library/busybox:latest",
			username: "docker",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, registry := cfg.RegistryCredentialsForImage(tt.image)
			if tt.username == "" {
				if registry != nil {
					t.Fatalf("expected no registry credentials, got username %q", registry.Username)
				}

				return
			}

			if registry == nil {
				t.Fatalf("expected registry credentials for %q", tt.image)
			}

			if registry.Username != tt.username {
				t.Fatalf("expected username %q, got %q", tt.username, registry.Username)
			}
		})
	}
}

func TestCpuPeriodMicroseconds(t *testing.T) {
	tests := []struct {
		name     string
		period   int64
		expected int64
	}{
		{
			name:     "default period",
			period:   100_000,
			expected: 100_000,
		},
		{
			name:     "shorter period",
			period:   20_000,
			expected: 20_000,
		},
		{
			name:     "below kernel minimum",
			period:   500,
			expected: 1_000,
		},
		{
			name:     "above kernel maximum",
			period:   5_000_000,
			expected: 1_000_000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DockerConfiguration{CpuPeriod: tt.period}
			if v := cfg.CpuPeriodMicroseconds(); v != tt.expected {
				t.Errorf("expected %d, got %d", tt.expected, v)
			}
		})
	}
}

func TestDockerRegistryPathCredentialsDoNotMatchSiblingPath(t *testing.T) {
	cfg := DockerConfiguration{
		Registries: map[string]RegistryConfiguration{
			"registry.example.com/team": {
				Username: "team",
				Password: "secret",
			},
		},
	}

	_, registry := cfg.RegistryCredentialsForImage("registry.example.com/team-evil/image:latest")
	if registry != nil {
		t.Fatalf("expected no registry credentials, got username %q", registry.Username)
	}
}

func TestNetworkPolicyDefaultsAndOverrides(t *testing.T) {
	for _, test := range []struct {
		name, input string
		want        NetworkPolicyConfiguration
	}{
		{"missing docker", "remote: https://panel.example.test\n", NetworkPolicyConfiguration{Enabled: true}},
		{"missing policy", "docker: {}\n", NetworkPolicyConfiguration{Enabled: true}},
		{"empty policy", "docker:\n  network_policy: {}\n", NetworkPolicyConfiguration{Enabled: true}},
		{"explicit disable", "docker:\n  network_policy:\n    enabled: false\n", NetworkPolicyConfiguration{}},
		{"partial policy", "docker:\n  network_policy:\n    max_upload_bps: 100000000\n", NetworkPolicyConfiguration{Enabled: true, MaxUploadBPS: 100_000_000}},
		{"runtime opt-in", "docker:\n  network_policy:\n    runtime: wings-nsm\n", NetworkPolicyConfiguration{Enabled: true, Runtime: "wings-nsm"}},
		{
			"custom values",
			"docker:\n  network_policy:\n    enabled: false\n    ipv6: true\n    max_upload_bps: 100000000\n    max_download_bps: 50000000\n",
			NetworkPolicyConfiguration{IPv6: true, MaxUploadBPS: 100_000_000, MaxDownloadBPS: 50_000_000},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
			require.NoError(t, err)
			require.NoError(t, yaml.Unmarshal([]byte(test.input), cfg))
			require.Equal(t, test.want, cfg.Docker.NetworkPolicy)

			require.NoError(t, WriteToDisk(cfg))
			data, err := os.ReadFile(cfg.path)
			require.NoError(t, err)
			var saved struct {
				Docker struct {
					Policy map[string]any `yaml:"network_policy"`
				} `yaml:"docker"`
			}
			require.NoError(t, yaml.Unmarshal(data, &saved))
			require.Len(t, saved.Docker.Policy, 5)
			require.Equal(t, test.want.Enabled, saved.Docker.Policy["enabled"])
			require.Equal(t, test.want.IPv6, saved.Docker.Policy["ipv6"])
			require.EqualValues(t, test.want.MaxUploadBPS, saved.Docker.Policy["max_upload_bps"])
			require.EqualValues(t, test.want.MaxDownloadBPS, saved.Docker.Policy["max_download_bps"])
			require.Equal(t, test.want.Runtime, saved.Docker.Policy["runtime"])

			reloaded, err := NewAtPath(cfg.path)
			require.NoError(t, err)
			require.NoError(t, yaml.Unmarshal(data, reloaded))
			require.Equal(t, test.want, reloaded.Docker.NetworkPolicy)
			require.Equal(t, cfg.PanelLocation, reloaded.PanelLocation)
		})
	}
}

func TestPanelJSONCannotChangeNetworkPolicyDefaults(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg, err := NewAtPath("")
		require.NoError(t, err)
		cfg.Docker.NetworkPolicy = NetworkPolicyConfiguration{
			Enabled:        enabled,
			IPv6:           true,
			MaxUploadBPS:   100_000_000,
			MaxDownloadBPS: 50_000_000,
			Runtime:        "wings-nsm",
		}
		want := cfg.Docker.NetworkPolicy
		require.NoError(t, json.Unmarshal([]byte(`{"docker":{"network_policy":{"enabled":true,"ipv6":false,"max_upload_bps":0,"max_download_bps":0,"runtime":"attacker"}}}`), cfg))
		require.Equal(t, want, cfg.Docker.NetworkPolicy)
		data, err := json.Marshal(cfg)
		require.NoError(t, err)
		require.NotContains(t, string(data), `"network_policy"`)
	}
}
