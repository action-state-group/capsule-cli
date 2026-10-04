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

**It does not prove the project meant to release the file.** Three things
all produce perfectly valid attestations, because each uses the real signing
path:
- a stolen token that can push a tag;
- a compromised CI;
- a maintainer acting alone.

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
- the tag itself (section 5).

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

## 5. What the monitor adds: comparison with intent, from outside

An **intended release** is a `v*` tag whose SSH signature verifies under a
maintainer key in an allowed-signers file. That key never touches CI, so a
stolen token or a compromised workflow can create a tag and a release, but
not a signed tag.

`capsulectl release watch` reads only public data and needs no account. It
lists the repository's tags and checks each signature with
`ssh-keygen -Y verify`. It then checks every release and every statement
under the release subject, and it **alarms** (exits non-zero and names the
problem) on any of:
- a tag or release that is not intended (unsigned, or signed by a key that
  is not allowed);
- an intended tag with no release once a grace window has passed since it
  was tagged (`--release-grace`, default two hours, measured from the
  tagger time the tag's signature covers). This catches a release that was
  never published or was deleted;
- an intended release with no statement, or whose statement does not match
  the tag, its commit or its assets;
- a statement whose signature does not verify under the key its payload
  names, or whose issuer or subject is not the release workflow's;
- a `release.json` with no attestation, or one that does not verify as the
  release workflow at `refs/tags/<tag>` on the tagged commit (checked with
  `gh attestation verify --bundle`, with no account);
- a receipt that does not verify offline against the service's pinned key;
- an intended release whose statement is missing from the log;
- **any statement under the release subject that does not verify as one
  of these releases.** Since registration is open, this also fires on spam
  or a probe. That is intended: a person looks at it, and the alarm names
  the entry. It also fires when a release run is repeated, since each run
  registers a new statement under a new key.

```sh
capsulectl release watch --allowed-signers ~/release-monitor/allowed_signers \
  --known-unsigned-file ~/release-monitor/known-unsigned
```

where `~/release-monitor/known-unsigned` lists every tag made before tag
signing starts, one per line. Until the first signed tag, add each new tag
as it is made; once signing starts, the file stops growing:

```text
# Tags made before tag signing began. Never add a tag made after it.
v0.1.0-rc1
v0.1.0-rc2
v0.1.0-rc3
v0.1.0-rc4
v0.1.0-rc5
```

- `--allowed-signers` (ssh `allowed_signers` format) belongs to whoever
  runs the monitor, never to this repository. An attacker who can change
  the repository must not be able to change what counts as intended.
- `--known-unsigned-file` (or `--known-unsigned`, comma-separated) names the
  tags made before tag signing began, which are accepted as known
  exceptions. Like the allowed signers, the file belongs to the monitor,
  never to this repository.
- `--trusted-root` passes a Sigstore trusted root to gh, for a fully
  offline check.

**It never checks a partial picture.** It follows every page of the tag and
release lists. The service returns all of a subject's statements in one
answer, so if that answer is ever paged (a cursor, or a `Link` header) or
exceeds the size limit, the monitor fails with an error instead of judging
part of the log. An error, like an alarm, needs a person.

**Where it runs.** A monitor running on the infrastructure it monitors is
decoration. Run it on a schedule somewhere a compromise of this
repository, its CI or its GitHub organisation does not reach, such as a
maintainer's own machine (cron or launchd). Ideally run it in more than
one such place.

**What it does not stop:** a maintainer whose own key is allowed. That
release is intended by this definition. The monitor makes it public,
timestamped and attributable to a key, which is the most a log can do.

## 6. What none of this addresses: name resolution

No signature, log or monitor tells a user that a domain belongs to this
project, or that `action-state-group/capsule-cli` is the repository they
heard about. Everything above proves facts about *this* repository's
releases, given that you already have the right repository. Getting there
rests on the domain named in the install instructions and on the installing
agent reporting what it verified, not on cryptography. Do not read any of
the checks above as covering it.
