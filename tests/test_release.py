import importlib.util
from pathlib import Path
import subprocess
import sys
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("release", ROOT / "scripts/release.py")
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseTests(unittest.TestCase):
    def test_valid_stable_and_preview_tags(self):
        for tag in ["1.2.3", "v0.0.0", "v10.200.300"]:
            self.assertFalse(release.validate_tag(tag), tag)
        for tag in ["v1.2.3-rc.1", "1.0.0-beta", "1.0.0-0", "1.0.0-preview-01"]:
            self.assertTrue(release.validate_tag(tag), tag)

    def test_invalid_tags_and_shell_payloads(self):
        for tag in ["", "main", "v1.2", "v01.2.3", "1.02.3", "1.2.03", "1.2.3-01",
                    "1.2.3+build", "1.2.3-", "v1.2.3-rc..1", "1.2.3\n", "$(whoami)",
                    "1.2.3;true", "../v1.2.3", "v1.2.3-" + "a" * 130]:
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                release.validate_tag(tag)

    def test_cli_output_and_errors(self):
        script = str(ROOT / "scripts/release.py")
        result = subprocess.run([sys.executable, script, "v1.2.3-rc.1"], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "prerelease=true\n")
        for args in [[], ["1.2.3", "extra"], ["1.2.3+bad"]]:
            self.assertNotEqual(subprocess.run([sys.executable, script, *args], capture_output=True).returncode, 0)


if __name__ == "__main__":
    unittest.main()
