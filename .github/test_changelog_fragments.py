# SPDX-License-Identifier: Apache-2.0
import pathlib
import tempfile
import unittest

import changelog_fragments as cf


def repo(files):
    d = pathlib.Path(tempfile.mkdtemp())
    for name, text in files.items():
        p = d / name
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(text, encoding="utf-8")
    return d


CL = "# Changelog\n\n## Unreleased\n\n- old entry\n\n## v1.0.0\n\n- released\n"


class Fragments(unittest.TestCase):
    def test_fold_appends_in_name_order_and_deletes(self):
        d = repo({"CHANGELOG.md": CL, "changelog.d/b.md": "- b\n", "changelog.d/a.md": "### A\n\n- a\n",
                  "changelog.d/README.md": "docs"})
        self.assertEqual(cf.fold(d), 0)
        self.assertEqual((d / "CHANGELOG.md").read_text(),
                         "# Changelog\n\n## Unreleased\n\n- old entry\n\n### A\n\n- a\n\n- b\n\n## v1.0.0\n\n- released\n")
        self.assertEqual([p.name for p in cf.fragments(d)], [])
        self.assertTrue((d / "changelog.d/README.md").exists())

    def test_fold_into_empty_unreleased_and_last_section(self):
        d = repo({"CHANGELOG.md": "# C\n\n## Unreleased\n", "changelog.d/a.md": "- a"})
        self.assertEqual(cf.fold(d), 0)
        self.assertEqual((d / "CHANGELOG.md").read_text(), "# C\n\n## Unreleased\n\n- a\n")

    def test_target_line_selects_another_changelog(self):
        d = repo({"CHANGELOG.md": CL, "rust/CHANGELOG.md": "# R\n\n## Unreleased\n",
                  "changelog.d/a.md": "<!-- changelog: rust/CHANGELOG.md -->\n- crate\n"})
        self.assertEqual(cf.fold(d), 0)
        self.assertIn("- crate", (d / "rust/CHANGELOG.md").read_text())
        self.assertEqual((d / "CHANGELOG.md").read_text(), CL)

    def test_check_refuses_bad_fragments(self):
        for name, text in {
            "empty.md": "  \n", "h2.md": "## Added\n- x\n", "missing.md": "<!-- changelog: nope.md -->\n- x\n",
            "escape.md": "<!-- changelog: ../x.md -->\n- x\n", "ext.txt": "- x\n",
            "blank-first.md": "\n<!-- changelog: rust/CHANGELOG.md -->\n- x\n",
            "nospace.md": "<!--changelog: rust/CHANGELOG.md-->\n- x\n",
        }.items():
            d = repo({"CHANGELOG.md": CL, f"changelog.d/{name}": text})
            self.assertEqual(cf.check(d), 1, name)
            self.assertEqual(cf.fold(d), 1, name)
            self.assertEqual((d / "CHANGELOG.md").read_text(), CL)

    def test_no_directory_is_fine(self):
        self.assertEqual(cf.check(repo({"CHANGELOG.md": CL})), 0)


if __name__ == "__main__":
    unittest.main()
