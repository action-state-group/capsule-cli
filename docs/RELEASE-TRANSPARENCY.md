# Release transparency

How a capsulectl release can be checked, what each check covers, and what
none of them covers. Each section maps one mechanism to the gap it closes.

## 1. What build provenance proves

Every release binary carries a GitHub build provenance attestation
(Sigstore, keyless), published with the release as
`capsulectl-<tag>.sigstore.json`. It proves that the file was built by
`.github/workflows/release.yml` in `action-state-group/capsule-cli`, from a
given commit, for a given tag:

```sh
gh attestation verify capsulectl-<tag>-<os>-<arch> --bundle capsulectl-<tag>.sigstore.json \
  --repo action-state-group/capsule-cli \
  --signer-workflow action-state-group/capsule-cli/.github/workflows/release.yml
```

No GitHub account is needed with `--bundle`. The attestation is also
recorded in Sigstore's public Rekor log, which neither this project nor
GitHub runs, and the check above confirms that entry.

## 2. What it does not prove

**It does not prove the project meant to release the file.** Anything
that can make the release workflow run at a tag produces a perfectly valid
attestation, because it uses the real signing path.

What limits that is GitHub's own enforcement, set up as
[RELEASING.md](RELEASING.md#repository-settings) describes:
- only maintainers and admins can create a `v*` tag, and nobody, admins
  included, can move or delete one;
- the release workflow waits for a named reviewer to approve the run. For a
  full release, whoever pushed the tag cannot be that reviewer.

These are settings in GitHub, so they hold only as long as GitHub and the
organisation's admins do. Someone who controls the organisation can change
them. And the approval is enforced only while the workflow at the tagged
commit asks for it.

## 3. What keeps a release nobody meant from reaching users

A release on GitHub is not yet installed anywhere. Installs take a release
by its pinned values, not by "the newest one":
- **The install page** pins the tag, its commit and every file's SHA-256 in
  the site's own repository. Moving it to a new release is a reviewed change
  there, then a deploy (RELEASING.md, steps 7 to 9). A release nobody meant
  never gets that change, so the page never offers it.
- **Other installers that pin the values the same way**, such as an agent
  platform's packaged capsulectl, are held by their own reviewed change.
- **A manual install checked only against `SHA256SUMS`** trusts the
  release's own `SHA256SUMS`, and the attestation only when it is checked.
  The first-party plugins come from their own releases (docs/PLUGINS.md),
  each with its own `SHA256SUMS` and attestation. That trust rests on
  the release staying what it was when it was pinned, which GitHub's
  immutable releases setting enforces (RELEASING.md, "Repository
  settings"). The setting covers releases published after it was turned
  on, so a release published before then is only as safe as its assets
  have stayed.

So the control is the reviewed change that pins a release's values. The
release reviewers watch the repository for releases, so GitHub tells them
about every one published, which surfaces one that nobody meant. A release made by someone who controls the whole GitHub
organisation is the accepted residual risk: such a person can change every
setting above.

Releases `v0.1.0-rc5` to `v0.1.0-rc17` also carry release-statement files
(`.release.json`, `.release.cose`, `.release.registration.json`,
`.release.sigstore.json`) from an earlier design that registered each
release in a transparency log. They stay published, unchanged, and nothing
depends on them.

## 4. What none of this addresses: name resolution

No signature or log tells a user that a domain belongs to this
project, or that `action-state-group/capsule-cli` is the repository they
heard about. Everything above proves facts about *this* repository's
releases, given that you already have the right repository. Getting there
rests on the domain named in the install instructions and on the installing
agent reporting what it verified, not on cryptography. Do not read any of
the checks above as covering it.
