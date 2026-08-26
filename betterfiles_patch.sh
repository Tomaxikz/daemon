#!/usr/bin/env bash
set -euo pipefail

RAW_BASE="${TOMAXIKZ_RAW_BASE:-https://raw.githubusercontent.com/Tomaxikz/daemon/develop}"
BACKUP_ROOT="${TOMAXIKZ_BACKUP_ROOT:-.tomaxikz-betterfiles-backups}"
BACKUP_DIR="${BACKUP_ROOT}/$(date -u +%Y%m%dT%H%M%SZ)"
SECURE_GO_TOOLCHAIN="go1.26.7"
SECURE_X_TEXT_VERSION="v0.39.0"

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
    printf '%s[betterfiles]%s %s\n' "$CYAN" "$RESET" "$*"
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
    printf '%s%s%s\n' "$BOLD" "Better Files Wings installer" "$RESET"
    printf '%s%s%s\n' "$DIM" "Anchor-based installer for Tomaxikz daemon Better Files features" "$RESET"
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

select_secure_go_toolchain() {
    export GOTOOLCHAIN="$SECURE_GO_TOOLCHAIN"
    local version
    version="$(go version)" || fail "could not start ${SECURE_GO_TOOLCHAIN}; install Go 1.21 or newer so it can download the patched toolchain"
    case "$version" in
        "go version ${SECURE_GO_TOOLCHAIN} "*) ;;
        *) fail "expected ${SECURE_GO_TOOLCHAIN}, but the selected toolchain reported: ${version}" ;;
    esac
    ok "using ${SECURE_GO_TOOLCHAIN}"
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

backup_local_file() {
    local local_path="$1"
    local target="${BACKUP_DIR}/${local_path}"

    [ -f "$local_path" ] || return
    [ -f "$target" ] && return
    mkdir -p "$(dirname "$target")"
    cp -p "$local_path" "$target"
}

banner

[ -f go.mod ] || fail "run this from the Wings source root, where go.mod exists"
grep -q 'github.com/pterodactyl/wings' go.mod || fail "go.mod does not look like a Pterodactyl Wings module"

section "Checking requirements"
need_cmd curl
need_cmd python3
need_cmd go
need_cmd gofmt
select_secure_go_toolchain
ok "required commands are available"

section "Downloading Better Files files"
fetch_file "environment/docker/client_accessor.go" "environment/docker/client_accessor.go"
fetch_file "router/betterfiles_entry.go" "router/betterfiles_entry.go"
fetch_file "router/betterfiles_paths.go" "router/betterfiles_paths.go"
fetch_file "router/betterfiles_paths_test.go" "router/betterfiles_paths_test.go"
fetch_file "router/betterfiles_test_helpers_test.go" "router/betterfiles_test_helpers_test.go"
fetch_file "router/betterfiles_http_test.go" "router/betterfiles_http_test.go"
fetch_file "router/middleware/middleware_test.go" "router/middleware/middleware_test.go"
fetch_file "router/router_cdn_stream.go" "router/router_cdn_stream.go"
fetch_file "router/router_download_directory.go" "router/router_download_directory.go"
fetch_file "router/router_download_directory_test.go" "router/router_download_directory_test.go"
fetch_file "router/router_download_directory_http_test.go" "router/router_download_directory_http_test.go"
fetch_file "router/router_download_helpers.go" "router/router_download_helpers.go"
fetch_file "router/router_download_helpers_test.go" "router/router_download_helpers_test.go"
fetch_file "router/router_file_operations.go" "router/router_file_operations.go"
fetch_file "router/router_file_operations_test.go" "router/router_file_operations_test.go"
fetch_file "router/router_openapi.go" "router/router_openapi.go"
fetch_file "router/router_openapi_test.go" "router/router_openapi_test.go"
fetch_file "router/router_resumable_upload.go" "router/router_resumable_upload.go"
fetch_file "router/router_resumable_upload_test.go" "router/router_resumable_upload_test.go"
fetch_file "router/router_system_config.go" "router/router_system_config.go"
fetch_file "router/router_system_config_test.go" "router/router_system_config_test.go"
fetch_file "router/router_server_archive_nbt.go" "router/router_server_archive_nbt.go"
fetch_file "router/router_server_betterfiles_collaboration.go" "router/router_server_betterfiles_collaboration.go"
fetch_file "router/router_server_files_revisions.go" "router/router_server_files_revisions.go"
fetch_file "router/router_server_files_copy_many.go" "router/router_server_files_copy_many.go"
fetch_file "router/router_server_files_copy_many_test.go" "router/router_server_files_copy_many_test.go"
fetch_file "router/router_server_files_fingerprints.go" "router/router_server_files_fingerprints.go"
fetch_file "router/router_server_files_fingerprints_test.go" "router/router_server_files_fingerprints_test.go"
fetch_file "router/router_server_files_largest.go" "router/router_server_files_largest.go"
fetch_file "router/router_server_files_largest_test.go" "router/router_server_files_largest_test.go"
fetch_file "router/router_server_files_rename.go" "router/router_server_files_rename.go"
fetch_file "router/router_server_files_search.go" "router/router_server_files_search.go"
fetch_file "router/router_server_files_search_v2.go" "router/router_server_files_search_v2.go"
fetch_file "router/router_server_files_search_v2_test.go" "router/router_server_files_search_v2_test.go"
fetch_file "router/router_server_git.go" "router/router_server_git.go"
fetch_file "router/router_server_git_test.go" "router/router_server_git_test.go"
fetch_file "router/websocket/betterfiles_collaboration.go" "router/websocket/betterfiles_collaboration.go"
fetch_file "router/websocket/betterfiles_collaboration_ot.go" "router/websocket/betterfiles_collaboration_ot.go"
fetch_file "router/websocket/betterfiles_collaboration_ot_test.go" "router/websocket/betterfiles_collaboration_ot_test.go"
fetch_file "router/websocket/file_collaboration_yjs.go" "router/websocket/file_collaboration_yjs.go"
fetch_file "router/websocket/file_collaboration_yjs_test.go" "router/websocket/file_collaboration_yjs_test.go"
fetch_file "router/websocket/listeners_operation_test.go" "router/websocket/listeners_operation_test.go"
fetch_file "server/file_operations.go" "server/file_operations.go"
fetch_file "server/file_operations_test.go" "server/file_operations_test.go"
fetch_file "server/file_history.go" "server/file_history.go"
fetch_file "server/file_history_test.go" "server/file_history_test.go"
fetch_file "server/filesystem/replace.go" "server/filesystem/replace.go"

section "Installing secure dependencies"
backup_local_file "go.mod"
backup_local_file "go.sum"
run_with_spinner "enforce patched Go minimum ${SECURE_GO_TOOLCHAIN}" go mod edit -go="${SECURE_GO_TOOLCHAIN#go}"
run_with_spinner "pin patched Unicode library ${SECURE_X_TEXT_VERSION}" go get "golang.org/x/text@${SECURE_X_TEXT_VERSION}"
run_with_spinner "pin operational transformation library" go get github.com/shiv248/operational-transformation-go@v1.0.0
run_with_spinner "pin Yjs-compatible CRDT library" go get github.com/reearth/ygo@v1.31.0
run_with_spinner "pin Better Files glob library" go get github.com/bmatcuk/doublestar/v4@v4.9.1
run_with_spinner "normalize Go dependencies" go mod tidy

section "Applying anchor-based source edits"
export BFM_COLOR_RESET="$RESET"
export BFM_COLOR_GREEN="$GREEN"
export BFM_COLOR_YELLOW="$YELLOW"
export BFM_COLOR_RED="$RED"
export BFM_COLOR_CYAN="$CYAN"
python3 - "$BACKUP_DIR" <<'PY'
from pathlib import Path
import os
import shutil
import sys

backup_dir = Path(sys.argv[1])
RESET = os.environ.get("BFM_COLOR_RESET", "")
GREEN = os.environ.get("BFM_COLOR_GREEN", "")
YELLOW = os.environ.get("BFM_COLOR_YELLOW", "")
RED = os.environ.get("BFM_COLOR_RED", "")
CYAN = os.environ.get("BFM_COLOR_CYAN", "")


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


def replace_if_present(path_name, old, new, description):
    path, text = read_text(path_name)
    if old not in text:
        return False
    return write_text(path, text, text.replace(old, new, 1), description)


def require_contains(path_name, expected, description):
    _, text = read_text(path_name)
    if expected not in text:
        fail(f"could not establish {description} in {path_name}")
    ok(f"verified {path_name}: {description}")


def route_conflict_guard(path_name, route, expected_handler):
    _, text = read_text(path_name)
    marker = f'"{route}"'
    if marker not in text:
        return
    expected = f'{marker}, {expected_handler}'
    if expected not in text:
        fail(f"{path_name} already contains route {route} with a different handler")


file_history_field = '''\
	// FileHistory controls bounded per-file revision storage for panel file edits.
	FileHistory FileHistoryConfiguration `json:"-" yaml:"file_history"`

'''

file_history_type = '''\
type FileHistoryConfiguration struct {
	Enabled             bool   `default:"true" yaml:"enabled"`
	ZstdLevel           int    `default:"12" yaml:"zstd_level"`
	AnchorInterval      uint64 `default:"4" yaml:"anchor_interval"`
	KeepChains          uint64 `default:"2" yaml:"keep_chains"`
	FileSizeCap         uint64 `default:"1048576" yaml:"file_size_cap"`
	PerFileDiskBudget   uint64 `default:"5242880" yaml:"per_file_disk_budget"`
	PerServerDiskBudget uint64 `default:"209715200" yaml:"per_server_disk_budget"`
}

'''

file_collaboration_field = '''\
	// FileCollaboration controls native Yjs collaborative editing limits.
	FileCollaboration FileCollaborationConfiguration `json:"-" yaml:"file_collaboration"`
'''

file_collaboration_type = '''\
type FileCollaborationConfiguration struct {
	Enabled     bool   `default:"true" yaml:"enabled"`
	FileSizeCap uint64 `default:"10485760" yaml:"file_size_cap"`
}

'''

insert_after(
    "config/config.go",
    '	BackupDirectory string `default:"/var/lib/pterodactyl/backups" json:"-" yaml:"backup_directory"`\n\n',
    file_history_field,
    "FileHistory FileHistoryConfiguration",
    "file history config field",
)
insert_before(
    "config/config.go",
    "type CrashDetection struct {\n",
    file_history_type,
    "type FileHistoryConfiguration struct",
    "file history config type",
)
insert_after(
    "config/config.go",
    '\tFileHistory FileHistoryConfiguration `json:"-" yaml:"file_history"`\n',
    file_collaboration_field,
    "FileCollaboration FileCollaborationConfiguration",
    "native collaboration config field",
)
insert_before(
    "config/config.go",
    "type CrashDetection struct {\n",
    file_collaboration_type,
    "type FileCollaborationConfiguration struct",
    "native collaboration config type",
)

for route, handler in [
    ("/download/stream", "getDownloadStream"),
    ("/api/system/config", "getSystemConfiguration"),
    ("/git/status", "getServerGitStatus"),
    ("/revisions", "getServerFileRevisions"),
    ("/search", "getServerFilesSearch"),
    ("/archive/list", "getServerArchiveList"),
    ("/collaboration/revoke", "postServerBetterFilesCollaborationRevoke"),
    ("/openapi.json", "getOpenAPI"),
    ("/download/directory", "getDownloadDirectory"),
    ("/copy-many", "postServerCopyMany"),
    ("/largest-directories", "getServerLargestDirectories"),
    ("/fingerprints", "getServerFileFingerprints"),
    ("/operations/:operation", "deleteServerFileOperation"),
]:
    route_conflict_guard("router/router.go", route, handler)

insert_after(
    "router/router.go",
    '	router.GET("/download/file", getDownloadFile)\n',
    '	router.GET("/download/stream", getDownloadStream)\n',
    'router.GET("/download/stream", getDownloadStream)',
    "stream download route",
)
insert_after(
    "router/router.go",
    '\trouter.Use(middleware.AttachServerManager(m), middleware.AttachApiClient(client))\n',
    '\trouter.GET("/openapi.json", getOpenAPI)\n',
    'router.GET("/openapi.json", getOpenAPI)',
    "OpenAPI capability route",
)
insert_after(
    "router/router.go",
    '\trouter.GET("/download/file", getDownloadFile)\n',
    '\trouter.GET("/download/directory", getDownloadDirectory)\n',
    'router.GET("/download/directory", getDownloadDirectory)',
    "directory download route",
)
insert_after(
    "router/router.go",
    '\trouter.POST("/upload/file", postServerUploadFiles)\n',
    '\trouter.HEAD("/upload/file", headServerUploadFile)\n',
    'router.HEAD("/upload/file", headServerUploadFile)',
    "resumable upload HEAD route",
)
insert_after(
    "router/router.go",
    '\trouter.POST("/upload/file", postServerUploadFiles)\n',
    '\trouter.PATCH("/upload/file", patchServerUploadFile)\n',
    'router.PATCH("/upload/file", patchServerUploadFile)',
    "resumable upload PATCH route",
)

insert_after(
    "router/router.go",
    '\tprotected.GET("/api/system", getSystemInformation)\n',
    '\tprotected.GET("/api/system/config", getSystemConfiguration)\n',
    'protected.GET("/api/system/config", getSystemConfiguration)',
    "native collaboration capability route",
)

git_routes = '''\
		git := server.Group("/git")
		{
			git.GET("/status", getServerGitStatus)
			git.POST("/install", postServerGitInstall)
			git.POST("/clone", postServerGitClone)
			git.POST("/pull", postServerGitPull)
			git.POST("/diff", postServerGitDiff)
		}
'''
insert_after(
    "router/router.go",
    '		server.POST("/commands", postServerCommands)\n',
    git_routes,
    'git.GET("/status", getServerGitStatus)',
    "Git API routes",
)

file_routes = '''\
			files.GET("/revisions", getServerFileRevisions)
			files.GET("/revisions/:revision", getServerFileRevision)
			files.POST("/revisions/:revision/restore", postServerFileRevisionRestore)
			files.GET("/search", getServerFilesSearch)
			files.GET("/archive/list", getServerArchiveList)
			files.POST("/archive/extract", postServerArchiveExtract)
'''
insert_after(
    "router/router.go",
    '			files.GET("/contents", getServerFileContents)\n',
    file_routes,
    'files.GET("/revisions", getServerFileRevisions)',
    "Better Files file routes",
)
insert_after(
    "router/router.go",
    '\t\t\tfiles.GET("/search", getServerFilesSearch)\n',
    '\t\t\tfiles.POST("/search", postServerFilesSearch)\n',
    'files.POST("/search", postServerFilesSearch)',
    "Better Files V2 search route",
)
replace_once(
    "router/router.go",
    '\t\t\tfiles.PUT("/rename", putServerRenameFiles)\n',
    '\t\t\tfiles.PUT("/rename", putServerRenameFilesSafe)\n',
    'files.PUT("/rename", putServerRenameFilesSafe)',
    "hardened mass rename handler",
)
insert_after(
    "router/router.go",
    '\t\t\tfiles.POST("/copy", postServerCopyFile)\n',
    '\t\t\tfiles.POST("/copy-many", postServerCopyMany)\n',
    'files.POST("/copy-many", postServerCopyMany)',
    "native bulk copy route",
)
insert_after(
    "router/router.go",
    '\t\t\tfiles.POST("/copy", postServerCopyFile)\n',
    '\t\t\tfiles.GET("/largest-directories", getServerLargestDirectories)\n',
    'files.GET("/largest-directories", getServerLargestDirectories)',
    "largest directories route",
)
insert_after(
    "router/router.go",
    '\t\t\tfiles.POST("/copy", postServerCopyFile)\n',
    '\t\t\tfiles.GET("/fingerprints", getServerFileFingerprints)\n',
    'files.GET("/fingerprints", getServerFileFingerprints)',
    "file fingerprints route",
)
insert_after(
    "router/router.go",
    '\t\t\tfiles.POST("/copy", postServerCopyFile)\n',
    '\t\t\tfiles.DELETE("/operations/:operation", deleteServerFileOperation)\n',
    'files.DELETE("/operations/:operation", deleteServerFileOperation)',
    "file operation cancellation route",
)
insert_after(
    "router/router.go",
    '			files.POST("/chmod", postServerChmodFile)\n',
    '			files.POST("/collaboration/revoke", postServerBetterFilesCollaborationRevoke)\n',
    'files.POST("/collaboration/revoke", postServerBetterFilesCollaborationRevoke)',
    "collaboration revoke route",
)

stream_cors = '''\

		if isStreamDownloadRequest(c) {
			c.Header("Access-Control-Allow-Headers", "Accept, Accept-Encoding, Authorization, Cache-Control, Content-Type, Content-Length, If-Match, If-Modified-Since, If-None-Match, If-Range, If-Unmodified-Since, Origin, Range, X-Real-IP, X-CSRF-Token")
			c.Header("Access-Control-Expose-Headers", "Accept-Ranges, Content-Disposition, Content-Encoding, Content-Length, Content-Range, Content-Type, ETag, Last-Modified, X-Content-Type-Options, X-Request-Id")
			c.Header("Vary", "Origin, Access-Control-Request-Headers")
		}
'''
replace_once(
    "router/middleware/middleware.go",
    '\t\tc.Header("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")\n',
    '\t\tc.Header("Access-Control-Allow-Methods", "GET, HEAD, POST, PATCH, PUT, DELETE, OPTIONS")\n',
    '"GET, HEAD, POST, PATCH, PUT, DELETE, OPTIONS"',
    "resumable upload HEAD CORS method",
)
replace_once(
    "router/middleware/middleware.go",
    '\t\tc.Header("Access-Control-Allow-Headers", "Accept, Accept-Encoding, Authorization, Cache-Control, Content-Type, Content-Length, Origin, X-Real-IP, X-CSRF-Token")\n',
    '''\
		c.Header("Access-Control-Allow-Headers", "Accept, Accept-Encoding, Authorization, Cache-Control, Content-Type, Content-Length, Origin, Upload-Complete, Upload-Length, Upload-Offset, X-Real-IP, X-CSRF-Token")
		if c.Request != nil && c.Request.URL != nil && c.Request.URL.Path == "/upload/file" {
			c.Header("Access-Control-Expose-Headers", "Retry-After, Upload-Offset, X-Request-Id")
		}
''',
    "Upload-Complete, Upload-Length, Upload-Offset",
    "resumable upload CORS headers",
)
insert_after(
    "router/middleware/middleware.go",
    '		c.Header("Access-Control-Allow-Headers", "Accept, Accept-Encoding, Authorization, Cache-Control, Content-Type, Content-Length, Origin, Upload-Complete, Upload-Length, Upload-Offset, X-Real-IP, X-CSRF-Token")\n',
    stream_cors,
    "If-Match, If-Modified-Since",
    "stream CORS headers",
)
replace_once(
    "router/middleware/middleware.go",
    '''\
		if allowPrivateNetwork {
			c.Header("Access-Control-Request-Private-Network", "true")
		}
''',
    '''\
		if allowPrivateNetwork {
			if isStreamDownloadRequest(c) {
				c.Header("Access-Control-Allow-Private-Network", "true")
			} else {
				c.Header("Access-Control-Request-Private-Network", "true")
			}
		}
''',
    "Access-Control-Allow-Private-Network",
    "stream private-network CORS header",
)
replace_if_present(
    "router/middleware/middleware.go",
    '''\
func isStreamDownloadRequest(c *gin.Context) bool {
	return c.Request != nil && c.Request.URL != nil && c.Request.URL.Path == "/download/stream"
}
''',
    '''\
func isStreamDownloadRequest(c *gin.Context) bool {
	if c.Request == nil || c.Request.URL == nil {
		return false
	}
	return c.Request.URL.Path == "/download/stream" || c.Request.URL.Path == "/download/directory"
}
''',
    "directory download CORS helper",
)
insert_before(
    "router/middleware/middleware.go",
    "// ServerExists will ensure that the requested server exists in this setup.\n",
    '''\
func isStreamDownloadRequest(c *gin.Context) bool {
	if c.Request == nil || c.Request.URL == nil {
		return false
	}
	return c.Request.URL.Path == "/download/stream" || c.Request.URL.Path == "/download/directory"
}

''',
    'Path == "/download/directory"',
    "stream CORS helper",
)

insert_before(
    "server/events.go",
    ")\n\n// Events returns the server's emitter instance.\n",
    '''\
	OperationProgressEvent  = "operation progress"
	OperationErrorEvent     = "operation error"
	OperationCompletedEvent = "operation completed"
''',
    "OperationProgressEvent",
    "file operation websocket events",
)
insert_after(
    "server/server.go",
    "\tfs *filesystem.Filesystem\n",
    "\tfileOperations *FileOperationManager\n",
    "fileOperations *FileOperationManager",
    "server file operation manager field",
)
insert_after(
    "server/server.go",
    "\ts.resources.State = system.NewAtomicString(environment.ProcessOfflineState)\n",
    "\ts.fileOperations = NewFileOperationManager(&s)\n",
    "s.fileOperations = NewFileOperationManager(&s)",
    "server file operation manager initialization",
)
insert_after(
    "server/server.go",
    "\ts.CtxCancel()\n",
    '''\
	if s.fileOperations != nil {
		s.fileOperations.CancelAll()
	}
''',
    "s.fileOperations.CancelAll()",
    "server file operation cancellation cleanup",
)
insert_before(
    "server/server.go",
    "// ID returns the UUID for the server instance.\n",
    '''\
// FileOperations returns the server-isolated manager for cancellable filesystem work.
func (s *Server) FileOperations() *FileOperationManager {
	return s.fileOperations
}

''',
    "func (s *Server) FileOperations()",
    "server file operation manager accessor",
)
insert_after(
    "router/websocket/listeners.go",
    "\tserver.TransferStatusEvent,\n",
    '''\
	server.OperationProgressEvent,
	server.OperationErrorEvent,
	server.OperationCompletedEvent,
''',
    "server.OperationProgressEvent",
    "file operation websocket listener allowlist",
)
replace_once(
    "router/websocket/listeners.go",
    '''\
			if str, ok := e.Data.(string); ok {
				message.Args = []string{str}
''',
    '''\
			if args, ok := websocketEventArgs(e.Data); ok {
				message.Args = args
			} else if str, ok := e.Data.(string); ok {
				message.Args = []string{str}
''',
    "if args, ok := websocketEventArgs(e.Data)",
    "multi-argument websocket event forwarding",
)
insert_before(
    "router/websocket/listeners.go",
    "// ListenForServerEvents will listen for different events happening on a server\n",
    '''\
func websocketEventArgs(data interface{}) ([]string, bool) {
	values, ok := data.([]interface{})
	if !ok {
		return nil, false
	}
	args := make([]string, len(values))
	for i, value := range values {
		arg, ok := value.(string)
		if !ok {
			return nil, false
		}
		args[i] = arg
	}
	return args, true
}

''',
    "func websocketEventArgs",
    "multi-argument websocket event helper",
)

insert_after(
    "router/websocket/websocket.go",
    '''\
	if m.Event != AuthenticationEvent {
		if err := h.TokenValid(); err != nil {
			h.unsafeSendJson(Message{
				Event: JwtErrorEvent,
				Args:  []string{err.Error()},
			})
			return nil
		}
	}
''',
    '''\

	if handled, err := h.HandleBetterFilesCollaboration(ctx, m); handled {
		return err
	}
''',
    "HandleBetterFilesCollaboration",
    "Better Files collaboration websocket handler",
)
insert_after(
    "router/websocket/websocket.go",
    '''\
	if handled, err := h.HandleBetterFilesCollaboration(ctx, m); handled {
		return err
	}
''',
    '''\
	if handled, err := h.HandleNativeFileCollaboration(ctx, m); handled {
		return err
	}
''',
    "HandleNativeFileCollaboration",
    "Wings-rs-compatible Yjs collaboration handler",
)
insert_after(
    "router/websocket/websocket.go",
    "\tlimiter      *LimiterBucket\n",
    "\tfileCollabCleanup sync.Once\n",
    "fileCollabCleanup sync.Once",
    "native collaboration disconnect guard",
)
replace_once(
    "router/websocket/websocket.go",
    "\tconn.SetReadLimit(4096)\n",
    '''\
	// Collaboration updates are chunked, but their base64 and JSON framing can
	// exceed the historical 4 KiB console-message limit. The router applies the
	// same 32 KiB bound before dispatching a decoded message.
	conn.SetReadLimit(32_768)
''',
    "conn.SetReadLimit(32_768)",
    "bounded collaboration websocket frame size",
)
insert_after(
    "router/tokens/websocket.go",
    '\tUserUUID    string   `json:"user_uuid"`\n',
    '''\
	UserName    string   `json:"user_name,omitempty"`
	UserAvatar  *string  `json:"user_avatar,omitempty"`
''',
    '`json:"user_name,omitempty"`',
    "Wings-rs-compatible participant identity claims",
)

insert_after(
    "router/websocket/limiter.go",
    "import (\n",
    '	"strings"\n',
    '"strings"',
    "collaboration limiter string helper import",
)
remove_once(
    "router/websocket/limiter.go",
    '''\
	// Better Files live collaboration snapshots need a small dedicated bucket.
	// Sharing Wings' default 4/sec bucket makes editors drift a character or two
	// behind during normal typing.
	if e == Event("betterfiles:collab:patch") {
		return rate.Every(time.Millisecond * 50), 40
	}
	if e == Event("betterfiles:collab:snapshot") {
		return rate.Every(time.Millisecond * 250), 12
	}
	if e == Event("betterfiles:collab:presence") {
		return rate.Every(time.Millisecond * 250), 12
	}
	if e == FileCollabUpdateEvent {
		return rate.Every(time.Millisecond * 25), 80
	}
	if e == FileCollabAwarenessEvent {
		return rate.Every(time.Millisecond * 100), 30
	}
	if IsNativeFileCollaborationEvent(e) {
		return rate.Every(time.Millisecond * 200), 10
	}
	if isBetterFilesCollaborationEvent(e) {
		return rate.Every(time.Millisecond * 200), 10
	}

''',
    "obsolete collaboration rate buckets",
)
replace_if_present(
    "router/websocket/limiter.go",
    "	if e == AuthenticationEvent || e == SendServerLogsEvent || e == SendCommandEvent || isBetterFilesCollaborationEvent(e) || IsNativeFileCollaborationEvent(e) {\n",
    "	if e == AuthenticationEvent || e == SendServerLogsEvent || e == SendCommandEvent {\n",
    "remove native collaboration limiter names",
)
replace_if_present(
    "router/websocket/limiter.go",
    "	if e == AuthenticationEvent || e == SendServerLogsEvent || e == SendCommandEvent || isBetterFilesCollaborationEvent(e) {\n",
    "	if e == AuthenticationEvent || e == SendServerLogsEvent || e == SendCommandEvent {\n",
    "remove legacy collaboration limiter names",
)
remove_once(
    "router/websocket/limiter.go",
    '''\
func isBetterFilesCollaborationEvent(e Event) bool {
	return IsBetterFilesCollaborationEvent(e)
}

''',
    "obsolete private collaboration classifier",
)
insert_after(
    "router/websocket/limiter.go",
    '''\
func limiterName(e Event) Event {
	if e == AuthenticationEvent || e == SendServerLogsEvent || e == SendCommandEvent {
		return e
	}

	return "_default"
}
''',
    '''\

// IsBetterFilesCollaborationEvent reports whether an event belongs to the
// ordered Better Files collaboration protocol.
func IsBetterFilesCollaborationEvent(e Event) bool {
	return strings.HasPrefix(string(e), "betterfiles:collab:")
}
''',
    "func IsBetterFilesCollaborationEvent(e Event) bool",
    "collaboration event classifier",
)
insert_after(
    "router/websocket/limiter.go",
    '''\
func IsBetterFilesCollaborationEvent(e Event) bool {
	return strings.HasPrefix(string(e), "betterfiles:collab:")
}
''',
    '''\

// IsFileCollaborationEvent reports whether an event is part of either file
// collaboration protocol. These stateful events must never be individually
// discarded by a rate limiter because doing so desynchronizes the document.
func IsFileCollaborationEvent(e Event) bool {
	return IsBetterFilesCollaborationEvent(e) || IsNativeFileCollaborationEvent(e)
}
''',
    "func IsFileCollaborationEvent(e Event) bool",
    "lossless collaboration event classifier",
)
require_contains(
    "router/websocket/limiter.go",
    "func IsFileCollaborationEvent(e Event) bool",
    "lossless collaboration event classifier",
)

replace_once(
    "router/websocket/websocket.go",
    '''\
	if h.IsThrottled(m.Event) {
		return nil
	}
''',
    '''\
	// Collaboration messages are bounded and processed synchronously by the
	// router. Dropping one update or chunk here would corrupt protocol state.
	if !IsFileCollaborationEvent(m.Event) && h.IsThrottled(m.Event) {
		return nil
	}
''',
    "!IsFileCollaborationEvent(m.Event) && h.IsThrottled(m.Event)",
    "lossless collaboration event handling",
)

replace_once(
    "router/router_server_ws.go",
    '''\
	// There is a separate rate limiter that applies to individual message types
	// within the actual websocket logic handler. _This_ rate limiter just exists
	// to avoid enormous floods of data through the socket since we need to parse
	// JSON each time. This rate limit realistically should never be hit since this
	// would require sending 50+ messages a second over the websocket (no more than
	// 10 per 200ms).
''',
    '''\
	// There is a separate rate limiter that applies to individual message types
	// within the actual websocket logic handler. This limiter protects ordinary
	// control traffic. Stateful collaboration events bypass both limiters and are
	// processed synchronously below, matching Wings-rs behavior. Silently dropping
	// one collaboration chunk would desynchronize the document.
''',
    "This limiter protects ordinary",
    "accurate collaboration limiter documentation",
)

insert_after(
    "router/router_server_ws.go",
    "\tvar throttled bool\n\trl := rate.NewLimiter(rate.Every(time.Millisecond*200), 10)\n",
    '''\
	allowOrdinaryMessage := func() bool {
		if !rl.Allow() {
			if !throttled {
				throttled = true
				_ = handler.Connection.WriteJSON(websocket.Message{Event: websocket.ThrottledEvent, Args: []string{"global"}})
			}
			return false
		}

		throttled = false
		return true
	}
''',
    "allowOrdinaryMessage := func() bool",
    "lossless collaboration message admission",
)
insert_before(
    "router/router_server_ws.go",
    "\n\tfor {\n",
    '''\
	handleMessage := func(msg websocket.Message) {
		if err := handler.HandleInbound(ctx, msg); err != nil {
			if errors.Is(err, server.ErrSuspended) {
				cancel()
			} else {
				_ = handler.SendErrorJson(msg, err)
			}
		}
	}
''',
    "handleMessage := func(msg websocket.Message)",
    "ordered collaboration message handler",
)
remove_once(
    "router/router_server_ws.go",
    '''\
		if !rl.Allow() {
			if !throttled {
				throttled = true
				_ = handler.Connection.WriteJSON(websocket.Message{Event: websocket.ThrottledEvent, Args: []string{"global"}})
			}
			continue
		}

		throttled = false

''',
    "pre-decode global collaboration throttle",
)
replace_once(
    "router/router_server_ws.go",
    '''\
		var j websocket.Message
		if err := json.Unmarshal(p, &j); err != nil {
			continue
		}
''',
    '''\
		var j websocket.Message
		if err := json.Unmarshal(p, &j); err != nil {
			allowOrdinaryMessage()
			continue
		}

		if !websocket.IsFileCollaborationEvent(j.Event) && !allowOrdinaryMessage() {
			continue
		}
''',
    "!websocket.IsFileCollaborationEvent(j.Event) && !allowOrdinaryMessage()",
    "collaboration bypass for global event limiter",
)
replace_if_present(
    "router/router_server_ws.go",
    '''\
		go func(msg websocket.Message) {
			if err := handler.HandleInbound(ctx, msg); err != nil {
				if errors.Is(err, server.ErrSuspended) {
					cancel()
				} else {
					_ = handler.SendErrorJson(msg, err)
				}
			}
		}(j)
''',
    '''\
		// Authentication and collaboration messages are stateful protocols. Keep
		// their WebSocket wire order instead of racing them in separate goroutines.
        if j.Event == websocket.AuthenticationEvent || websocket.IsFileCollaborationEvent(j.Event) {
			handleMessage(j)
			continue
		}
		go handleMessage(j)
''',
    "ordered collaboration message dispatch",
)
replace_if_present(
    "router/router_server_ws.go",
    "\t\tif j.Event == websocket.AuthenticationEvent || websocket.IsBetterFilesCollaborationEvent(j.Event) || websocket.IsNativeFileCollaborationEvent(j.Event) {\n",
    "\t\tif j.Event == websocket.AuthenticationEvent || websocket.IsFileCollaborationEvent(j.Event) {\n",
    "simplify ordered collaboration dispatch",
)
require_contains(
    "router/router_server_ws.go",
    "websocket.IsFileCollaborationEvent(j.Event)",
    "ordered lossless collaboration dispatch",
)

remove_once(
    "router/router_server_files.go",
    '	"path/filepath"\n',
    "unused filepath import",
)
insert_after(
    "router/router_server_files.go",
    '''\
				if err := fs.Rename(pf, pt); err != nil {
					// Return nil if the error is an is not exists.
					if errors.Is(err, os.ErrNotExist) {
						s.Log().WithField("error", err).
							WithField("from_path", pf).
							WithField("to_path", pt).
							Warn("failed to rename: source or target does not exist")
						return nil
					}
					return err
				}
''',
    "				s.RenameFileHistory(pf, pt)\n",
    "RenameFileHistory",
    "file history rename hook",
)
replace_once(
    "router/router_server_files.go",
    '''\
			default:
				return s.Filesystem().Delete(pi)
''',
    '''\
			default:
				if err := s.Filesystem().Delete(pi); err != nil {
					return err
				}
				s.ForgetFileHistory(pi)
				return nil
''',
    "ForgetFileHistory",
    "file history delete hook",
)
insert_after(
    "router/router_server_files.go",
    '''\
	if c.Request.ContentLength == -1 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Missing Content-Length",
		})
		return
	}
''',
    '''\

	var before []byte
	trackRevision := server.ShouldRecordFileHistory(f, uint64(c.Request.ContentLength))
	if trackRevision {
		before, _ = captureFileRevisionContent(s, f)
	}
''',
    "trackRevision := server.ShouldRecordFileHistory",
    "file history write pre-image",
)
insert_after(
    "router/router_server_files.go",
    '''\
	if err := s.Filesystem().Write(f, c.Request.Body, c.Request.ContentLength, 0o644); err != nil {
		if filesystem.IsErrorCode(err, filesystem.ErrCodeIsDirectory) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "Cannot write file, name conflicts with an existing directory by the same name.",
			})
			return
		}

		middleware.CaptureAndAbort(c, err)
		return
	}
''',
    '''\

	if trackRevision {
		after, ok := captureFileRevisionContent(s, f)
		if ok {
			revisionID, err := s.RecordFileRevision(f, before, after, strings.TrimSpace(c.Query("user")))
			if err != nil {
				s.Log().WithError(err).WithField("path", f).Warn("failed to record file revision")
			} else if revisionID > 0 {
				c.Header("X-File-Revision-Id", strconv.FormatInt(revisionID, 10))
			}
		}
	}
''',
    "X-File-Revision-Id",
    "file history write post-image",
)
replace_once(
    "router/router_server_files.go",
    '	if !ok || !token.HasScope(tokens.FileUpload) {\n',
    '	if !ok || !token.HasScope(tokens.FileUpload) || !token.IsUniqueRequest() {\n',
    "token.IsUniqueRequest()",
    "single-use upload token validation",
)
replace_once(
    "router/router_server_files.go",
    '	directory := c.Query("directory")\n',
    '''\
	directory := path.Clean("/" + strings.TrimLeft(c.Query("directory"), "/"))
	if directory == "." {
		directory = "/"
	}
''',
    'directory := path.Clean("/" + strings.TrimLeft(c.Query("directory"), "/"))',
    "upload directory normalization",
)
insert_after(
    "router/router_server_files.go",
    '	maxFileSize := config.Get().Api.UploadLimit\n',
    '''\
	const maxUploadLimitMB = int64(1<<63-1) / (1024 * 1024)
	if maxFileSize <= 0 || maxFileSize > maxUploadLimitMB {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"error": "Upload limit is not configured correctly.",
		})
		return
	}
''',
    "maxUploadLimitMB",
    "upload limit validation",
)
replace_once(
    "router/router_server_files.go",
    '		if header.Size > maxFileSizeBytes {\n',
    '		if header.Size < 0 || header.Size > maxFileSizeBytes {\n',
    "header.Size < 0",
    "negative upload size validation",
)
insert_after(
    "router/router_server_files.go",
    '''\
		totalSize += header.Size
''',
    '''\
		if totalSize > maxFileSizeBytes {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "Total upload size is larger than the maximum file upload size of " + strconv.FormatInt(maxFileSize, 10) + " MB.",
			})
			return
		}
''',
    "Total upload size is larger",
    "total upload size validation",
)
insert_after(
	    "router/router_server_files.go",
	    '''\
	for _, header := range headers {
		// We run this in a different method so I can use defer without any of
		// the consequences caused by calling it in a loop.
''',
	    '''\
		filename, ok := cleanUploadFilename(header.Filename)
		if !ok {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "Invalid upload filename.",
			})
			return
		}

		// We run this in a different method so I can use defer without any of
		// the consequences caused by calling it in a loop.
''',
    "cleanUploadFilename(header.Filename)",
    "upload filename validation",
)
replace_once(
    "router/router_server_files.go",
    "		if err := handleFileUpload(filepath.Join(directory, header.Filename), s, header); err != nil {\n",
    "		if err := handleFileUpload(path.Join(directory, filename), s, header); err != nil {\n",
    "path.Join(directory, filename)",
    "safe upload target path",
)
replace_once(
    "router/router_server_files.go",
    '''\
				"file":      header.Filename,
				"directory": filepath.Clean(directory),
''',
    '''\
				"file":      filename,
				"directory": directory,
''',
    '"file":      filename',
    "safe upload activity metadata",
)
insert_before(
    "router/router_server_files.go",
    "func handleFileUpload(p string, s *server.Server, header *multipart.FileHeader) error {\n",
    '''\
func cleanUploadFilename(name string) (string, bool) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		return "", false
	}
	return name, true
}

''',
    "func cleanUploadFilename",
    "upload filename helper",
)
PY

section "Formatting and building"
run_with_spinner "format Go files" gofmt -w \
    config/config.go \
    environment/docker/client_accessor.go \
    router/betterfiles_entry.go \
    router/betterfiles_paths.go \
    router/betterfiles_paths_test.go \
    router/betterfiles_test_helpers_test.go \
    router/betterfiles_http_test.go \
    router/middleware/middleware.go \
    router/middleware/middleware_test.go \
    router/router.go \
    router/router_cdn_stream.go \
    router/router_download_directory.go \
    router/router_download_directory_test.go \
    router/router_download_directory_http_test.go \
    router/router_download_helpers.go \
    router/router_download_helpers_test.go \
    router/router_file_operations.go \
    router/router_file_operations_test.go \
    router/router_openapi.go \
    router/router_openapi_test.go \
    router/router_resumable_upload.go \
    router/router_resumable_upload_test.go \
    router/router_system_config.go \
    router/router_system_config_test.go \
    router/router_server_archive_nbt.go \
    router/router_server_betterfiles_collaboration.go \
    router/router_server_files.go \
    router/router_server_files_copy_many.go \
    router/router_server_files_copy_many_test.go \
    router/router_server_files_fingerprints.go \
    router/router_server_files_fingerprints_test.go \
    router/router_server_files_largest.go \
    router/router_server_files_largest_test.go \
    router/router_server_files_rename.go \
    router/router_server_files_revisions.go \
    router/router_server_files_search.go \
    router/router_server_files_search_v2.go \
    router/router_server_files_search_v2_test.go \
    router/router_server_git.go \
    router/router_server_git_test.go \
    router/router_server_ws.go \
    router/tokens/websocket.go \
    router/websocket/betterfiles_collaboration.go \
    router/websocket/betterfiles_collaboration_ot.go \
    router/websocket/betterfiles_collaboration_ot_test.go \
    router/websocket/file_collaboration_yjs.go \
    router/websocket/file_collaboration_yjs_test.go \
    router/websocket/listeners.go \
    router/websocket/listeners_operation_test.go \
    router/websocket/limiter.go \
    router/websocket/websocket.go \
    server/events.go \
    server/file_operations.go \
    server/file_operations_test.go \
    server/file_history.go \
    server/file_history_test.go \
    server/filesystem/replace.go \
    server/server.go
run_with_spinner "test Better Files packages" go test ./router ./router/middleware ./router/websocket ./server ./server/filesystem
if [ "${BETTERFILES_RACE:-0}" = "1" ]; then
    run_with_spinner "race-test Better Files packages" go test -race ./router ./router/middleware ./router/websocket ./server ./server/filesystem
fi
# The supported Wings base has inherited copylock and unreachable-code findings.
# Keep every other standard vet analyzer fatal for installer validation.
run_with_spinner "vet Wings packages" go vet -copylocks=false -unreachable=false ./...
run_with_spinner "compile Wings packages" go build ./...

section "Done"
ok "Better Files Wings edits are installed and validation succeeded."
if [ -d "$BACKUP_DIR" ]; then
    log "backups, if any, are in: ${BACKUP_DIR}"
fi
