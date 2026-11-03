import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location("artifacts", Path(__file__).resolve().parents[1] / "scripts" / "artifacts.py")
artifacts = importlib.util.module_from_spec(spec)
spec.loader.exec_module(artifacts)
INFO = {"GoVersion": "go1.27.3", "Main": {"Path": "github.com/psvmcc/roi", "Version": "v1.0.0"},
        "Deps": [{"Path": "example.org/library", "Version": "v2.0.0", "Sum": "h1:example"}],
        "Settings": [{"Key": "GOOS", "Value": "linux"}, {"Key": "CGO_ENABLED", "Value": "0"},
                     {"Key": "unrelated", "Value": "not-exported"}]}


class ArtifactTests(unittest.TestCase):
    def test_bom_inventory_and_checksums(self):
        bom = artifacts.make_bom(INFO, "a" * 64)
        self.assertEqual(bom["bomFormat"], "CycloneDX")
        self.assertEqual(bom["metadata"]["component"]["hashes"][0]["content"], "a" * 64)
        self.assertEqual(len(bom["components"]), 2)
        self.assertEqual(bom["components"][1]["properties"][0]["value"], "h1:example")
        self.assertNotIn("not-exported", json.dumps(bom))
        refs = {c["bom-ref"] for c in bom["components"]}
        self.assertEqual(refs, set(bom["dependencies"][0]["dependsOn"]))

    def test_actual_file_hash_and_metadata(self):
        with tempfile.TemporaryDirectory() as d:
            binary = Path(d) / "roi-linux-amd64"
            binary.write_bytes(b"test binary contents")
            with mock.patch.object(artifacts.subprocess, "check_output", return_value=json.dumps(INFO)) as command:
                sbom, checksum = artifacts.generate(binary)
            self.assertEqual(command.call_args.args[0][-1], str(binary))
            digest = hashlib.sha256(binary.read_bytes()).hexdigest()
            self.assertEqual(checksum.read_text(), digest + "  " + binary.name + "\n")
            self.assertEqual(json.loads(sbom.read_text())["metadata"]["component"]["hashes"][0]["content"], digest)

    def test_replacements_and_deduplication(self):
        module = {"Path": "old.org/library", "Version": "v1", "Replace": {"Path": "new.org/library", "Version": "v2"}}
        self.assertEqual(artifacts.component(module)["name"], "new.org/library")
        info = copy.deepcopy(INFO)
        info["Deps"] *= 2
        self.assertEqual(len(artifacts.make_bom(info, "a" * 64)["components"]), 2)
        module["Replace"] = {"Path": "/private/local/library"}
        with self.assertRaises(ValueError):
            artifacts.component(module)


if __name__ == "__main__":
    unittest.main()
