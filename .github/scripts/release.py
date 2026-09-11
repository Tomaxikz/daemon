#!/usr/bin/env python3
"""Build, assemble, verify and publish one commit's Wings release artifacts."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]
ARCHES = {"amd64": 62, "arm64": 183}
BINARIES = {f"wings_linux_{arch}{suffix}" for arch in ARCHES for suffix in ("", "_debug")}
PATCHES = {"betterfiles.patch", "betterconsole.patch", "nsm.patch", "tomaxikz.patch"}
INSTALLERS = {"betterfiles_patch.sh", "betterconsolepatch.sh", "install_networkstats.sh"}
PAYLOAD = BINARIES | PATCHES | INSTALLERS | {"README.md"}
ASSETS = PAYLOAD | {"RELEASE.json", "SHA256SUMS"}
VERSION_TAG = re.compile(r"v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z.-]+))?")
SHA = re.compile(r"[0-9a-f]{40}")
SAFE_NAME = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.-]*")


def run(args, *, cwd=ROOT, env=None, data=None):
    result = subprocess.run(args, cwd=cwd, env=env, input=data, text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode:
        raise RuntimeError(f"{args[0]} failed: {result.stderr.strip()}")
    return result.stdout.strip()


def digest(path):
    with path.open("rb") as file:
        return hashlib.file_digest(file, "sha256").hexdigest()


def version_for(ref, commit):
    if not SHA.fullmatch(commit):
        raise ValueError("Expected a full commit SHA")
    if ref.startswith("refs/tags/"):
        tag = ref.removeprefix("refs/tags/")
        if not VERSION_TAG.fullmatch(tag):
            raise ValueError("Release tags must be vMAJOR.MINOR.PATCH with an optional prerelease suffix")
        return tag[1:]
    return f"dev-{commit[:12]}"


def context(root=ROOT):
    commit = run(["git", "rev-parse", "HEAD"], cwd=root)
    if os.environ.get("GITHUB_SHA", commit) != commit:
        raise ValueError("Checkout does not match the workflow commit")
    repository = os.environ.get("GITHUB_REPOSITORY", "Tomaxikz/daemon")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ValueError("Invalid repository name")
    ref = os.environ.get("GITHUB_REF", "refs/heads/develop")
    go = re.search(r"^go (\d+\.\d+(?:\.\d+)?)$", (root / "go.mod").read_text(), re.M)
    if not go:
        raise ValueError("No Go version found in go.mod")
    return {"schema_version": 1, "commit": commit, "repository": repository, "ref": ref,
            "version": version_for(ref, commit), "go_version": "go" + go[1],
            "workflow_url": f"https://github.com/{repository}/actions/runs/{os.environ.get('GITHUB_RUN_ID', 'local')}"}


def verify_elf(path, arch):
    with path.open("rb") as file:
        header = file.read(20)
    if len(header) != 20 or header[:6] != b"\x7fELF\x02\x01" or int.from_bytes(header[18:20], "little") != ARCHES[arch]:
        raise ValueError(f"{path.name} is not a Linux {arch} executable")


def build(directory, arch):
    metadata = context()
    if run(["go", "env", "GOARCH"]) != arch or run(["go", "env", "GOOS"]) != "linux":
        raise ValueError("Build and smoke tests must run on the matching native Linux architecture")
    if run(["go", "env", "GOVERSION"]) != metadata["go_version"]:
        raise ValueError("Compiler must match go.mod exactly")
    directory.mkdir(parents=True, exist_ok=True)
    hashes = {}
    for suffix in ("", "_debug"):
        name = f"wings_linux_{arch}{suffix}"
        flags = f"-X github.com/pterodactyl/wings/system.Version={metadata['version']}"
        if not suffix:
            flags = "-s -w " + flags
        run(["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags", flags,
             "-o", str(directory / name), "."], env=dict(os.environ, CGO_ENABLED="0", GOTOOLCHAIN="local"))
        verify_elf(directory / name, arch)
        version = run([str(directory / name), "version"])
        if metadata["version"] not in version:
            raise ValueError(f"Incorrect version in {name}")
        hashes[name] = digest(directory / name)
    (directory / f"build-{arch}.json").write_text(json.dumps({**metadata, "files": hashes}, indent=2) + "\n")


def pin_installer(text, repository, commit):
    old = "https://raw.githubusercontent.com/Tomaxikz/daemon/develop"
    if text.count(old) != 1:
        raise ValueError("Installer must have exactly one canonical RAW_BASE default")
    return text.replace(old, f"https://raw.githubusercontent.com/{repository}/{commit}")


def bundle(directory, root=ROOT):
    metadata = dict(context(root))
    expected = BINARIES | {f"build-{arch}.json" for arch in ARCHES}
    if {file.name for file in directory.iterdir()} != expected:
        raise ValueError("Incomplete or unexpected build artifacts")
    for arch in ARCHES:
        record_path = directory / f"build-{arch}.json"
        record = json.loads(record_path.read_text())
        if any(record.get(key) != value for key, value in metadata.items()):
            raise ValueError("Architecture artifacts came from different sources or workflow runs")
        for name in (f"wings_linux_{arch}", f"wings_linux_{arch}_debug"):
            verify_elf(directory / name, arch)
            if record.get("files", {}).get(name) != digest(directory / name):
                raise ValueError(f"Build artifact checksum mismatch: {name}")
    for name in sorted(PATCHES | INSTALLERS | {"README.md"}):
        data = (root / name).read_bytes()
        if name in INSTALLERS:
            data = pin_installer(data.decode(), metadata["repository"], metadata["commit"]).encode()
        (directory / name).write_bytes(data)
    metadata["files"] = {name: digest(directory / name) for name in sorted(PAYLOAD)}
    (directory / "RELEASE.json").write_text(json.dumps(metadata, indent=2) + "\n")
    for arch in ARCHES:
        (directory / f"build-{arch}.json").unlink()
    sums = "".join(f"{digest(directory / name)}  {name}\n" for name in sorted(ASSETS - {"SHA256SUMS"}))
    (directory / "SHA256SUMS").write_text(sums)
    verify_bundle(directory, metadata)


def verify_bundle(directory, expected=None):
    if {file.name for file in directory.iterdir()} != ASSETS:
        raise ValueError("Release bundle has missing or unexpected assets")
    if any(file.is_symlink() or not file.is_file() for file in directory.iterdir()):
        raise ValueError("Release assets must be regular files")
    entries = {}
    for line in (directory / "SHA256SUMS").read_text().splitlines():
        match = re.fullmatch(r"([a-f0-9]{64})  ([A-Za-z0-9_.-]+)", line)
        if not match or match[2] not in ASSETS - {"SHA256SUMS"} or match[2] in entries:
            raise ValueError("Invalid checksum manifest")
        entries[match[2]] = match[1]
    if entries.keys() != ASSETS - {"SHA256SUMS"}:
        raise ValueError("Incomplete checksum manifest")
    for name, value in entries.items():
        if digest(directory / name) != value:
            raise ValueError(f"Release checksum mismatch: {name}")
    metadata = json.loads((directory / "RELEASE.json").read_text())
    if not SHA.fullmatch(metadata.get("commit", "")) or metadata.get("schema_version") != 1:
        raise ValueError("Invalid release identity")
    if metadata.get("files") != {name: entries[name] for name in sorted(PAYLOAD)}:
        raise ValueError("Release metadata disagrees with checksums")
    if expected and any(metadata.get(key) != expected[key] for key in ("commit", "repository", "version", "go_version", "ref")):
        raise ValueError("Release bundle does not match the selected source")
    for arch in ARCHES:
        for suffix in ("", "_debug"):
            verify_elf(directory / f"wings_linux_{arch}{suffix}", arch)
    return metadata


class Publisher:
    def __init__(self, repository):
        self.repository = repository
        self.base = f"repos/{repository}"

    def api(self, method, route, payload=None, missing=False):
        args = ["gh", "api", "--method", method, f"{self.base}/{route}"]
        if payload is not None:
            args += ["--input", "-"]
        result = subprocess.run(args, input=json.dumps(payload) if payload is not None else None,
                                text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if result.returncode:
            if missing and re.search(r"\(HTTP 404\)\s*$", result.stderr):
                return None
            raise RuntimeError(result.stderr.strip())
        return json.loads(result.stdout) if result.stdout.strip() else None

    def release(self, tag):
        found = self.api("GET", f"releases/tags/{tag}", missing=True)
        if found is not None:
            return found
        # The tag endpoint only returns published releases. Authenticated list
        # responses include drafts, which must be reused after a failed run.
        page = 1
        while True:
            releases = self.api("GET", f"releases?per_page=100&page={page}")
            if not isinstance(releases, list):
                raise RuntimeError("GitHub returned an invalid release listing")
            matches = [item for item in releases if item["tag_name"] == tag]
            if len(matches) > 1 or (matches and found is not None):
                raise RuntimeError(f"Multiple releases match {tag}; refusing an ambiguous update")
            if matches:
                found = matches[0]
            if len(releases) < 100:
                return found
            page += 1

    def release_by_id(self, release_id):
        # Once discovered or created, the ID remains usable while a release is
        # a draft, including during dev-latest updates and rollback.
        found = self.api("GET", f"releases/{release_id}")
        if not isinstance(found, dict) or found.get("id") != release_id or not isinstance(found.get("assets"), list):
            raise RuntimeError(f"GitHub returned an invalid response for release {release_id}")
        return found

    def tag_commit(self, tag):
        ref = self.api("GET", f"git/ref/tags/{tag}", missing=True)
        if ref is None:
            return None
        obj = ref["object"]
        for _ in range(10):
            if obj["type"] == "commit":
                return obj["sha"]
            if obj["type"] != "tag":
                break
            obj = self.api("GET", f"git/tags/{obj['sha']}")["object"]
        raise ValueError("Release tag does not resolve to a commit")

    def set_tag(self, tag, commit, mutable=False):
        current = self.tag_commit(tag)
        if current == commit:
            return
        if current is not None:
            if not mutable:
                raise ValueError(f"Refusing to move published version tag {tag}")
            self.api("PATCH", f"git/refs/tags/{tag}", {"sha": commit, "force": True})
        else:
            self.api("POST", "git/refs", {"ref": f"refs/tags/{tag}", "sha": commit})

    def upload(self, tag, directory):
        run(["gh", "release", "upload", tag, "--repo", self.repository, "--clobber",
             *[str(path) for path in sorted(directory.iterdir())]])

    def download(self, tag, directory):
        directory.mkdir(parents=True, exist_ok=True)
        run(["gh", "release", "download", tag, "--repo", self.repository, "--dir", str(directory)])

    def check_assets(self, release, directory):
        expected = {file.name: digest(file) for file in directory.iterdir()}
        assets = release["assets"]
        if {asset["name"] for asset in assets} != expected.keys():
            raise ValueError("Remote release asset inventory mismatch")
        for asset in assets:
            if asset["size"] != (directory / asset["name"]).stat().st_size:
                raise ValueError("Remote release asset size mismatch")
            value = asset.get("digest")
            if value and value != "sha256:" + expected[asset["name"]]:
                raise ValueError("Remote release asset digest mismatch")
        if any(not asset.get("digest") for asset in assets):
            with tempfile.TemporaryDirectory(prefix="wings-remote-check-") as temp:
                downloaded = Path(temp)
                self.download(release["tag_name"], downloaded)
                if {file.name: digest(file) for file in downloaded.iterdir()} != expected:
                    raise ValueError("Downloaded release assets failed verification")

    def remove_extra_assets(self, release, names):
        for asset in release["assets"]:
            if asset["name"] not in names:
                self.api("DELETE", f"releases/assets/{asset['id']}")

    def snapshot(self, tag, metadata, directory):
        self.set_tag(tag, metadata["commit"])
        release = self.release(tag)
        if release and not release["draft"]:
            # Reruns reuse a published snapshot; they never replace its bytes.
            if {asset["name"] for asset in release["assets"]} != ASSETS:
                raise ValueError("Published release is not a complete pipeline bundle")
            with tempfile.TemporaryDirectory(prefix="wings-existing-release-") as temp:
                saved = Path(temp)
                self.download(tag, saved)
                original = verify_bundle(saved, metadata)
                if original["files"] != metadata["files"]:
                    raise ValueError("Published snapshot differs from this commit's build")
                for name in ASSETS:
                    (directory / name).write_bytes((saved / name).read_bytes())
            return
        notes = f"Wings {metadata['version']}\n\nCommit: {metadata['commit']}\nBuild: {metadata['workflow_url']}\n\nBinaries, patches, installers and checksums are from this commit. Installer downloads are pinned to it."
        if not release:
            release = self.api("POST", "releases", {"tag_name": tag, "target_commitish": metadata["commit"],
                "name": f"Wings {metadata['version']}", "body": notes, "draft": True, "prerelease": True})
        self.upload(tag, directory)
        release = self.release_by_id(release["id"])
        self.remove_extra_assets(release, ASSETS)
        self.check_assets(self.release_by_id(release["id"]), directory)
        stable = VERSION_TAG.fullmatch(tag)
        is_stable = stable is not None and stable[4] is None
        latest = self.api("GET", "releases/latest", missing=True) if is_stable else None
        latest_version = VERSION_TAG.fullmatch(latest["tag_name"]) if latest else None
        promote = is_stable and (latest is None or (latest_version is not None and
            tuple(map(int, stable.group(1, 2, 3))) >= tuple(map(int, latest_version.group(1, 2, 3)))))
        self.api("PATCH", f"releases/{release['id']}", {"draft": False, "prerelease": not is_stable,
            "body": notes, "make_latest": "true" if promote else "false"})

    def promote_dev(self, metadata, directory):
        # Slow or rerun builds may finish after a newer develop commit.
        if self.api("GET", "commits/develop")["sha"] != metadata["commit"]:
            print("Snapshot published; skipping dev-latest because develop has advanced.")
            return
        tag = "dev-latest"
        previous = self.release(tag)
        old_commit = self.tag_commit(tag)
        with tempfile.TemporaryDirectory(prefix="wings-alias-backup-") as temp:
            backup = Path(temp)
            if previous and previous["assets"]:
                if any(not SAFE_NAME.fullmatch(asset["name"]) for asset in previous["assets"]):
                    raise ValueError("Unexpected existing release asset name")
                self.download(tag, backup)
                self.check_assets(previous, backup)
            release = previous
            if not release:
                self.set_tag(tag, metadata["commit"], mutable=True)
                release = self.api("POST", "releases", {"tag_name": tag, "name": "Wings development build",
                    "target_commitish": metadata["commit"], "draft": True, "prerelease": True})
            try:
                # Hide the alias while replacing assets so users cannot fetch a
                # mixture of architectures/checksums from different commits.
                self.api("PATCH", f"releases/{release['id']}", {"draft": True})
                self.upload(tag, directory)
                self.remove_extra_assets(self.release_by_id(release["id"]), ASSETS)
                self.check_assets(self.release_by_id(release["id"]), directory)
                self.set_tag(tag, metadata["commit"], mutable=True)
                self.api("PATCH", f"releases/{release['id']}", {"draft": False, "prerelease": True,
                    "make_latest": "false", "name": "Wings development build",
                    "body": f"Commit: {metadata['commit']}\n\nSnapshot: https://github.com/{self.repository}/releases/tag/dev-{metadata['commit']}\n\nBuild: {metadata['workflow_url']}"})
            except (Exception, KeyboardInterrupt):
                if previous:
                    try:
                        self.api("PATCH", f"releases/{release['id']}", {"draft": True})
                        self.remove_extra_assets(self.release_by_id(release["id"]), {file.name for file in backup.iterdir()})
                        if previous["assets"]:
                            self.upload(tag, backup)
                            self.check_assets(self.release_by_id(release["id"]), backup)
                        if old_commit:
                            self.set_tag(tag, old_commit, mutable=True)
                        self.api("PATCH", f"releases/{release['id']}", {key: previous[key]
                            for key in ("name", "body", "draft", "prerelease")})
                        print("Publication failed; restored the previous dev-latest release.")
                    except Exception as rollback_error:
                        print(f"Alias recovery failed; use the versioned snapshot and rerun publication: {rollback_error}")
                raise


def publish(directory):
    metadata = verify_bundle(directory, context())
    if (os.environ.get("GITHUB_ACTIONS") != "true" or metadata["repository"] != "Tomaxikz/daemon"
            or os.environ.get("GITHUB_EVENT_NAME") not in ("push", "workflow_dispatch")):
        raise ValueError("Publishing is only allowed from the repository's trusted GitHub workflow")
    ref = metadata["ref"]
    if ref != "refs/heads/develop" and not ref.startswith("refs/tags/v"):
        raise ValueError("Only develop or version tags can publish")
    publisher = Publisher(metadata["repository"])
    tag = ref.removeprefix("refs/tags/") if ref.startswith("refs/tags/") else "dev-" + metadata["commit"]
    publisher.snapshot(tag, metadata, directory)
    if ref == "refs/heads/develop":
        publisher.promote_dev(metadata, directory)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("build", "bundle", "verify", "publish"))
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--arch", choices=ARCHES)
    args = parser.parse_args()
    directory = args.directory.resolve()
    if args.command == "build":
        if not args.arch:
            parser.error("build requires --arch")
        build(directory, args.arch)
    elif args.command == "bundle":
        bundle(directory)
    elif args.command == "verify":
        verify_bundle(directory)
    else:
        # A graceful cancellation should attempt the same alias recovery as a
        # failed upload. Forced runner termination cannot be recovered in-process.
        def interrupted(_signum, _frame):
            raise KeyboardInterrupt()
        signal.signal(signal.SIGTERM, interrupted)
        publish(directory)


if __name__ == "__main__":
    main()
