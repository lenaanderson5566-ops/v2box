import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("credentials", Path(__file__).with_name("node-credentials.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class CredentialsTest(unittest.TestCase):
    def test_replaces_only_selected_node_and_restricts_permissions(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config.json"
            path.write_text(json.dumps({"Nodes": [{"ApiKey": "old"}, {"ApiKey": "unchanged"}], "Log": {"Level": "info"}}))
            secret = "nk_" + "a" * 32 + "." + "b" * 64
            module.replace_key(path, 0, secret)
            data = json.loads(path.read_text())
            self.assertEqual(data["Nodes"][0]["ApiKey"], secret)
            self.assertEqual(data["Nodes"][1]["ApiKey"], "unchanged")
            self.assertEqual(data["Log"], {"Level": "info"})
            if os.name != "nt":
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(len(list(Path(directory).iterdir())), 1)

    def test_shared_listener_selection_preserves_unrelated_settings(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config.json"
            original = {"Nodes": [{"ApiKey": "first", "CertificateDirectory": "/keep"}, {"ApiKey": "second"}, {"ApiKey": "third"}], "Log": {"Level": "warn"}}
            path.write_text(json.dumps(original))
            module.set_shared_listener(path, [0, 1], "public-443")
            updated = json.loads(path.read_text())
            self.assertEqual(updated["Nodes"][0]["SharedListener"], "public-443")
            self.assertEqual(updated["Nodes"][1]["SharedListener"], "public-443")
            self.assertEqual(updated["Nodes"][2], original["Nodes"][2])
            self.assertEqual(updated["Nodes"][0]["CertificateDirectory"], "/keep")
            self.assertEqual(updated["Log"], original["Log"])
            self.assertEqual([n["ApiKey"] for n in updated["Nodes"]], ["first", "second", "third"])
            module.set_shared_listener(path, [1], "-")
            self.assertNotIn("SharedListener", json.loads(path.read_text())["Nodes"][1])
            before = path.read_bytes()
            for indices, group in [([], "g"), ([0, 0], "g"), ([99], "g"), ([0], "../invalid")]:
                with self.assertRaises(ValueError):
                    module.set_shared_listener(path, indices, group)
                self.assertEqual(path.read_bytes(), before)
            with patch.object(module.os, "replace", side_effect=OSError("disk failure")):
                with self.assertRaises(OSError):
                    module.set_shared_listener(path, [0], "new-group")
            self.assertEqual(path.read_bytes(), before)
            self.assertEqual(len(list(Path(directory).iterdir())), 1)

    def test_bad_key_and_failed_write_preserve_config(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config.json"
            original = '{"Nodes":[{"ApiKey":"old"}]}'
            path.write_text(original)
            with self.assertRaises(ValueError):
                module.replace_key(path, 0, "invalid")
            secret = "nk_" + "a" * 32 + "." + "b" * 64
            with patch.object(module.os, "replace", side_effect=OSError("failed")):
                with self.assertRaises(OSError):
                    module.replace_key(path, 0, secret)
            self.assertEqual(path.read_text(), original)
            self.assertEqual(len(list(Path(directory).iterdir())), 1)


if __name__ == "__main__":
    unittest.main()
