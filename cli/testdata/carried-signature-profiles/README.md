# Carried signature profile cases

43 of the 45 object cases of agent-action-capsule's
`vectors/bundle/composed-signature-profiles/vectors.json` at commit
1dbfadb3881406e7a9603f3d27d89e1be7da91b4 (sha256 of that file in `cases.json`, `source_sha256`). The two
left out are responses carrying `responder`, which AAC reads in none of its profiles and this CLI
reads as an evidence-interop envelope (`InteropResponseProfile`). The six cases marked
`lenient_ed25519_verifies` (a small-order key and an identity R in each profile) verify under
plain Ed25519 and must be `invalid` under the strict rule. `cli/carried_signatures_test.go` checks
that `verify --bundle`'s signature checks give AAC's expected state and profile for each case.
