# Releasing capsulectl

How a release is cut, approved, verified and shipped. The examples use a release candidate,
`v0.1.0-rcNN`; a full release follows the same steps under the stricter approval rule below. This is the procedure the rc9 and rc10 cuts
followed, written down once and generalised to rcNN. The tag runs
[`release.yml`](../.github/workflows/release.yml), which runs
[`scripts/release-build.sh`](../scripts/release-build.sh). What each release proves, and how an
outsider checks it, is in [RELEASE-TRANSPARENCY.md](RELEASE-TRANSPARENCY.md). This page does not
repeat either; it says what to do, and in which order.

> **The book plugin ships from evidencebook's `capsulectl/vX.Y.Z` release, and the deal plugin
> and the deal skill from evidencebook-deal's, each built against this one** (capsule-cli#326).
> A capsulectl release that users install needs a capsulectl-book and a capsulectl-deal release
> whose `go.mod` requires it; each repository's RELEASING.md is the procedure.

**Provenance.** This is the rc10 cutover procedure, generalised. The verification standard is the
one the rc9 and rc10 cuts used.

## What ships

**The shipping unit is the release triple, not the tag.** It has three parts:
1. capsulectl's binaries and their release assets;
2. capsulectl-book, from an evidencebook `capsulectl/vX.Y.Z` release, and capsulectl-deal and
   the deal skill (`deal-skill-<tag>.tar.gz`), from an evidencebook-deal release, each built
   against this capsulectl release;
3. the companion plugin and skill, if you serve them.

A tag can be cut and published while installs stay on the previous triple. A release is shipped
when the install page moves to the new triple (step 7).

| Part | Where it comes from | What is verified |
| --- | --- | --- |
| Binaries (linux/amd64, linux/arm64, darwin/arm64), `SHA256SUMS`, the attestation bundle | the release workflow, on the tag | `SHA256SUMS`, the build attestation, the version each binary reports |
| capsulectl-book | an evidencebook `capsulectl/vX.Y.Z` release whose `go.mod` requires this tag | its own `SHA256SUMS` and attestation; the plugin's handshake reports this tag as `core_version` |
| capsulectl-deal and the deal skill | an evidencebook-deal release whose `go.mod` requires this tag | its own `SHA256SUMS` and attestation; the plugin's handshake reports this tag as `core_version` |
| The companion plugin and skill, if you serve them | their own release, built separately | their own published SHA-256 files, served byte for byte |

## Before the tag

The checks before the tag are lettered A to C; the release steps after them are numbered 1 to 9.

A. **Everything the tag must carry has merged.**
   - If the companion plugin embeds definitions this release depends on, their inputs are final
     first. A plugin that embeds a digest re-cuts when that digest moves.
B. **CI on `main` is green.** In particular, these jobs block a release:
   - `report page renders and checks itself`: the shipped page renders and verifies its own
     bundle.

   `quality`, `test` and `seal-pr-capsule` must be green too.
C. **The CHANGELOG is cut at the tag.** In the pull request that becomes the tagged commit, first
   run `python3 .github/changelog_fragments.py fold` (it appends the pending `changelog.d/`
   fragments to `## Unreleased` and deletes them), then move
   everything under `## Unreleased` under a new `## v0.1.0-rcNN` heading. Leave `## Unreleased`
   empty. A version shipped without this leaves its changes under Unreleased. Then the release
   state has to be rebuilt from the tags (`git log v0.1.0-rcPREV..v0.1.0-rcNN`).

## Who releases

**The release reviewers are Ethan Zhang (@ethanyzhang) and Steven Mih (@StevenMih).** Either may
cut a release. Steps marked "reviewer decision" are taken only on a release reviewer's go; every
release is one.

**A person approves every release run, in GitHub.** Pushing a tag starts
[`release.yml`](../.github/workflows/release.yml), which waits in a deployment environment until a
reviewer approves it. Nothing is built or published before that.

| Tag | Environment | Who may approve the run |
| --- | --- | --- |
| a pre-release, with a hyphen (`v0.1.0-rcNN`, `v1.0.0-beta1`) | `release-candidate` | either reviewer, including whoever pushed the tag |
| a full release (`v1.0.0`) | `release` | the other reviewer: whoever pushed the tag cannot approve it |

**Agents never approve a release run.** A reviewer approves it in GitHub's web UI or mobile app.
This is a rule, not a technical control: an agent working with a reviewer's GitHub credentials
could call the approval API, and GitHub cannot tell it from the person. An agent may cut the tag,
watch the run and report that it waits for approval; it never approves a deployment, and it
never asks for credentials or a token with which it could.

## The cut

1. **Tag** (reviewer decision). On a fresh `main`, fetch, fast-forward, and tag the merge commit
   that carries the CHANGELOG cut (step C).

   ```sh
   git fetch origin && git checkout main && git pull --ff-only
   git rev-parse HEAD      # the merge commit to tag: record it
   git tag -a v0.1.0-rcNN <merge-commit> -m "capsulectl v0.1.0-rcNN" && git push origin v0.1.0-rcNN
   ```

   Only maintainers and admins can create a `v*` tag, and nobody can move or delete one once it is
   pushed (the tag rulesets under [Repository settings](#repository-settings)). Check the tag
   name and commit before pushing: a wrong tag stays, and the fix is the next version.
2. **Approve the run** (reviewer decision). Open the run (Actions, Release, the run for the tag),
   check that its tag and commit are the ones recorded in step 1, and approve it under "Review
   deployments". For a full release, the reviewer who did not push the tag approves.
3. **Release CI is green.** It builds, checks the version each binary reports, attests the
   binaries, and publishes the release (a pre-release for a tag with a hyphen).
4. **Verify from the downloaded assets, not from the build.** In an empty directory:

   ```sh
   V=v0.1.0-rcNN; R=action-state-group/capsule-cli
   gh release download "$V" --repo "$R"
   sha256sum -c SHA256SUMS
   for f in capsulectl-"$V"-*-*; do
     gh attestation verify "$f" --bundle "capsulectl-$V.sigstore.json" --repo "$R" \
       --signer-workflow "$R/.github/workflows/release.yml"
   done
   ```

   Record the tag, its commit, each binary's SHA-256 and the attestation bundle's. The install
   page and the companion releases compare against these values, not against a build. The book
   and deal plugins and the deal skill are verified the same way from their own releases
   (evidencebook's `capsulectl/RELEASING.md`, evidencebook-deal's RELEASING.md).
5. **Publish the reviewed release notes, the upgrade note and security first.** When the release
   cannot be undone on an existing store, its upgrade note opens the notes: it is what a reader
   must act on before installing. A fix that changes what verification accepts or what a record
   discloses comes next, or opens the notes when there is no such upgrade note. Then come
   behaviour and wire changes, migration, what is new, fixes, dependencies, and the verify
   commands. The notes are reviewed before they are published.

## After the tag

6. **Release capsulectl-book and capsulectl-deal against this tag** (in evidencebook's
   `capsulectl/` and in evidencebook-deal: bump `go.mod` to the tag, merge, tag), and **re-cut the companion plugin and skill, if you serve them,** from their own
   source. Hand over
   each release's `.sha256` files with them, unregenerated, so the install page can compare them byte for
   byte.
7. **Re-aim the install page at the new triple.**
   - Add the new version alongside the existing ones. Leave every older binary, plugin and skill
     file untouched; a published version is frozen.
   - Bind the page's check to the steps and digests it publishes.
8. **Build the site image and run the render gate on real installs.** Render the reports that
   fresh installs actually produce, on more than one base system (not a fixture). Check:
   - the title and every row;
   - no console error;
   - a tampered copy reads as failing verification.
9. **One deploy** (reviewer decision), held until A to C and steps 1 to 8 are reported.

**Acceptance is a fresh install on a clean machine,** running a real deal check end to end against
the new triple. It is a release reviewer's own run, not a fixture. If a previous release left profiles
in an older layout, confirm such a profile does not block the install.

## When it goes wrong

- **Release CI failed after the tag was pushed, or a run was rejected.** Fix it on `main` and cut
  the next version (the next rc, or the next patch for a full release). A pushed tag cannot be
  moved or deleted.
- **A release workflow was re-run.** A published release cannot change, so a re-run of any
  release fails at the publish step; only a draft left by a run that died mid-upload is finished.
  A full release re-run by anyone other than whoever pushed its tag stops even earlier, before it
  builds: whoever re-runs a run counts as starting it, so the tag's pusher could otherwise approve
  it. Cut the next version instead.

## Repository settings

The approval rule, the tag protection and immutable releases are repository settings. A repository admin applies them
with [`scripts/release-settings.sh`](../scripts/release-settings.sh): run it with no argument to
print what it would set, `--apply` to set it, and `--show` to print what GitHub holds. It sets:

- **Two deployment environments,** each with both release reviewers as required reviewers and no
  admin bypass:
  - `release`, with prevent self-review on, accepting only tags matching `v*`;
  - `release-candidate`, with prevent self-review off, accepting only tags matching `v*-*`, so a
    full release cannot run in it.
- **Two tag rulesets on `refs/tags/v*`,** both active:
  - "release tags: maintainers create": only the maintain and admin roles may create a release tag;
  - "release tags: immutable": nobody may update, force-move or delete a release tag. It has no
    bypass actors, so admins are bound too.
- **Immutable releases:** once a release is published, nobody can replace or remove its assets.
  The install page relies on this: it pins the `SHA256SUMS` of the release it fetches
  from (RELEASE-TRANSPARENCY.md, section 3). It covers releases published after it is turned on.

Each release reviewer watches the repository for releases (Watch, Custom, Releases), so GitHub
tells them about every release published; that is how a release nobody meant gets noticed.

Every Action in `release.yml` is pinned by full commit SHA; keep it so when you bump one.
