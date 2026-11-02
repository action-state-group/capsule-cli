# Changelog fragments

Add one file here per pull request, `changelog.d/<short-name>.md`, instead of editing
`CHANGELOG.md`: parallel pull requests that all edit `## Unreleased` conflict on every merge.

The file holds exactly the Markdown that belongs under `## Unreleased`, in the changelog's own
style, and no `## ` heading. To add the entry to another changelog (a crate's, Python's), start
the file with `<!-- changelog: <path> -->` on its own line.

CI runs `python3 .github/changelog_fragments.py check`. The release pull request runs
`python3 .github/changelog_fragments.py fold`, which appends every fragment to its changelog's
Unreleased section in file-name order and deletes the fragments; see RELEASING.md.
