# x-deal-v1 deal-book bundle (capsule-cli#604)

`bundle.json` is the Evidence Bundle the deal-book library's `Book.Bundle`
writes for a real x-deal-v1 deal book, with the holder's standalone report
beside it (`deal report --bundle`, evidencebook#626 B4c), as written by
evidencebook main at 160dabf78d82d7569f167e8bd99577b633feeee1 (evidencebook#734,
merged 2026-11-22; test `TestAV1ReportBundleVerifiesAndWritesNothing`, with the
bundle file copied out of its temp dir). The bytes were first written on that PR's branch
(`claude/v1-report-bundle` at 2824aba8). The test keys are throwaway, so the
merged commit's test writes a bundle with the same 178 JSON paths but different
keys and digests, not the same bytes. Its one
step record has no `evidence_record_header`; the header is its disclosed `agent_input`.

To regenerate, copy the bundle file that test writes over `bundle.json`.

The bytes are neutral to the verifier: public keys and digests only, no
private seed. The generator stays in the book library's repository, not here.
