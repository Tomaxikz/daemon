package sftp

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/pterodactyl/wings/system"
	"github.com/stretchr/testify/require"
)

func TestValidSftpUsername(t *testing.T) {
	require.True(t, validSftpUsername("user.1234abcd"))
	require.True(t, validSftpUsername("user-name_2.abcdef12"))
	require.False(t, validSftpUsername("missing-server"))
	require.False(t, validSftpUsername("user.1234567"))
	require.False(t, validSftpUsername("user.123456789"))
	require.False(t, validSftpUsername("user\nname.1234abcd"))
	require.False(t, validSftpUsername(strings.Repeat("a", maxSftpUsernameBytes)+".1234abcd"))
	require.False(t, validSftpUsername(string([]byte{'u', 0xff, '.', '1', '2', '3', '4', 'a', 'b', 'c', 'd'})))
}

func TestSSHSessionLimiter(t *testing.T) {
	var limiter sshSessionLimiter
	for range maxSSHSessionChannels {
		require.True(t, limiter.acquire())
	}
	require.False(t, limiter.acquire())
	limiter.release()
	require.True(t, limiter.acquire())
	require.Equal(t, int32(maxSSHSessionChannels), limiter.active.Load())
}

func TestSftpSessionClosesOnUserRevocation(t *testing.T) {
	bag := system.NewContextBag(context.Background())
	closed := make(chan struct{})
	stop := closeSftpSessionOnRevocation(bag.Context("user-uuid"), func() { close(closed) })
	defer stop()

	bag.Cancel("user-uuid")
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("session was not closed after user revocation")
	}
}

func TestBetterConsolePreservesAnsiColors(t *testing.T) {
	input := "safe\x1b[31mred\x1b[38;2;10;20;30mtruecolor\x1b[0mtext"
	require.Equal(t, input+"\x1b[0m", betterConsoleSanitizeTerminalLine(input))

	c1Input := "\u009b38;5;42mcolor"
	require.Equal(t, "\x1b[38;5;42mcolor\x1b[0m", betterConsoleSanitizeTerminalLine(c1Input))
}

func TestBetterConsoleStripsUnsafeTerminalControls(t *testing.T) {
	input := "safe\x1b]52;c;Y2xpcGJvYXJk\aafter\u009d0;title\u009c\x1b[2Jdone\x7f"
	output := betterConsoleSanitizeTerminalLine(input)
	require.Equal(t, "safeafterdone", output)
	for _, r := range output {
		require.Falsef(t, unicode.IsControl(r), "terminal output contains control rune U+%04X", r)
	}
}
