# Presentation goldens

Nine synthetic bundles and what capsulectl's pages show a reader for them, taken with
the viewer first vendored at agent-action-capsule `5d80d40`; the pages and snapshots
are now those of the viewer `cli/assets/README.md` pins. `TestPresentationGoldens`
(`cli/presentation_golden_test.go`) holds every later viewer to them:

- **Without Chrome:** it re-emits each page from its `bundle.json` the way capsulectl
  does and checks the page gate's decision. While the vendored viewer is unchanged,
  the result must equal `page.html` byte for byte.
- **With `CAPSULECTL_CHROME` set:** it also opens each page headless, with no network:
  Chrome resolves no host name or IP literal, and a request for anything outside the
  page fails the test, whether the page, one of its iframes or workers, or a WebSocket
  makes it. At 1280px its DOM must match `snapshot.json`: the verification state, the
  views, the evidence identifiers, and the words in order. Layout is not compared.
- **With `CAPSULECTL_PRESENTATION_PINNED=1` as well** (CI only), the browser must be the
  Chrome `known-overflow.json` names. At 390px, 1280px and in print, each page is held
  to that file:
  - an overflow it does not list fails;
  - a listed view wider than recorded by more than `tolerance_px` fails;
  - a listed view that no longer overflows fails until it is removed from the list.

  Widths depend on the browser and the fonts, so CI pins both: Chrome for Testing at
  the named version, and DejaVu as the only fonts (`fonts.conf`). Elsewhere widths are
  logged, not compared.

Every fixture is synthetic: example operators, example cases, example criteria,
generated keys. `scripts/presentation-goldens/build-fixtures.sh` rebuilds them all
through the real CLI. Run it only to change a fixture on purpose.

| Fixture | What it is | How the page is emitted | Page |
| --- | --- | --- | --- |
| `1-rules-comparison` | a `report/v1` root, three rule rows (same / different / same) citing their sealed records; record type `example.rules_compare/v0`, a neutral stand-in | `disclose --root … --html` | written |
| `2-unilateral-receipt` | a deal receipt (`x-deal-v0`), audience `keep`, from the synthetic retail-checkout demo | `deal report --html` | written |
| `3-bilateral-composed` | `composed/v1` of a deal's two copies: the buyer asks itself and the seller for their copies over the evidence-request carrier and composes the recorded answers. The container's root is the composition record on the buyer's compositions log for the deal, under its checkpoint, and each member carries its responder's checkpoint, so it verifies VALID. The copies join on the deal id (a pre-agreed identifier), declared and derived `agree` | `deal compose`, then `presentation build --bundle` | written |
| `4-monthly-outcome` | a month of judged support conversations: a Result v0 root citing 32 `evaluation-report/v1` records over 8 days, opted into the outcome card (calendar, drill-down) | `book report build --card outcome` | written |
| `5-monthly-compliance` | a month of judged sessions against an example obligations pack, opted into the EU AI Act obligations card | `book report build --card obligation` | written |
| `6-generic-fallback` | a plain record with no report and no aggregate: the viewer's fallback | `disclose --root … --html` | written |
| `6-generic-fallback-unknown-extension` | fixture 6's bundle with an extension no reader knows (`x-example-unknown/v1`) | the same gate and viewer, in the test | written |
| `7-receipt-declared-rules` | fixture 2's deal receipt with a pinned stub rules checker that allows the purchase and reports three rules its ruleset declares but does not measure (`declared, not measured`); the check line says them quietly beside "no differences" | `deal report --html` | written |
| `8-seller-buyer-copy` | a deal where the user sells (`party_role: seller`, typed records): the agent's statements of condition (which the user ordered, so a `represent` check passes) and refund terms (a `represent` check the user approved in their words), an offer, the buyer's recorded acceptance and the commit; the buyer's shared copy opens the agent's statements, labelled by class and checked against their sealed commitments, and none of the user's own | `deal report --share counterparty --html` (its `bundle.json` is the bundle the page carries) | written |
| `9-seller-later-evidence` | a seller's own Receipt (`x-deal-v0`, typed records, audience `keep`) after the close, from the deal plugin's `testdata/deal/seller-later-evidence`: its condition and refund-terms statements go through `represent` checks as in fixture 8, the sale closes with the delivery's due date carried, then two later `evidence` records chain `confirms` to the close. The carrier's scan is checked and resolves the due date (`resolves_obligation`); the buyer's refund request (source `buyer_refund_request`, so its line says what it is) is a claim, so unverified, about the agent's refund-terms statement. Each shows under the close with its tier (Seller PRD §5.4, exit criterion 9) | `deal report --html` | written |

## What these fixtures found

- **Fixture 3 got no page at first.** It was then agent-action-capsule's composed/v1
  vector with one mismatch, and like every published composed/v1 vector it carries no
  checkpoint, so `verify --bundle` called it INCOMPLETE and the page gate refused it.
  No command rendered a bundle file, and the viewer had no composed/v1 view. Since
  `presentation build --bundle` (capsule-cli#183) and capsule-viewer's composed/v1
  section, the fixture is rebuilt with `deal compose`, whose container is checkpointed,
  and gets a page (capsule-cli#149). The page shows each member, the join (declared and
  derived) and the agreement. `deal compose` derives `agree`, `one_sided` or `unjoined`
  and never `mismatch`, so the mismatch case has no golden until agent-action-capsule
  publishes a checkpointed composed/v1 vector (agent-action-capsule#374). The page's own
  composed/v1 check reads "withheld", and each member's interval coverage and membership
  read `pass (checkpoint_unverified)`: the browser verifier checks neither the members'
  response signatures nor their checkpoints, which `verify --bundle` does.
- **Fixtures 4 and 5 seal their Result v0 as the payload of a SQLite record.**
  `result build` seals only into a jsonl evidence book, and when these fixtures were
  taken that book's bundles left out producer signatures, so `verify --bundle` called
  them INCOMPLETE and `report build` wrote no page for them (capsule-cli#148). Since
  evidencebook v0.7.0 they carry them, verify VALID and get a page.
- **Fixtures 1, 4 and 5 cited their records with `digest_alg` `sha256`.** A closure
  follows a capsule reference only when it reads `SHA-256`, so they verified VALID
  without their citations being walked. They were rebuilt to cite with `SHA-256`
  (capsule-cli#352), and `TestPresentationGoldensCiteWhatTheyCarry` holds every
  golden to citing that way and carrying each record it cites.
- **Fixture 6-unknown-extension renders exactly as fixture 6** except for the bundle
  digest on the verification page: the viewer ignores an extension it does not know.
- **One page overflows** in CI's pinned environment: the composed page (fixture 3) is
  2267px wide at 1280px and 2091px in print, because its members table holds unbroken
  64-hex ids (capsule-viewer#57); it fits at 390px. It was 2091px at 1280px too until
  capsule-viewer's page shell gave every page a 60rem frame (capsule-viewer#60).
  `known-overflow.json` lists those two views. No other page overflows. Four did when the fixtures were taken: the
  compliance page (793px), the fallback page (686px, fixtures 6 and 6-unknown-extension),
  the rules comparison (682px) and the outcome report (421px, fixed in
  agent-action-capsule#323; capsule-cli#150).
- **The outcome card counts its criteria**: a pack with four criteria reads "all four
  criteria met or not applicable". It said "all nine" whatever the count until
  agent-action-capsule#323 (capsule-cli#147).

## The presentation pass

`TestPresentationPass` and its siblings (`cli/presentation_pass_test.go`, CI job
`presentation-pass`) drive the same bundles through `presentation list` and `presentation build`,
the one registry, shell and builder capsulectl has, with no plugin installed:

- **One page per (bundle, audience), for audiences `*` and `counterparty`.** The html page is
  capsule-viewer's offline packaging of the bundle as given (`emitter.BuildOfflineHTML`),
  so no bundle kind has a renderer of its own.
- **Refused packagings.** `presentation build` does not build fragment or embedded, and static is
  unavailable: the test asserts each refusal for every bundle.
- **Views agree.** In Chrome, each page's semantic snapshot is the same at 390px, at 1280px, in
  print and for both audiences. The snapshot reads the DOM, whatever CSS shows or hides. The
  vendored runtime builds the same DOM at every width and medium, so this check catches only a
  script that changes the content by viewport.
- **Flips.** Wording (an installed module's wording pack, the one way a page gets one), the pack's
  locale and depth (`--depth L0`/`L1`/`L2`) each change the page's bytes. None changes the bundle
  bytes it carries, its evidence identifiers or its verification state. `presentation build` has
  no theme input, so the theme is not flipped. Whether a flip changes what a reader sees is not
  checked: the built-in modules do not read the wording pack, and the runtime does not read its
  locale.
- **Failing bundles.** A record edited after sealing gets no page, and `list`
  resolves nothing. A page that carries the bundle anyway shows the refusal and nothing else
  (`refusal-snapshot.json`).

What `presentation build` shows, against these goldens (built through other commands):

| Fixture | Golden reproduced? | Why not |
| --- | --- | --- |
| `1-rules-comparison` | yes | |
| `2-unilateral-receipt` | no: `build-snapshot.json` | The golden is `deal report --html`, with the deal view (`capsulectl.deal-view/v0`). `presentation list` names the deal view, but `presentation build` does not carry it, so the page falls back to `aac.builtin.no-aggregate/v0`. The page has no `data-sealed="x-deal-v0"` and shows the no-aggregate notice, in about a fifth of the golden's words. The evidence identifiers and `data-verify` are the golden's. |
| `3-bilateral-composed` | yes | |
| `4-monthly-outcome` | yes, less the card | `book report build` stamps `report-card/v1` into the bundle, and `presentation build` packages the bundle as given. Two things differ: the bundle digest, and the card's extension row (`report-card/v1 covered Integrity verified; meaning not interpreted by this viewer`). The test derives build's snapshot from the golden by exactly those two changes. |
| `5-monthly-compliance` | yes, less the card | As fixture 4. |
| `6-generic-fallback` | yes | |
| `6-generic-fallback-unknown-extension` | yes | The extension row reads "Integrity verified; meaning not interpreted by this viewer", which the test asserts. |
| `7-receipt-declared-rules` | no: `build-snapshot.json` | As fixture 2. |
| `8-seller-buyer-copy` | no: `build-snapshot.json` | As fixture 2. |
| `9-seller-later-evidence` | no: `build-snapshot.json` | As fixture 2. |

Two gaps are pinned so they fail once they close:

- **The deal view.** `presentation list` offers it and `presentation build` does not carry it
  (`TestPresentationPassDealViewNotCarried`).
- **A record signed by another key than the one it names.** `verify --bundle` calls it INVALID
  (`producer_signature_invalid`), so build writes no page. The page's own gate (the
  vendored viewer, and agent-action-capsule's Go resolver that `list` uses) checks each record's
  identity, not its producer signature. So `list` resolves the bundle, and a page carrying it
  says it verified (`TestPresentationPassWrongSigner`).

`CAPSULECTL_UPDATE_PRESENTATION_PASS=1` rewrites `build-snapshot.json` and
`refusal-snapshot.json`.
