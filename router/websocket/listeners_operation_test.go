package websocket

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWebsocketEventArgsPreservesOperationWireArguments(t *testing.T) {
	args, ok := websocketEventArgs([]interface{}{"operation-id", `{"type":"copy_many"}`})
	require.True(t, ok)
	require.Equal(t, []string{"operation-id", `{"type":"copy_many"}`}, args)
	_, ok = websocketEventArgs([]interface{}{"operation-id", 1})
	require.False(t, ok)
}
