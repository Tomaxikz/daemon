#!/usr/bin/env python3
"""Exercise checked-in distribution artifacts against their supported Git bases."""

import argparse
from contextlib import contextmanager
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]
UPSTREAM = "6987d5e6f0612133e031255a99655e822d30579a"
PREVIOUS = "60f5d300139f677e1324a4e0126c4b7e65e8bf71"
CASES = {
    "betterfiles": ("betterfiles.patch", "betterfiles_patch.sh"),
    "betterconsole": ("betterconsole.patch", "betterconsolepatch.sh"),
    "networkstats": ("nsm.patch", "install_networkstats.sh"),
    "full": ("tomaxikz.patch", None),
}


def checked(args, cwd=ROOT, env=None):
    print(f"[{cwd.name}] {' '.join(map(str, args))}", flush=True)
    result = subprocess.run(args, cwd=cwd, env=env, text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if result.returncode:
        print(result.stdout, flush=True)
        raise RuntimeError(f"Command failed with exit status {result.returncode}")
    return result.stdout


@contextmanager
def fixture(parent, name):
    path = parent / name
    checked(["git", "worktree", "add", "--detach", str(path), UPSTREAM])
    try:
        yield path
    finally:
        if path.resolve().parent != parent.resolve() or path.resolve() == ROOT:
            raise RuntimeError("Refusing to clean an unexpected fixture path")
        checked(["git", "worktree", "remove", "--force", str(path)])


def source_snapshot(path):
    result = {}
    for file in path.rglob("*"):
        relative = file.relative_to(path)
        if any(part.startswith(".") for part in relative.parts):
            continue
        if file.is_file() and (file.suffix == ".go" or file.name in ("go.mod", "go.sum")):
            result[str(relative)] = hashlib.sha256(file.read_bytes()).hexdigest()
    return result


def verify_source(path):
    env = dict(os.environ, CGO_ENABLED="0", GOTOOLCHAIN="local")
    checked(["go", "test", "-mod=readonly", "./..."], cwd=path, env=env)
    checked(["go", "build", "-mod=readonly", "./..."], cwd=path, env=env)


def install(script, path):
    # All downloads come from this checkout, never the moving develop branch.
    env = dict(os.environ, TOMAXIKZ_RAW_BASE=ROOT.as_uri(), NO_COLOR="1")
    checked(["bash", str(ROOT / script)], cwd=path, env=env)


def validate(case):
    patch, script = CASES[case]
    checked(["git", "cat-file", "-e", UPSTREAM + "^{commit}"])
    checked(["git", "cat-file", "-e", PREVIOUS + "^{commit}"])
    with tempfile.TemporaryDirectory(prefix="wings-installer-validation-") as temporary:
        parent = Path(temporary)
        with fixture(parent, "patch") as path:
            checked(["git", "apply", "--check", str(ROOT / patch)], cwd=path)
            checked(["git", "apply", "--3way", "--index", "--whitespace=nowarn", str(ROOT / patch)], cwd=path)
            added = checked(["git", "diff", "--cached", "--diff-filter=A", "--name-only"], cwd=path).splitlines()
            for name in added:
                if name.endswith(".go"):
                    if (path / name).read_text() != (ROOT / name).read_text():
                        raise RuntimeError(f"{patch} contains a stale copy of {name}")
            verify_source(path)
        if script:
            with fixture(parent, "clean-install") as path:
                install(script, path)
                verify_source(path)
                before = source_snapshot(path)
                install(script, path)
                if source_snapshot(path) != before:
                    raise RuntimeError(f"{script} is not idempotent")
            old = subprocess.run(["git", "show", f"{PREVIOUS}:{patch}"], cwd=ROOT,
                                 stdout=subprocess.PIPE, check=True).stdout
            old_patch = parent / "previous.patch"
            old_patch.write_bytes(old)
            with fixture(parent, "upgrade") as path:
                checked(["git", "apply", "--whitespace=nowarn", str(old_patch)], cwd=path)
                install(script, path)
                verify_source(path)
    print(f"PASS: {case} patch, source consistency" + (", clean install, repeat install and upgrade" if script else ""), flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("case", choices=CASES)
    validate(parser.parse_args().case)
