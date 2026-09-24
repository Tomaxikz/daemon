#!/usr/bin/env bash
set -euo pipefail

RAW_BASE="${TOMAXIKZ_RAW_BASE:-https://raw.githubusercontent.com/Tomaxikz/daemon/develop}"
BACKUP_ROOT="${TOMAXIKZ_BACKUP_ROOT:-.tomaxikz-networkstats-backups}"

for command in curl python3 git go; do
    command -v "$command" >/dev/null || { echo "Missing required command: $command" >&2; exit 1; }
done
[ -f go.mod ] && grep -q 'github.com/pterodactyl/wings' go.mod || {
    echo 'Run this from the Wings Git source root.' >&2
    exit 1
}
[ "$(git rev-parse --show-toplevel)" = "$(pwd -P)" ] || {
    echo 'Run this from the Wings Git source root.' >&2
    exit 1
}

temporary=$(mktemp -d)
trap 'rm -f "$temporary/distribution.patch"; rmdir "$temporary"' EXIT
patch=nsm.patch
if git merge-base --is-ancestor d0e07134d8862c22ca2594526f5b879280eacff1 HEAD 2>/dev/null; then
    patch=nsm-networking.patch
fi
echo "Downloading $patch"
curl -fsSL --retry 3 -o "$temporary/distribution.patch" "${RAW_BASE%/}/$patch"

python3 - "$temporary/distribution.patch" "$BACKUP_ROOT" "$patch" <<'PY'
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

root = Path.cwd()
artifact = Path(sys.argv[1]).resolve()
backup_root = Path(sys.argv[2]).resolve()


def git(*args, env=None):
    cwd = env["GIT_WORK_TREE"] if env else root
    result = subprocess.run(["git", *args], cwd=cwd, env=env, capture_output=True)
    if result.returncode:
        raise RuntimeError(result.stderr.decode(errors="replace").strip())
    return result.stdout


names = []
for entry in git("apply", "--numstat", "-z", str(artifact)).split(b"\0"):
    if not entry:
        continue
    name = entry.decode().split("\t", 2)[2]
    path = Path(name)
    if path.is_absolute() or ".." in path.parts or any(part.startswith(".") for part in path.parts):
        raise RuntimeError("Unsafe distribution path")
    if path.suffix not in (".go", ".json") and name not in ("go.mod", "go.sum"):
        raise RuntimeError("Unexpected distribution file: " + name)
    for candidate in [root / path, *(root / path).parents]:
        if candidate == root:
            break
        if candidate.is_symlink():
            raise RuntimeError("Refusing symlink in source path: " + name)
    names.append(name)
if not names:
    raise RuntimeError("Empty distribution patch")

# Merge against a private index/work tree before touching customer source files.
with tempfile.TemporaryDirectory(prefix="wings-nsm-merge-") as directory:
    scratch = Path(directory)
    tree = scratch / "tree"
    tree.mkdir()
    env = dict(os.environ, GIT_DIR=git("rev-parse", "--absolute-git-dir").decode().strip(),
               GIT_INDEX_FILE=str(scratch / "index"), GIT_WORK_TREE=str(tree))
    git("read-tree", "HEAD", env=env)
    for name in names:
        source = root / name
        if source.exists():
            if not source.is_file():
                raise RuntimeError("Source path is not a regular file: " + name)
            target = tree / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, target)
    tracked = set(git("ls-files", "-z", env=env).decode().split("\0"))
    present = [name for name in names if (tree / name).exists() or name in tracked]
    if present:
        git("add", "-A", "--", *present, env=env)
    before = git("write-tree", env=env).decode().strip()

    router = tree / "router/router.go"
    if router.exists() and sys.argv[3] == "nsm.patch":
        text = router.read_text()
        legacy = '\t\tserver.GET("/stats/protocols", getServerProtocolStats)\n'
        if '/network-policy/capabilities' not in text and text.count(legacy) == 1:
            # Older statistics patches placed this route at a different anchor.
            router.write_text(text.replace(legacy, "", 1))
            git("add", "router/router.go", env=env)

    git("apply", "--3way", "--index", str(artifact), env=env)
    delta = git("diff", "--cached", "--binary", before, "--", *names, env=env)
    if delta:
        update = scratch / "update.patch"
        update.write_bytes(delta)
        git("apply", "--check", str(update))
        backup_root.mkdir(parents=True, exist_ok=True)
        backup = Path(tempfile.mkdtemp(prefix="install-", dir=backup_root))
        created = []
        for name in names:
            source = root / name
            if source.exists():
                target = backup / name
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(source, target)
            else:
                created.append(name)
        (backup / "new-files.json").write_text(json.dumps(created, indent=2) + "\n")
        git("apply", str(update))
        print("Installed source changes; backup: " + str(backup))
    else:
        print("Network Statistics source is already current.")
PY

echo 'Testing and building Wings; no services will be changed.'
go test -mod=readonly ./...
go vet -mod=readonly ./...
go build -mod=readonly -o wings .
echo 'Build complete: ./wings. Review configuration and install the binary manually.'
