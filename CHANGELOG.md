# Changelog

## Unreleased

### A deal's copy over evidence requests, and `compose` from what was recorded

- **Added:** on a deal profile, `request`, `respond` and `request --response` carry a deal's
  shared copy. The subject is `{"kind":"correlation","value":"<deal id>"}`: the deal id both
  parties hold (`deal open --deal-id`).
  - `request --request` records the ask on the deal's requests log (`deal/<id>/requests`) and
    writes the canonical request bytes. Their digest is the one evidencebook gives the same
    request.
  - `respond --requester WHO` answers with the copy `disclose --deal --share counterparty` makes,
    in an evidencebook signed artifact response. The share is sealed on the disclosure log as
    before, and its record names the request's digest. The artifact is the copy's JCS bytes, so
    the signed `artifact_digest` is its bundle digest.
  - Under `coverage.min_freshness` the copy is fresh: a report is sealed and a checkpoint cut.
    Under `coverage.expected_pin` (`{root, mmr_size}`) the copy is re-served at exactly that
    checkpoint, with nothing new sealed. That works only while the deal's log ends at the pin
    with a copy's report as its newest record. Otherwise, for example once the deal has moved on,
    the answer is a signed `coverage_unsatisfiable` refusal.
  - `request --response` records the answer under the pinned responder keys. A signed copy that
    does not verify VALID is recorded `artifact_failed_verification`, never as a grant; so is one
    that is not anchored under the responder's checkpoint key, not on the requested deal's log,
    or not at the pinned or fresh enough checkpoint. `--absent-until` records an absence.
- **Added:** `capsulectl compose --deal ID --member REQUEST_RECORD_ID=RESPONSE_FILE ...` (two or
  more members; `REQUEST_RECORD_ID` alone for a recorded absence) writes a composed/v1 bundle from
  recorded answers only. It refuses:
  - a member whose request this profile never recorded;
  - a request still pending;
  - response bytes other than the ones recorded.

  Each member's `request_digest` is the recorded request's digest, and its observer is the
  responder key the answer verified under. A copy's member carries `response`: the responder's
  signed response without its artifact bytes, which binds the copy to the request it answered.
  Each pair of members is joined on the deal id (`pre_agreed_identifier`, `pointer: "/deal_id"`),
  declared in the state the verifier derives. The containing bundle's root is a composition
  record sealed on `deal/<id>/compositions`.
- **Added (wire):** every deal step's Capsule carries the deal id as its top-level `deal_id`
  member (x-deal-v0 section 1), so two parties' copies join on it. A step an earlier release
  prepared, without it, is still recovered as that release prepared it.
- **Changed:** `verify --bundle` reads a composed/v1 block holding a refusal member as
  INCOMPLETE (the member reports `reads_as: "INCOMPLETE"`): what was asked for was not served.
  A refusal is still no failed claim, so it never makes the verdict INVALID. A copy's carried
  `response` is checked under profile `evidencebook-artifact-response`: an invalid signature, or
  one under a key other than the member's observer, fails.
- **Fixed:** `verify --bundle` checks a carried refusal in evidencebook's own form (`key_id`, and
  `sig` over `{request_digest, reason, issued_at}`), the form `respond` sends, and reports
  profile `evidencebook-refusal`. Before this fix, such a refusal read `invalid`.

### Offer, commit, sign and cancel acts seal their disposition and counterparty

- **Changed:** an approved `offer`, `commit`, `sign` or `cancel` act is sealed as a `decide`
  Capsule, as a `pay` is, with the same disposition rule (the typed record's own
  `authority_basis`; the approval in `x-deal-v0`) and its effect type: `make_offer`
  (`two_way`), `accept_agreement`, `sign_document`, `cancel_commitment`. These four effect
  types are **provisional**: proposed for agent-action-capsule's registry, held for
  ratification and not yet registered, and provisional here until they are registered (the deal
  profile's PROFILE.md marks each one). Before, only a `pay` had a disposition, so a reader could not tell an accepted
  commit from a repeat, and after a sale's first commit every later commit check in it was
  refused.
- **Changed:** the same acts seal the counterparty of the check they rest on (an act names no
  payee), exactly as that check sealed it, so a rule keying earlier acts on who they were with
  (a repeated booking in one deal) finds it in the sealed record, not only beside it in the
  checker's history.
- Steps sealed before keep their bytes: each change is marked on the step (`act_effects`,
  `act_counterparty`), and a step without the mark re-derives as it was sealed. An unchecked act
  stays `fyi`. The deal skill's SKILL.md is unchanged, so its digest is too.

### A merchant email whose key cannot be read now is sealed at the next tick

- **Changed:** when `deal note --kind evidence --email` cannot reach DNS for the merchant's DKIM
  key or DMARC records (a timeout or unreachable resolver; an absent record is still sealed as
  absent), it no longer refuses with a way on that asked for the record to be looked up some
  other way. It answers `"state": "deferred"` with `"say": "the merchant's signature will be
  checked later"`, seals nothing, and keeps the email and its note on this device
  (`deal_deferred_emails`, local only). Each `deal tick` first reads the records again with the
  store unlocked and, when it can, seals the email on its deal as the deal stands then, with
  `key_source: dns` and a checkpoint at once. The step's time is when the key was read; after a
  close, the email is later evidence linked to the close. The tick's result lists the queue under
  `merchant_emails`: `sealed` (with the same fields `deal note` reports), `waiting` (each email's
  deal and when it was queued; there is no retry limit) and `refused` (an email its deal no longer
  takes, such as a cancel-by date on a deal closed meanwhile: kept on the device, never retried,
  listed at every tick). A failure on one email is listed under `failed` and never stops the
  tick. An email whose exact bytes its deal already holds is not sealed twice. No record shape
  changes.
- A capture that fails for any other reason (the email is not raw RFC 822 with its headers
  intact, or its DKIM-Signature headers cannot be read at all) is still refused at once. A
  signature that is merely malformed fails and is sealed as failing, as before. `--key-record` / `--dmarc-record` are unchanged.
- The deal skill no longer tells the agent to have the user look up a key record: it says only
  that the merchant's signature will be checked later. This changes the deal SKILL.md digest
  (sha256 `b5d4d9aec20328a746684283ba3ec7a0237b7fd6a11b5ecef348c39ae8722fa3` to
  `643ac179237d9a5f30f9bbd373b93db84f4cff0428fa0edcfa66461bf042d29a`).

### The user's original request, sealed when the task starts

- **Added:** `deal task new --input task.json` seals what the user asked for when a buying or
  booking task starts, before any search and before there is a merchant: `verbatim` (their
  request exactly as given, never trimmed or normalized) and, when they stated them, `what`,
  `use` and `budget_max` (`{amount_minor, currency}`). It is a new x-deal-v0 record type,
  `task_intent`, the only record of a log of its own (`task-` and 16 hex). Nothing is in the
  clear: the request is a salted commitment (`verbatim_commitment`, `sha256-jcs-nonce256`), and
  each constraint is a `fields[]` entry whose `class` is in the clear and whose value is
  committed under its own nonce, so a copy can open one without the others.
- **Added:** `deal open --task <task_id>` cites it: the baseline carries one `cites` ref (a new
  `rel`) to the task intent's record digest, which shows the request existed before the deal
  opened. A deal opened without `--task` stays valid; its task bounds are then not available.
- **Added:** a pick (`deal note --kind intent`) takes `shown` (the options the agent showed) and
  `recommended` (the one it recommended), sealed as `shown_commitment` and
  `recommended_commitment`; the pick's `verbatim` is the user's choice. `deal open` refuses them:
  they go on the pick.
- **Changed:** the user's own copy carries the cited record and its openings (`task_intent`) and
  each pick's openings (`pick_openings`); `deal report --html` checks them before writing the
  page and refuses a page whose openings do not recompute. The page's "What you asked" shows the
  original request and the stated limits, then "You picked" with the words the deal opened on and
  each pick (the options shown, the agent's recommendation, the user's choice); the page title
  quotes the original request. A deal with no task intent says so: "No record of your original
  request was sealed before this deal opened, so it is not shown." A cited record this device no
  longer holds is reported, never shown. The email receipt does the same. A shared copy carries
  none of it.
- `deal list` lists a task with kind `task`. The x-deal-v0 schema (both copies), PROFILE.md
  (section 3, section 6 rule 12) and `check_profile.py` know the new record type and ref. The deal
  skill seals the request at the start of a purchase or booking and passes `--task` at the final
  review, which changes its SKILL.md digest (sha256 `643ac179237d9a5f30f9bbd373b93db84f4cff0428fa0edcfa66461bf042d29a` to
  `83d1a5e98e57a3d4ab23954f79b0a15a164b4d9e143933a02a0a322d71bafcf7`).

### `deal list --json`: a plain summary per deal

- **Added:** each row of `deal list --json` carries `summary`: `what` (the user's own asked
  words, as `deal report` gives them), `counterparty` (the name the opening step sealed as
  `who.name`, or `"not recorded"`: never read out of a message or other free text), `amount`
  (`amount_minor` and `currency` as recorded, with `basis` `"paid"` for the net of the sealed acts
  in the first act's currency, `deal report`'s `net_minor`, acts in another currency counted in
  `other_currency_acts` and never summed; or `"opening_terms"` for the opening step's price when
  no act moved money, a later change of terms not read; `null` when neither is recorded),
  `outcome` (the close step's own outcome word, or `"open"`) and `text`, the four as one line.
  It is read from the sealed steps the list already reads: the list still seals nothing and
  cuts no checkpoint, which `deal report` does even without a file flag. The summary is the
  user's own view and private; no shared copy (`deal report --share`) changes. The tab-separated
  output is unchanged.

### A presentation page carries only the modules for its audience

- **Fixed (privacy):** `presentation build` put every installed plugin's presentation module in
  the page, whatever `--audience` said, so a page built for the other party carried the script of
  a module meant only for the user's own copy. That module could never render there, but its code
  travelled with the page. The page now carries only the modules whose manifest audiences include
  `--audience` (or `"*"`), the same rule the page's resolver applies. Each module left out is
  listed under `not_included`, its reason naming the module's audiences, and `--module` naming one
  is refused, with no page written. A module for `"*"` is carried as before.
- The builder's ambiguity and duplicate-id checks now look only at the modules the page carries,
  so two modules that could match one bundle are refused only on a page that would carry both.
  When one module is left, its wording pack is the page's, as for any one-module page.

### A built-in materiality predicate, pinned by name

- **Added:** `deal init --materiality builtin:neutral` and `profile update --materiality builtin:neutral`
  pin the materiality predicate built into capsulectl ("quantity, size, variant and delivery picks",
  version 1.0.0, digest `05ec66b4a21025a66288cafc74e7fa1c540cc62695c5f536df84f4a7d7f66d4a`). With
  it, a quantity of 1 the agent filled in does not pause a check; another quantity, or a size,
  variant or delivery the agent picked, still does. The profile keeps the name `builtin:neutral`
  and the digest, not a path, so the pin survives an upgrade or a move. A check still refuses when
  the pinned digest is not the one this capsulectl ships. A predicate given as a FILE is pinned by
  its absolute path, as before; with none, every pick still pauses. The example
  `skills/deal/profile/materiality-predicate/neutral.json` is unchanged. The deal skill's SKILL.md
  names the built-in, which changes its digest.

### The approval prompt is one block, each fact once

- **Changed:** `deal check` returns one block to show the user, built from the check's own
  fields: the headline (what, how much, to whom, by what rail, refundable or not), the rules line
  once, then one line per distinct finding. A first dealing, the profile being new and every
  detail told for the first time are one line. A pick the check paused on is its own line,
  beside what the user asked for and the agent's other picks. Unverified claims are one line.
  The block lists no options (`options` stay the host's to render), and the rules line names no
  CLI syntax. Before, the agent was told to show both `approval_text` and `card`, so the rules
  line printed three times, every finding twice, and the buttons appeared again as text.
- On a pause, `card` (the text sealed as `shown_card`) is that same block, so `--shown-card`
  takes the `approval_text` shown. A pass still has no card. Records sealed before keep their
  card as it was. The deal skill now has one show-it-verbatim rule naming `approval_text`, which
  changes its SKILL.md digest.
- **Changed:** the proceed option is an override ("Pay anyway", "Share anyway") only when something
  the user set was broken: a rule of theirs that failed and asks for approval, or a limit they gave.
  Rules not checked or not fully checked, and findings such as a changed payee or a first telling,
  now offer "Pay" / "Share". A deny still offers only "Hold".
- **Changed:** in the block, the rules line is a bullet like the findings, and what the user asked
  for and what the agent picked are separate bullets. A paused share in a deal that may still pay,
  with no pay sealed yet, says `Step 1 of 2: I will ask again before paying $X.` after the headline,
  with the deal's total.
- **Changed (deal skill):** the agent may put one sentence of its own before the block, and nothing
  else but one plain question when the host has no buttons. It never prints an option label as
  text, and offers `options` as buttons where the host supports them. After a share's "Step 1 of
  2", it asks the user before paying, even when the pay check passes.

### `presentation list`: which presentation module a bundle resolves to

- **Added:** `capsulectl presentation list --bundle <file> [--audience <token>] [--format html|fragment|embedded] [--text]`
  says which presentation module the bundle resolves to among every module this tool can put in a
  page: the page's built-ins, its deal view (`capsulectl.deal-view/v0`) and the modules the
  installed plugins offer through the `presentations[]` handshake, each listed with its source,
  trust class, tier and whether it matches. agent-action-capsule's Go resolver (`go/presentation`)
  makes every decision: the bundle's descriptor, the runtime's refusals (each refused module is
  shown with its reason; one that matched, with the resolver's refusal line), the ambiguity test at registration and the
  resolution. Exit 0 for a module or for no presentation; 1 when the bundle does not pass the
  viewer's verification gate (nothing is matched); 2 when two modules of one tier can match one
  bundle (the resolver's named ambiguity, never first-wins) or the registry refuses a manifest.
  The JSON report is the default; `--text` prints a short summary.

## v0.1.0-rc15

### Release tags are signed from v0.1.0-rc15

- **Changed (release procedure).** From `v0.1.0-rc15` on, every release tag is SSH-signed by the
  maintainer's key, whose public key is in the release monitor's allowed-signers file. The tag
  step in `docs/RELEASING.md` is now two parts: prepare (record the merge commit) and one line
  for the maintainer that signs and pushes the tag, with SSH pinned for the signature.
- **Added: `release/known-unsigned.txt`**, a reference copy of the tags made before signing
  began, exactly `v0.1.0-rc1` to `v0.1.0-rc14`, frozen. The monitor's operator copies it once
  from the signed `v0.1.0-rc15` tag and checks its SHA-256 against the value in
  `docs/RELEASE-TRANSPARENCY.md`, section 5. The monitor never reads the repository's file, and
  a later edit of it is itself a red flag. An unsigned tag made after signing began is an
  alarm, whatever its name.

### A report opened outside a secure context verifies

- **Fixed:** a valid deal report opened where the browser gives no WebCrypto (a non-secure or
  opaque origin, such as an app's attachment viewer on iPhone) said it did not verify: the page hashed only
  through `crypto.subtle`, which exists only in a secure context, so every digest, membership
  and range check failed. The vendored runtime is rebuilt from agent-action-capsule with a plain
  SHA-256 fallback (and `@action-state-group/cll` 0.2.1 for the MMR proofs); the ten checks
  now pass with no `crypto` at all. `scripts/check-viewer-without-webcrypto.mjs PAGE.html` runs a
  page's own verifier that way and prints its checks.
- **Changed:** without WebCrypto the deal page lists the countersignatures and the cited signers
  under "Checked only by capsulectl verify": it cannot check an Ed25519 signature there.
- **Added:** a viewer with no SHA-256 at all shows "Unable to verify in this viewer" and the
  `capsulectl verify` command for the file, never the failure banner.

### A sale registers its threads, and the user's own sale bundle proves the list is whole

- **Added (wire, additive; records sealed before re-derive unchanged).** `deal open --sale` seals a
  `thread` record on the sale's own log before the thread opens. It holds the thread's deal id as
  a commitment only. The thread's `task-authority/v0` names that record with a `registration`
  ref. A thread's task authority that should name a registration and does not is refused, never
  sealed. An open that failed after registering reuses its registration on the next try.
- **Added: `deal sale bundle --sale ID --out FILE`,** the user's own copy of a sale:
  - the sale log's Evidence Bundle, whose completeness certificate covers every registration;
  - each thread's own copy, in the `x-deal-sale/v0` extension;
  - `sale_threads`: for each registration, its opening and whether its thread is `present`,
    `missing` or `never_opened`.
- **Added (verify).** `verify --bundle` checks a sale bundle's binding. A missing thread, or a sale
  with threads opened before registration, is INCOMPLETE. Any mismatch, or a carried thread that
  fails, is INVALID. A bundle that is not a sale's is `not_a_sale_bundle`. No buyer's copy carries
  any of it.

### A sale bundle is cut fresh, and a sale's log says which threads opened

- **Added (wire, additive; records sealed before re-derive unchanged).** When a sale's thread
  opens, the sale's log seals a `thread_opened` record: its registration, and its task authority
  by commitment. `deal sale bundle` seals each thread's report, then a `sale_cut` naming each
  opened thread's last record by commitment, then the sale log's checkpoint. The bundle's
  certified interval then covers every thread's acts up to the bundle. Present `sale_threads`
  entries carry the openings (`opened`, `head`).
- **Added (verify).** A present thread must open to its `thread_opened`, and its last record must
  be its head in the latest cut, so a copy cut short is INVALID. A `never_opened` registration has
  neither; when a withheld record could be its opening, it is INCOMPLETE. A sale copy without
  these records reads INCOMPLETE; any opening or head that does not match is INVALID.

### `verify --bundle`: one name per sale-cut finding, against the latest cut only

- **Fixed.** In a sale bundle, `no_sale_cut` (no readable cut) is said once, not once per thread,
  and a readable cut without a present thread's head says `thread_not_in_the_cut` for that thread.
  Only the latest cut counts: a cut is read only when its disclosure matches what its record
  sealed and no record this copy cannot read follows it on the sale's log. A withheld or edited
  latest cut is `no_sale_cut`, never a silent fall-back to an older one.

### A rules checker sees who an earlier act was with

- **Added (checker input only; nothing sealed changes).** Each act in a check's `history` now
  carries `counterparty`: the counterparty the act's check sealed, exactly as sealed and keyed per
  deal, beside the act record (which names none). A rule keyed on the counterparty sees a repeated
  booking in one deal as a repeat on acts, as on checks; `counterparty_profile` remains the key
  across deals. Absent for an act with no check behind it, or a check that names no counterparty.

### A seller's check tells the rules checker what the agent stated earlier

- **Added (checker input only; nothing sealed changes).** Where the user sells, the checked record
  given to the rules checker carries `deal_claims`: the deal's agent claims that state a class,
  sealed before the checked step, in seal order. Each is its sealed claim record's own values, with
  the words as a commitment only. A rule on a required disclosure can then find a statement the
  agent made before the offer. Absent when there is none; a buyer's checker input is unchanged.

### The deal page says which checks ran on it and which only `capsulectl verify` runs

- **Changed:** the deal page lists, in plain words, what the page itself checked and what only
  `capsulectl verify --bundle` checks, with the command to run.
  - **On the page:** each record's digest, its place in the log, the range proof, opened words, a
    countersignature's signature, and the agent's signature only on the records the page cites as
    signers.
  - **Only by `capsulectl verify`:** the checkpoint's signature when the page could not check it
    (the page says it shows the bundle as passing without it), the agent's signature on every
    record, the witness receipt, and that the log only grew since its earlier checkpoint.

  The lists are read from what the page's verification recorded and what the file carries.
- **Changed:** the command includes `--witness-directory` when the file carries a witness receipt.
- **Changed:** the page says none of these checks says who the agent or its user is, or that the
  file is the latest copy. "This page checked itself" is gone.

### A deal page's title says what that copy shows, with or without JavaScript

- **Changed:** a deal page's `<title>`, which a tab or a host's preview shows even with JavaScript
  off, now says what that copy shows in the clear:
  - on the user's own copy, the words they asked, only when checked against their sealed
    commitment when the page was built (whitespace folded, at most 120 characters);
  - on a shared copy, only whom it is for.

  It never carries a structured field such as a floor or a limit. The share gate scans it with the
  rest of a shared page.
- **Not in this change:** a `<noscript>` line in the page body, which waits on a slot in
  agent-action-capsule's page shell.

### `deal list`: every deal in the profile's store, read-only

- **Added:** `capsulectl deal list [--json]` prints one line per deal in the profile's store, in
  deal id order, tab-separated:
  - `deal_id`;
  - `kind`: `deal`, or `sale` for a `deal sale new` log. A sale's buyer threads are deals and are
    listed as `deal`. A sale never closes, so its `state` is always `open`;
  - `state`: `open`, or `closed` once a final close is in the index (as `deal deadlines` decides);
  - `opened_at` and `last_record_at`: the `at` of the deal's first and last step;
  - `counterparty_fingerprint`: the payee fingerprint of the latest `counterparty_profile` step,
    or `-` when there is none;
  - `log_id`.

  `--json` prints the same fields as one JSON array, in that key order, with `null` for no
  fingerprint. A store with no deal yet prints `[]` with `--json` and nothing without, and exits 0.
- **Reads the store's index, not the deal logs:**
  - Every query is a SELECT, under the store's shared lock when its lock file exists, like
    `deal checkpoint status`. It works on a `read_only` profile.
  - Each row gets the index checks a deal command makes before it reads a deal: one strict record
    whose deal, step number and kind are the row's, chained to the step before it by digest. A row
    that fails them is refused as a conflict.
  - The steps are not re-verified against the deal's log; `deal export` and `deal report` do that.
    A last step that was indexed but never reached the log (a crash between the two writes) is
    listed, and the deal's next write command recovers it.
  - On a store whose last writer closed cleanly, every file under the store is byte-for-byte the
    same after the list.

### The deal page is built through the emitter's slots, under a per-page CSP

- **Changed:** `deal report --html` builds its page through agent-action-capsule's emitter
  options (agent-action-capsule `fa49b4d`, with the browser runtime rebuilt from it):
  - the title;
  - the deal view's stylesheet (in the theme slot, until module stylesheets can be pinned);
  - the deal view as a digest-pinned module;
  - a bootstrap that builds the verified context once.

  The page's Content-Security-Policy lists the digest of every inline script and style and allows
  no network. Nothing is spliced into the page any more.
- **Changed: the deal view reads only the verified context.** It no longer verifies the bundle,
  reads `window.__BUNDLE__` or the bundle's disclosures or records, or recomputes a commitment.
- **The words a page shows are checked when it is built.** These are the user's words and each
  statement the agent made. capsulectl recomputes each against its sealed commitment and writes no
  page whose words do not match. The page now says they were "checked when this page was built".
- **The sealed-report refusal** stays in the build-time page gate.
- **Interim:** the deal view's stylesheet rides in the theme slot until the emitter can pin a
  module's own stylesheet (agent-action-capsule #214).
- **Not in this change:**
  - the static, no-script page: capsulectl does not produce it, and will report it as unavailable
    (`no-document`) for now;
  - the share gate's removal of the two vendored scripts before it scans a shared page is retired in
    a later change.

### A hex id or digest is never read as money

- **Fixed.** The share gate no longer reads a currency code inside a run of hex as money ("…3cad8…"
  as 8 Canadian dollars, a digest's "8aed…" as 8 dirham), and the spending-limit and floor check
  takes out the deal's own ids and every digest, key and signature before it reads a copy. A deal's
  share could be refused at random when such a run matched the user's limit; a code written against
  an amount as a word of its own ("1700usd", "USD1,700") still counts.

### The deal page's view is a registered presentation module

- **Changed:** `deal report --html` puts the deal view in the page as the presentation module `capsulectl.deal-view/v0`, with its own manifest. The page's runtime selects it for a verified deal bundle. It reads only the verified context and what capsulectl checked when it built the page.
- **Changed:** the deal view's stylesheet is its own, pinned in the page's Content-Security-Policy by its manifest. The theme slot holds theme tokens only.
- **Checked when the page is built:** capsulectl writes no deal page whose view the page's runtime would refuse, or that could be ambiguous with a built-in view.
- **Changed:** a deal page whose bundle does not verify shows the runtime's refusal and no deal section.
- **Not in this change:** the deal view's words in a wording pack; the share gate on the page builder's inputs; the static, no-script page, which stays unavailable (`no-document`).

### The deal page says first what the witness covers

- **Changed:** the deal page opens with one line saying what its witness receipt covers, read from the bundle's witness block only:
  - "Witnessed through step K of N at `<time>` (receipt attached); later steps: witness receipt pending the next tick", when the receipt covers part of the deal;
  - "Witnessed through the last step this copy holds at `<time>` (receipt attached)", when it covers every step;
  - "Not witnessed yet: witness receipt pending the next tick", before the first tick;
  - "Not witnessed.", when there is no receipt.

  The line never says the whole deal is witnessed. It is on every copy, shared ones included.

### A plugin may offer presentations

- **Added:** the cli-plugin/v1 handshake may carry `presentations`.
  - **What each entry holds:** an `aac.presentation-manifest/v0` manifest (agent-action-capsule's
    presentation contract) and the files it names by role (`script`, `style`, `wording`) with their
    SHA-256.
  - **Compatibility:** an older capsulectl ignores the member. A module's compatibility is its
    manifest's `presentation_api` and `runtime_min`, not cli-plugin/v1.
- **How each file is checked:** capsulectl reads it itself, from the launcher's own directory,
  under the launcher's trust rules, at most 1 MiB. It must hash to both its entry's digest and the
  digest the manifest pins.
  - The manifest and any wording pack are checked against the contract's schema.
  - A plugin's code module is carried in the module slot.
  - Any failure refuses that plugin's presentations; its subcommands still run. `plugin ls` reports
    the refusal as `presentations_refused: {reason, file, detail}`, with a machine-readable
    reason: `digest_mismatch`, `path_escape`, `not_regular_file`, `writable`, `oversize`,
    `invalid_manifest`,
    `invalid_files`, `invalid_wording`, `unsupported_carrier`, `duplicate_id`, `missing_file`,
    `untrusted_root` or `unreadable`.
- **Layout:** a plugin keeps its files under `<launcher>.d/presentations/<module>/`, user-owned
  `0644` files in `0755` directories.
- **Presentations load only from the two built-in plugin roots.** `CAPSULECTL_PLUGIN_ROOTS` never
  makes a directory a source of them.
- **Not in this change:** putting presentations into a page.

### `presentation build`: the offline page with the installed plugins' presentation modules

- **Added:** `capsulectl presentation build --bundle FILE --out FILE [--audience A] [--depth L] [--module ID]` writes the offline evidence page with the installed plugins' trusted-executable presentation modules in its module slots. The page's own runtime decides which module renders.
- **Checked before anything is written:**
  - the page's runtime (`aac.presentation-api/v0`, version `0.1.0`) would refuse no module given to it (`presentation_api_unsupported`, `runtime_too_old`);
  - no two manifests the page would hold, the built-ins included, are of one tier and can match the same bundle (`ambiguous`);
  - no id is given twice (`duplicate_id`), and no manifest can never match (`dead_manifest`).

  A refusal writes no page, and the result names the module and the reason.
- **The page is written only when `verify --bundle` calls the bundle VALID.**
- **What the result reports:** each module carried, and what was left out (`not_included` for a declarative module; `plugins_refused` for a plugin whose presentations were refused).
- **Not in this change:**
  - `presentation list`;
  - the fragment and embedded packagings;
  - the static page, which is reported as unavailable (`no-document`).

### A seller's offer is classed `marketplace.offer`

- **Changed (wire; records sealed before re-derive unchanged).** An offer is now classed
  `marketplace.offer`; before this, `external_commitment.other`. It is the taxonomy-version-6 class a
  seller's rules select on: under the old class no seller rule reached an offer, so an offer below the
  floor was not held to it.
- **No taxonomy change.** Version 6 already has `marketplace.offer`. Each step keeps its own class:
  an offer sealed before this change re-derives as `external_commitment.other`.

### A sale thread's check names the thread's buyer

- **Changed (wire; records sealed before re-derive unchanged).** A check on a sale's thread that
  names no one now seals the thread's buyer as its `counterparty`: the keyed fingerprints the
  thread's opening sealed, per deal, as a purchase's check carries them (typed records:
  `body.counterparty`, `hmac-sha256-chain-key`). Rules keyed on the counterparty, such as one
  commitment per sale, now have a target on a seller's deal.
- Shared copies carry the fingerprints, never the buyer's identifiers. No profile-keyed companion is
  sealed for such a check.

### A seller's commit tells the rules checker when the accepted offer was made

- **Added (checker input only; nothing sealed changes).** The check of a seller's `commit` that rests
  on an accepted offer now carries `record.proposal_at`. That is the offer's proposed-action record's
  sealed `at`, so a rule on an offer's expiry can be evaluated: before, it could not be checked on
  any commit.
- It is the latest offer, with a recorded acceptance and no change of details since: the same
  acceptance the commit gate requires. It is absent on a buyer's deal, on any other action, with no
  acceptance in force, and on history entries.
- `record` is open in `external-check-input-v0.schema.json`, so a checker that predates the member
  ignores it.

### A history act on a sale's thread carries the sale's item reference

- **Added (checker input, additive).** In `external-check-input/v0`, each `history` entry whose
  act was on a sale's thread carries `item_ref`: the sale's item reference, the same plain value
  the top-level `item_ref` carries on a check of that sale's threads. A rule on a second buyer's
  thread can see the first thread's acceptance of the same item. An act of any other deal carries
  none. It is beside the record, as `counterparty_profile` is, never in it: no record changes.
  History entries are open objects, so a checker that does not read it is unaffected.

### A claim's class is one of a seller's fourteen representation classes

- **Changed (schema, additive; records sealed before re-derive unchanged).** A claim's optional
  `class` is one of `price`, `condition`, `features`, `authenticity`, `availability`,
  `delivery_date`, `service_scope`, `warranty`, `refund_terms`, `payment_methods`, `pickup`,
  `deadline`, `address` or `other`: the representation classes of capsule-engine's
  `capsule_engine/guards/wickets/catalog_defs/required_disclosure.yaml` at `21975ce45837`, in
  the same order. Any other class is refused.
- **`delivery_promise` becomes `delivery_date`.** An input that says `delivery_promise` is sealed
  as `delivery_date`; capsulectl never seals `delivery_promise` again. A claim sealed with it
  stays valid in the profile schema, re-derives, and is labelled as before on the page.
- The page labels every class; the deal skill and the profile list them.

### Every copy of a seller's deal engages `x-deal-seller/v0`

- **Added (bundle packaging; no record changes).** Every copy of a deal the user sells (the
  user's own, the counterparty's and an adjudicator's, a sale's threads included) engages the
  bundle extension kind `x-deal-seller/v0`, with an empty block, so a viewer can choose a
  seller's presentation from the engaged kinds. It carries no value and is never authority:
  which side the user is on stays the opening intent's sealed `party_role`. A buyer's deal
  engages nothing new.

### Every amount-bearing check names the payee, not only a pay

- **Changed (wire, additive; records sealed before re-derive unchanged).** Where the user buys, a
  `commit`, `sign` or `cancel` check that states an `amount_minor` now names the deal's payee as a
  `pay` check does (`counterparty.ids.payee`, the per-deal fingerprint), and is followed by its
  `counterparty_profile` companion (the payee keyed per profile, withheld from every shared copy).
  A check without an amount, a share, and a seller's `commit` or `offer` name none. In x-deal-v0
  and typed records alike.
- **The rules checker sees one merchant as one payee on these checks too:** the checked record and
  each earlier act in its history carry the `counterparty_profile` of the check they rest on, so a
  repeated booking in one deal reads as a repeat, and a merchant booked before is not new in a
  later deal.

### A close seals who ended the deal and why

- **Added (wire, additive; records sealed before re-derive unchanged).** A close's outcome and
  close records (and, in typed records, its `action-outcome/v0`) carry `actor` (`user`, `agent`
  or `platform`) and `reason` from a closed set: `completed`, `delivered_mismatch`,
  `awaiting_delivery`, `not_selected`, `user_closed`, `agent_could_not_complete`,
  `merchant_rejected`, `payment_failed`, `cancelled_in_window`, `not_delivered`, `other`. The
  reason follows from the status where it can; a close with `not_received` must state it.
- **Changed (CLI).** `deal close` with `status: not_received` and no `reason` is refused. `actor`
  defaults to `agent` (`user` for `user_closed`); a reason or actor the close contradicts is
  refused.
- The report and page read a close as an actor-marked line ("You closed this deal", "The agent
  could not complete this deal").

### Both parties' copies of a deal can carry one deal id

- **Added.** `deal open --deal-id deal-…` opens your side of a deal the other party opened, under the
  deal id their `deal open` minted. Both copies then seal one `deal_id`, so a `composed/v1` bundle
  joins them as one deal. The id is opaque (`deal-` and 16 hex), each party keeps its own per-deal
  key, and an id the profile already holds is refused. Without the flag a deal gets a new random id,
  as before.

### `verify --bundle` prints the same bytes on every run

- **Fixed.** Each `disclosures` array in `verify --bundle`'s result is now ordered by capsule id,
  then member, then status. That covers the containing bundle's array and each composed member
  bundle's. Before, a record that withheld both `agent_input` and `agent_output` listed them in
  either order, so the same bundle could print differently from run to run. The verdict was
  never affected.

## v0.1.0-rc14

### A typed deal's history acts carry the profile payee key

- **Fixed (checker input only; nothing sealed changes).** A history act in a deal recorded with typed
  records now carries `counterparty_profile`, resolved from the check its authorization rests on,
  whether that act needed an approval or not. Before, it was resolved only through an approval, so a
  repeated payment in a typed deal was not seen as a repeat by a rules checker. Untyped deals are
  unchanged.

### New deal records seal action taxonomy version 6

- **Changed (wire; records sealed before re-derive unchanged).** A deal record's
  `taxonomy_version` is now `"6"`: capsule-engine's `capsule_engine/guards/action_taxonomy.json`
  at `7ae6b95b37a3a1c4955af90f56ae4c1c0c404aa9` (file sha256
  `7563fcaeb6c192a27877fae3131bca1021526046c8976475c46eebf4fb374272`, JCS sha256
  `fc12eb90bc2b3c85bb49c4d3d7be8f384c6e0b8e189bc3b68a753a4f1af5cd94`).
- **Every `action_class` is unchanged.** Against version 5 (`e5ec2c1`), version 6 changes only
  `approver_role` (to `account_holder`) on `agreement.accept`, `marketplace.offer` and
  `marketplace.sale`. No class is renamed, removed or added, and none changes trigger class.
  - A seller's steps carry: an offer `external_commitment.other`, a commit (accepting a buyer)
    `agreement.accept`, and telling the buyer where the item is `disclosure.personal`. A rental's
    or service's commit or sign is also `agreement.accept`, whose meaning is unchanged.
  - Deal records never carry `marketplace.offer` or `marketplace.sale`.
- **Each step keeps its own version.** A record sealed under `"2"`, `"3"`, `"4"` or `"5"` re-derives
  with it.
- **Release pairing.** A rules checker compares `taxonomy_version` exactly, so a record sealing
  `"6"` needs a checker that holds version 6. The next release from `main` ships with such a
  checker; none is cut from `main` before one is available.

### A rules checker is told which side of the deal the user is on

- **Changed (checker input).** Every deal check now sends `party_role` (`buyer` or `seller`) at the
  top level of `external-check-input/v0`. It is the side the deal's opening intent sealed, or
  `buyer` when it names none, and it is always explicit, so a checker selects its rules without
  inferring the side.
- **Release pairing.** The input's top level refuses members it does not list, so a checker built
  before this change refuses every check that carries `party_role`. A release that sends it ships
  with a checker that accepts it.
- **The deal id has one source per record:** an x-deal-v0 record's `deal_id`, or a typed record's
  `chain_id`, which is exactly that value. No record carries both, and a test now holds it.

### A shared copy lists the counterparty_profile step under a neutral kind

- **Changed:** a shared copy (counterparty or adjudicator) lists every step of the log, withheld
  ones included, so a verifier can tell withheld from missing. The check's `counterparty_profile`
  companion was listed by that kind, which tells the reader the user keeps a fingerprint of
  merchants across deals. It is now listed as kind `private`, still withheld, with the line "A
  record kept for the user only (withheld)".
- **Unchanged:** the record itself, and the user's own copy, which names it as it is.

### A floor in force goes to the rules checker opened, or the check is refused

- **Fixed (fail closed).** A rules checker reads a floor only from its verified
  `commercial_bounds_opening`, and given none it finds the floor rule not applicable. When a floor
  was in force but its opening could not be built, the checker was run without it. Now:
  - `deal check` refuses before sealing anything, naming the cause and never the floor. The causes
    are: the deal's limits cannot be read, no opening on this device recomputes to the commitment,
    or the opening is not a commercial-bounds document.
  - A floor is in force when the limits in force set one, whatever can be recomputed from its
    opening.
  - With no floor in force, nothing changes.
- **Fixed: typed deals sent the wrong opening.** In a typed deal, the opening sent was the open
  step's, but a checker verifies it against the task authority it is given, which seals the floor
  under its own nonce. The two never matched, so a checker verifying the opening failed every such
  check. The task authority's own opening is now sent.

### The user's floor and limit are recognised in any written form

- **Fixed:** the check that keeps a seller's floor (`min_total_minor`) and a spending limit
  (`max_total_minor`) out of a shared copy only matched the form capsulectl writes ("$1700.00").
  It now reads money as prose writes it, and compares by value:
  - currency symbols before or after the amount (`$`, `€`, `£`, `¥` and others), ISO codes
    (`USD 1,700`, `1700 usd`, also written against the number: `USD1,700`, `1700usd`) and
    currency words (`dollars`, `euros`);
  - thousands separators as locales use them (comma, dot, space, apostrophe, no-break or thin
    space), with or without cents (`1.700,00 €`, `1'700.00 CHF`), and `k` (`$1.7k`);
  - a bare number a few words after a money word ("the lowest I'll take is 1700").

  Dates, times, ids and digests are not read as money. An amount the other party is shown anyway
  (the asked price, an offer, a payment) is still not protected.
- **Added:** a message the agent writes (`from: agent`), and, where the user sells, an agent's
  claim (which the buyer's copy opens), is refused before it is sealed when it states the floor or
  the limit in any of these forms. The refusal names the field, never the value. The user's own
  words are recorded as said.

## v0.1.0-rc13

### The payee keyed per profile, for the user's own history

- **Added (wire, additive; records sealed before re-derive unchanged).** A check that names a
  payee is now followed, before any rules checker runs, by a `counterparty_profile` record.
  - **What it holds:** only `{"fp_alg": "hmac-sha256-profile-key", "ids": {"payee": …}}`, the
    payee fingerprinted with the same construction as the per-deal fingerprints. Its key is
    derived from the profile store's own secret, so one merchant has one value across the
    profile's deals and another in any other profile.
  - **Where it points:** it names its check with an `about` ref. Each deal record's per-deal
    `counterparty` fingerprints are unchanged.
- **Never shared:** no shared copy, for the counterparty or an adjudicator, carries it; the check
  itself stays shareable.
- **The rules checker gets it:** `external-check-input/v0` gives the checker
  `record.counterparty_profile` for the check being decided, and `history[i].counterparty_profile`
  for each earlier act (from its approval's check). It is supplied beside the sealed capsule and
  never part of it. Acts checked before this release carry none.

### New deal records seal action taxonomy version 5

- **Changed (wire; records sealed before re-derive unchanged).** A deal record's
  `taxonomy_version` is now `"5"`: capsule-engine's `capsule_engine/guards/action_taxonomy.json`
  at `e5ec2c1ba84f9d9269cbb164eb6a92400e4d5d49` (file sha256
  `7d31e892cdea7352d0485c89430dba028bf02ce91fb0e932f4752c1f4066022f`, JCS sha256
  `af3a054cae880e13ce5d8d8bdaa64383e6f3af14fa2bf2ddb88aab226865b13a`).
- **Every `action_class` is unchanged.** Against version 4 (`5300e0b`), version 5 changes only
  `approver_role` (to `account_holder`) on `communication.send`, a class no deal record carries;
  no class is renamed, removed or added, and none changes trigger class.
- **Each step keeps its own version.** A record sealed under `"2"`, `"3"` or `"4"` re-derives with
  it.

### `deal checkpoint status`: a deal log's latest checkpoint, read-only

- **Added:** `capsulectl deal checkpoint status --deal ID` returns, as JSON:
  - the deal log's latest checkpoint: its `id` (the SHA-256 of the signed statement), `at`, and
    the log head it covers (`mmr_size`, `entries`, `root`);
  - the log's `log_entries`;
  - the checkpoint's witness state as stored.

  A deal with no checkpoint yet has `checkpoint` and `witness` `null`, and exits 0.
- **Read-only:** it never seals, never cuts a checkpoint and never contacts the witness, and every
  file under the store is byte-for-byte the same after it. It works on a `read_only` profile.
- **Fixed:** opening a log that does not exist could panic while cleaning up the failed open. It
  now returns the log's error.

### Where the user sells: a seller profile, an exact acceptance, and one item for several buyers

- **Added (wire, additive; every deal sealed before re-derives unchanged).** `party_role`
  (`buyer` | `seller`) on the intent, set when a deal opens and fixed; absent means buyer.
  `deliver_by` and `perform_by` obligations with a `due_by` date. A `not_selected` close.
- **A seller's floor stays private.** `min_total_minor` is never sealed. Records that state it
  seal `bounds_commitment`, a salted commitment (`sha256-jcs-nonce256`) to a
  `commercial-bounds/v0` document. Only the user's own copy carries its openings, and only the
  user's own rules checker is given the opening in force (`commercial_bounds_opening`). No
  shared copy, for any audience, carries the floor (see "Not yet"). A check below the floor asks
  (`under_floor`).
- **An exact acceptance.** `offer` is a check, allowed only where the user sells; a later offer
  supersedes the earlier one (`supersedes`). `deal note --kind acceptance` seals the buyer's
  acceptance of the exact offer by its digest. A seller's `commit` needs an acceptance of the
  latest offer with no change since, must equal that offer's amount, currency, terms and
  recourse, and cites the acceptance. A deal where the user sells must use typed records.
- **A sale is money in.** A seller's commit and offer count 0 toward any spend total.
- **What the buyer's copy shows (seller deals only).** The offer, the task authority in full,
  the acceptance, and the seller's agent's own representations to the buyer (claims, with an
  optional `class`: `condition`, `warranty`, `refund_terms`, `delivery_promise`, `other`) whose
  words hold none of the user's private details. A buyer's deal is shared exactly as before.
- **One item, several buyers.** `deal sale new` seals a sale's own log; `deal open --sale ID`
  opens a buyer's thread under it. Each thread seals the item reference and the sale's task
  authority only as per-thread salted commitments, so no digest or commitment links two buyers'
  copies; both still show the item's name and the offer, which are the seller's own listing.
  The user's own rules checker is given the plain `item_ref`.
- **Not yet:** the check that keeps the floor out of written text misses some formats
  ("$1,700.00", bare amounts); a fix follows.

### A refund's check names the payment it returns

- **Added (wire, additive; records sealed before re-derive unchanged).** A deal `check` of a
  `cancel` that returns a sealed payment (a refund) now carries `reverses_ref`: that payment's
  `action`, by its record digest, as a typed reference (`{"type": "record", "digest_alg":
  "SHA-256", "digest": …}`). It names the same payment, by the same digest, as the refund
  `action`'s `reverses` ref. A typed deal's `proposed-action/v0` carries it too.
- **Amounts unchanged:** a refund's `check` and `action` both keep `amount_minor`. A partial
  cancel's keep `cancelled_amount_minor` and name no payment. No `returned_minor` is sealed.

## v0.1.0-rc12

### New deal records seal action taxonomy version 4

- **Changed (wire; records sealed before re-derive unchanged).** A deal record's
  `taxonomy_version` is now `"4"`: capsule-engine's `capsule_engine/guards/action_taxonomy.json`
  at `5300e0b`.
  A reader that holds a later version found no class-keyed result for a record that named `"2"`.
- **Every `action_class` is unchanged.** Versions 2, 3 and 4 name the same classes. Versions 3
  and 4 add `approver_role` `account_holder`:
  - version 3 to `money.purchase`, `money.subscription`, `booking.create` and `booking.modify`;
  - version 4 to `booking.cancel`, `data.delete`, `communication.publish` and
    `disclosure.personal`.
- **Each step keeps its own version.** A record sealed under `"2"` or `"3"` re-derives with it.

### Rules a checker declares but does not measure are said quietly, not as a warning

- **Fixed:** a rules checker reports the rules its ruleset declares but it does not measure as
  `not_evaluable` with the reason `declared, not measured`. On every escalated purchase, the deal
  card listed each one as a reason to ask ("Your rules ask for approval: …: declared, not
  measured"); under an overall `not_evaluable` it would read "not fully checked".
- **Now:** such a finding is never a difference.
  - The approval text names the rules quietly beside the verdict: "3 rules are declared by the
    ruleset but not measured by this checker (r14, r24, r25)."
  - So does the user's own receipt, on the check's line. A shared copy does not name them.
  - The verdict is unchanged: an allowed purchase stays allowed.
  - Any other `not_evaluable` is still a difference, said as before.
- **Presentation goldens:** a new fixture, `7-receipt-declared-rules`.

### `doctor --check-witness` is ok only when the witness answers with success

- **Fixed:** the witness probe sent a HEAD to the endpoint and called any answer ok. A witness that
  serves no HEAD routes answered 405 and was reported `reachable: true, status_code: 405, ok: true`,
  while the first delivery could still fail.
- **Now:** the probe is a read-only GET of `<endpoint>/health`, the witness's health endpoint. It is
  ok only on a 2xx answer. Any 4xx or 5xx is `ok: false`, with an issue naming the status, the
  method and the URL.
- **Changed:** a delivery the witness answered with an error keeps the witness's HTTP status (never
  its response body), and its pending text says it: "pending: the witness answered with an error
  (HTTP 409); it is retried at every tick."

### A deal claim's words are sealed as a commitment; only whose it is shows

- **Changed (wire, additive; records sealed before re-derive unchanged).** A claim, on a `claim`
  step and in the baseline's `claims[]`, is now sealed as:
  - `text_commitment`: the claim's words, salted, like every other text a deal record carries;
  - `source_kind`: whose it is (`merchant`, `agent`, `platform`, `user` or `external`), in the
    clear;
  - optional `source_ref_commitment`: the free-form note of where it was read, committed.

  A verdict names the claims it found unverified by reference (`unverified_claims`), not by their
  words. The typed `action-evaluation/v0` does the same.
- **`source_kind` is stated by the caller.** A source that can only mean the other party
  (`counterparty`, `seller_message`, `merchant_email`) is taken as `merchant`. Any other source (a
  page, a photo, a snapshot) must state it, and a claim that leaves it open is refused, naming the
  field. The deal skill says how to choose.
- **Openings:**
  - the user's own report carries `claim_openings`;
  - an adjudicator's copy carries those of claims holding none of the user's private details;
  - a counterparty's copy shows each disclosed claim record (its `source_kind` and salted
    commitments) and no opening.
- **The profile checker:** `check_profile.py --openings=FILE` recomputes each opening against the
  sealed record. A shared vector (claim words and source note) is checked by both the Go and the
  Python implementation.

### A check carries the inputs a rules checker reads

- **Added (wire, additive):** a check sealed from this version carries scalars a rules checker
  reads. They go on the `x-deal-v0` check and the typed `proposed-action/v0`, and each is absent
  when not known:
  - `recipient_role` (`fulfilling_merchant` or `third_party`, on a share);
  - `channel` and `first_contact_channel` (channel kinds);
  - `upfront_amount_minor` (the stated deposit, never inferred);
  - `material_fields_changed` and `offer_fields_changed` (counts over two fixed field lists: against
    what was agreed, and against the keys the user's own words state, absent when they state none),
    with `material_fields_basis` and `offer_fields_basis` (each list's digest);
  - on typed records, `task_authority_ref`.

  None carries a name, a contact detail or free text. Records sealed before re-derive unchanged.
- **Added:** a typed deal's `task-authority/v0` also carries its plan: `outcome_id` (a fixed id per
  deal type, `capsulectl.deal.<purchase|rental|booking|service>/1.0.0`), its allowed actions as
  `allowed_actions`, and `preconditions: []`. The external-check input gains an optional `task_authority_record`
  member: that whole sealed record, whose SHA-256 over its JCS bytes is the check's
  `task_authority_ref` digest; its plan is at `body`.
- **The deal skill** now says to always state `recourse` (the rail and whether it is refundable) on
  a pay, and to put a deposit the checkout asks for in `terms`.

## v0.1.0-rc11

### A rules checker's answer that cannot be sealed pauses the check instead of failing it

- **Fixed:** on the second purchase in a week, a checker's finding value such as "2000 authorised
  (capture 2000); 7d total 4000 (2000 earlier)" reads as a phone number to the personal-data scan.
  `deal check` refused to seal its verdict and exited 2, with no verdict and no prompt.
- **Now:** a checker answer that cannot be sealed never fails the check. The check pauses with
  "Your rules were not checked: the rules checker's answer could not be sealed (…)", and the verdict
  seals the rules as not evaluated, with cause `unreadable`. The personal-data scan itself is
  unchanged.
- **Numeric limits and values:** a checker that reports `limit` and `value` as numbers (as the
  contract allows) is never caught this way.

### `result build` and `report build`

- `result build` seals a caller-supplied Evidence Result v0 into a jsonl profile's evidence book
  as one `evidence_result` record, after checking it against the vendored schema, recomputing its
  headline values, resolving every citation in the book, and walking each close claim's Close.
  Only UNILATERAL close claims can be sealed end to end today; see `capsulectl result build
  --help`.
- `report build` verifies a held, disclosed bundle rooted at a sealed Result v0 and renders an
  offline `report.html` (and, with `--permalink`, a viewer fragment over the same bytes), through
  the same vendored evidence-graph viewer `deal report` embeds.
- `disclose` / `permalink --attach-input-originals` (jsonl profiles): carry each published
  capsule's retained agent_input original in the bundle, in the
  `capsulectl/agent-input-originals/v1` extension, checked against the capsule's
  `agent_input_digest`. Opt-in at disclose time; the book never stores an original.
- **Changed:** `close --capsule-out`: a file already holding exactly the Close's bytes is now left
  alone, so a repeat with the same flags succeeds as a no-op; a file holding anything else is
  refused by name and nothing is overwritten. Before, any existing file at that path failed the
  command.
- **Changed:** `disclose --suppress agent_input` also withholds an agent_input original a book
  record commits as a payload (records written by a pre-release build that stored originals in
  the book); under `--payloads all` it refuses instead, since that mode cannot withhold a payload.
- **Removed:** the `--html` flag on `bundle`, `disclose` and `permalink`. It was a stub that never
  rendered a page; `report build` replaces it.

### `book` verbs read another party's evidence book through a mesh node

- **Added:** a `mesh-plugin` profile type, and read-only `book list|get|request|head` verbs that
  ask another party's evidence book through a mesh node's `evidence-request/1` tool.
  - Requests are sent and digested in canonical form, so a signed refusal binds to the request
    that was sent.
  - A refusal is accepted only if its Ed25519 signature verifies offline and it names that request.
    `--responder-key` pins its signer.
  - `book head` verifies the newest reported checkpoint's COSE statement and matches every reported
    field to it. `--responder-checkpoint-key` pins its signer.
- **The node token** is optional, redacted in `profile show`, sent only over https or to a loopback
  address, and never follows a redirect. Storage verbs refuse the new profile type.

### `book head` checks its pinned keys and requester id before sending

- **Fixed:** `--responder-checkpoint-key` is checked like `--responder-key` (a 32-byte Ed25519 key in
  hex) before anything is sent. A malformed one used to reach the node and fail only as a mismatch.
- **Changed:** a requester id the request names must be a node's full peer id (64 lowercase hex),
  not any non-empty string.

### Dependencies

- **The checkpointed-log library** moves from `github.com/action-state-group/cll-go` (an untagged
  pseudo-version) to its successor module `github.com/action-state-group/checkpointed-local-log/go`
  v0.1.0.
- **Pinned at untagged commits:** `github.com/action-state-group/agent-action-capsule/go` v0.7.0 →
  `v0.7.1-0.20261008155852-5d80d40097f6`, and `github.com/action-state-group/evidencebook` v0.1.1 →
  `v0.1.2-0.20261008160739-b970240ce88a`.

## v0.1.0-rc10

### A deal receipt's words for where the deal stands and the merchant's receipt

Consumer wording only, on the receipt page and in the emailed receipt. No record type, field or verb
changes. "receipt" is wording here, never a field, record type or key: a test keeps every deal and
external-check schema free of one.
- **An open deal** with no merchant email sealed yet reads "Open: waiting for the merchant's
  receipt." One with the merchant's email sealed reads "Open: the merchant's email is sealed; no
  close is sealed on this deal yet." Both are followed by the expected close date, as before.
- **A completed close** reads "Closed at …: matched, nothing left to match." Other outcomes are
  unchanged.
- **A merchant email sealed after the close** reads "The merchant's receipt arrived. It matches what
  you approved." or "The merchant's receipt arrived. It differs: you approved $48.00; their receipt
  says $64.00." This applies only when an approved amount is on record (not only a limit). A copy
  not confirmed by the merchant's signature says so. Shared copies keep their fixed words.
- **The user's choice on a check** reads as the option was shown: "you chose Pay anyway" over a
  finding, "you chose Hold". It used to read the option's id ("proceed", "hold").

### `deal check` runs the user's pinned rules checker and seals what it said

- **Added:** a deal profile may pin an external rules checker (`profile update --rules-checker
  FILE`): a command under a trusted plugin root, pinned by its executable's SHA-256 and,
  optionally, by the definition digest of the ruleset it must report. It is the user's policy,
  never the agent's.
  - **What it gets.** Every `deal check` runs it, from a private copy of the exact bytes it
    hashed, with one `external-check-input/v0` object on stdin
    (`skills/deal/profile/external-check-input-v0.schema.json`):
    - the capsule of the step being checked, with its disclosed record;
    - as history, the profile's sealed acts with an amount from every deal in the last 31
      days (at most 1,000), stating whether that is all of them.

    A rolling limit is evaluated over that history. A checker given too little reports it
    `not_evaluable`, and the check pauses.
  - **What it prints.** One `external-check-result/v0` object: the ruleset's id and definition
    digest, a verdict (`allow`, `deny`, `escalate` or `not_evaluable`), and its findings, each
    limit and value a number or a short line, and optionally how it reached the verdict (`tier`:
    `recomputed` or `judged`, as in Result v0; absent reads as judged, never recomputed) and
    the grade of its evidence (`grade`: `self-attested`, `witnessed` or `countersigned`, by
    reference to Result v0's Grade). The tier and grade are
    sealed as reported and named in the approval text; they change nothing. The schema ships at
    `skills/deal/profile/external-check-result-v0.schema.json`, and output outside it is
    refused.
- **One verdict, one prompt.** The checker's answer folds into the check's own differences.
  - **`deny`** is a new verdict: the card names the rule, its limit and the value, and offers
    only "Hold". An approval to proceed is refused.
  - **`escalate` and `not_evaluable`** pause.
  - **A checker that cannot answer pauses**, saying "Your rules were not checked:" and why. That
    covers a checker that changed since it was pinned, refused the record, timed out (10s by
    default, at most 60s), printed anything else, or reported another ruleset digest than the
    pinned one.
  - **No checker configured** does not pause, but the text says "Your rules were not checked:
    no rules checker configured."
- **Sealed (wire, additive).**
  - The `x-deal-v0` verdict gains `result: "deny"` and `rules`: the status, the ruleset id and
    definition digest, the checker's SHA-256, the verdict, every finding (id, verdict, limit,
    value), and `history` (how many earlier acts the checker was given, over how many days, and
    whether that is all of them). A checker that was not evaluated seals a
    `cause` token. No words the checker wrote are sealed in the clear: they reach the card,
    which is committed to.
  - Typed records: `action-evaluation/v0` gains disposition `DENY` and `rules_checks`, and
    `check-response/v0` gains `DENY`.
  - Shared copies carry none of the user's rules: the verdict record is withheld, and its
    findings read in fixed words.
- **Changed:** `approval_text` leads with the rules line and then "Before you go ahead:" in
  place of "Deal check:". The "Checked at … Stale after …" line has left the text; staleness
  stays in `checked_at`, `stale_after_minutes` and the sealed record.
- **Changed:** a card whose only finding is that the rules could not be checked offers "Pay" (or
  "Confirm", and so on), not "Pay anyway": there is nothing to override.

### A deal receipt's text is sealed with it

- **Fixed:** a deal receipt's readable text (summary lines, step lines, amounts, what was told)
  is now sealed. Writing a receipt (`deal report --html/--bundle/--email`, `bundle --deal`,
  `disclose --deal`) seals that copy's own text as a `deal_report` record on the deal's log,
  salted with a fresh nonce. The bundle's `x-deal-v0` extension only names it.
  - **What fails:** editing a line, removing the extension, or naming another record makes
    `verify --bundle` INVALID, and the page says the report did not verify.
  - **What the page says:** for a sealed copy, that it checked the text has not changed since
    it was sealed.
- **Changed (wire):**
  - The deal's log now holds a sealed report after its steps for each receipt written. A
    report that writes no file seals nothing.
  - Later copies carry every earlier report's record with its text withheld: a copy
    discloses its own text and no other copy's, and a shared copy seals the text already
    rewritten for its audience.
  - Witness coverage is counted in steps. A tick that holds the deal's steps, with only
    reports after them, still shows as witnessed, or as pending for its reason.
- Receipts written before this change verify as before, with their text reported
  `extension_unbound`.

### A deal receipt says its text is not verified; records declare their commitments' construction

- **Changed:** a deal receipt's readable text (summary lines, step lines, amounts, what was
  told) rides in the bundle's `x-deal-v0` extension, which no record seals: editing it left
  `verify --bundle` VALID and the page showing the edited text. `verify --bundle` now reports
  that extension with the finding `extension_unbound` and a note (the verdict is unchanged),
  and the receipt page says first that it does not check that text. Binding the text to a
  sealed record is planned.
- **Wire change (schema-visible, additive):** every deal record sealed from now on carries
  `commit_alg: "sha256-jcs-nonce256"` (in the `x-deal-v0` block; in a typed record's header),
  naming the construction of its `*_commitment` values: SHA-256 over the JCS of
  `{"nonce", "text"}`, a fresh 256-bit nonce per commitment, no key, recomputable only from the
  opening. Records sealed before carry none and re-derive unchanged. PROFILE.md section 4
  states the construction, its nonce and key scope, and where openings travel.

### A page is written only when it can stand behind what it shows

- **Behaviour change.** `bundle --html`, `disclose --html` and `deal report --html`
  write a page only for a bundle that `verify --bundle` calls VALID. For an INCOMPLETE
  or INVALID bundle no page is written: the command exits as `verify --bundle` would
  (3 or 1) and names the claims that did not pass. The page's viewer would otherwise
  have said "Bundle verification passed" for a bundle the CLI calls INCOMPLETE.
  `bundle` still writes the bundle (`--out`, or stdout), since `verify --bundle`
  states its verdict; `disclose` writes nothing and puts no disclosure on record when
  its page is refused.
- A page whose `report/v1` root would show fewer rows than it has (a row cites a
  record the bundle does not carry) is refused, naming how many rows it would show.
- A jsonl profile writes no page: its bundle carries its evidence book's records about
  what was sealed, not the records themselves, so a page cannot show them. Publish on
  a sqlite profile for a page; `bundle --out` still writes the bundle.
- Every page path measured on the previous release was VALID and is unchanged: deal
  receipts (own and shared copies) and the README's self-checking report.

### `profile retire NAME`: free a profile's name without touching its records

- Renames the profile to `NAME-retired-YYYYMMDD` (`-2`, `-3` … the same day), with
  its log id, keys and data directory unchanged: every record, checkpoint and witness
  receipt stays in its own log, and the retired profile still bundles and verifies
  them. Nothing is deleted, moved or re-logged. The freed name can be created anew,
  for example as a sqlite profile.
- The retired profile is not made read-only: a jsonl profile's bundle puts itself on
  record, which a read-only profile refuses.
- `--move-data NEW_DIR` also renames a jsonl profile's data directory (to a new path
  on the same filesystem), for an install that wants the old path. It refuses a sqlite
  profile, an existing path, and a key file inside the data directory.
- Anything that runs capsulectl with `--profile NAME` (a scheduled checkpoint cadence,
  a plugin) is not changed; the command says so.

### `verify --bundle` verifies composed/v1

- **Changed:** a bundle carrying a `composed/v1` extension (Evidence Bundle -01 §7.2) is now
  verified instead of being listed as uninterpreted: the composed digest, each member (with a full
  per-member assessment for carried member bundles), composition closure, joins and the
  redundancy flag. It counts toward the verdict: a failed block or an INVALID member bundle makes
  the bundle INVALID; a declared-missing member, a join that can't be derived or an unverified
  refusal signature makes it INCOMPLETE. Uses agent-action-capsule go/v0.7.0. Other extensions are
  still listed as carried but not checked.

### A spend limit binds the most a payment may take

- **Fixed: an authorization buffer evaded every limit.** A pay check carried only the expected
  charge, so a $4.54 order passed a $5 limit while the card approval let the merchant take up to
  $9.54.
- **New check field `authorized_max_minor`:** on a `pay`, the most the counterparty may take
  under the payment's authorization (a card hold, a pre-authorization with a buffer for tax
  settled later). It must be at least `amount_minor`, which stays the expected charge. The intent
  limit evaluates it, and the difference names it ("up to $9.54 may be taken (the authorized
  maximum; the expected charge is $4.54)", field `authorized_max_minor`).
- **Behaviour change, fail safe:** a pay by card, wallet, PayPal, or any rail not known to be
  holdless is refused (exit 2) unless it states `authorized_max_minor` (the same as
  `amount_minor` when no larger maximum is shown). A pay on a holdless rail (bank transfer,
  Zelle, wire, cash and the like) defaults it to the amount.
- **Sealed:** the check carries `authorized_max_minor`, and `spend_authorized_minor` beside
  `spend_minor` (which a per-action cap reads; a rolling cap sums the actions' `spend_minor`). A
  typed `proposed-action/v0` carries it, so an approval never extends to a larger maximum.
  Optional members, kept per step: records already sealed re-derive unchanged.
- **The capture:** an action that takes more than the authorized maximum differs from its
  check; one below it, though not the estimate, is what a hold is for.
- The deal skill tells the agent to read the maximum from the approval or card hold and pass it.

### A second report on a profile works after a checkpoint covers a disclosure

- **Fixed:** once a checkpoint covered a disclosure, every later `bundle` and `disclose` on that
  profile failed ("operation failed … sensitive details suppressed"). A re-run of an install, or
  any later report, hit it.
- **Wire change: what `disclose` puts on the log.** It used to append the bare digest of a
  disclosure record, kept nowhere else, which no later bundle could supply. It now seals the
  disclosure record as an ordinary `fyi` capsule (`action_id: capsulectl-disclosure`) with the
  profile's signing key, stores it, and appends its capsule id. Its input holds only ids, digests
  and labels, never disclosed payload bytes, plus a random 256-bit nonce, so the digest a later
  bundle shows cannot confirm a guess at what was disclosed.
- **A later bundle** proves an earlier disclosure capsule's place in the log but withholds its
  input, so a copy made for one party never shows what was disclosed to another. Such a bundle
  states `payloads_mode: selected`.
- **Logs written by an earlier release** that disclosed and then checkpointed cannot be bundled
  over: the record behind a bare digest is gone. The error now says what it can know, exit 2:
  "log entry N is checkpointed but not in this profile's store: either a disclosure an earlier
  release appended as a bare digest, or a lost record; no bundle over this log can include it;
  start a new profile (or log id) for new reports".

### A merchant email whose key record DNS could not reach: offer a supplied record

- **Fixed:** when DNS could not be reached, sealing a merchant email (`deal note --kind evidence
  --email`) stopped with an error that only said to retry or pass `--key-record`, and the deal
  skill never mentioned `--key-record`, so an agent stopped there.
- **The error now names the record it needed and the way on:** "could not reach DNS for the DKIM
  key record SELECTOR._domainkey.DOMAIN (…): retry, or get that TXT record another way (another
  resolver, or a lookup the user runs) and pass it with --key-record FILE; it is sealed as
  supplied, which is weaker evidence than a record read from DNS". A DMARC record it could not
  read names `--dmarc-record FILE` beside `--key-record FILE` (it used to name `--key-record`
  alone).
- **The deal skill** tells the agent to offer that way on instead of stopping. It says:
  - which record is needed, and where it can come from;
  - that the email is sealed as supplied (`key_source: supplied`), and the report and the emailed
    receipt say so;
  - that a supplied record is weaker evidence than one read from DNS.

### `verify --capsule` reads a bare capsule

- `verify --capsule` also reads a bare Agent Action Capsule as capsule-emit seals it (the
  capsule's own fields with an inline `signature` and `key_id`), with the same checks as the
  artifact.Record wrapper. The file's shape is read from its members, never guessed, and named
  in the output (`shape`); a file with the members of both shapes, or of neither, is refused.

## v0.1.0-rc9

### The checkpoint cadence: every 5m by default

- **Behaviour change for existing deal profiles.** A deal profile's witness cadence
  (its checkpoint cadence) now defaults to a tick every 5m, give or take 1m, instead
  of every 1h, give or take 10m. A profile that sets no cadence moves at upgrade; one
  that sets both `cadence.interval` and `cadence.jitter` keeps them.
- **A profile that sets `cadence.jitter` but not `cadence.interval` may now be
  refused.** Its jitter must be under half the 5m default, so under 2m30s. The error
  names both values; set `cadence.interval` (for example `1h`), or lower the jitter.
- The poll is `cll checkpoint cadence` (`deal tick` stays callable). Schedule it every
  5 minutes with `--wait-up-to 5m`. A run with no tick due publishes nothing.

### Deal checks: which agent picks pause is a pinned materiality predicate

The pinned predicate itself first shipped in v0.1.0-rc8, whose release notes did not describe
it. v0.1.0-rc9 added sealing it at a deal's opening, flagging a re-pin mid-deal, the
missing-file message, and the commitment to its name and version.

- **Behaviour change for existing deal profiles.** Which attributes the agent picked
  on its own (a size, a delivery option the user never named) pause a deal check is
  no longer compiled into capsulectl: it is a materiality predicate
  (`materiality-predicate/v0`, see `skills/deal/profile/materiality-predicate/`).
  **A profile with no predicate now pauses on every attribute the agent picked.**
  To keep the earlier behaviour, pin the example predicate, which reproduces it:
  `capsulectl --profile NAME profile update --materiality skills/deal/profile/materiality-predicate/neutral.json`.
  `capsulectl doctor --profile NAME` says which applies.
- The predicate is pinned in the profile by its digest (`materiality.digest`). A check
  refuses, sealing nothing, when the pinned file has changed; re-pin it with
  `profile update --materiality FILE`. Setting the predicate is the user's policy: a
  check takes no predicate of its own.
- A profile that names a predicate without a pinned digest (written by a build between
  the two changes) refuses checks until it is pinned the same way.
- Every check says which predicate decided (name, version and digest, or digest
  `none`) in its output, and seals it in its verdict (`materiality`, an additive
  x-deal-v0 field).
- `deal open` seals the predicate pinned when the deal opened into the baseline
  (`materiality`, additive), from the profile and never from the input. A later check
  under another predicate (a re-pin mid-deal) carries a `materiality_changed`
  difference: it shows on the card and pauses the check.
- A pinned predicate whose file is gone refuses with its own message ("materiality
  predicate file missing; re-pin it with profile update").
- Sealed records carry the predicate's digest and a commitment to its name and
  version (`label_commitment`); the user's own copy opens it (`materiality_openings`).
  A copy shared with a counterparty discloses the digest only: the name and version
  describe the user's own policy. This also fixes shared copies, which withheld every
  check step once the verdict began recording `materiality` in v0.1.0-rc8. Verdicts
  rc8 sealed with the name and version in the clear stay withheld from shared copies.
