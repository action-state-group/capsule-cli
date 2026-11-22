# parent-log-witness/v1 cases

Eight cases from agent-action-capsule's `vectors/bundle/parent-log-witness/vectors.json`
(last changed at agent-action-capsule commit 8f169a6543f513970315738df29c4436b199bd4f; file
sha256 prefix ef4536ebb9d41ea3): a witnessed whole and a witnessed partial chain, a chain with no
witness directory, a bundle whose checkpoint is unsigned, and four broken chains. Each case keeps
its bundle, whether the vector gave a witness directory, AAC's expected verdict and the expected
`parent_witness` result. `cli/verify_parent_witness_test.go` checks that `capsulectl verify
--bundle` reports the same status, findings, extent and receipts (capsule-cli#608).
