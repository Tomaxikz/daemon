#!/usr/bin/env bash
set -euo pipefail

RAW_BASE="${TOMAXIKZ_RAW_BASE:-https://raw.githubusercontent.com/Tomaxikz/daemon/develop}"
BACKUP_ROOT="${TOMAXIKZ_BACKUP_ROOT:-.tomaxikz-networkstats-backups}"
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
    printf '%s[networkstats]%s %s\n' "$CYAN" "$RESET" "$*"
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
    printf '%s%s\n' "$BOLD" "Network Statistics Wings installer" "$RESET"
    printf '%s%s%s\n' "$DIM" "Anchor-based installer for Tomaxikz daemon Network Statistics Manager" "$RESET"
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
    tmp="$(mktemp)"
    start_spinner "download ${remote_path}"
    curl -fsSL --retry 3 --retry-delay 1 -o "$tmp" "$url" || {
        rm -f "$tmp"
        stop_spinner error "failed to download ${url}"
    }

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

section "Downloading Network Statistics files"
fetch_file "router/nsm_router.go" "router/nsm_router.go"
fetch_file "server/protocol_stats.go" "server/protocol_stats.go"
fetch_file "server/protocol_monitor.go" "server/protocol_monitor.go"

section "Applying anchor-based source edits"
export NSM_COLOR_RESET="$RESET"
export NSM_COLOR_GREEN="$GREEN"
export NSM_COLOR_YELLOW="$YELLOW"
export NSM_COLOR_RED="$RED"
export NSM_COLOR_CYAN="$CYAN"
python3 - "$BACKUP_DIR" <<'PY'
from pathlib import Path
import os
import shutil
import sys

backup_dir = Path(sys.argv[1])
RESET = os.environ.get("NSM_COLOR_RESET", "")
GREEN = os.environ.get("NSM_COLOR_GREEN", "")
YELLOW = os.environ.get("NSM_COLOR_YELLOW", "")
RED = os.environ.get("NSM_COLOR_RED", "")
CYAN = os.environ.get("NSM_COLOR_CYAN", "")


def info(message):
    print(f"{CYAN}[patch]{RESET} {message}")


def ok(message):
    print(f"{GREEN}[OK]{RESET} {message}")


def warn(message):
    print(f"{YELLOW}[WARN]{RESET} {message}")


def fail(message):
    print(f"{RED}[ERROR]{RESET} {message}", file=sys.stderr)
    sys.exit(1)


def backup(path):
    target = backup_dir / path
    if target.exists():
        return
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(path, target)


def read_lines(path):
    p = Path(path)
    if not p.exists():
        fail(f"required file is missing: {path}")
    return p, p.read_text().splitlines(keepends=True)


def write_lines(path, original, updated):
    if updated == original:
        return False
    backup(path)
    path.write_text("".join(updated))
    ok(f"patched {path}")
    return True


def line_ending(lines):
    for line in lines:
        if line.endswith("\r\n"):
            return "\r\n"
    return "\n"


def leading_indent(line):
    return line[: len(line) - len(line.lstrip(" \t"))]


def insert_missing_after_anchor(
    path_name,
    anchor_contains,
    entries,
    description,
):
    path, lines = read_lines(path_name)
    text = "".join(lines)
    missing = [(key, code) for key, code in entries if key not in text]
    if not missing:
        ok(f"already patched {path_name}: {description}")
        return False

    anchor_index = None
    for i, line in enumerate(lines):
        if all(token in line for token in anchor_contains):
            anchor_index = i
            break
    if anchor_index is None:
        fail(f"could not find anchor in {path_name} for {description}: {' | '.join(anchor_contains)}")

    newline = line_ending(lines)
    indent = leading_indent(lines[anchor_index])
    insert = [f"{indent}{code}{newline}" for _, code in missing]
    updated = lines[: anchor_index + 1] + insert + lines[anchor_index + 1 :]
    return write_lines(path, lines, updated)


def add_route():
    path, lines = read_lines("router/router.go")
    text = "".join(lines)
    exact = 'server.GET("/stats/protocols", getServerProtocolStats)'
    if exact in text:
        ok("already patched router/router.go: protocol stats route")
        return False
    if '"/stats/protocols"' in text:
        fail('router/router.go already contains "/stats/protocols" but not the expected getServerProtocolStats route')

    anchor_index = None
    for i, line in enumerate(lines):
        if 'server.GET("/logs", getServerLogs)' in line:
            anchor_index = i
            break
    if anchor_index is None:
        fail('could not find server.GET("/logs", getServerLogs) in router/router.go')

    newline = line_ending(lines)
    indent = leading_indent(lines[anchor_index])
    updated = lines[: anchor_index + 1] + [f'{indent}{exact}{newline}'] + lines[anchor_index + 1 :]
    return write_lines(path, lines, updated)


changed = False
changed |= insert_missing_after_anchor(
    "environment/stats.go",
    ["TxBytes", 'json:"tx_bytes"'],
    [
        ("RxPackets", 'RxPackets uint64 `json:"rx_packets"`'),
        ("TxPackets", 'TxPackets uint64 `json:"tx_packets"`'),
        ("RxErrors", 'RxErrors uint64 `json:"rx_errors"`'),
        ("TxErrors", 'TxErrors uint64 `json:"tx_errors"`'),
        ("RxDropped", 'RxDropped uint64 `json:"rx_dropped"`'),
        ("TxDropped", 'TxDropped uint64 `json:"tx_dropped"`'),
    ],
    "network packet fields",
)
changed |= insert_missing_after_anchor(
    "environment/docker/stats.go",
    ["st.Network.TxBytes", "nw.TxBytes"],
    [
        ("st.Network.RxPackets", "st.Network.RxPackets += nw.RxPackets"),
        ("st.Network.TxPackets", "st.Network.TxPackets += nw.TxPackets"),
        ("st.Network.RxErrors", "st.Network.RxErrors += nw.RxErrors"),
        ("st.Network.TxErrors", "st.Network.TxErrors += nw.TxErrors"),
        ("st.Network.RxDropped", "st.Network.RxDropped += nw.RxDropped"),
        ("st.Network.TxDropped", "st.Network.TxDropped += nw.TxDropped"),
    ],
    "Docker network packet collection",
)
changed |= insert_missing_after_anchor(
    "server/resources.go",
    ["ru.Network.RxBytes", "= 0"],
    [
        ("ru.Network.TxPackets", "ru.Network.TxPackets = 0"),
        ("ru.Network.RxPackets", "ru.Network.RxPackets = 0"),
    ],
    "network packet reset",
)
changed |= add_route()

if not changed:
    warn("original Wings files were already patched")
PY

section "Formatting and building"
run_with_spinner "format Go files" gofmt -w \
    environment/stats.go \
    environment/docker/stats.go \
    router/router.go \
    router/nsm_router.go \
    server/protocol_stats.go \
    server/protocol_monitor.go \
    server/resources.go

run_with_spinner "build Wings" go build

section "Done"
ok "Network Statistics Wings edits are installed and the build succeeded."
log "Backups, if any, are in: ${BOLD}${BACKUP_DIR}${RESET}"
