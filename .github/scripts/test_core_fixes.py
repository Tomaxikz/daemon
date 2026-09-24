from pathlib import Path
import subprocess
import tempfile
import unittest

from apply_core_fixes import CORE_FILES, apply
from validate_installers import ROOT, UPSTREAM


class CoreFixTests(unittest.TestCase):
    def fixture(self, root):
        for name in [*CORE_FILES, "server/configuration.go"]:
            contents = subprocess.run(
                ["git", "show", f"{UPSTREAM}:{name}"], cwd=ROOT, stdout=subprocess.PIPE, check=True
            ).stdout
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(contents)

    def test_repeat_preserves_sources_and_backups(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "source"
            backup = Path(temporary) / "backup"
            self.fixture(root)
            originals = {name: (root / name).read_bytes() for name in CORE_FILES}

            apply(root, backup)
            fixed = {name: (root / name).read_bytes() for name in CORE_FILES}
            self.assertNotEqual(originals, fixed)

            apply(root, backup)

            self.assertEqual(fixed, {name: (root / name).read_bytes() for name in CORE_FILES})
            self.assertEqual(originals, {name: (backup / name).read_bytes() for name in CORE_FILES})

    def test_unknown_configuration_stops_before_source_edits(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "source"
            self.fixture(root)
            configuration = root / "server/configuration.go"
            configuration.write_text(
                configuration.read_text().replace(
                    "type Configuration struct {", "type Configuration struct {\n\tCustom string"
                )
            )
            originals = {name: (root / name).read_bytes() for name in CORE_FILES}
            with self.assertRaisesRegex(ValueError, "Configuration fields differ"):
                apply(root, Path(temporary) / "backup")

            self.assertEqual(originals, {name: (root / name).read_bytes() for name in CORE_FILES})


if __name__ == "__main__":
    unittest.main()
