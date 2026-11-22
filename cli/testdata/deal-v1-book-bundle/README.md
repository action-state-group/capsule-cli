# x-deal-v1 deal-book bundle (capsule-cli#604)

`bundle.json` is the Evidence Bundle the deal-book library's `Book.Bundle`
writes for a real x-deal-v1 deal book, with the holder's standalone report
beside it (`deal report --bundle`, evidencebook#626 B4c), as written by
evidencebook branch `claude/v1-report-bundle` at 2824aba8 on 2026-11-22 (test
`TestAV1ReportBundleVerifiesAndWritesNothing`, with the bundle file copied out
of its temp dir). The test keys are throwaway. Its one step record has no
`evidence_record_header`; the header is its disclosed `agent_input`.

To regenerate, copy the bundle file that test writes over `bundle.json`.

The bytes are neutral to the verifier: public keys and digests only, no
private seed. The generator stays in the book library's repository, not here.
