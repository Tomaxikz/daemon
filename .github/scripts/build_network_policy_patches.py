#!/usr/bin/env python3
"""Rebuild NSM distribution patches without changing the user's Git index."""

import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

from validate_installers import ROOT, UPSTREAM, checked, fixture, verify_source


def git_bytes(args, cwd=ROOT, env=None, data=None):
    return subprocess.run(
        ["git", *args], cwd=cwd, env=env, input=data, stdout=subprocess.PIPE, check=True
    ).stdout


def check_roundtrip(path, patch):
    checked(["git", "apply", "--reverse", "--check", str(patch)], cwd=path)
    checked(["git", "apply", "--reverse", "--index", str(patch)], cwd=path)
    checked(["git", "diff", "--exit-code", "HEAD"], cwd=path)
    checked(["git", "apply", "--check", str(patch)], cwd=path)
    checked(["git", "apply", "--index", str(patch)], cwd=path)


def add_network_fields(path):
    name = "environment/docker/environment.go"
    text = (path / name).read_text()
    text = text.replace('\t"sync"\n', '\t"sync"\n\t"sync/atomic"\n', 1)
    text = text.replace(
        '\t"github.com/pterodactyl/wings/events"\n',
        '\t"github.com/pterodactyl/wings/events"\n\t"github.com/pterodactyl/wings/internal/networkpolicy"\n',
        1,
    )
    fields = re.search(r"^\tnetworkMu\s.*?^\tnetworkDeleted\s+bool\n", (ROOT / name).read_text(), re.M | re.S)
    if fields is None or text.count("type Environment struct {\n") != 1:
        raise ValueError("Network environment anchors changed")
    text = text.replace("type Environment struct {\n", "type Environment struct {\n" + fields[0], 1)
    (path / name).write_text(text)


def main():
    manifest = json.loads((ROOT / "nsm-networking.manifest.json").read_text())
    base = manifest["implementation_base"]
    sources = manifest["source_files"]
    statistics = [
        "environment/stats.go",
        "environment/docker/stats.go",
        "router/nsm_router.go",
        "server/protocol_stats.go",
        "server/protocol_monitor.go",
        "server/resources.go",
    ]
    with tempfile.TemporaryDirectory(prefix="wings-nsm-patches-") as temporary:
        parent = Path(temporary)
        # Keep the overlay based on the recorded fork, not whatever HEAD becomes.
        env = dict(os.environ, GIT_INDEX_FILE=str(parent / "index"))
        git_bytes(["read-tree", base], env=env)
        for name in sources:
            contents = (ROOT / name).read_bytes().replace(b"\r\n", b"\n")
            blob = git_bytes(["hash-object", "-w", "--stdin"], data=contents).decode().strip()
            git_bytes(["update-index", "--add", "--cacheinfo", "100644", blob, name], env=env)

        (ROOT / "nsm-networking.patch").write_bytes(
            git_bytes(["diff", "--cached", "--binary", base], env=env)
        )
        with fixture(parent, "fork-upgrade", base) as path:
            checked(["git", "apply", "--index", str(ROOT / "nsm-networking.patch")], cwd=path)
            verify_source(path)
            check_roundtrip(path, ROOT / "nsm-networking.patch")

        for name, full in (("nsm.patch", False), ("tomaxikz.patch", True)):
            with fixture(parent, "full" if full else "stock") as path:
                if full:
                    files = git_bytes([
                        "ls-files", "--cached", "--others", "--exclude-standard",
                        "--", "*.go", "go.mod", "go.sum",
                    ]).decode().splitlines()
                    files = sorted(set(files + ["internal/networkpolicy/testdata/shared.json"]))
                    for source in files:
                        target = path / source
                        target.parent.mkdir(parents=True, exist_ok=True)
                        target.write_bytes((ROOT / source).read_bytes().replace(b"\r\n", b"\n"))
                    checked(["git", "add", "--", *files], cwd=path)
                else:
                    original = git_bytes(["show", f"{base}:nsm.patch"])
                    git_bytes(["apply", "--index", "--whitespace=nowarn", "-"], cwd=path, data=original)
                    checked([
                        "git", "apply", "--3way", "--index",
                        "--exclude=environment/docker/environment.go", "--exclude=system/const.go",
                        str(ROOT / "nsm-networking.patch"),
                    ], cwd=path)
                    add_network_fields(path)
                    (path / "system/const.go").write_bytes((ROOT / "system/const.go").read_bytes())
                    checked(["go", "mod", "edit", "-go=1.26.7", "-toolchain=none"], cwd=path)
                    checked(["go", "mod", "tidy"], cwd=path)
                    checked(["gofmt", "-w", "environment/docker/environment.go"], cwd=path)
                    checked(["git", "add", "--", *sorted(set(sources + statistics))], cwd=path)
                artifact = ROOT / name
                artifact.write_bytes(
                    git_bytes(["diff", "--cached", "--binary", UPSTREAM], cwd=path)
                )
                verify_source(path)
                check_roundtrip(path, artifact)

    print(
        "PASS: generated three patches; matching-base tests/builds and application/reversal passed"
    )


if __name__ == "__main__":
    main()
