#!/usr/bin/env python3
"""Unit tests for strip-helm-yaml-comments.py (gd-qiUiXvTX0k).

Usage: python3 scripts/strip-helm-yaml-comments_test.py
"""
import importlib.util
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "strip-helm-yaml-comments.py")
spec = importlib.util.spec_from_file_location("syc", SCRIPT)
syc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(syc)
strip = syc.strip


class Strip(unittest.TestCase):
    def test_drops_comment_only_lines_keeps_everything_else(self):
        src = (
            "# top of file\n"
            "apiVersion: v1\n"
            "  # indented comment\n"
            "kind: ConfigMap   # trailing comment stays\n"
            "metadata:\n"
            "  name: x # also stays\n"
            "  url: \"http://h/#frag\"\n"
        )
        want = (
            "apiVersion: v1\n"
            "kind: ConfigMap   # trailing comment stays\n"
            "metadata:\n"
            "  name: x # also stays\n"
            "  url: \"http://h/#frag\"\n"
        )
        self.assertEqual(strip(src), want)

    def test_block_scalar_content_is_kept(self):
        src = (
            "data:\n"
            "  # comment before the script: dropped\n"
            "  run.sh: |\n"
            "    #!/bin/sh\n"
            "    # a shell comment: content, kept\n"
            "    echo hi\n"
            "\n"
            "    # after a blank line, still content\n"
            "  # back at key indent: a yaml comment, dropped\n"
            "  other: >-\n"
            "    # folded content kept\n"
            "    text\n"
            "  plain: value\n"
        )
        want = (
            "data:\n"
            "  run.sh: |\n"
            "    #!/bin/sh\n"
            "    # a shell comment: content, kept\n"
            "    echo hi\n"
            "\n"
            "    # after a blank line, still content\n"
            "  other: >-\n"
            "    # folded content kept\n"
            "    text\n"
            "  plain: value\n"
        )
        self.assertEqual(strip(src), want)

    def test_block_scalar_indicators_variants(self):
        for ind in ("|", "|-", "|+", ">", ">-", "|2", "|  # note", "| {{- if .x }}"):
            src = "  key: %s\n    # content\n  # comment\n" % ind
            got = strip(src)
            self.assertIn("    # content\n", got, ind)
            self.assertNotIn("  # comment\n", got, ind)

    def test_sequence_item_block_scalars(self):
        src = (
            "args:\n"
            "  - |\n"
            "    # content in a bare sequence block\n"
            "  - name: cmd\n"
            "    script: |\n"
            "      # content in a nested mapping block\n"
            "  # comment between items\n"
            "  - plain\n"
        )
        got = strip(src)
        self.assertIn("# content in a bare sequence block", got)
        self.assertIn("# content in a nested mapping block", got)
        self.assertNotIn("comment between items", got)

    def test_template_only_lines_are_transparent_to_block_tracking(self):
        src = (
            "  script: |\n"
            "    line1\n"
            "{{- if .Values.x }}\n"
            "    # content after a template line at column 0\n"
            "{{- end }}\n"
            "  {{- if .Values.y }}\n"
            "  # yaml comment at key indent: dropped\n"
            "  other: 1\n"
            "  {{- end }}\n"
        )
        got = strip(src)
        self.assertIn("# content after a template line", got)
        self.assertNotIn("yaml comment at key indent", got)
        self.assertIn("  other: 1\n", got)

    def test_comment_lines_with_template_actions_are_kept(self):
        src = "a: 1\n# {{- $x := 1 }}\n#   {{ include \"h\" . }}\n# plain\n"
        self.assertEqual(strip(src), "a: 1\n# {{- $x := 1 }}\n#   {{ include \"h\" . }}\n")

    def test_define_bodies_are_left_alone(self):
        src = (
            "# outside: dropped\n"
            "{{- define \"x.script\" -}}\n"
            "#!/bin/sh\n"
            "# inside a define: kept\n"
            "{{- if .y }}\n"
            "# nested if inside define: kept\n"
            "{{- end }}\n"
            "{{- end -}}\n"
            "# after the define: dropped\n"
            "{{- if .z }}\n"
            "# inside a plain if: dropped\n"
            "{{- end }}\n"
        )
        want = (
            "{{- define \"x.script\" -}}\n"
            "#!/bin/sh\n"
            "# inside a define: kept\n"
            "{{- if .y }}\n"
            "# nested if inside define: kept\n"
            "{{- end }}\n"
            "{{- end -}}\n"
            "{{- if .z }}\n"
            "{{- end }}\n"
        )
        self.assertEqual(strip(src), want)

    def test_only_yaml_templates_are_rewritten_in_place(self):
        chart = tempfile.mkdtemp()
        tpl = os.path.join(chart, "templates")
        os.makedirs(os.path.join(tpl, "sub"))
        files = {
            "a.yaml": "# c\nk: v\n",
            "sub/b.yml": "# c\nk: v\n",
            "_helpers.tpl": "# kept\n{{- define \"x\" }}y{{ end }}\n",
            "NOTES.txt": "# kept\n",
        }
        for n, body in files.items():
            with open(os.path.join(tpl, n), "w") as f:
                f.write(body)
        r = subprocess.run([sys.executable, SCRIPT, chart], capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("removed 2 comment lines from 2/2", r.stderr)
        self.assertEqual(open(os.path.join(tpl, "a.yaml")).read(), "k: v\n")
        self.assertEqual(open(os.path.join(tpl, "sub", "b.yml")).read(), "k: v\n")
        self.assertEqual(open(os.path.join(tpl, "_helpers.tpl")).read(), files["_helpers.tpl"])
        self.assertEqual(open(os.path.join(tpl, "NOTES.txt")).read(), files["NOTES.txt"])

    def test_idempotent(self):
        src = "# c\na: |\n  # content\nb: 1\n"
        once = strip(src)
        self.assertEqual(strip(once), once)


if __name__ == "__main__":
    unittest.main()
