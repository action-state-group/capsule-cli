# Design note: a counterparty's verified History as a rules input

capsule-cli#327. Status: draft for review (round 1). Design only: no field
is added and no code is written until this note converges, including
Sherlock2's product-boundary check (the issue's own gate, and the recorded
non-stop decision on the issue). Checked against capsule-cli `main`
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
   unanswered) and its decision is local-only and never sealed. Muse runs it
   in `check --action/--record` only, not under `--emit`, because the deal
   check's checker "can neither be told who the received file is about nor
   show the decision" (`internal/plugin/admission.go`).

So the gap ruling 7 names is narrower than in October and has a clear
shape: today nothing **verified and attributed** about the counterparty can
reach the **sealed** verdict of a deal check. `received_cards` is neither
verified nor attributed and is kept out of the sealed result on purpose.

## 3. The decision for the boundary check

**D1 (to Steven, through Sherlock2's boundary check).** May a counterparty's
verified, attributed History change the deal check's **sealed** verdict and
findings?

- **(a) Yes, bound by digest (recommended).** The sealed run is given the
  verified History; the sealed rules block records each History read by kind
  and SHA-256 of its exact bytes, never its content. This is what ruling 7
  and step 4 ask for: an admission decision that a deal cites must be sealed,
  and a sealed verdict that depends on an input it does not bind cannot be
  re-checked.
- **(b) No, notice only.** Verified History joins `received_cards`' path:
  read by a second run for an unsealed line. Then #327 adds only
  verification and attribution on top of #166, and step 4's "the decision is
  sealed" stays unmet on the deal path.

Why it goes to Steven: it decides what is sealed about a counterparty in
my book and in any share of that check record (a digest of their History),
and whether their History can change my sealed decision, which is the
reputation-adjacent line the ruling flagged. The rest of this note assumes
(a); under (b), sections 5.3 and 6's sealing test drop out and the member is
read by the notice run only.

## 4. Owners

| Part | Owner | Change |
|---|---|---|
| `received_history` member and its limits, result schema descriptions | evidencebook (`dealprofile/x-deal-v0/`) | additive optional member; descriptions state the hard limits |
| Reading `--history`, verifying, attributing, building the input, sealing the digests | evidencebook-deal (`deal check`, `rulesInput`) | new flag and input member |
| Verifying a History file or a stapled card | capsule-cli core (`cli/verify_history*.go`), over AAC `go/personalhistory`, `go/standardhistory` | reuse; export a library entry if the plugin cannot call `HistoryVerdict` as is |
| Named folds over the member | the checker's ruleset: capsule-engine `go/admission` rows, Muse's pack | outside this note; each fold reads one counterparty |

No AAC digest or signature rule is reimplemented: verification is AAC's
verifier through capsule-cli's existing History checks. Build issues are
filed in evidencebook and evidencebook-deal after convergence, and #327
closes against them.

## 5. The design (under D1 (a))

### 5.1 Input to the verb

`deal check --history FILE --counterparty-key HEX[,HEX]`. FILE holds one
or more History documents the host received about this deal's
counterparty: a `personal_history_basic/1` History (H3) or a stapled
`history_card/1` (H4, an `evidence-commitment/1` or a `{history, request,
reply}` file), the two shapes ruling 7 names, each exactly as received.
`--counterparty-key` is the key the host expects this counterparty to hold,
from deployment policy or from the staple the counterparty presented with
this deal's offer; the plugin never takes a key from the History itself. It
is checked as `verify --history` checks it today: as the holder key
(`--holder-key`) for a History, as the counterparty key
(`--counterparty-key`) for a staple. The flag needs a pinned rules checker,
like `--received`.

### 5.2 Verified and attributed, or refused with a reason

Before any checker run, each document is checked with capsule-cli's History
verifier (`cli/verify_history.go`, `cli/verify_history_staple.go`) against
the given key. The whole check is refused (input error, no checker run,
nothing sealed) when any document:

- is not accepted (`ErrHistoryNotAccepted`): a property failed, or it was
  answered under a key other than `--counterparty-key`, so it is not
  attributable to this counterparty; the reason names the property;
- is any other kind: "not a History this seam reads".

One exception, already a stated limit of the verifier: a stapled card that
checks out but answers no request of ours (`ErrHistoryAnswersNoRequest`,
`request_binding` NOT_PRESENT, because a commitment staples the reply octets
only) is read as verified, and the member says so (`request_binding:
"NOT_PRESENT"`), since a card stapled to an offer answers no request of the
reader's by construction.

Refusing, rather than dropping the document, keeps the host from believing
a History was read when it was not. This meets the acceptance "an
unverified one is refused with a reason".

### 5.3 The member

`external-check-input/v0` gains one optional member, absent when no
`--history` was given:

```json
"received_history": {
  "about": "counterparty",
  "counterparty_keys": ["<hex>"],
  "histories": [
    {"kind": "personal_history_basic/1", "sha256": "<hex>", "verified": "PASS", "document": { }}
  ]
}
```

- **Attributed.** The whole member is about one party: this deal's
  counterparty, under the keys the host gave. A checker's finding that reads
  it reads "their History says ...".
- **One counterparty per input.** A deal check has one counterparty, so the
  input never holds two parties' Histories; nothing in the seam can combine
  across counterparties.
- **Read-only.** The member is input; the result schema gains no member to
  write History back, and the plugin stores nothing of `document`.
- **Distinct from `received_cards`.** That member stays as it is: unverified,
  for the notice run only. Merging them would hide from a checker which
  cards were verified.

### 5.4 What is sealed

The sealed rules block gains `received_history`:
`[{"kind": ..., "sha256": ...}]`, in input order, so the record shows which
verified Histories the verdict read. No History content, holder key or
counterparty string is sealed. A share of the check record carries the
digests only under the holder's existing share policy for the rules block;
this note adds no share path.

### 5.5 Named folds, never a score

The folds belong to the checker's ruleset (for example capsule-engine's
admission rows, or a Muse pack rule "known counterparty: at least N verified
deals"). The seam's own limits:

- The result schema's `received_history`-related text states the ruling's
  limits: a finding that reads the member reports one named fold about this
  counterparty, as its `value` against its `limit`; no member is a score,
  rank or reputation, and none combines Histories into one number.
- The result schema gains no new member for this item. Its members stay
  closed ("Members not listed here are refused"), so a checker cannot add a
  `score` member.
- The plugin cannot police how a checker computes a `value`; as with
  `received_cards`, the schema description is the contract, and the
  boundary is that the input holds one counterparty.

## 6. Tests (acceptance)

1. A History that fails a property, one answered under another key, and a
   non-History document are each refused with their reason; no checker run,
   nothing sealed (evidencebook-deal).
2. A stapled card that answers no request reaches the checker with
   `request_binding: "NOT_PRESENT"`; one under another key is refused
   (evidencebook-deal).
3. A verified History reaches the checker as `received_history`; a stub
   checker's verdict that reads it is sealed, and the sealed block names the
   History's kind and SHA-256 and holds none of its content (evidencebook-deal).
4. **No score or aggregate.** The result schema has no member outside the
   current closed set, and no member or `$defs` name matches score, rank,
   reputation or aggregate; a result that adds such a member is refused
   (evidencebook, evidencebook-deal). The input schema refuses a
   `received_history` with more than one `about` or a second party
   (evidencebook).
5. `received_cards` behaves byte for byte as before when given alongside
   `--history` (evidencebook-deal).

## 7. One-way doors

- **D1**, above: to Steven through Sherlock2's boundary check.
- **The member's name and shape** on a public profile schema
  (`external-check-input/v0`, frozen at donation). Engineering's door once D1
  is ruled; Sherlock2 confirms it carries no product meaning beyond D1.
- **The sealed `received_history` digests** follow from D1 (a).

## 8. Order

1. Converge this note (Codex, Kimi, Sherlock2 boundary check, D1 ruled).
2. evidencebook: the input member and schema text; release.
3. evidencebook-deal: `--history`, verification, the input and the sealed
   digests; pin the evidencebook release.
4. capsule-cli: only if step 3 needs a library entry for the verifier.
5. Muse and capsule-engine: their pack folds read the member (their own
   items, not this one).

capsule-cli#264's remaining PR (`deal history` writing
`personal_history_basic/1` envelopes) is not a precondition: the seam reads
any verified `personal_history_basic/1` or stapled `history_card/1`, whoever
produced it.
