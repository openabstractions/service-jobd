"""Isolated payload checks; dummy bytes are staged, never executed.
Run: python installer/test_central_cli.py
"""
import importlib.util
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result

WIN = module("windows_packager", HERE / "build.py")
POSIX = module("posix_packager", HERE / "posix/build.py")
BYTES = b"fixture central CLI bytes; never executable"


class CentralCliPayload(unittest.TestCase):
    def test_windows_stages_required_console_tool(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            binaries = root / "bin"
            binaries.mkdir()
            for _, path, kind, _, _, _, _ in WIN.rows("payload.tsv"):
                if kind == "gobuild":
                    (binaries / Path(path).name).write_bytes(BYTES)
            with patch.object(WIN.subprocess, "run", side_effect=AssertionError("unexpected build")):
                dropped = WIN.stage({"installer": HERE}, root / "out", "x64", binaries)
            central = "tools/openabstractions.exe"
            self.assertNotIn(central, dropped)
            self.assertEqual((root / "out/payload" / central).read_bytes(), BYTES)
            row = next(row for row in WIN.rows("payload.tsv") if row[1] == central)
            self.assertEqual((row[0], row[-1]), ("tools", "console"))
            self.assertNotIn(central, WIN.GATE)
            (binaries / "openabstractions.exe").unlink()
            dropped = WIN.stage({"installer": HERE}, root / "absent", "x64", binaries)
            self.assertIn(central, dropped)  # main refuses non-gated missing inputs

    def test_posix_exact_bytes_mode_and_missing_input(self):
        for platform in ("linux", "macos"):
            with self.subTest(platform=platform), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                binaries = root / "bin"
                binaries.mkdir()
                items = [r for r in POSIX.payload(platform) if r[1] == "gobuild"]
                for path, _, _, _, _ in items:
                    (binaries / Path(path).name).write_bytes(BYTES)
                files = POSIX.stage(items, {}, root / "out", platform, "amd64", prebuilt=binaries)
                self.assertIn((".local/bin/openabstractions", 0o755), files)
                self.assertEqual((root / "out/.local/bin/openabstractions").read_bytes(), BYTES)
                (binaries / "openabstractions").unlink()
                with self.assertRaises(FileNotFoundError):
                    POSIX.stage(items, {}, root / "absent", platform, "amd64", prebuilt=binaries)

    def test_signing_and_installed_verification_include_cli(self):
        workflow = (HERE.parent / "research/ci76/redist-release.yml").read_text(encoding="utf-8")
        self.assertEqual(workflow.count("'jobd.exe','jobdw.exe','dl.exe','jobctl.exe','openabstractions.exe'"), 2)
        self.assertEqual(workflow.count("for f in jobd dl jobctl openabstractions; do"), 2)
        self.assertIn("for name in jobd dl jobctl openabstractions; do", workflow)
        self.assertIn('Source="payload/tools/openabstractions.exe"', (HERE / "abstraction.wxs").read_text(encoding="utf-8"))
        tools = (HERE.parent / "research/org-edits/redist/tools.tsv").read_text(encoding="utf-8")
        self.assertIn("openabstractions.exe\tgithub.com/openabstractions/abstractions/serve@v0.1.0\t.\ttools\tconsole", tools)


if __name__ == "__main__":
    unittest.main()
