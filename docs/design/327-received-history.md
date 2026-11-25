# Design note: a counterparty's verified History as a rules input

capsule-cli#327. Status: converged (round 3); Steven ruled D1 (a)
(section 3). Converged with Codex, Kimi and Sherlock2's product-boundary
check (review record on capsule-cli#566). Checked against capsule-cli `main`
`dbf7470e`, evidencebook-deal `main` `2404d9d`, evidencebook `main`
`1f917bb`, capsule-engine `main` `e59ace8` and company-internal-all `main`
`6b2ac1c`.

## 1. The ruling and the bar

SEAMS ruling 7 (Steven, 2026-10-21, Buzz `7b2f0884` in the Muse channel
thread `6c3cba55`; `ACTION_STATE_SEAMS_PLAN.md` row H-c):

> (b) add an optional `received_history` input to the external-check seam:
> verified H3 and H4 from the counterparty, read by named folds only (e.g.
> "known counterparty, N verified deals") ... with hard limits: **read-only
> and attributed** ("their History says…"); **never a score**; **never
> combined into one number**. ... **Steven gate:** reads close to
> reputation, so the boundary check applies.

The bar it moves is `THE_LOOP.md` step 4, **Admit**: "the buyer's admission
pack reads the seller's history and returns GO, ASK, LIMIT or NO; any ASK
is one derived Evidence Request; the decision is sealed", first live "across
ecosystems once Muse joins", and the Muse row's need "admission on real
histories (a Muse buyer's pack reads a Mesh seller's history)". Latest Steven
ruling on it: ruling 7 above; none since. When this item is done, step 4's
cross-ecosystem path has a sealed, deal-path input for a verified History;
the step itself stays `NOT_CHECKED` under `cross` until Muse's pack reads it
and the loop adds the cross step.

## 2. What changed since the How ack

The How ack (2026-10-21) placed the seam in capsule-cli. Three things moved
since, and they change where the work lands and how much is left.

1. **The deal plugin left this repository** (capsule-cli#326). The
   checker input is built in evidencebook-deal (`internal/deal/rules_checker.go`,
   `rulesInput`), the pinned argv is run there (`runRulesChecker`), and the
   input and result schemas belong to evidencebook
   (`dealprofile/x-deal-v0/external-check-{input,result}-v0.schema.json`).
   capsule-cli keeps only the pin (`cli/rules_checker.go`) and the History
   verifier (`cli/verify_history*.go`: `HistoryVerdict`, the staple checks).
2. **Unverified cards already reach the checker, for a notice only**
   (evidencebook-deal#166, closed 2026-11-06). `deal check --received FILE`
   hands the checker an optional `received_cards` array "as received". The
   sealed rules come from a run given no cards; a second run with them is
   read for the result's optional `notice.line` alone, which is shown and
   never sealed. Sherlock2's door condition there: nothing derived from
   received cards reaches a sealed verdict, and sealing anything derived
   from cards would go to Steven.
3. **An admission evaluator exists** (capsule-engine `go/admission`, used by
   actionstate-muse#314). It reads a counterparty's card "as the caller
   verified it" (`Inputs`, `Card.State` verified, unverifiable, refused,
   unanswered). Its decision's `Block()` is the `admission_decision` block a
   caller seals, local-only and never exported for another party
   (`go/admission/decision.go`); Muse shows the decision and does not seal
   it. Muse runs it in `check --action/--record` only, not under `--emit`,
   because the deal
   check's checker "can neither be told who the received file is about nor
   show the decision" (`internal/plugin/admission.go`).

So the gap ruling 7 names is narrower than in October and has a clear
shape: today nothing **verified and attributed** about the counterparty can
reach the **sealed** verdict of a deal check. `received_cards` is neither
verified nor attributed and is kept out of the sealed result on purpose.

## 3. The decision for the boundary check

**Ruled: (a), with the three conditions below.** Steven ruled (a),
2026-11-16, Buzz `cc0e2943` in thread `b42cb0e4`, on this note at
`050a8791`; Sherlock2 recorded it in ops `5aaabe11`. Not ruled: whether a
deal-path verdict that read History counts for step 4 (condition 3); until
Steven rules, it does not.

**D1 (to Steven, through Sherlock2's boundary check).** May a counterparty's
verified, attributed History change the deal check's **sealed** verdict and
findings?

- **(a) Yes, bound by digest (recommended by this note and by Sherlock2,
  Buzz `0d98dcdd`).** The sealed run is given the verified History; the
  sealed rules block binds what it read by digest, never by content
  (section 5.4). This is what ruling 7 and step 4 ask for: a decision that a
  deal cites must be sealed, and a sealed verdict that depends on an input it
  does not bind cannot be re-checked. #166's condition (nothing derived from
  received cards reaches a sealed verdict) was set for unverified,
  unattributed cards and stays true: `received_cards` is unchanged.
- **(b) No, notice only.** Verified History joins `received_cards`' path:
  read by a second run for an unsealed line. Then #327 adds only
  verification and attribution on top of #166, and step 4's "the decision is
  sealed" stays unmet on the deal path.

The conditions that come with (a):

1. **No NO from History alone.** A fold over `received_history` may add a
   finding or make the check ask, but never by itself turns the verdict into
   a deny. This follows Steven's line on history read at admission: referee
   verdicts on a node's history are "read by an admission row that never
   says NO alone" (Steven 2026-11-11, Buzz thread `64f994e8`; ops
   `neutral/inbox.md` u1598).
2. **Withheld from shares.** A check record that holds `received_history`
   digests is in no shared copy, whatever the share policy for the rules
   verdict (section 5.4), because a digest of their History is a linkable
   handle: anyone holding that History could tell I checked them.
3. **Which record is step 4's.** The loop's admit record stays the
   `admission_decision` block. This seam is an input to the deal check's own
   rules verdict and creates no second admission record; whether a deal-path
   rules verdict that read History also counts for step 4 is Steven's call.

Why D1 goes to Steven: it decides what is sealed about a counterparty in my
book, and whether their History can change my sealed decision, the
reputation-adjacent line ruling 7 flagged. Sherlock2's view (Buzz
`0d98dcdd`): one buyer's local admission reading one counterparty through
named folds, nothing combined across parties, is the admission step Steven
already ruled on. The rest of this note assumes (a); under (b), sections 5.4
and 6's sealing tests drop out and the member is read by the notice run only.

## 4. Owners

| Part | Owner | Change |
|---|---|---|
| `received_history` member and its limits, result schema descriptions | evidencebook (`dealprofile/x-deal-v0/`) | additive optional member; descriptions state the limits positively |
| Reading `--history`, verifying, attributing, building the input, sealing the digests, withholding from shares | evidencebook-deal (`deal check`, `rulesInput`, `deal_share.go`) | new flags, input member, sealed member |
| Verifying a History file or a stapled card | capsule-cli core (`cli/verify_history*.go`), over AAC `go/personalhistory`, `go/standardhistory` | a library entry for the staple path: `StapleVerdict` (capsule-cli#567; before it `verifyStaple` took a `*cobra.Command` and `stapleOutcome` was unexported); `HistoryVerdict` is already exported |
| The named folds over the member | capsule-engine `go/admission` (its closed predicate list, `pack.go`), and packs that use it, such as Muse's | the predicates in section 5.5; their behaviour tests |

No AAC digest or signature rule is reimplemented: verification is AAC's
verifier through capsule-cli's existing History checks. Build issues are
filed in evidencebook, evidencebook-deal, capsule-cli and capsule-engine
after convergence, and #327 closes against them.

## 5. The design (under D1 (a))

### 5.1 Input to the verb

`deal check --history FILE [--history FILE] --counterparty-key HEX[,HEX]
[--checkpoint-key HEX[,HEX]]`. Each `--history` names one document, as the
verifier reads one document per file: a `personal_history_basic/1` History
(H3) or a stapled `history_card/1` (H4, an `evidence-commitment/1` or a
`{history, request, reply}` file), exactly as received. At most one document
of each kind; a second of the same kind, or the same bytes twice, is refused.

Both keys come from the host's deployment policy, never from the documents
and never from anything the counterparty sent with this deal, since a party
who re-signs a staple under its own key would otherwise pass (the attack the
staple verifier names, `cli/verify_history_staple.go`, `commitmentChecks`).
`--counterparty-key` is checked as `verify --history` checks it today: as the
holder key for a History, as the counterparty key for a staple.
`--checkpoint-key` is the log key a staple's checkpoints are signed by,
which need not be the responder's (IP §6.2.1); a staple needs it. The flags
need a pinned rules checker, like `--received`, and that checker must accept
the member: `external-check-input/v0` is closed, so a checker that predates
it refuses the input, and the check is then sealed `not_evaluated`
(`refused`), unlike `received_cards`, whose refusal costs only the notice.

### 5.2 Verified and attributed, or refused with a reason

Before any checker run, each document is checked with capsule-cli's History
verifier against the given keys. The whole check is refused (input error,
no checker run, nothing sealed) when any document:

- is not accepted (`ErrHistoryNotAccepted`): a property failed, or it was
  answered under a key other than `--counterparty-key`, so it is not
  attributable to this counterparty; the reason names the property;
- leaves a check not run (`ErrPartial`, for example a staple given no
  `--checkpoint-key`): "not verified: <property> was not checked";
- is any other kind, or a second document of its kind.

One exception, already a stated limit of the verifier: a stapled card whose
checks all pass but which answers no request of ours
(`ErrHistoryAnswersNoRequest`, `request_binding` NOT_PRESENT, because a
commitment staples the reply octets only) is read as verified, and its entry
carries `request_binding: "NOT_PRESENT"`, so a fold can tell it from a bound
History. Such a card can be old; capsule-engine's `card.fresh` predicate
reads its latest checkpoint time.

Refusing, rather than dropping the document, keeps the host from believing a
History was read when it was not. This meets the acceptance "an unverified
one is refused with a reason".

### 5.3 The member

`external-check-input/v0` gains one optional member, absent when no
`--history` was given, closed like the rest of the schema:

```json
"received_history": {
  "about": "counterparty",
  "counterparty_keys": ["<hex>"],
  "histories": [
    {"kind": "personal_history_basic/1", "sha256": "<hex>", "request_binding": "BOUND", "document": { }},
    {"kind": "history_card/1", "sha256": "<hex>", "request_binding": "NOT_PRESENT", "document": { }}
  ]
}
```

- **Attributed.** The whole member is about one party, this deal's
  counterparty, under the keys the host's policy gave. A finding that reads
  it reads "their History says ...".
- **One counterparty, one document per kind.** A deal check has one
  counterparty, so the input never holds two parties' Histories, and at most
  one H3 and one H4, so no fold can add the same deals twice.
- **Read-only.** The member is input; the result schema gains no member to
  write History back, and the plugin stores nothing of `document`.
- **Distinct from `received_cards`.** That member stays as it is:
  unverified, for the notice run only. Merging them would hide from a
  checker which cards were verified.

### 5.4 What is sealed, and what is shared

The sealed rules block gains `received_history`:
`{"history_sha256": [<hex>, ...], "input_sha256": <hex>}`. `history_sha256`
lists each document's SHA-256 in input order; `input_sha256` is the SHA-256
of the JCS form of the whole member as the checker received it, so the keys,
statuses and documents the verdict depended on are bound without being
disclosed. No History content, key or counterparty string is sealed.

Two existing paths would otherwise carry more than the digests:

- **The checker's own text.** A refused or failed run seals the first line
  of the checker's stderr as the reason (`runRulesChecker`), and findings
  carry the checker's `reason`, `limit` and `value`. When the input holds
  `received_history`, a refused run seals the cause alone, as
  `receivedNotice` already does for cards ("its stderr may carry a card").
  Findings stay in the holder's own book (next point); the result schema's
  description says a finding that reads the member reports the named fold's
  value, never document content.
- **Shares.** A shared copy discloses a record only when all its string
  values sit under `dealShareKeys`, and withholds it whole otherwise
  (`deal_share.go`). `history_sha256` and `input_sha256` stay off that list,
  so a check record holding them is withheld from every shared copy,
  including a counterparty copy whose policy carries the rules verdict
  (condition 2 in section 3).

### 5.5 Named folds, one counterparty, never combined

The folds a checker may run over the member are capsule-engine
`go/admission` predicates, which are already a closed list: a pack row whose
check is not one of them is refused at load (`pack.go`). The member feeds
the predicates that read the counterparty's card, each over one document:

- **known counterparty:** `card.verified` (an H3 or H4 verified under the
  policy key);
- **N deals, as their History lists them:** a new predicate,
  `card.history_deals` (`min`), over the one H3: the number of deal
  references the holder selected and the verifier accepted. A Basic History
  never establishes an outcome (sale, payment, delivery), that the selection
  is complete, or who holds the key (`historyNotEstablished`,
  `cli/verify_history.go`), so the row reads "their History lists N deals",
  never "N deals held", and the caller fills neither `Claims` nor `Bands`
  from it (those are claims held and corroborated, net-settled subjects,
  which an H3 does not show). A `history_card/1` carries no counts
  (`Card.Claims`, `go/admission/evaluate.go`), so over an H4 this row reads
  `not_read`;
- **how recent:** `card.fresh` (`max_age_s`) and `card.key_age`
  (`min_days`);
- **witnessed:** `card.witnessed`, `card.witness_receipts`.

Each predicate reads one document of one counterparty and yields one row
result; a pack has no predicate that combines rows into one number. The new
`card.history_deals` and any later predicate over the member are
capsule-engine changes and go through this same boundary. The schema descriptions state the limits positively: a
finding that reads `received_history` reports one named fold about this
counterparty against its limit, read side by side with the others and never
combined into one number. The member's verification and attribution are
enforced by the plugin; the fold rule is enforced where the folds live,
capsule-engine's predicate list, since the plugin cannot see how a checker
computes a value.

## 6. Tests (acceptance)

1. A History that fails a property, one answered under another key, a staple
   given no `--checkpoint-key`, a second document of one kind, and a
   non-History document are each refused with their reason; no checker run,
   nothing sealed (evidencebook-deal).
2. A stapled card that answers no request reaches the checker with
   `request_binding: "NOT_PRESENT"`; one re-signed under another key is
   refused (evidencebook-deal).
3. A verified History reaches the checker as `received_history`; a stub
   checker's verdict that reads it is sealed; the sealed block holds the
   History's SHA-256 and the member's `input_sha256`, and none of the
   document's content or keys; changing a key or a status changes
   `input_sha256` (evidencebook-deal).
4. A stub checker that refuses with the document on stderr seals the cause
   alone; a check record with `received_history` is withheld from the
   counterparty, adjudicator and every other shared copy, with the rules
   verdict's share policy on; the test names `history_sha256` and
   `input_sha256` exactly, since a key ending in `_digest` or `_commitment`
   would be shared (evidencebook-deal).
5. **No score or aggregate.** The input member is closed, `about` is the
   const `counterparty`, and `histories` holds at most one entry per kind
   (evidencebook). The result schema stays closed with no new member
   (evidencebook). A pack row over the member whose check is not a listed
   predicate is refused, and each listed predicate's result is about one
   document; `card.history_deals` counts an H3's accepted deal references,
   leaves `Claims` and `Bands` unset, and reads `not_read` over an H4
   (capsule-engine).
6. `received_cards` behaves byte for byte as before when given alongside
   `--history` (evidencebook-deal).

## 7. One-way doors

- **D1** and its three conditions, above: to Steven, with Sherlock2's
  boundary check (Buzz `0d98dcdd`, recommending (a)).
- **The member's name and shape** on `external-check-input/v0`, a public
  profile schema frozen at donation. Engineering's door; Sherlock2
  confirmed the name carries no product meaning beyond D1 (Buzz `0d98dcdd`).
- **The sealed rules block's shape.** Under #166, sealed check bytes were
  identical with and without `--received`; under (a) a check given
  `--history` seals the new `received_history` member. It is additive: an
  older reader of the sealed block that does not know it still reads the
  verdict and findings as before.

## 8. Order

1. Converge this note (Codex, Kimi, Sherlock2 boundary check) and decide D1
   (done: section 3).
2. evidencebook: the input member and schema text; release.
3. capsule-cli: a library entry for the staple verifier; release.
4. evidencebook-deal: `--history`, the keys, verification, the input, the
   sealed digests and the share rule; pin both releases.
5. capsule-engine: `card.history_deals` and the listed predicates read the
   member; Muse's pack follows (its own item, not this one).

The issue records "no code before #264" (How ack and Triage). That
dependency is revised here: the seam reads any verified
`personal_history_basic/1` or stapled `history_card/1`, and AAC's reader for
both is already in capsule-cli's verifier, so #264's remaining PR (`deal
history` writing `personal_history_basic/1` envelopes) is not a
precondition. This revision is an engineering call, accepted by the
coordinator on 2026-11-16 and recorded on capsule-cli#327.
