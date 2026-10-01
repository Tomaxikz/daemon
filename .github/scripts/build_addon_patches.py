#!/usr/bin/env python3
"""Refresh standalone addon source copies without changing stock integration hunks."""

import argparse
from pathlib import Path
import tempfile

from validate_installers import ROOT, UPSTREAM, checked, fixture, verify_source


def build(addon):
    artifact = ROOT / f"{addon}.patch"
    with tempfile.TemporaryDirectory(prefix="wings-addon-patch-") as temporary:
        with fixture(Path(temporary), addon) as path:
            checked(["git", "apply", "--index", "--whitespace=nowarn", str(artifact)], cwd=path)
            added = checked(
                ["git", "diff", "--cached", "--diff-filter=A", "--name-only"], cwd=path
            ).splitlines()
            for name in added:
                if name.endswith(".go"):
                    (path / name).write_text((ROOT / name).read_text(), newline="\n")

            checked(["git", "add", "--", *added], cwd=path)
            verify_source(path)
            patch = checked(["git", "diff", "--cached", "--binary", UPSTREAM], cwd=path)
            candidate = Path(temporary) / artifact.name
            candidate.write_text(patch, newline="\n")
            checked(["git", "apply", "--reverse", "--check", str(candidate)], cwd=path)
            artifact.write_text(patch, newline="\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("addon", choices=("betterfiles", "betterconsole"))
    build(parser.parse_args().addon)
