# Presentation goldens

Six synthetic bundles and what capsulectl's pages show a reader for them, taken with
the viewer vendored at agent-action-capsule `5d80d40` (`evidence-graph.iife.js`
sha256 `008d4c2c…090cdd`). `TestPresentationGoldens`
(`internal/cli/presentation_golden_test.go`) holds every later viewer to them:

- **Without Chrome:** it re-emits each page from its `bundle.json` the way capsulectl
  does and checks the page gate's decision. While the vendored viewer is unchanged,
  the result must equal `page.html` byte for byte.
- **With `CAPSULECTL_CHROME` set:** it also opens each page headless. At 1280px its
  DOM must match `snapshot.json`: the verification state, the views, the evidence
  identifiers, and the words in order. Layout is not compared. At 390px, 1280px and in
  print, the page must not be wider than its viewport, except for the views
  `known-overflow.json` lists.

Every fixture is synthetic: example operators, example cases, example criteria,
generated keys. `scripts/presentation-goldens/build-fixtures.sh` rebuilds them all
through the real CLI. Run it only to change a fixture on purpose.

| Fixture | What it is | How the page is emitted | Page |
| --- | --- | --- | --- |
| `1-rules-comparison` | a `report/v1` root, three rule rows (same / different / same) citing their sealed records; record type `example.rules_compare/v0`, a neutral stand-in | `disclose --root … --html` | written |
| `2-unilateral-receipt` | a deal receipt (`x-deal-v0`), audience `keep`, from the synthetic retail-checkout demo | `deal report --html` | written |
| `3-bilateral-composed` | `composed/v1`, two responders joined on a pre-agreed identifier, one mismatch (their transcript digests differ; declared and derived `mismatch`), built with agent-action-capsule's own composed/v1 vector generator and its published test seeds | none | **refused** |
| `4-monthly-outcome` | a month of judged support conversations: a Result v0 root citing 32 `evaluation-report/v1` records over 8 days, opted into the outcome card (calendar, drill-down) | `report build --card outcome` | written |
| `5-monthly-compliance` | a month of judged sessions against an example obligations pack, opted into the EU AI Act obligations card | `report build --card obligation` | written |
| `6-generic-fallback` | a plain record with no report and no aggregate: the viewer's fallback | `disclose --root … --html` | written |
| `6-generic-fallback-unknown-extension` | fixture 6's bundle with an extension no reader knows (`x-example-unknown/v1`) | the same gate and viewer, in the test | written |

## What these fixtures found

- **Fixture 3 gets no page.** No capsulectl command renders a bundle file to a page.
  The page gate refuses this bundle, as it refuses every published composed/v1 vector:
  `verify --bundle` calls it INCOMPLETE because it carries no checkpoint. The vendored
  viewer has no composed/v1 view either. The fixture is kept so a composed view, once
  added, has a bundle to render.
- **Fixtures 4 and 5 seal their Result v0 as the payload of a SQLite record.**
  `result build` seals only into a jsonl evidence book. That book's bundles leave out
  producer signatures, so `verify --bundle` calls them INCOMPLETE and `report build`
  writes no page for them.
- **Fixture 6-unknown-extension renders exactly as fixture 6** except for the bundle
  digest on the verification page: the viewer ignores an extension it does not know.
- **Five pages overflow a 390px viewport** (`known-overflow.json`). Nothing overflows
  at 1280px or in print.
- **The outcome card says "all nine criteria met"** for a pack with four criteria.
  The snapshot records the text as the viewer prints it today.
