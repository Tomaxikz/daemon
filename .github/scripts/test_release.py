import copy
import hashlib
import json
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch

import release

COMMIT = "a" * 40
OLD_COMMIT = "b" * 40


class FakePublisher(release.Publisher):
    """In-memory GitHub release/ref service: tests cannot publish anything."""
    def __init__(self):
        super().__init__("Tomaxikz/daemon")
        self.tags = {}
        self.releases = {}
        self.head = COMMIT
        self.fail_upload = False
        self.calls = []

    def api(self, method, route, payload=None, missing=False):
        self.calls.append((method, route, copy.deepcopy(payload)))
        if route == "commits/develop":
            return {"sha": self.head}
        if route.startswith("git/ref/tags/"):
            value = self.tags.get(route.removeprefix("git/ref/tags/"))
            return {"object": {"type": "commit", "sha": value}} if value else None
        if route == "git/refs":
            self.tags[payload["ref"].removeprefix("refs/tags/")] = payload["sha"]
            return {}
        if route.startswith("git/refs/tags/"):
            self.tags[route.removeprefix("git/refs/tags/")] = payload["sha"]
            return {}
        if route.startswith("releases/tags/"):
            value = self.releases.get(route.removeprefix("releases/tags/"))
            if value is None:
                return None
            result = {key: value[key] for key in value if key != "data"}
            result["assets"] = [{"id": f"{value['id']}-{name}", "name": name, "size": len(data),
                                  "digest": "sha256:" + hashlib.sha256(data).hexdigest()}
                                 for name, data in value["data"].items()]
            return copy.deepcopy(result)
        if route == "releases/latest":
            for value in self.releases.values():
                if value.get("make_latest") == "true":
                    return self.api("GET", "releases/tags/" + value["tag_name"])
            return None
        if route == "releases" and method == "POST":
            value = {"id": len(self.releases) + 1, "body": "", "data": {}, **payload}
            self.releases[value["tag_name"]] = value
            return self.api("GET", "releases/tags/" + value["tag_name"])
        if route.startswith("releases/assets/") and method == "DELETE":
            identity, name = route.removeprefix("releases/assets/").split("-", 1)
            value = next(value for value in self.releases.values() if value["id"] == int(identity))
            del value["data"][name]
            return None
        if route.startswith("releases/") and method == "PATCH":
            value = next(value for value in self.releases.values() if value["id"] == int(route.split("/")[1]))
            value.update(payload)
            return self.api("GET", "releases/tags/" + value["tag_name"])
        raise AssertionError((method, route, payload))

    def upload(self, tag, directory):
        if not self.releases[tag]["draft"]:
            raise AssertionError("Assets must never be replaced while publicly downloadable")
        for file in sorted(directory.iterdir()):
            self.releases[tag]["data"][file.name] = file.read_bytes()
            if self.fail_upload:
                self.fail_upload = False
                raise RuntimeError("simulated interrupted asset upload")

    def download(self, tag, directory):
        directory.mkdir(parents=True, exist_ok=True)
        for name, data in self.releases[tag]["data"].items():
            (directory / name).write_bytes(data)


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.metadata = {"schema_version": 1, "commit": COMMIT, "repository": "Tomaxikz/daemon",
                         "ref": "refs/heads/develop", "version": "dev-" + COMMIT[:12],
                         "go_version": "go1.26.7", "workflow_url": "https://github.com/Tomaxikz/daemon/actions/runs/1"}

    def make_bundle(self, name="bundle", metadata=None):
        metadata = metadata or self.metadata
        source = self.root / (name + "-source")
        source.mkdir()
        directory = self.root / name
        directory.mkdir()
        for filename in release.PATCHES | {"README.md"}:
            (source / filename).write_text(filename)
        for filename in release.INSTALLERS:
            (source / filename).write_text('RAW_BASE="${TOMAXIKZ_RAW_BASE:-https://raw.githubusercontent.com/Tomaxikz/daemon/develop}"\n')
        for arch, machine in release.ARCHES.items():
            data = b"\x7fELF\x02\x01" + bytes(12) + machine.to_bytes(2, "little")
            files = {}
            for suffix in ("", "_debug"):
                filename = f"wings_linux_{arch}{suffix}"
                (directory / filename).write_bytes(data)
                files[filename] = release.digest(directory / filename)
            (directory / f"build-{arch}.json").write_text(json.dumps({**metadata, "files": files}))
        with patch.object(release, "context", return_value=metadata):
            release.bundle(directory, source)
        return directory, release.verify_bundle(directory, metadata)

    def test_tag_names_cannot_inject_shell_or_select_arbitrary_versions(self):
        self.assertEqual(release.version_for("refs/tags/v1.2.3", COMMIT), "1.2.3")
        self.assertEqual(release.version_for("refs/tags/v1.2.3-rc.1", COMMIT), "1.2.3-rc.1")
        for ref in ("refs/tags/v1.2.3;whoami", "refs/tags/vnightly", "refs/tags/v01.2.3"):
            with self.assertRaises(ValueError):
                release.version_for(ref, COMMIT)

    def test_bundle_pins_sources_and_has_portable_verified_checksums(self):
        directory, metadata = self.make_bundle()
        self.assertEqual(set(metadata["files"]), release.PAYLOAD)
        for filename in release.INSTALLERS:
            text = (directory / filename).read_text()
            self.assertIn("/" + COMMIT, text)
            self.assertNotIn("/develop", text)
            self.assertIn("TOMAXIKZ_RAW_BASE", text)
        self.assertNotIn("dist/", (directory / "SHA256SUMS").read_text())
        (directory / "wings_linux_amd64").write_bytes(b"tampered")
        with self.assertRaises(ValueError):
            release.verify_bundle(directory)

    def test_bundle_rejects_mixed_architecture_sources(self):
        directory, _ = self.make_bundle()
        with self.assertRaises(ValueError):
            release.verify_bundle(directory, {**self.metadata, "commit": OLD_COMMIT})
        with self.assertRaises(ValueError):
            release.verify_elf(directory / "wings_linux_amd64", "arm64")

    def test_remote_permission_errors_are_not_treated_as_missing_releases(self):
        publisher = release.Publisher("Tomaxikz/daemon")
        for code in (403, 500):
            response = SimpleNamespace(returncode=1, stdout="", stderr=f"gh: request failed (HTTP {code})\n")
            with patch.object(release.subprocess, "run", return_value=response), self.assertRaises(RuntimeError):
                publisher.release("dev-latest")
        response = SimpleNamespace(returncode=1, stdout="", stderr="gh: Not Found (HTTP 404)\n")
        with patch.object(release.subprocess, "run", return_value=response):
            self.assertIsNone(publisher.release("dev-latest"))

    def test_corrupt_remote_upload_cannot_be_published(self):
        directory, metadata = self.make_bundle()
        publisher = FakePublisher()
        original_upload = publisher.upload
        def corrupt_upload(tag, source):
            original_upload(tag, source)
            publisher.releases[tag]["data"]["wings_linux_amd64"] = b"corrupt"
        publisher.upload = corrupt_upload
        with self.assertRaises(ValueError):
            publisher.snapshot("dev-" + COMMIT, metadata, directory)
        self.assertTrue(publisher.releases["dev-" + COMMIT]["draft"])

    def test_manifest_rejects_traversal_and_duplicate_names(self):
        for line in ("0" * 64 + "  ../outside\n", ("0" * 64 + "  README.md\n") * 2):
            with self.subTest(line=line):
                name = "bundle-" + str(len(list(self.root.iterdir())))
                directory, _ = self.make_bundle(name)
                with (directory / "SHA256SUMS").open("a") as output:
                    output.write(line)
                with self.assertRaises(ValueError):
                    release.verify_bundle(directory)

    def test_only_trusted_workflow_events_can_publish(self):
        directory, _ = self.make_bundle()
        with patch.object(release, "context", return_value=self.metadata), patch.dict(release.os.environ,
                {"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "pull_request"}), patch.object(release, "Publisher") as publisher:
            with self.assertRaises(ValueError):
                release.publish(directory)
            publisher.assert_not_called()

    def test_snapshot_published_only_after_complete_verified_upload(self):
        directory, metadata = self.make_bundle()
        publisher = FakePublisher()
        tag = "dev-" + COMMIT
        publisher.snapshot(tag, metadata, directory)
        self.assertFalse(publisher.releases[tag]["draft"])
        self.assertEqual(set(publisher.releases[tag]["data"]), release.ASSETS)
        self.assertEqual(publisher.tags[tag], COMMIT)
        self.assertTrue(publisher.releases[tag]["prerelease"])

    def test_failed_snapshot_stays_a_draft_and_can_be_retried(self):
        directory, metadata = self.make_bundle()
        publisher = FakePublisher()
        publisher.fail_upload = True
        tag = "dev-" + COMMIT
        with self.assertRaises(RuntimeError):
            publisher.snapshot(tag, metadata, directory)
        self.assertTrue(publisher.releases[tag]["draft"])
        publisher.snapshot(tag, metadata, directory)
        self.assertFalse(publisher.releases[tag]["draft"])

    def test_published_snapshot_is_reused_without_replacing_assets(self):
        directory, metadata = self.make_bundle()
        publisher = FakePublisher()
        tag = "dev-" + COMMIT
        publisher.snapshot(tag, metadata, directory)
        original = copy.deepcopy(publisher.releases[tag]["data"])
        other, newer = self.make_bundle("rerun", {**self.metadata, "workflow_url": "https://github.com/Tomaxikz/daemon/actions/runs/2"})
        publisher.snapshot(tag, newer, other)
        self.assertEqual(publisher.releases[tag]["data"], original)
        self.assertEqual((other / "RELEASE.json").read_bytes(), original["RELEASE.json"])

    def test_version_tag_cannot_be_moved(self):
        directory, metadata = self.make_bundle()
        publisher = FakePublisher()
        publisher.tags["v1.2.3"] = OLD_COMMIT
        with self.assertRaises(ValueError):
            publisher.snapshot("v1.2.3", metadata, directory)
        self.assertEqual(publisher.tags["v1.2.3"], OLD_COMMIT)

    def test_stale_development_run_cannot_replace_alias(self):
        directory, metadata = self.make_bundle()
        publisher = FakePublisher()
        publisher.head = OLD_COMMIT
        publisher.promote_dev(metadata, directory)
        self.assertNotIn("dev-latest", publisher.releases)

    def test_alias_failure_restores_previous_tag_metadata_and_assets(self):
        directory, metadata = self.make_bundle()
        publisher = FakePublisher()
        publisher.tags["dev-latest"] = OLD_COMMIT
        publisher.api("POST", "releases", {"tag_name": "dev-latest", "name": "Previous build", "body": "old notes",
                                            "draft": False, "prerelease": True})
        publisher.releases["dev-latest"]["data"] = {"wings_linux_amd64": b"old binary", "SHA256SUMS": b"old sums"}
        previous = copy.deepcopy(publisher.releases["dev-latest"])
        publisher.fail_upload = True
        with self.assertRaises(RuntimeError):
            publisher.promote_dev(metadata, directory)
        self.assertEqual(publisher.tags["dev-latest"], OLD_COMMIT)
        self.assertEqual(publisher.releases["dev-latest"], previous)
        publisher.promote_dev(metadata, directory)
        self.assertEqual(publisher.tags["dev-latest"], COMMIT)
        self.assertEqual(set(publisher.releases["dev-latest"]["data"]), release.ASSETS)
        self.assertFalse(publisher.releases["dev-latest"]["draft"])

    def test_older_stable_release_does_not_replace_latest(self):
        publisher = FakePublisher()
        for tag in ("v2.0.0", "v1.9.0", "v2.1.0-rc.1"):
            metadata = {**self.metadata, "ref": "refs/tags/" + tag, "version": tag[1:]}
            directory, metadata = self.make_bundle(tag, metadata)
            publisher.snapshot(tag, metadata, directory)
        self.assertEqual(publisher.api("GET", "releases/latest")["tag_name"], "v2.0.0")
        self.assertTrue(publisher.releases["v2.1.0-rc.1"]["prerelease"])


if __name__ == "__main__":
    unittest.main()
