package system

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDaemonIdentityPreservesSystemFields(t *testing.T) {
	data, err := json.Marshal(Information{Daemon: "wings", Version: "test"})
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	require.JSONEq(t, `"wings"`, string(fields["daemon"]))
	for _, key := range []string{"version", "docker", "system"} {
		require.Contains(t, fields, key)
	}
}
