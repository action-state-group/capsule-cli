package cli

import (
	"errors"
	"fmt"
	"slices"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/witness"
)

// ExtensionReader interprets, for a caller's own verb, the extensions this
// package leaves uninterpreted (capsule-cli#428 step 5): a plugin that defines
// an extension reads it here, with the core's assessor around it, instead of
// copying that assessor. The rule is that a reader can add to the core's
// result and never weaken it: a witness claim it supplies is reported beside
// the core's and gates only when it fails, its entries are for kinds the core
// does not read, and every entry and claim names the reader (read_by), so a
// plugin verb's output never looks core-checked.
//
// The core never picks a reader up on its own: `verify --bundle`,
// BundleVerdict, PageGate and page writing pass a nil factory. There is no
// registration, discovery or environment hook; a reader runs only inside a
// caller's own verb. The exported types carry agent-action-capsule Go types,
// so a caller's agent-action-capsule version must match this package's.
type ExtensionReader interface {
	// Name is the reader's own name, reported as read_by on what it supplies.
	Name() string
	// Witnesses runs once before the extensions are read. A true ok adds the
	// claim and receipts beside the core's witness claim (reader_witnesses):
	// the core's claim and its gating stay as they are, and the reader's gates
	// only when it fails. ExtensionContext.Anchored is true afterwards.
	Witnesses(ctx *ExtensionContext) (claim aacbundle.ClaimResult, receipts []map[string]any, ok bool)
	// Read runs for each extension AAC lists uninterpreted that the core does
	// not read itself, in report order. read false leaves the core's
	// uninterpreted entry. read true needs a non-nil entry, which replaces it
	// (the core sets its kind and read_by); the claims join the verdict.
	Read(ctx *ExtensionContext, kind string) (entry map[string]any, claims []aacbundle.ClaimResult, read bool)
	// Finish runs once after the extensions, for what no extension triggers
	// (a section a copy lost). Its entries are appended and its claims join the
	// verdict; an entry may not use the kind of an extension the core reads.
	Finish(ctx *ExtensionContext) (entries []map[string]any, claims []aacbundle.ClaimResult)
}

// ExtensionReaderFactory builds a reader for one assessed bundle: the
// top-level bundle and each composed member get their own, so a reader keeps
// its state per bundle and the core keeps none of it.
type ExtensionReaderFactory func() ExtensionReader

// ExtensionContext is what the core has computed when it calls a reader.
type ExtensionContext struct {
	Value           map[string]interface{}
	Result          aacbundle.VerificationResult
	SignatureStates map[string]string
	Directory       *witness.Directory
	// Statement is the verified checkpoint statement, nil when it did not verify.
	Statement []byte
	// Witnesses and Receipts are the core's own witness claim and receipts.
	Witnesses aacbundle.ClaimResult
	Receipts  []map[string]any
	// Anchored is true after the reader's Witnesses returned ok.
	Anchored bool
}

// coreReadKinds are extension kinds the core reads itself although AAC's plain
// verifier lists them uninterpreted; they never reach a reader.
var coreReadKinds = []string{aacbundle.ParentLogWitnessKind}

// assessEnv carries what one assessment needs through composed members: the
// witness directory, the optional reader factory, and the reader bugs found
// (a refused entry), which AssessBundle returns as its error.
type assessEnv struct {
	directory   *witness.Directory
	newReader   ExtensionReaderFactory
	readerError error
}

func (e *assessEnv) reader() ExtensionReader {
	if e == nil || e.newReader == nil {
		return nil
	}
	return e.newReader()
}

func (e *assessEnv) fail(format string, args ...any) {
	e.readerError = errors.Join(e.readerError, fmt.Errorf("extension reader: "+format, args...))
}

// AssessBundle is the verdict, the page verdict and the report `verify --bundle`
// prints for an Evidence Bundle value, with an optional factory of readers for
// the extensions the core does not read. A nil factory is exactly the core's
// own assessment. A non-nil error is a reader bug (an entry for a kind the core
// reads, a read without an entry): the other results must not be used.
func AssessBundle(value map[string]interface{}, directory *witness.Directory, newReader ExtensionReaderFactory) (verdict, pageVerdict string, report map[string]any, err error) {
	env := &assessEnv{directory: directory, newReader: newReader}
	verdict, pageVerdict, report = bundleAssessment(value, env)
	return verdict, pageVerdict, report, env.readerError
}

// readsKind says whether a reader may be asked for an extension: AAC lists it
// uninterpreted and the core does not read it.
func readsKind(kind, status string) bool {
	return status == "uninterpreted" && !slices.Contains(coreReadKinds, kind)
}

// readerClaims folds a reader's claims into the verdict and reports them with
// the reader's name.
func readerClaims(reader ExtensionReader, claims []aacbundle.ClaimResult, into *[]aacbundle.ClaimResult) []map[string]any {
	reported := make([]map[string]any, 0, len(claims))
	for _, c := range claims {
		*into = append(*into, c)
		reported = append(reported, map[string]any{"read_by": reader.Name(), "status": c.Status, "findings": nonNilStrings(c.Findings)})
	}
	return reported
}
