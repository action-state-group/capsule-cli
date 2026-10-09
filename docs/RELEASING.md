# Releasing capsulectl (a release candidate, v0.1.0-rcNN)

How a release candidate is cut, verified and shipped. This is the procedure the rc9 and rc10 cuts
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
when the install page moves to the new triple (step 6).

| Part | Where it comes from | What is verified |
| --- | --- | --- |
| Binaries (linux/amd64, linux/arm64, darwin/arm64), `SHA256SUMS`, the attestation bundle, the release statement files | the release workflow, on the tag | `SHA256SUMS`, the build attestation, the version each binary reports |
| The deal skill | the tag's source archive (`archive/refs/tags/<tag>.tar.gz`), `skills/deal/SKILL.md` inside it | the archive is stable (downloaded twice, same SHA-256), and its `SKILL.md` equals the tagged commit's |
| The companion plugin and skill, if you serve them | their own release, built separately | their own published SHA-256 files, served byte for byte |

## Before the tag

The checks before the tag are lettered A to C; the release steps after them are numbered 1 to 8.

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

## The cut

**Steps marked "maintainer decision" are taken only on the maintainer's go.** Every release is.

1. **Tag** (maintainer decision). Tag the merge commit on a fresh `main`: fetch first, then
   fast-forward. Make an annotated tag, and echo the commit you tag in the same shell:

   ```sh
   git fetch origin && git checkout main && git pull --ff-only
   git rev-parse HEAD      # the commit being tagged: record it
   git tag -a v0.1.0-rcNN -m "capsulectl v0.1.0-rcNN"
   git push origin v0.1.0-rcNN
   ```

   Once tag signing has started, sign it (`git tag -s`). Until then, add the tag to the release
   monitor's list of tags made before signing (RELEASE-TRANSPARENCY.md, section 5).
2. **Release CI is green.** It builds, checks the version each binary reports, attests the
   binaries, registers the release statement in the transparency log, and publishes the
   pre-release.
3. **Verify from the downloaded assets, not from the build.** In an empty directory, with a clone
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
4. **Publish the reviewed release notes, security first.** A fix that changes what verification
   accepts or what a record discloses opens the notes. Then come behaviour and wire changes,
   migration, what is new, fixes, dependencies, and the verify commands. The notes are reviewed
   before they are published.

## After the tag

5. **Re-cut the companion plugin and skill, if you serve them,** from their own source. Hand over
   both `.sha256` files with them, unregenerated, so the install page can compare them byte for
   byte.
6. **Re-aim the install page at the new triple.**
   - Add the new version alongside the existing ones. Leave every older binary, plugin and skill
     file untouched; a published version is frozen.
   - Bind the page's check to the steps and digests it publishes.
7. **Build the site image and run the render gate on real installs.** Render the reports that
   fresh installs actually produce, on more than one base system (not a fixture). Check:
   - the title and every row;
   - no console error;
   - a tampered copy reads as failing verification.
8. **One deploy** (maintainer decision), held until A to C and steps 1 to 7 are reported.

**Acceptance is a fresh install on a clean machine,** running a real deal check end to end against
the new triple. It is the maintainer's own run, not a fixture. If a previous release left profiles
in an older layout, confirm such a profile does not block the install.

## When it goes wrong

- **A change to `skills/deal/` merged after the tag.** The install page's skill value no longer
  matches the tag. Do not edit the published values: cut the next rc.
- **Release CI failed after the tag was pushed.** Fix it on `main` and cut the next rc. Deleting or
  moving a pushed tag is what the release monitor alarms on (RELEASE-TRANSPARENCY.md, section 5).
- **A release workflow was re-run.** Each run registers a new release statement under a new key,
  and the monitor names it. Expect that alarm and confirm it is the re-run.
