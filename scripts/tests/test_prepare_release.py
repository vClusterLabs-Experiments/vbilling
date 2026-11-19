import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
REPOSITORY = "ghcr.io/vclusterlabs-experiments/vbilling"
DIGEST = "sha256:" + "a" * 64


class ReleasePackagingTests(unittest.TestCase):
    def setUp(self):
        self.work = tempfile.TemporaryDirectory()
        self.addCleanup(self.work.cleanup)
        self.chart = Path(self.work.name) / "chart"
        shutil.copytree(ROOT / "deploy/helm/vbilling", self.chart)
        self.metadata = Path(self.work.name) / "release-metadata.json"
        self.env = dict(os.environ, RELEASE_TAG="v0.3.1", SOURCE_COMMIT="b" * 40,
                        IMAGE_REPOSITORY=REPOSITORY, IMAGE_DIGEST=DIGEST)

    def prepare(self):
        return subprocess.run(["python3", str(ROOT / "scripts/prepare-release.py"),
                               "--chart", str(self.chart), "--metadata", str(self.metadata)],
                              env=self.env, capture_output=True, text=True)

    def render_image(self, *args):
        result = subprocess.run(["helm", "template", "vbilling", str(self.chart),
                                 "--show-only", "templates/statefulset.yaml", *args],
                                capture_output=True, text=True, check=True)
        return re.search(r'^\s+image: "([^"]+)"', result.stdout, re.MULTILINE)[1]

    def test_published_chart_and_metadata_use_the_built_digest(self):
        result = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.render_image(), f"{REPOSITORY}@{DIGEST}")
        meta = json.loads(self.metadata.read_text())
        self.assertEqual(meta["source_commit"], "b" * 40)
        self.assertEqual(meta["image"], self.render_image())
        self.assertEqual(meta["platforms"], ["linux/amd64", "linux/arm64"])

    def test_older_saved_values_cannot_hide_the_new_published_digest(self):
        old_values = Path(self.work.name) / "old-values.yaml"
        old_values.write_text((self.chart / "values.yaml").read_text().replace('  digest: ""\n', ""))
        self.assertEqual(self.prepare().returncode, 0)
        self.assertEqual(self.render_image("-f", str(old_values)), f"{REPOSITORY}@{DIGEST}")

    def test_explicit_image_overrides_are_respected(self):
        self.assertEqual(self.prepare().returncode, 0)
        self.assertEqual(self.render_image("--set", "image.tag=development"), f"{REPOSITORY}:development")
        self.assertEqual(self.render_image("--set", "image.repository=registry.example.com/custom"),
                         "registry.example.com/custom:v0.3.1")
        override = "sha256:" + "c" * 64
        self.assertEqual(self.render_image("--set", "image.digest=" + override), f"{REPOSITORY}@{override}")

    def test_mismatched_release_version_cannot_be_packaged(self):
        before = (self.chart / "Chart.yaml").read_text()
        self.env["RELEASE_TAG"] = "v0.3.2"
        self.assertNotEqual(self.prepare().returncode, 0)
        self.assertEqual((self.chart / "Chart.yaml").read_text(), before)
        self.assertFalse(self.metadata.exists())

    def test_invalid_digest_cannot_be_packaged(self):
        self.env["IMAGE_DIGEST"] = "latest"
        self.assertNotEqual(self.prepare().returncode, 0)
        self.assertFalse(self.metadata.exists())


if __name__ == "__main__":
    unittest.main()
