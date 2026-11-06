#!/usr/bin/env python3
"""Unit tests for keycloak-theme-configmap.py (gd-qiUiXvTX0k).

Runs against a synthetic theme tree in a temp dir (so the assertions are about
the encoding, not about today's theme) plus one pass over the real tree in
gardenio/keycloak-theme/gardenio, which must render under the cap.

Usage: python3 gardenio/scripts/keycloak-theme-configmap_test.py
"""
import base64
import hashlib
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "keycloak-theme-configmap.py")
REAL_THEME = os.path.join(HERE, "..", "keycloak-theme", "gardenio")

spec = importlib.util.spec_from_file_location("ktc", SCRIPT)
ktc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ktc)


def write(root, rel, content):
    p = os.path.join(root, *rel.split("/"))
    os.makedirs(os.path.dirname(p), exist_ok=True)
    mode = "wb" if isinstance(content, bytes) else "w"
    with open(p, mode) as f:
        f.write(content)


def run(*args, **kw):
    return subprocess.run([sys.executable, SCRIPT, *args], capture_output=True, text=True, **kw)


class SyntheticTree(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.theme = os.path.join(self.tmp, "theme")
        write(self.theme, "login/theme.properties", "parent=keycloak.v2\n")
        write(self.theme, "login/template.ftl", "<#import \"x\">\n")
        write(self.theme, "login/resources/css/gardenio.css", "body { color: red }\n")
        # Non-UTF-8 bytes with a binary extension: must ride in binaryData.
        self.font = bytes([0x77, 0x4F, 0x46, 0x32, 0xFF, 0xFE, 0x00, 0x80])
        write(self.theme, "login/resources/fonts/pixel.woff2", self.font)
        write(self.theme, "email/theme.properties", "parent=base\n")

    def render(self, *extra):
        r = run("--theme-dir", self.theme, "--release", "rel", "--namespace", "ns", *extra)
        self.assertEqual(r.returncode, 0, r.stderr)
        return json.loads(r.stdout)

    def test_keys_encode_slash_as_double_underscore(self):
        cm = self.render()
        self.assertEqual(cm["kind"], "ConfigMap")
        self.assertEqual(sorted(cm["data"]), [
            "email__theme.properties",
            "login__resources__css__gardenio.css",
            "login__template.ftl",
            "login__theme.properties",
        ])
        self.assertEqual(cm["data"]["login__resources__css__gardenio.css"], "body { color: red }\n")
        for k in list(cm["data"]) + list(cm["binaryData"]):
            self.assertRegex(k, r"^[-._a-zA-Z0-9]+$")

    def test_binary_file_goes_through_binary_data_byte_for_byte(self):
        cm = self.render()
        self.assertEqual(list(cm["binaryData"]), ["login__resources__fonts__pixel.woff2"])
        self.assertEqual(base64.b64decode(cm["binaryData"]["login__resources__fonts__pixel.woff2"]), self.font)
        self.assertNotIn("login__resources__fonts__pixel.woff2", cm["data"])

    def test_name_namespace_labels_default_and_override(self):
        cm = self.render()
        self.assertEqual(cm["metadata"]["name"], "rel-keycloak-runtime-theme-files")
        self.assertEqual(cm["metadata"]["namespace"], "ns")
        self.assertEqual(cm["metadata"]["labels"]["app.kubernetes.io/instance"], "rel")
        self.assertEqual(cm["metadata"]["labels"]["app.kubernetes.io/component"], "keycloak-runtime")
        self.assertNotIn("helm.sh/chart", cm["metadata"]["labels"])
        cm2 = self.render("--name", "custom-theme")
        self.assertEqual(cm2["metadata"]["name"], "custom-theme")

    def test_checksum_is_sha256_over_sorted_file_lines(self):
        files = ktc.theme_files(self.theme)
        acc = "".join("%s %s\n" % (rel, hashlib.sha256(b).hexdigest()) for rel, b in files)
        want = hashlib.sha256(acc.encode()).hexdigest()
        cm = self.render()
        self.assertEqual(cm["metadata"]["annotations"]["gardenio.dev/theme-checksum"], want)
        r = run("--theme-dir", self.theme, "--checksum-only")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(r.stdout.strip(), want)

    def test_checksum_tracks_edits_and_new_files_including_binary(self):
        base = self.render()["metadata"]["annotations"]["gardenio.dev/theme-checksum"]
        write(self.theme, "login/resources/css/gardenio.css", "body { color: blue }\n")
        edited = self.render()["metadata"]["annotations"]["gardenio.dev/theme-checksum"]
        self.assertNotEqual(base, edited)
        write(self.theme, "login/resources/fonts/pixel.woff2", self.font[:-1] + bytes([self.font[-1] ^ 0xFF]))
        flipped = self.render()["metadata"]["annotations"]["gardenio.dev/theme-checksum"]
        self.assertNotEqual(edited, flipped)
        write(self.theme, "login/extra.ftl", "<#-- new -->\n")
        added = self.render()["metadata"]["annotations"]["gardenio.dev/theme-checksum"]
        self.assertNotEqual(flipped, added)

    def test_checksum_ignores_name_and_namespace(self):
        a = self.render()["metadata"]["annotations"]["gardenio.dev/theme-checksum"]
        b = self.render("--name", "other", "--namespace", "elsewhere")["metadata"]["annotations"]["gardenio.dev/theme-checksum"]
        self.assertEqual(a, b)

    def test_refuses_path_with_double_underscore(self):
        write(self.theme, "login/bad__name.ftl", "x")
        r = run("--theme-dir", self.theme)
        self.assertEqual(r.returncode, 1)
        self.assertIn("'__'", r.stderr)

    def test_refuses_non_utf8_text_file(self):
        write(self.theme, "login/odd.ftl", bytes([0xFF, 0xFE]))
        r = run("--theme-dir", self.theme)
        self.assertEqual(r.returncode, 1)
        self.assertIn("not UTF-8", r.stderr)

    def test_refuses_over_cap_counting_binary_as_base64(self):
        # 4 text bytes + ceil(8*4/3)=12 base64 bytes = too big for a 15-byte cap.
        r = run("--theme-dir", self.theme, "--max-bytes", "15")
        self.assertEqual(r.returncode, 1)
        self.assertIn("as stored", r.stderr)

    def test_missing_or_empty_tree_is_exit_1(self):
        r = run("--theme-dir", os.path.join(self.tmp, "nope"))
        self.assertEqual(r.returncode, 1)
        empty = os.path.join(self.tmp, "empty")
        os.makedirs(empty)
        r = run("--theme-dir", empty)
        self.assertEqual(r.returncode, 1)

    def test_output_is_one_json_document_kubectl_can_apply(self):
        r = run("--theme-dir", self.theme)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(r.stdout.count("\n"), 1)
        json.loads(r.stdout)


class RealTree(unittest.TestCase):
    def test_shipped_theme_renders_under_the_cap(self):
        r = run("--theme-dir", REAL_THEME)
        self.assertEqual(r.returncode, 0, r.stderr)
        cm = json.loads(r.stdout)
        self.assertIn("login__theme.properties", cm["data"])
        self.assertIn("login__resources__fonts__pixelify-sans-latin-600-normal.woff2", cm.get("binaryData", {}))
        stored = sum(len(v) for v in cm["data"].values()) + sum(len(v) for v in cm.get("binaryData", {}).values())
        self.assertLessEqual(stored, ktc.DEFAULT_MAX_BYTES)


if __name__ == "__main__":
    unittest.main()
