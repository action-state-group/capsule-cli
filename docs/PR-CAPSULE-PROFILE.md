# PR capsule profile

This profile says what a capsule sealed for a pull request records, how a verifier checks it, and how
a report cites it. The `Seal PR Capsule` action (`action.yml`) produces it. Any producer that follows
this page produces the same capsules: same input, same `capsule_id`.

A PR yields two capsules:

| Capsule | Event | `action_type` | Records |
|---|---|---|---|
| Head capsule | `pull_request` opened / synchronize / reopened | `fyi` | the PR at one head commit: diff, CI, reviews |
| Decision capsule | `pull_request` closed | `decide` | the merge decision for that head, chained to the head capsule |

Each is an ordinary Agent Action Capsule, sealed from a `capsule-seal-request/v1` request by
`scripts/pr-capsule-request.sh` and anchored by `scripts/pr-capsule-seal.sh`. The profile adds no
wire fields: everything below is a field the capsule format already has.

## Purpose label

Proposed: `code_change_review`. **Unregistered.** The capsule-registry "Cross-Profile Purpose
Labels" section has no entries yet; this label is proposed there and is not carried in the capsule
until it is registered. Until then the profile is identified by the shape below, not by a label.

## What a PR capsule records

Free text never enters a capsule. The diff, CI output, reviews and any prompt are reduced to sha256
digests on the runner; only the digests are written to the request, the capsule, the witness or the
PR comment.

| Field | Head capsule | Decision capsule |
|---|---|---|
| `action_id` | `pr-<owner>-<repo>-<number>-<head sha, 12 hex>` | the same, plus `-merged` or `-closed` |
| `operator` | `owner/repo` | `owner/repo` |
| `developer` | the PR author's login, as GitHub reports it | the same |
| `timestamp` | the head commit's committer date | GitHub's `merged_at` (merged) or `closed_at` (closed) |
| `domain` / `provenance` | `action` / `gate` | `action` / `gate` |
| `disposition` | absent | see [The merge decision](#the-merge-decision) |
| `chain` | absent | `{parent_capsule_id: <head capsule id>, relation: confirms}`, when the head capsule id is known |
| `model_attestation` | only if the PR body declares one (self-attested) | absent |

References, each `{type, digest_alg: sha256, digest, citation_purpose}`:

| `citation_purpose` | Type | Digest of | Present |
|---|---|---|---|
| `diff_digest` | `text/x-diff` | `git diff --no-color <base>...<head>` | always |
| `ci_result_digest` | `application/json` | the CI job name → conclusion object | always |
| `review_digest` | `application/json` | the PR's reviews | when reviews are recorded (`record-reviews: 'true'`) |
| `prompt_digest` | `text/plain` | a prompt the PR author declared and digested themselves | only if declared |

Timestamps come from git and GitHub, never the wall clock. A re-run over the same head, CI outcome
and reviews is the same request and the same `capsule_id`, so it is a retry, not a new capsule.

### The merge decision

| Closed event | `decision` | `verdict_class` | Decided by | `approver` | `human_disposed` |
|---|---|---|---|---|---|
| merged | `accept` | `executed` | a `User` account | `human` | `true` |
| merged | `accept` | `executed` | a `Bot` account (merge queue, auto-merge app) | `policy` | `false` |
| closed without merge | `reject` | `denied` | a `User` account | `human` | `true` |

"Decided by" is `merged_by.type` for a merge and `sender.type` for a close.

### Digest canonical forms

`ci_result_digest` and `review_digest` are sha256 over the compact JSON that `jq -S -c` prints,
**including its trailing newline**. Keys are sorted and there is no whitespace.

- CI: the object as given, e.g. `{"quality":"success","test":"success"}`.
- Reviews: an array of `{commit_id, reviewer, state, submitted_at}`, one entry per review the
  reviews API returns (every review, not only the latest per reviewer), sorted by `submitted_at`,
  then `reviewer`, `commit_id` and `state`. `reviewer` is the review's `user.login`.

## How a verifier checks it

You need the capsule (`capsule.json`, or the artifact record) and read access to the repository.

1. **Identity and signature.** `agent-action-capsule verify capsule.json` recomputes `capsule_id`
   from the payload. `capsulectl verify --profile <profile pinning the producer key> --capsule
   artifact.json` also checks the producer signature and trust.
2. **Anchor.** `capsulectl cll checkpoint status` re-verifies the witness receipt against the pinned
   witness key.
3. **Recompute the digests from the repository.**
   ```bash
   git diff --no-color "$BASE...$HEAD" | sha256sum                                          # diff_digest
   jq -S -c . <<<'{"quality":"success","test":"success"}' | sha256sum                        # ci_result_digest
   gh api --paginate "repos/$REPO/pulls/$PR/reviews" \
     --jq '.[] | {reviewer: .user.login, state, commit_id, submitted_at}' | jq -s -c . \
     | jq -S -c 'sort_by(.submitted_at, .reviewer, .commit_id, .state)' | sha256sum        # review_digest
   ```
   For the CI digest, take the conclusions from the head commit's check runs, under the job names
   the producer recorded.
4. **Decision capsule.** Check these against GitHub's own record of the PR:
   - its `disposition` matches the PR's state and who decided it;
   - its `timestamp` equals `merged_at` / `closed_at`;
   - its `chain.parent_capsule_id` names a head capsule whose `action_id` carries the same head SHA.

   A decision capsule chained to an older head's capsule is a finding: the merged head was not the
   one the head capsule records.

## In a report

A PR capsule is ordinary evidence. `capsule_id` commits the payload, not the signing key, so
publishing the request (the action's `request-path` output) into a durable evidence book gives the
same id that the PR comment names:

```bash
capsulectl publish --profile <book> --request pr-capsule-request.json   # head capsule
capsulectl publish --profile <book> --request pr-decision-request.json  # decision capsule
```

An Evidence Result v0 **requirement** claim, under a `process` requirement of the Evidence Contract,
cites both capsule ids in `evidence[]`, e.g. "merged only after review and green CI" (abridged):

```json
{
  "id": "pr-45-merge-review",
  "contract_ref": { "...": "the contract this requirement is in" },
  "requirement_ref": "r-merge-after-review",
  "tier": "recomputed",
  "grade": "...",
  "sufficiency": "SATISFIED",
  "verdict": "met",
  "evidence": [
    { "digest_alg": "SHA-256", "digest": "<head capsule id>" },
    { "digest_alg": "SHA-256", "digest": "<decision capsule id>" }
  ],
  "proofs": [],
  "presentation": { "...": "..." }
}
```

`capsulectl result build` resolves each cited id to the book record that carries that capsule.
`capsulectl report build --card process` then renders the claim and its evidence the same way it
renders any other evidence; the PR profile needs no card of its own.

## Limits

- **Same-repository PRs only.** A fork PR's `pull_request` events, `closed` included, carry no
  repository secrets, so there is no signing key to seal with. Do not use `pull_request_target`
  to reach forks: it runs with the base repository's secrets. The gap is documented, not silent.
- **Self-attested.** The author login, the reviews and the CI conclusions are what GitHub reports
  to the runner, recorded under the producer's key. The capsule shows they were recorded and not
  changed since. It does not show that GitHub reported them correctly.
- **A digest of public data commits; it does not hide.** Logins and review states come from a small
  space. Anyone who can guess them can confirm a guess against `review_digest`. The digest keeps
  the logins out of the capsule's text; it is not confidentiality.
- **The decision capsule's parent is found, not given.** The example workflow reads it from the
  action's last PR comment. A verifier checks it as in step 4 and does not trust the comment.
- **One ephemeral log per run.** Each capsule is anchored in its own size-one log. A durable,
  continuously appended record is the book you publish the requests into ([In a report](#in-a-report)).
