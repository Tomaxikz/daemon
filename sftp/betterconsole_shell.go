package sftp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/internal/models"
	"github.com/pterodactyl/wings/server"
	"github.com/pterodactyl/wings/system"
)

const betterConsoleShellCliName = ".wings"
const betterConsoleShellMaxLineBytes = 4096

func (c *SFTPServer) serveBetterConsoleCli(channel ssh.Channel, srv *server.Server, handler *Handler, ip string) {
	logOutput := make(chan []byte, 128)
	installOutput := make(chan []byte, 32)
	eventOutput := make(chan []byte, 32)
	done := make(chan struct{})
	canReceiveInstall := handler.can("admin.websocket.install")
	activity := srv.NewRequestActivity(handler.User(), ip)

	srv.Sink(system.LogSink).On(logOutput)
	if canReceiveInstall {
		srv.Sink(system.InstallSink).On(installOutput)
	}
	srv.Events().On(eventOutput)
	defer srv.Sink(system.LogSink).Off(logOutput)
	if canReceiveInstall {
		defer srv.Sink(system.InstallSink).Off(installOutput)
	}
	defer srv.Events().Off(eventOutput)

	term := &betterConsoleSshTerminal{channel: channel}
	term.writeLine("Better Console live server console")
	term.writeLine("Connected to Wings. Type a server command and press enter.")
	term.writeLine("Type \"" + betterConsoleShellCliName + " help\" for daemon commands or \"exit\" to close this SSH console session.")
	if lines, err := srv.Environment.Readlog(75); err == nil && len(lines) > 0 {
		for _, line := range lines {
			term.writeHistory(line)
		}
	}
	term.prompt()

	go func() {
		for {
			select {
			case <-done:
				return
			case line, ok := <-logOutput:
				if !ok {
					logOutput = nil
					continue
				}
				term.writeConsole(string(line))
			case line, ok := <-installOutput:
				if !ok {
					installOutput = nil
					continue
				}
				term.writeConsole(string(line))
			case raw, ok := <-eventOutput:
				if !ok {
					eventOutput = nil
					continue
				}
				var e events.Event
				if err := events.DecodeTo(raw, &e); err != nil {
					continue
				}
				if e.Topic == server.InstallOutputEvent && !canReceiveInstall {
					continue
				}
				if e.Topic != server.ConsoleOutputEvent && e.Topic != server.DaemonMessageEvent && e.Topic != server.InstallOutputEvent {
					continue
				}
				term.writeConsole(betterConsoleEventString(e.Data))
			}
		}
	}()

	reader := bufio.NewReader(channel)
	for {
		b, err := reader.ReadByte()
		if err != nil {
			close(done)
			if err != io.EOF {
				term.writeLine("Session closed.")
			}
			return
		}

		switch b {
		case '\r', '\n':
			line := strings.TrimSpace(term.commitLine())
			if line == "" {
				term.prompt()
				continue
			}
			if line == "exit" || line == "quit" {
				close(done)
				term.writeLine("Disconnected from Better Console.")
				return
			}
			if strings.HasPrefix(line, betterConsoleShellCliName) {
				c.handleBetterConsoleShellCliCommand(term, srv, handler, activity, line)
			} else if !handler.can("control.console") {
				term.writeLine("Permission denied: control.console")
			} else if srv.IsInstalling() {
				term.writeLine("The server is currently installing.")
			} else if srv.Environment.State() == environment.ProcessOfflineState {
				term.writeLine("The server is currently offline.")
			} else if err := srv.Environment.SendCommand(line); err != nil {
				term.writeLine("Unable to send command: " + err.Error())
			} else {
				srv.SaveActivity(activity, server.ActivityConsoleCommand, models.ActivityMeta{"command": line})
			}
			term.prompt()
		case 0x03:
			term.cancelLine()
			term.prompt()
		case 0x04:
			close(done)
			term.writeLine("Disconnected from Better Console.")
			return
		case 0x7f, '\b':
			term.backspace()
		default:
			if b < 32 || (b >= 0x80 && b <= 0x9f) {
				continue
			}
			if !term.appendByte(b) {
				term.writeLine("Command line is too long.")
				term.clearLine()
				term.prompt()
			}
		}
	}
}

func (c *SFTPServer) handleBetterConsoleShellCliCommand(term *betterConsoleSshTerminal, srv *server.Server, handler *Handler, activity server.RequestActivity, line string) {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		term.writeLine("Usage: " + betterConsoleShellCliName + " <help|power|stats>")
		return
	}

	switch parts[1] {
	case "help":
		term.writeLine("Available commands:")
		term.writeLine("  " + betterConsoleShellCliName + " help                    Show this help message")
		term.writeLine("  " + betterConsoleShellCliName + " power start             Start the server")
		term.writeLine("  " + betterConsoleShellCliName + " power restart           Restart the server")
		term.writeLine("  " + betterConsoleShellCliName + " power stop              Stop the server")
		term.writeLine("  " + betterConsoleShellCliName + " power kill              Kill the server")
		term.writeLine("  " + betterConsoleShellCliName + " stats                   Show server statistics")
	case "power":
		c.handleBetterConsoleShellPower(term, srv, handler, activity, parts)
	case "stats":
		stats := srv.Proc()
		term.writeLine("Server Statistics:")
		term.writeLine(fmt.Sprintf("  Status: %s", srv.Environment.State()))
		term.writeLine(fmt.Sprintf("  CPU Usage: %.2f%%", stats.CpuAbsolute))
		term.writeLine(fmt.Sprintf("  Memory Usage: %s", betterConsoleFormatBytes(stats.Memory)))
		term.writeLine(fmt.Sprintf("  Disk Usage: %s", betterConsoleFormatSignedBytes(stats.Disk)))
		term.writeLine("  Network Usage:")
		term.writeLine(fmt.Sprintf("    Received: %s", betterConsoleFormatBytes(stats.Network.RxBytes)))
		term.writeLine(fmt.Sprintf("    Sent: %s", betterConsoleFormatBytes(stats.Network.TxBytes)))
	default:
		term.writeLine("Unknown daemon command. Type \"" + betterConsoleShellCliName + " help\".")
	}
}

func (c *SFTPServer) handleBetterConsoleShellPower(term *betterConsoleSshTerminal, srv *server.Server, handler *Handler, activity server.RequestActivity, parts []string) {
	if len(parts) < 3 {
		term.writeLine("Usage: " + betterConsoleShellCliName + " power <start|restart|stop|kill>")
		return
	}

	var action server.PowerAction
	var permission string
	switch parts[2] {
	case "start":
		action = server.PowerActionStart
		permission = "control.start"
	case "restart":
		action = server.PowerActionRestart
		permission = "control.restart"
	case "stop":
		action = server.PowerActionStop
		permission = "control.stop"
	case "kill":
		action = server.PowerActionTerminate
		permission = "control.stop"
	default:
		term.writeLine("Usage: " + betterConsoleShellCliName + " power <start|restart|stop|kill>")
		return
	}

	if !handler.can(permission) {
		term.writeLine("Permission denied: " + permission)
		return
	}

	if err := srv.HandlePowerAction(action); err != nil {
		term.writeLine("Unable to perform power action: " + err.Error())
		return
	}

	srv.SaveActivity(activity, models.Event(server.ActivityPowerPrefix+action), nil)
	term.writeLine("Power action sent: " + string(action))
}

type betterConsoleSshTerminal struct {
	channel ssh.Channel
	mu      sync.Mutex
	buffer  strings.Builder
}

func (t *betterConsoleSshTerminal) prompt() {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = io.WriteString(t.channel, "> ")
}

func (t *betterConsoleSshTerminal) writeLine(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	line = betterConsoleSanitizeTerminalLine(line)
	_, _ = fmt.Fprintf(t.channel, "\r\n%s\r\n", line)
}

func (t *betterConsoleSshTerminal) writeConsole(line string) {
	line = strings.TrimRight(line, "\r\n")
	line = betterConsoleSanitizeTerminalLine(line)
	if line == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	current := t.buffer.String()
	if current != "" {
		_, _ = io.WriteString(t.channel, "\r\n")
	}
	_, _ = fmt.Fprintf(t.channel, "%s\r\n", line)
	_, _ = io.WriteString(t.channel, "> ")
	if current != "" {
		_, _ = io.WriteString(t.channel, current)
	}
}

func (t *betterConsoleSshTerminal) writeHistory(line string) {
	line = strings.TrimRight(line, "\r\n")
	line = betterConsoleSanitizeTerminalLine(line)
	if line == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = fmt.Fprintf(t.channel, "%s\r\n", line)
}

func (t *betterConsoleSshTerminal) appendByte(b byte) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.buffer.Len() >= betterConsoleShellMaxLineBytes {
		return false
	}
	t.buffer.WriteByte(b)
	_, _ = t.channel.Write([]byte{b})
	return true
}

func (t *betterConsoleSshTerminal) backspace() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.buffer.Len() == 0 {
		return
	}
	line := t.buffer.String()
	t.buffer.Reset()
	t.buffer.WriteString(line[:len(line)-1])
	_, _ = io.WriteString(t.channel, "\b \b")
}

func (t *betterConsoleSshTerminal) commitLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	line := t.buffer.String()
	t.buffer.Reset()
	_, _ = io.WriteString(t.channel, "\r\n")
	return line
}

func (t *betterConsoleSshTerminal) cancelLine() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buffer.Reset()
	_, _ = io.WriteString(t.channel, "^C\r\n")
}

func (t *betterConsoleSshTerminal) clearLine() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buffer.Reset()
}

func betterConsoleEventString(data interface{}) string {
	switch v := data.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
}

func betterConsoleFormatBytes(value uint64) string {
	const unit = 1024
	if value < unit {
		if value == 1 {
			return "1 byte"
		}
		return fmt.Sprintf("%d bytes", value)
	}

	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	size := float64(value)
	unitIndex := -1
	for size >= unit && unitIndex < len(units)-1 {
		size /= unit
		unitIndex++
	}

	return fmt.Sprintf("%.2f %s (%d bytes)", size, units[unitIndex], value)
}

func betterConsoleFormatSignedBytes(value int64) string {
	if value < 0 {
		return fmt.Sprintf("%d bytes", value)
	}
	return betterConsoleFormatBytes(uint64(value))
}

func betterConsoleSanitizeTerminalLine(line string) string {
	var output strings.Builder
	hasSgr := false

	for i := 0; i < len(line); {
		if line[i] == '\x1b' {
			consumed, sgr := betterConsoleEscapeSequence(line[i:])
			if consumed == 0 {
				consumed = 1
			}
			if sgr != "" {
				output.WriteString(sgr)
				hasSgr = true
			}
			i += consumed
			continue
		}

		r, size := utf8.DecodeRuneInString(line[i:])
		if r == '\u009b' {
			consumed, sgr := betterConsoleCsiSequence(line[i+size:])
			if sgr != "" {
				output.WriteString(sgr)
				hasSgr = true
			}
			i += size + consumed
			continue
		}
		if betterConsoleIsControlStringStart(r) {
			i += size + betterConsoleControlStringLength(line[i+size:])
			continue
		}
		if r != utf8.RuneError || size != 1 {
			if r == '\t' || !unicode.IsControl(r) {
				output.WriteString(line[i : i+size])
			}
		}
		i += size
	}

	if hasSgr {
		output.WriteString("\x1b[0m")
	}
	return output.String()
}

func betterConsoleEscapeSequence(input string) (int, string) {
	if len(input) < 2 {
		return len(input), ""
	}

	switch input[1] {
	case '[':
		consumed, sgr := betterConsoleCsiSequence(input[2:])
		return 2 + consumed, sgr
	case ']', 'P', 'X', '^', '_':
		return 2 + betterConsoleControlStringLength(input[2:]), ""
	default:
		return 2, ""
	}
}

func betterConsoleCsiSequence(input string) (int, string) {
	for i := 0; i < len(input); i++ {
		b := input[i]
		if b < 0x40 || b > 0x7e {
			continue
		}

		params := input[:i]
		if b != 'm' || len(params) > 64 || !betterConsoleValidSgrParams(params) {
			return i + 1, ""
		}
		return i + 1, "\x1b[" + params + "m"
	}

	return len(input), ""
}

func betterConsoleValidSgrParams(params string) bool {
	for i := 0; i < len(params); i++ {
		if (params[i] < '0' || params[i] > '9') && params[i] != ';' && params[i] != ':' {
			return false
		}
	}
	return true
}

func betterConsoleIsControlStringStart(r rune) bool {
	return r == '\u0090' || r == '\u0098' || r == '\u009d' || r == '\u009e' || r == '\u009f'
}

func betterConsoleControlStringLength(input string) int {
	for i := 0; i < len(input); {
		if input[i] == '\a' {
			return i + 1
		}
		if input[i] == '\x1b' && i+1 < len(input) && input[i+1] == '\\' {
			return i + 2
		}
		r, size := utf8.DecodeRuneInString(input[i:])
		if r == '\u009c' {
			return i + size
		}
		i += size
	}
	return len(input)
}
