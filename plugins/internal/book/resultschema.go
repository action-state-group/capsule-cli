package book

import (
	"github.com/action-state-group/capsule-cli/cli"
	"github.com/action-state-group/evidencebook"
)

// The Evidence Result v0 document gate -- the vendored schema, the
// cross-checks of its headline values and its canonical form -- is the
// core's (cli/result.go, capsule-cli#324), so `book result build`,
// `deal terms-result` and the core's `result build` check and canonicalize a
// Result the same way and write the same bytes for it. These are the
// plugin's names for it; the book adds the checks that need the book
// (resultbuild.go).

const (
	resultVersion    = cli.ResultVersion
	resultRecordType = evidencebook.RecordTypeEvidenceResult

	claimTypeRequirement = cli.ClaimTypeRequirement
	claimTypeReconcile   = cli.ClaimTypeReconcile
	claimTypeClose       = cli.ClaimTypeClose
)

// resultClaim is one claim as the core's gate reads it.
type resultClaim = cli.ResultClaim

// resultDocument is what the gate establishes about a Result: the contracts
// its claims name, the one contract it reports (if any), and its claims.
type resultDocument struct {
	contracts   []string
	contractRef string
	claims      []resultClaim
}

func documentOf(checked cli.CheckedResult) resultDocument {
	return resultDocument{contracts: checked.Contracts, contractRef: checked.ContractRef, claims: checked.Claims}
}

func checkResultSchema(doc map[string]interface{}) error { return cli.CheckResultSchema(doc) }

func crossCheckResult(doc map[string]interface{}, contract string) (resultDocument, error) {
	checked, err := cli.CrossCheckResult(doc, contract)
	return documentOf(checked), err
}

// checkResult is the whole document gate: schema, then cross-checks.
func checkResult(doc map[string]interface{}, contract string) (resultDocument, error) {
	checked, err := cli.CheckResult(doc, contract)
	return documentOf(checked), err
}

// statementDigest is the document's canonical (JCS) bytes and their
// SHA-256: the bytes `--out` writes, and the bytes the header carries as its
// statement.
func statementDigest(doc map[string]interface{}) ([]byte, string, error) {
	return cli.ResultStatement(doc)
}

func decodeResultDocument(raw []byte) (map[string]interface{}, error) { return cli.DecodeResult(raw) }

// writeOutput is cli.WriteOutput for a file written after a book record is
// sealed.
func writeOutput(flag, path string, data []byte) error {
	return cli.WriteOutput(flag, path, data, "the record is sealed: see record_id")
}
