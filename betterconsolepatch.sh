#!/usr/bin/env bash
set -euo pipefail

RAW_BASE="${TOMAXIKZ_RAW_BASE:-https://raw.githubusercontent.com/Tomaxikz/daemon/develop}"
BACKUP_ROOT="${TOMAXIKZ_BACKUP_ROOT:-.tomaxikz-betterconsole-backups}"
BACKUP_DIR="${BACKUP_ROOT}/$(date -u +%Y%m%dT%H%M%SZ)"

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
    BOLD="$(printf '\033[1m')"
    DIM="$(printf '\033[2m')"
    RESET="$(printf '\033[0m')"
    RED="$(printf '\033[31m')"
    GREEN="$(printf '\033[32m')"
    YELLOW="$(printf '\033[33m')"
    BLUE="$(printf '\033[34m')"
    CYAN="$(printf '\033[36m')"
else
    BOLD=""
    DIM=""
    RESET=""
    RED=""
    GREEN=""
    YELLOW=""
    BLUE=""
    CYAN=""
fi

SPINNER_PID=""

log() {
    printf '%s[betterconsole]%s %s\n' "$CYAN" "$RESET" "$*"
}

ok() {
    printf '%s[OK]%s %s\n' "$GREEN" "$RESET" "$*"
}

warn() {
    printf '%s[WARN]%s %s\n' "$YELLOW" "$RESET" "$*"
}

section() {
    printf '\n%s==>%s %s%s%s\n' "$BLUE" "$RESET" "$BOLD" "$*" "$RESET"
}

fail() {
    printf '%s[ERROR]%s %s\n' "$RED" "$RESET" "$*" >&2
    exit 1
}

banner() {
    printf '%s%s%s\n' "$BOLD" "Better Console Wings installer" "$RESET"
    printf '%s%s%s\n' "$DIM" "Anchor-based installer for Tomaxikz daemon Better Console features" "$RESET"
}

start_spinner() {
    local message="$1"
    if [ ! -t 1 ] || [ -n "${NO_COLOR:-}" ]; then
        log "$message"
        return
    fi

    (
        local frames='|/-\'
        local i=0
        while :; do
            printf '\r%s[%s]%s %s' "$CYAN" "${frames:i++%${#frames}:1}" "$RESET" "$message"
            sleep 0.1
        done
    ) &
    SPINNER_PID="$!"
}

stop_spinner() {
    local status="$1"
    local message="$2"
    if [ -n "${SPINNER_PID:-}" ]; then
        kill "$SPINNER_PID" >/dev/null 2>&1 || true
        wait "$SPINNER_PID" 2>/dev/null || true
        SPINNER_PID=""
        printf '\r\033[K'
    fi

    case "$status" in
        ok) ok "$message" ;;
        warn) warn "$message" ;;
        *) fail "$message" ;;
    esac
}

run_with_spinner() {
    local message="$1"
    shift
    start_spinner "$message"
    if "$@"; then
        stop_spinner ok "$message"
    else
        stop_spinner error "$message failed"
    fi
}

cleanup_spinner() {
    if [ -n "${SPINNER_PID:-}" ]; then
        kill "$SPINNER_PID" >/dev/null 2>&1 || true
        wait "$SPINNER_PID" 2>/dev/null || true
    fi
}

trap cleanup_spinner EXIT

need_cmd() {
    command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

fetch_file() {
    local remote_path="$1"
    local local_path="$2"
    local url="${RAW_BASE%/}/${remote_path}"
    local tmp

    mkdir -p "$(dirname "$local_path")"
    case "$local_path" in
        *.go) tmp="$(mktemp --suffix=.go)" ;;
        *) tmp="$(mktemp)" ;;
    esac

    start_spinner "download ${remote_path}"
    curl -fsSL --retry 5 --retry-delay 1 --retry-all-errors -o "$tmp" "$url" || {
        rm -f "$tmp"
        if [ -f "$local_path" ]; then
            stop_spinner warn "could not download ${remote_path}; keeping existing ${local_path}"
            return
        fi
        stop_spinner error "failed to download ${url}"
    }
    if [ "${local_path%.go}" != "$local_path" ]; then
        gofmt -w "$tmp" || {
            rm -f "$tmp"
            stop_spinner error "downloaded Go file is not valid: ${url}"
        }
    fi

    if [ -f "$local_path" ] && cmp -s "$tmp" "$local_path"; then
        stop_spinner ok "unchanged ${local_path}"
        rm -f "$tmp"
        return
    fi

    if [ -f "$local_path" ]; then
        mkdir -p "${BACKUP_DIR}/$(dirname "$local_path")"
        cp -p "$local_path" "${BACKUP_DIR}/${local_path}"
        warn "backed up ${local_path}"
    fi

    mv "$tmp" "$local_path"
    stop_spinner ok "updated ${local_path}"
}

banner

[ -f go.mod ] || fail "run this from the Wings source root, where go.mod exists"
grep -q 'github.com/pterodactyl/wings' go.mod || fail "go.mod does not look like a Pterodactyl Wings module"

section "Checking requirements"
need_cmd curl
need_cmd python3
need_cmd go
need_cmd gofmt
ok "required commands are available"

section "Downloading Better Console files"
fetch_file "environment/docker/betterconsole_pull_progress.go" "environment/docker/betterconsole_pull_progress.go"
fetch_file "environment/docker/betterconsole_pull_progress_test.go" "environment/docker/betterconsole_pull_progress_test.go"
fetch_file "server/betterconsole_events.go" "server/betterconsole_events.go"
fetch_file "server/betterconsole_pull_progress.go" "server/betterconsole_pull_progress.go"
fetch_file "server/betterconsole_pull_progress_test.go" "server/betterconsole_pull_progress_test.go"
fetch_file "router/websocket/betterconsole_events.go" "router/websocket/betterconsole_events.go"
fetch_file "router/websocket/listeners_test.go" "router/websocket/listeners_test.go"
fetch_file "sftp/betterconsole_shell.go" "sftp/betterconsole_shell.go"

section "Applying anchor-based source edits"
export BCON_COLOR_RESET="$RESET"
export BCON_COLOR_GREEN="$GREEN"
export BCON_COLOR_YELLOW="$YELLOW"
export BCON_COLOR_RED="$RED"
python3 - "$BACKUP_DIR" <<'PY'
from pathlib import Path
import os
import shutil
import sys

backup_dir = Path(sys.argv[1])
RESET = os.environ.get("BCON_COLOR_RESET", "")
GREEN = os.environ.get("BCON_COLOR_GREEN", "")
YELLOW = os.environ.get("BCON_COLOR_YELLOW", "")
RED = os.environ.get("BCON_COLOR_RED", "")


def ok(message):
    print(f"{GREEN}[OK]{RESET} {message}")


def fail(message):
    print(f"{RED}[ERROR]{RESET} {message}", file=sys.stderr)
    sys.exit(1)


def backup(path):
    target = backup_dir / path
    if target.exists():
        return
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(path, target)


def read_text(path_name):
    path = Path(path_name)
    if not path.exists():
        fail(f"required file is missing: {path_name}")
    return path, path.read_text()


def write_text(path, original, updated, description):
    if updated == original:
        return False
    backup(path)
    path.write_text(updated)
    ok(f"patched {path}: {description}")
    return True


def replace_once(path_name, old, new, present, description):
    path, text = read_text(path_name)
    if present in text:
        ok(f"already patched {path_name}: {description}")
        return False
    if old not in text:
        fail(f"could not find expected source in {path_name} for {description}")
    return write_text(path, text, text.replace(old, new, 1), description)


def insert_after(path_name, anchor, addition, present, description):
    path, text = read_text(path_name)
    if present in text:
        ok(f"already patched {path_name}: {description}")
        return False
    if anchor not in text:
        fail(f"could not find anchor in {path_name} for {description}")
    return write_text(path, text, text.replace(anchor, anchor + addition, 1), description)


def insert_before(path_name, anchor, addition, present, description):
    path, text = read_text(path_name)
    if present in text:
        ok(f"already patched {path_name}: {description}")
        return False
    if anchor not in text:
        fail(f"could not find anchor in {path_name} for {description}")
    return write_text(path, text, text.replace(anchor, addition + anchor, 1), description)


def remove_once(path_name, old, description):
    path, text = read_text(path_name)
    if old not in text:
        ok(f"already clean {path_name}: {description}")
        return False
    return write_text(path, text, text.replace(old, "", 1), description)


sftp_shell_field = '''\
	// Shell controls the optional interactive SSH shell exposed through the SFTP listener.
	Shell SftpShellConfiguration `yaml:"shell"`
'''
sftp_shell_type = '''\

type SftpShellConfiguration struct {
	// Enabled allows authenticated SFTP users to open the Better Console SSH CLI.
	Enabled bool `default:"false" yaml:"enabled"`
}
'''
insert_after(
    "config/config.go",
    '	ReadOnly bool `default:"false" yaml:"read_only"`\n',
    sftp_shell_field,
    "Shell SftpShellConfiguration",
    "SFTP shell config field",
)
insert_after(
    "config/config.go",
    "type SftpConfiguration struct {\n",
    "",
    "type SftpShellConfiguration struct",
    "SFTP shell config type check",
)
if "type SftpShellConfiguration struct" not in Path("config/config.go").read_text():
    insert_after(
        "config/config.go",
        "}\n\n// ApiConfiguration defines the configuration for the internal API that is\n",
        sftp_shell_type,
        "type SftpShellConfiguration struct",
        "SFTP shell config type",
    )

remove_once(
    "environment/docker/container.go",
    '	"github.com/buger/jsonparser"\n',
    "unused jsonparser import",
)
replace_once(
    "environment/docker/container.go",
    '''\
func (e *Environment) ensureImageExists(img string) error {
	e.Events().Publish(environment.DockerImagePullStarted, "")
	defer e.Events().Publish(environment.DockerImagePullCompleted, "")

''',
    '''\
func (e *Environment) ensureImageExists(img string) error {
''',
    "safeImage := betterConsoleDockerPullSafeImageRef(img)",
    "move Docker pull events after successful pull",
)
insert_after(
    "environment/docker/container.go",
    "	out, err := e.client.ImagePull(ctx, img, imagePullOptions)\n",
    "	safeImage := betterConsoleDockerPullSafeImageRef(img)\n",
    "safeImage := betterConsoleDockerPullSafeImageRef(img)",
    "safe image name for server image pulls",
)
replace_once(
    "environment/docker/container.go",
    '					"image":        img,\n',
    '					"image":        safeImage,\n',
    '"image":        safeImage',
    "sanitized fallback image log",
)
replace_once(
    "environment/docker/container.go",
    '		return errors.Wrapf(err, "environment/docker: failed to pull \\"%s\\" image for server", img)\n',
    '		return errors.Wrapf(err, "environment/docker: failed to pull \\"%s\\" image for server", safeImage)\n',
    "failed to pull \\\"%s\\\" image for server\", safeImage",
    "sanitized pull error",
)
replace_once(
    "environment/docker/container.go",
    '	log.WithField("image", img).Debug("pulling docker image... this could take a bit of time")\n',
    '''\
	e.Events().Publish(environment.DockerImagePullStarted, "")
	defer e.Events().Publish(environment.DockerImagePullCompleted, "")

	log.WithField("image", safeImage).Debug("pulling docker image... this could take a bit of time")
''',
    'log.WithField("image", safeImage).Debug("pulling docker image... this could take a bit of time")',
    "structured Docker pull start",
)
replace_once(
    "environment/docker/container.go",
    '''\
	for scanner.Scan() {
		b := scanner.Bytes()
		status, _ := jsonparser.GetString(b, "status")
		progress, _ := jsonparser.GetString(b, "progress")

		e.Events().Publish(environment.DockerImagePullStatus, status+" "+progress)
	}
''',
    '''\
	for scanner.Scan() {
		if payload := betterConsoleDockerPullProgress(img, scanner.Bytes()); payload != "" {
			e.Events().Publish(environment.DockerImagePullStatus, payload)
		}
	}
''',
    "betterConsoleDockerPullProgress(img, scanner.Bytes())",
    "structured Docker pull progress",
)
replace_once(
    "environment/docker/container.go",
    '	log.WithField("image", img).Debug("completed docker image pull")\n',
    '	log.WithField("image", safeImage).Debug("completed docker image pull")\n',
    'log.WithField("image", safeImage).Debug("completed docker image pull")',
    "sanitized Docker pull completion log",
)

insert_after(
    "server/install.go",
    "	r, err := ip.client.ImagePull(ip.Server.Context(), ip.Script.ContainerImage, imagePullOptions)\n",
    "	safeImage := betterConsoleDockerPullSafeImageRef(ip.Script.ContainerImage)\n",
    "safeImage := betterConsoleDockerPullSafeImageRef(ip.Script.ContainerImage)",
    "safe image name for installer image pulls",
)
replace_once(
    "server/install.go",
    '					"image": ip.Script.ContainerImage,\n',
    '					"image": safeImage,\n',
    '"image": safeImage',
    "sanitized installer fallback image log",
)
replace_once(
    "server/install.go",
    '	log.WithField("image", ip.Script.ContainerImage).Debug("pulling docker image... this could take a bit of time")\n',
    '''\
	log.WithField("image", safeImage).Debug("pulling docker image... this could take a bit of time")
	ip.Server.Events().Publish(ImagePullStartedEvent, "")
	defer ip.Server.Events().Publish(ImagePullCompletedEvent, "")
''',
    'ip.Server.Events().Publish(ImagePullStartedEvent, "")',
    "installer image pull start events",
)
replace_once(
    "server/install.go",
    '''\
	for scanner.Scan() {
		log.Debug(scanner.Text())
	}
''',
    '''\
	for scanner.Scan() {
		log.Debug(betterConsoleDockerPullSafeText(scanner.Text()))
		payload := betterConsoleDockerPullProgress(ip.Script.ContainerImage, scanner.Bytes())
		if payload != "" {
			ip.Server.Events().Publish(ImagePullProgressEvent, payload)
		}
		if line := betterConsoleDockerPullInstallLine(scanner.Bytes()); line != "" {
			ip.Server.Sink(system.InstallSink).Push([]byte(line))
		}
	}
''',
    "betterConsoleDockerPullProgress(ip.Script.ContainerImage, scanner.Bytes())",
    "installer image pull progress",
)
replace_once(
    "server/install.go",
    "	err = system.ScanReader(reader, ip.Server.Sink(system.InstallSink).Push)\n",
    "	err = ip.streamRawInstallOutput(reader)\n",
    "ip.streamRawInstallOutput(reader)",
    "raw installer output streaming",
)
insert_before(
    "server/install.go",
    "// resourceLimits returns resource limits for the installation container. This\n",
    '''\
func (ip *InstallationProcess) streamRawInstallOutput(reader io.Reader) error {
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			ip.Server.Sink(system.InstallSink).Push(chunk)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return err
		}
	}
}

''',
    "func (ip *InstallationProcess) streamRawInstallOutput",
    "raw installer output helper",
)

replace_once(
    "server/listeners.go",
    '''\
					case environment.DockerImagePullStatus:
						s.Events().Publish(InstallOutputEvent, e.Data)
					case environment.DockerImagePullStarted:
						s.PublishConsoleOutputFromDaemon("Pulling Docker container image, this could take a few minutes to complete...")
					case environment.DockerImagePullCompleted:
						s.PublishConsoleOutputFromDaemon("Finished pulling Docker container image")
''',
    '''\
					case environment.DockerImagePullStatus:
						s.Events().Publish(ImagePullProgressEvent, e.Data)
						if str, ok := e.Data.(string); ok {
							if line := betterConsoleDockerPullInstallLine([]byte(str)); line != "" {
								s.Sink(system.LogSink).Push([]byte(line))
							}
						}
					case environment.DockerImagePullStarted:
						s.Events().Publish(ImagePullStartedEvent, "")
						s.PublishConsoleOutputFromDaemon("Pulling Docker container image, this could take a few minutes to complete...")
					case environment.DockerImagePullCompleted:
						s.Events().Publish(ImagePullCompletedEvent, "")
						s.PublishConsoleOutputFromDaemon("Finished pulling Docker container image")
''',
    "s.Events().Publish(ImagePullProgressEvent, e.Data)",
    "server image pull events",
)

insert_after(
    "router/websocket/listeners.go",
    "import (\n\t\"context\"\n\t\"encoding/json\"\n",
    "\t\"strings\"\n",
    '"strings"',
    "strings import for event guard",
)

path, text = read_text("router/websocket/listeners.go")
if "betterConsoleListenerEvents" in text:
    ok("already patched router/websocket/listeners.go: Better Console event list")
elif "serverImporterListenerEvents" in text and "}, serverImporterListenerEvents...)" in text:
    text2 = text.replace("}, serverImporterListenerEvents...)", "}, append(betterConsoleListenerEvents, serverImporterListenerEvents...)...)", 1)
    write_text(path, text, text2, "Better Console event list with existing importer events")
elif "var e = []string{" in text:
    text2 = text.replace("var e = []string{", "var e = append([]string{", 1)
    text2 = text2.replace('	server.TransferStatusEvent,\n}\n', '	server.TransferStatusEvent,\n}, betterConsoleListenerEvents...)\n', 1)
    if text2 == text:
        fail("could not find event list end in router/websocket/listeners.go")
    write_text(path, text, text2, "Better Console event list")
else:
    fail("could not identify websocket event list shape in router/websocket/listeners.go")

insert_after(
    "router/websocket/listeners.go",
    "}, betterConsoleListenerEvents...)\n",
    '''\

var allowedServerEvents = func() map[string]struct{} {
	events := make(map[string]struct{}, len(e))
	for _, event := range e {
		events[event] = struct{}{}
	}
	return events
}()
''',
    "var allowedServerEvents = func() map[string]struct{}",
    "allowed websocket event guard map",
)
if "append(betterConsoleListenerEvents, serverImporterListenerEvents...)...)" in Path("router/websocket/listeners.go").read_text():
    insert_after(
        "router/websocket/listeners.go",
        "}, append(betterConsoleListenerEvents, serverImporterListenerEvents...)...)\n",
        '''\

var allowedServerEvents = func() map[string]struct{} {
	events := make(map[string]struct{}, len(e))
	for _, event := range e {
		events[event] = struct{}{}
	}
	return events
}()
''',
        "var allowedServerEvents = func() map[string]struct{}",
        "allowed websocket event guard map",
    )

insert_after(
    "router/websocket/listeners.go",
    '''\
	eventChan := make(chan []byte)
	logOutput := make(chan []byte, 8)
	installOutput := make(chan []byte, 4)
''',
    '''\
	canReceiveInstall := false
	if jwt := h.GetJwt(); jwt != nil {
		canReceiveInstall = jwt.HasPermission(PermissionReceiveInstall)
	}
''',
    "canReceiveInstall := false",
    "install event permission flag",
)
replace_once(
    "router/websocket/listeners.go",
    '''\
	h.server.Events().On(eventChan) // TODO: make a sinky
	h.server.Sink(system.LogSink).On(logOutput)
	h.server.Sink(system.InstallSink).On(installOutput)
''',
    '''\
	h.server.Events().On(eventChan) // TODO: make a sinky
	h.server.Sink(system.LogSink).On(logOutput)
	if canReceiveInstall {
		h.server.Sink(system.InstallSink).On(installOutput)
	}
''',
    "if canReceiveInstall {\n\t\th.server.Sink(system.InstallSink).On(installOutput)",
    "permission-aware install sink registration",
)
insert_after(
    "router/websocket/listeners.go",
    '''\
			if err := events.DecodeTo(b, &e); err != nil {
				continue
			}
''',
    '''\
			if _, ok := allowedServerEvents[e.Topic]; !ok && !strings.HasPrefix(e.Topic, server.BackupCompletedEvent+":") {
				continue
			}
''',
    "allowedServerEvents[e.Topic]",
    "unknown websocket event guard",
)
replace_once(
    "router/websocket/listeners.go",
    '''\
	h.server.Events().Off(eventChan)
	h.server.Sink(system.LogSink).Off(logOutput)
	h.server.Sink(system.InstallSink).Off(installOutput)
''',
    '''\
	h.server.Events().Off(eventChan)
	h.server.Sink(system.LogSink).Off(logOutput)
	if canReceiveInstall {
		h.server.Sink(system.InstallSink).Off(installOutput)
	}
''',
    "if canReceiveInstall {\n\t\th.server.Sink(system.InstallSink).Off(installOutput)",
    "permission-aware install sink cleanup",
)
replace_once(
    "router/websocket/websocket.go",
    "		if v.Event == server.InstallOutputEvent {\n",
    "		if isBetterConsoleInstallOutputEvent(v.Event) {\n",
    "isBetterConsoleInstallOutputEvent(v.Event)",
    "Better Console install-output permission check",
)

insert_after(
    "sftp/server.go",
    '	"strings"\n',
    '	"time"\n',
    '"time"',
    "SFTP shell timeout import",
)
insert_after(
    "sftp/server.go",
    "var validUsernameRegexp = regexp.MustCompile(`^(?i)(.+)\\.([a-z0-9]{8})$`)\n",
    "\nconst sshHandshakeTimeout = 10 * time.Second\n",
    "const sshHandshakeTimeout",
    "SFTP handshake timeout constant",
)
insert_after(
    "sftp/server.go",
    '''\
type SFTPServer struct {
	manager  *server.Manager
	BasePath string
	ReadOnly bool
	Listen   string
}
''',
    '''\

type sshPtyRequest struct {
	Term   string
	Cols   uint32
	Rows   uint32
	Width  uint32
	Height uint32
}
''',
    "type sshPtyRequest struct",
    "SFTP shell PTY request type",
)
sftp_accept_old = '''\
func (c *SFTPServer) AcceptInbound(conn net.Conn, config *ssh.ServerConfig) error {
	// Before beginning a handshake must be performed on the incoming net.Conn
	sconn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return errors.WithStack(err)
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	for ch := range chans {
		// If its not a session channel we just move on because its not something we
		// know how to handle at this point.
		if ch.ChannelType() != "session" {
			_ = ch.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		channel, requests, err := ch.Accept()
		if err != nil {
			continue
		}

		go func(in <-chan *ssh.Request) {
			for req := range in {
				// Channels have a type that is dependent on the protocol. For SFTP
				// this is "subsystem" with a payload that (should) be "sftp". Discard
				// anything else we receive ("pty", "shell", etc)
				ok := req.Type == "subsystem" && len(req.Payload) >= 4 && string(req.Payload[4:]) == "sftp"
				_ = req.Reply(ok, nil)
			}
		}(requests)

		if srv, ok := c.manager.Get(sconn.Permissions.Extensions["uuid"]); ok {
			if err := c.Handle(sconn, srv, channel); err != nil {
				return err
			}
		}
	}

	return nil
}
'''
sftp_accept_new = '''\
func (c *SFTPServer) AcceptInbound(conn net.Conn, config *ssh.ServerConfig) error {
	// Before beginning a handshake must be performed on the incoming net.Conn
	_ = conn.SetDeadline(time.Now().Add(sshHandshakeTimeout))
	sconn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return errors.WithStack(err)
	}
	_ = conn.SetDeadline(time.Time{})
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	for ch := range chans {
		// If its not a session channel we just move on because its not something we
		// know how to handle at this point.
		if ch.ChannelType() != "session" {
			_ = ch.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		channel, requests, err := ch.Accept()
		if err != nil {
			continue
		}

		if srv, ok := c.manager.Get(sconn.Permissions.Extensions["uuid"]); ok {
			go c.handleSession(sconn, srv, channel, requests)
		} else {
			_ = channel.Close()
		}
	}

	return nil
}

func (c *SFTPServer) handleSession(conn *ssh.ServerConn, srv *server.Server, channel ssh.Channel, requests <-chan *ssh.Request) {
	var requestedPty sshPtyRequest

	for req := range requests {
		switch req.Type {
		case "subsystem":
			ok := len(req.Payload) >= 4 && string(req.Payload[4:]) == "sftp"
			_ = req.Reply(ok, nil)
			if ok {
				if err := c.Handle(conn, srv, channel); err != nil {
					srv.Log().WithField("user", conn.User()).WithError(err).Warn("sftp: failed to handle session")
				}
				return
			}
		case "pty-req":
			ok := config.Get().System.Sftp.Shell.Enabled
			if ok {
				handler, err := NewHandler(conn, srv)
				ok = err == nil && handler.can("control.console")
				if ok {
					_ = ssh.Unmarshal(req.Payload, &requestedPty)
				}
			}
			_ = req.Reply(ok, nil)
		case "shell":
			ok := config.Get().System.Sftp.Shell.Enabled
			if ok {
				handler, err := NewHandler(conn, srv)
				if err != nil || !handler.can("control.console") {
					_ = req.Reply(false, nil)
					_ = channel.Close()
					return
				}
				_ = req.Reply(true, nil)
				if err := c.HandleShell(conn, srv, channel, requests, requestedPty, handler); err != nil {
					srv.Log().WithField("user", conn.User()).WithError(err).Warn("sftp: failed to handle shell session")
				}
				return
			}
			_ = req.Reply(false, nil)
		default:
			_ = req.Reply(false, nil)
		}
	}

	_ = channel.Close()
}
'''
replace_once(
    "sftp/server.go",
    sftp_accept_old,
    sftp_accept_new,
    "func (c *SFTPServer) handleSession",
    "Better Console SFTP shell session routing",
)
insert_before(
    "sftp/server.go",
    "// Generates a new ED25519 private key that is used for host authentication when\n",
    '''\
// HandleShell starts the optional Better Console SSH CLI for the authenticated user's server.
func (c *SFTPServer) HandleShell(conn *ssh.ServerConn, srv *server.Server, channel ssh.Channel, requests <-chan *ssh.Request, _ sshPtyRequest, handler *Handler) error {
	defer channel.Close()

	if !handler.can("control.console") {
		_, _ = io.WriteString(channel, "Permission denied: control.console\\r\\n")
		return nil
	}

	go func() {
		for req := range requests {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}()

	c.serveBetterConsoleCli(channel, srv, handler, conn.RemoteAddr().String())

	return nil
}

''',
    "func (c *SFTPServer) HandleShell",
    "Better Console SFTP shell handler",
)
replace_once(
    "sftp/server.go",
    "	if err := os.MkdirAll(path.Dir(c.PrivateKeyPath()), 0o755); err != nil {\n",
    "	if err := os.MkdirAll(path.Dir(c.PrivateKeyPath()), 0o700); err != nil {\n",
    "MkdirAll(path.Dir(c.PrivateKeyPath()), 0o700)",
    "private SFTP key directory permissions",
)
PY

section "Formatting and building"
run_with_spinner "format Go files" gofmt -w \
    config/config.go \
    environment/docker/betterconsole_pull_progress.go \
    environment/docker/betterconsole_pull_progress_test.go \
    environment/docker/container.go \
    router/websocket/betterconsole_events.go \
    router/websocket/listeners.go \
    router/websocket/listeners_test.go \
    router/websocket/websocket.go \
    server/betterconsole_events.go \
    server/betterconsole_pull_progress.go \
    server/betterconsole_pull_progress_test.go \
    server/install.go \
    server/listeners.go \
    sftp/betterconsole_shell.go \
    sftp/server.go
run_with_spinner "build Wings" go build

section "Done"
ok "Better Console Wings edits are installed and the build succeeded."
if [ -d "$BACKUP_DIR" ]; then
    log "backups, if any, are in: ${BACKUP_DIR}"
fi
