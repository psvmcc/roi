"""Exercise smoke-test cleanup/failure paths without a container daemon."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class ContainerSmokeTests(unittest.TestCase):
    def run_smoke(self, failure):
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            engine = work / "engine"
            engine.write_text('''#!/bin/sh
printf '%s\\n' "$*" >> "$SMOKE_LOG"
if [ "$1" = run ] && [ "$SMOKE_FAILURE" = config ]; then exit 4; fi
case "$1" in
  port) echo 127.0.0.1:19999;;
  image) echo 12345;;
esac
''')
            curl = work / "curl"
            curl.write_text('''#!/bin/sh
case "$*" in
  */health) [ "$SMOKE_FAILURE" != health ] || exit 7; echo ok;;
  */metrics) [ "$SMOKE_FAILURE" != metrics ] || exit 7; echo 'roi_feeds 0';;
esac
''')
            pause = work / "sleep"
            pause.write_text("#!/bin/sh\nexit 0\n")
            for executable in [engine, curl, pause]:
                executable.chmod(0o700)
            log = work / "commands"
            env = dict(os.environ, CONTAINER_ENGINE=str(engine), SMOKE_LOG=str(log),
                       SMOKE_FAILURE=failure, PATH=str(work) + os.pathsep + os.environ["PATH"], TMPDIR=directory)
            result = subprocess.run(["sh", "scripts/container-smoke.sh", "roi:test"], cwd=ROOT, env=env,
                                    capture_output=True, text=True, timeout=20)
            commands = log.read_text()
            self.assertIn("rm -f roi-smoke-", commands)
            self.assertEqual(list(work.glob("tmp.*")), [])
            return result, commands

    def test_success_checks_metrics_and_cleans_up(self):
        result, commands = self.run_smoke("")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("--read-only --cap-drop=ALL", commands)
        self.assertIn("image inspect roi:test", commands)

    def test_config_health_and_metrics_failures(self):
        for failure in ["config", "health", "metrics"]:
            with self.subTest(failure=failure):
                result, commands = self.run_smoke(failure)
                self.assertNotEqual(result.returncode, 0)
                if failure == "health":
                    self.assertIn("logs roi-smoke-", commands)


if __name__ == "__main__":
    unittest.main()
