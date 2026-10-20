# Releasing capsulectl

How a release is cut, approved, verified and shipped. The examples use a release candidate,
`v0.1.0-rcNN`; a full release follows the same steps under the stricter approval rule below. This is the procedure the rc9 and rc10 cuts
followed, written down once and generalised to rcNN. The tag runs
[`release.yml`](../.github/workflows/release.yml), which runs
[`scripts/release-build.sh`](../scripts/release-build.sh). What each release proves, and how an
outsider checks it, is in [RELEASE-TRANSPARENCY.md](RELEASE-TRANSPARENCY.md). This page does not
repeat either; it says what to do, and in which order.

> **The deal skill ships inside the release. Change its words before the tag, or wait for the
> next rc.**
>
> `skills/deal/SKILL.md` is installed from the tag's own source archive, and release verification
> checks that the installed `SKILL.md` is byte-for-byte the one at the tagged commit. Any wording
> change to it after the tag moves its digest. Then the install page's published value is wrong,
> and the release has to be cut again. Every change to `skills/deal/` lands before the tag. If it
> is not ready, it goes in the next rc.

**Provenance.** This is the rc10 cutover procedure, generalised. The verification standard is the
one the rc9 and rc10 cuts used.

## What ships

**The shipping unit is the release triple, not the tag.** It has three parts:
1. capsulectl's binaries and their release assets;
2. the deal skill's archive (the tag's source archive);
3. the companion plugin and skill, if you serve them.

A tag can be cut and published while installs stay on the previous triple. A release is shipped
when the install page moves to the new triple (step 7).

| Part | Where it comes from | What is verified |
| --- | --- | --- |
| Binaries (linux/amd64, linux/arm64, darwin/arm64), `SHA256SUMS`, the attestation bundle | the release workflow, on the tag | `SHA256SUMS`, the build attestation, the version each binary reports |
| The deal skill | the tag's source archive (`archive/refs/tags/<tag>.tar.gz`), `skills/deal/SKILL.md` inside it | the archive is stable (downloaded twice, same SHA-256), and its `SKILL.md` equals the tagged commit's |
| The companion plugin and skill, if you serve them | their own release, built separately | their own published SHA-256 files, served byte for byte |

## Before the tag

The checks before the tag are lettered A to C; the release steps after them are numbered 1 to 9.

A. **Everything the tag must carry has merged.** That means every code change, and every change
   to `skills/deal/` (see the box above).
   - If the companion plugin embeds definitions this release depends on, their inputs are final
     first. A plugin that embeds a digest re-cuts when that digest moves.
B. **CI on `main` is green.** In particular, these jobs block a release:
   - `iife`: the vendored report viewer (the evidence-graph IIFE) equals a fresh build from the
     upstream commit it pins, and that commit is on the upstream main;
   - `interop at the resolved versions (Go <-> Python)`: the resolved dependency versions agree
     across implementations;
   - `report page renders and checks itself`: the shipped page renders and verifies its own
     bundle.

   `quality`, `test` and `seal-pr-capsule` must be green too.
C. **The CHANGELOG is cut at the tag.** In the pull request that becomes the tagged commit, move
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
4. **Verify from the downloaded assets, not from the build.** In an empty directory, with a clone
   of this repository at `$CLONE` (for the tagged commit's `SKILL.md`):

   ```sh
   V=v0.1.0-rcNN; R=action-state-group/capsule-cli; CLONE=~/src/capsule-cli
   gh release download "$V" --repo "$R"
   sha256sum -c SHA256SUMS
   for f in capsulectl-"$V"-*-*; do
     gh attestation verify "$f" --bundle "capsulectl-$V.sigstore.json" --repo "$R" \
       --signer-workflow "$R/.github/workflows/release.yml"
   done
   curl -sSL -o a1.tar.gz "https://github.com/$R/archive/refs/tags/$V.tar.gz"
   curl -sSL -o a2.tar.gz "https://github.com/$R/archive/refs/tags/$V.tar.gz"
   sha256sum a1.tar.gz a2.tar.gz          # the skill archive: the same twice
   tar -xzf a1.tar.gz --to-stdout "capsule-cli-${V#v}/skills/deal/SKILL.md" | sha256sum
   git -C "$CLONE" fetch --tags origin
   git -C "$CLONE" show "$V:skills/deal/SKILL.md" | sha256sum   # must equal the line above
   ```

   Record the tag, its commit, each binary's SHA-256, the attestation bundle's, the skill
   archive's and `SKILL.md`'s. The install page and the companion release compare against these
   values, not against a build.
5. **Publish the reviewed release notes, the upgrade note and security first.** When the release
   cannot be undone on an existing store, its upgrade note opens the notes: it is what a reader
   must act on before installing. A fix that changes what verification accepts or what a record
   discloses comes next, or opens the notes when there is no such upgrade note. Then come
   behaviour and wire changes, migration, what is new, fixes, dependencies, and the verify
   commands. The notes are reviewed before they are published.

## After the tag

6. **Re-cut the companion plugin and skill, if you serve them,** from their own source. Hand over
   both `.sha256` files with them, unregenerated, so the install page can compare them byte for
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

- **A change to `skills/deal/` merged after the tag.** The install page's skill value no longer
  matches the tag. Do not edit the published values: cut the next rc.
- **Release CI failed after the tag was pushed, or a run was rejected.** Fix it on `main` and cut
  the next version (the next rc, or the next patch for a full release). A pushed tag cannot be
  moved or deleted.
- **A release workflow was re-run.** A full release re-run by anyone other than whoever pushed
  its tag stops before it builds: whoever re-runs a run counts as starting it, so the tag's pusher
  could otherwise approve it. Cut the next version instead.

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
  `capsulectl plugin install` relies on this: it trusts the `SHA256SUMS` of the release it fetches
  from (RELEASE-TRANSPARENCY.md, section 3). It covers releases published after it is turned on. Enforce it
  for the organisation too, so that turning it off takes an organisation owner, not a repository
  admin.

Every Action in `release.yml` is pinned by full commit SHA; keep it so when you bump one.
