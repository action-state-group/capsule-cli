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

No GitHub account is needed with `--bundle`.

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
them. The approval is enforced only while the workflow at the tagged commit
asks for it; a run that skipped it is what the monitor's approval check
(section 5) catches.

## 3. What registration adds: "logged", not "from us"

For each release, `release.yml` runs `capsulectl release register`, which:
1. Writes `capsulectl-<tag>.release.json`: the tag, the commit, the SHA-256
   of every binary and of the attestation bundle, and the public half of a
   key made for this one release.
2. Signs it as a SCITT Signed Statement (COSE_Sign1). The protected header
   carries the subject `pkg:github/action-state-group/capsule-cli` and the
   issuer, the release workflow's URL. The private key is then discarded, so
   no release key is held anywhere.
3. Registers the statement at the public transparency service
   (`https://witness.agentactioncapsule.org/transparency/register-statement`).
   It then checks the returned RFC 9162 receipt offline against the
   service's pinned key. If registration fails, the release is not
   published.
4. Attests `release.json` with the same keyless attestation as the binaries.

Four files are published with the release and listed in `SHA256SUMS`:
`.release.json`, `.release.cose`, `.release.registration.json` (with the
receipt), and `.release.sigstore.json` (the attestation).

**What a receipt proves:** that this exact statement is in the service's
append-only log, at a position and under a signed tree head. The release's
record can't be quietly removed or rewritten later.

**What it does not prove:** who registered it. The service does not verify
a statement's signature or its issuer when it registers it. Registration is
open, and anyone can register any statement under any subject, including
ours. **Registration proves "logged", never "from us".** A statement is from
the release workflow only when a verifier has checked all three of these
itself:
- its COSE signature, under the key its payload names;
- the attestation of its `release.json`, made by the release workflow at
  that tag and commit;
- the tag itself: it exists and still points at that commit (section 5).

## 4. What Rekor anchoring adds

The transparency service publishes its own signed tree head to public
Sigstore Rekor on a timer (at most every five minutes, and only when the
tree has grown). Rekor is a log that neither the project nor an attacker
runs. So the service cannot quietly fork or rewrite its history: a
statement registered for a release is covered by the next tree head
anchored in Rekor.

The anchoring is of the service's tree head, not of each statement, so
registering releases adds no load on Rekor.

Where this is in the service's source (capsule-anchor, at `04447927e2db`):
- [`app.py` lines 266 and 282](https://github.com/action-state-group/capsule-anchor/blob/04447927e2db3d299ad8c4dede394e1f640feab0/packages/capsule_anchor/app.py#L266-L282): the publisher thread
  is started with the interval `CAPSULE_ANCHOR_PUBLIC_LOG_INTERVAL`, which
  defaults to 300 seconds.
- [`public_log/scheduler.py`, `publish_if_new`](https://github.com/action-state-group/capsule-anchor/blob/04447927e2db3d299ad8c4dede394e1f640feab0/packages/capsule_anchor/public_log/scheduler.py#L53-L90):
  this publishes the current signed tree head only if that tree size has
  not been published yet. The publisher thread calls it on each interval
  ([`start_publisher_thread`](https://github.com/action-state-group/capsule-anchor/blob/04447927e2db3d299ad8c4dede394e1f640feab0/packages/capsule_anchor/public_log/scheduler.py#L124)).

## 5. What the monitor adds: a check from outside

`capsulectl release watch` reads only public data and needs no account or
key. It lists the repository's `v*` tags and releases, checks each release
and its statement, and reads every statement under the release subject. It
**alarms** (exits 6 and names the problem) on any of:
- a `v*` tag with no release once a grace window has passed since it was
  tagged (`--release-grace`, default 24 hours, to cover the wait for a
  reviewer's approval). This catches a release that was never approved,
  failed, or was deleted;
- a tag dated more than an hour in the future with no release, since such a
  date would hold the alarm above off;
- a release with no `v*` tag of its name, or whose statement names another
  commit than the tag points at now. This catches a deleted or moved tag;
- a release with no statement, or whose statement does not match its
  assets. The releases from before `release register` existed,
  `v0.1.0-rc1` to `v0.1.0-rc4`, are the only ones that need none;
- a statement whose signature does not verify under the key its payload
  names, or whose issuer or subject is not the release workflow's;
- a `release.json` with no attestation, or one that does not verify as the
  release workflow at `refs/tags/<tag>` on the tagged commit (checked with
  `gh attestation verify --bundle`, with no account). The attestation's own
  inclusion in Sigstore's public Rekor log is part of that check;
- a release whose run has no approval in the environment its tag implies
  (`release-candidate` for a tag with a hyphen, `release` otherwise). The run
  is the one named in the attestation's signing certificate, which the
  workflow cannot choose. For a full release, the approval must come from
  someone other than whoever pushed the tag or started that run attempt.
  This catches a release built by a `release.yml` that no longer asks for
  approval. Releases up to `v0.1.0-rc17`, cut before approval began, are
  exempt;
- a receipt that does not verify offline against the service's pinned key;
- a release whose statement is missing from the log;
- **any statement under the release subject that does not verify as one
  of these releases.** Since registration is open, this also fires on spam
  or a probe. That is intended: a person looks at it, and the alarm names
  the entry. It also fires when a release run is repeated, since each run
  registers a new statement under a new key.

```sh
capsulectl release watch
```

- `--release-grace` sets how long a tag may wait for its release.
- `--trusted-root` passes a Sigstore trusted root to gh, for a fully
  offline check.

**It never checks a partial picture.** It follows every page of the tag and
release lists. The service returns all of a subject's statements in one
answer, so if that answer is ever paged (a cursor, or a `Link` header) or
exceeds the size limit, the monitor fails with an error instead of judging
part of the log. An error, like an alarm, needs a person.

**Where it runs.** A monitor running on the infrastructure it monitors is
decoration. Run it on a schedule (cron or launchd) somewhere a compromise of
this repository, its CI or its GitHub organisation does not reach. That
rules out GitHub Actions. It also rules out the cloud project that runs the
transparency service, which the monitor checks too. Ideally run it in more
than one such place. Each run needs:
- a capsulectl binary that was verified once by hand from a release (its
  `SHA256SUMS` and its attestation). It is never updated automatically from
  the repository it watches;
- `gh`, for the attestation check, which runs with no account;
- for GitHub's API, nothing at first: a run makes a few calls plus one or two
  per release after `v0.1.0-rc17`, and GitHub allows 60 anonymous calls an
  hour. Before releases make one run exceed that, set `GITHUB_TOKEN` to a
  token with no scopes or repository access. It only raises the limit; the
  data read is public either way. A refusal for the rate limit is an error,
  like any other, and needs a person;
- a way to reach a person on any exit other than 0, with the output;
- a notice when it has **not** run. A monitor that stopped looks like one
  that found nothing, so each successful run should check in with
  something that alerts when the check-ins stop.

**What it does not stop:** anyone GitHub lets cut a release. A maintainer
who tags a commit, and a reviewer who approves the run, make a release that
passes every check above. The monitor makes it public, timestamped and
consistent, which is the most a log can do. The approval it checks is
GitHub's record of who approved, and it cannot tell a person from an agent
using that person's account.

## 6. What none of this addresses: name resolution

No signature, log or monitor tells a user that a domain belongs to this
project, or that `action-state-group/capsule-cli` is the repository they
heard about. Everything above proves facts about *this* repository's
releases, given that you already have the right repository. Getting there
rests on the domain named in the install instructions and on the installing
agent reporting what it verified, not on cryptography. Do not read any of
the checks above as covering it.
