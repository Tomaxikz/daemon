#!/usr/bin/env python3
"""Apply the shared lock-copy fixes without replacing customized core files."""

import argparse
from pathlib import Path
import re
import shutil

CORE_FILES = [
    "server/resources.go",
    "server/server.go",
    "router/downloader/downloader.go",
    "router/router.go",
    "server/filesystem/filesystem_test.go",
]
TEST_FILES = [
    "server/resources_test.go",
    "server/configuration_test.go",
    "router/downloader/downloader_test.go",
]
CONFIG_FIELDS = [
    "Uuid",
    "Meta",
    "Suspended",
    "Invocation",
    "SkipEggScripts",
    "EnvVars",
    "Labels",
    "Allocations",
    "Build",
    "CrashDetectionEnabled",
    "Mounts",
    "Egg",
    "Container",
]


def replace(text, before, after, path):
    if after in text:
        return text
    if text.count(before) != 1:
        raise ValueError(f"{path}: unrecognized core implementation; review custom edits")

    return text.replace(before, after, 1)


def apply(root, backup):
    configuration = (root / "server/configuration.go").read_text()
    body = re.search(r"type Configuration struct \{\n(.*?)^\}", configuration, re.M | re.S)
    fields = re.findall(r"^\t([A-Z]\w*)\s", body[1], re.M) if body else []
    if fields != CONFIG_FIELDS:
        raise ValueError(
            "Configuration fields differ from the supported base; review the synchronization fix"
        )

    edits = {}
    for name in CORE_FILES:
        text = (root / name).read_text()
        if name == "server/resources.go":
            text = replace(
                text,
                "\t//goland:noinspection GoVetCopyLock\n\treturn s.resources",
                (
                    "\treturn ResourceUsage{\n"
                    "\t\tStats: s.resources.Stats,\n"
                    "\t\tState: s.resources.State,\n"
                    "\t\tDisk:  s.resources.Disk,\n"
                    "\t}"
                ),
                name,
            )
        elif name == "server/server.go":
            before = """\t// Lock the new configuration. Since we have the deferred Unlock above we need
\t// to make sure that the NEW configuration object is already locked since that
\t// defer is running on the memory address for "s.cfg.mu" which we're explicitly
\t// changing on the next line.
\tc.mu.Lock()

\t//goland:noinspection GoVetCopyLock
\ts.cfg = c"""
            after = "\t// Keep the mutex: readers may already be waiting on it.\n"
            after += "\n".join(f"\ts.cfg.{field} = c.{field}" for field in CONFIG_FIELDS)
            text = replace(text, before, after, name)
        elif name == "router/downloader/downloader.go":
            text = replace(
                text,
                "//goland:noinspection GoVetCopyLock\nfunc (dl Download) MarshalJSON()",
                "func (dl *Download) MarshalJSON()",
                name,
            )
        elif name == "router/router.go":
            text = replace(
                text,
                "\t\tpanic(errors.WithStack(err))\n\t\treturn nil\n\t}",
                "\t\tpanic(errors.WithStack(err))\n\t}",
                name,
            )
        else:
            text = text.replace("\t\tpanic(err)\n\t\treturn nil, nil", "\t\tpanic(err)")
        edits[name] = text

    # Validate every anchor before modifying any source file.
    for name, text in edits.items():
        target = root / name
        if target.read_text() == text:
            continue

        saved = backup / name
        saved.parent.mkdir(parents=True, exist_ok=True)
        if not saved.exists():
            shutil.copy2(target, saved)
        target.write_text(text)
        print(f"Updated {name}", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--backup", required=True, type=Path)
    args = parser.parse_args()
    apply(Path.cwd(), args.backup)
