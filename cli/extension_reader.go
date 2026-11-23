package cli

import (
	"errors"
	"fmt"
	"reflect"
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

// ExtensionContext is what the core has computed when it calls a reader. The
// core has finished its own reading by then and hands over copies of the slices
// it reports; every field is read-only for the reader (Value is the caller's
// own bundle, which the reader must not edit).
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

// coreOwnedKinds are the kinds no reader may supply an entry for, whether or not
// the bundle carries them: the core's own (coreReadKinds) and the ones AAC's
// verifier interprets.
var coreOwnedKinds = []string{aacbundle.ParentLogWitnessKind, aacbundle.ComposedKind}

// assessEnv carries what one assessment needs through composed members: the
// witness directory, the optional reader factory, and the reader bugs found
// (a refused entry), which AssessBundle returns as its error.
type assessEnv struct {
	directory   *witness.Directory
	newReader   ExtensionReaderFactory
	readerError error
}

func (e *assessEnv) reader() (reader ExtensionReader) {
	if e == nil || e.newReader == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			e.fail("the factory panicked: %v", r)
			reader = nil
		}
	}()
	return e.newReader()
}

// guard runs one reader call; a panic is a reader bug returned to the caller,
// not a crash of the verification.
func (e *assessEnv) guard(what string, call func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			e.fail("%s panicked: %v", what, r)
			ok = false
		}
	}()
	call()
	return true
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

// applyReader runs the reader over a bundle the core has assessed: it returns
// the extension entries with the reader's replacements and additions, the
// claims with the reader's, the reader's witness claim (nil when it added
// none) and every reader claim, reported with the reader's name. The context
// carries copies of what the core reported, so nothing the reader does changes
// the core's result.
func applyReader(env *assessEnv, reader ExtensionReader, ctx *ExtensionContext, listed []aacbundle.ExtensionResult, entries []map[string]any, claims []aacbundle.ClaimResult) ([]map[string]any, []aacbundle.ClaimResult, map[string]any, []map[string]any) {
	var name string
	if !env.guard("Name", func() { name = reader.Name() }) {
		return entries, claims, nil, nil
	}
	// The reader gets its own copy of everything the core reported or still
	// reads (the member bundle a composed container checks after this call
	// included), so nothing it does can change what the core found.
	ctx.Value = deepCopy(ctx.Value).(map[string]interface{})
	ctx.Result = deepCopy(ctx.Result).(aacbundle.VerificationResult)
	ctx.Witnesses = deepCopy(ctx.Witnesses).(aacbundle.ClaimResult)
	ctx.Receipts = deepCopy(ctx.Receipts).([]map[string]any)
	ctx.SignatureStates = deepCopy(ctx.SignatureStates).(map[string]string)
	var reported []map[string]any
	add := func(from []aacbundle.ClaimResult) []map[string]any {
		out := readerClaims(name, from, &claims)
		reported = append(reported, out...)
		return out
	}
	var witnesses map[string]any
	var witnessClaim aacbundle.ClaimResult
	var receipts []map[string]any
	var ok bool
	if env.guard("Witnesses", func() { witnessClaim, receipts, ok = reader.Witnesses(ctx) }) && ok {
		ctx.Anchored = true
		witnesses = map[string]any{"read_by": name, "status": witnessClaim.Status, "findings": nonNilStrings(witnessClaim.Findings), "receipts": receipts}
		// As for the checkpoint's own receipts, only a failing claim changes the verdict.
		if witnessClaim.Status == "fail" {
			claims = append(claims, witnessClaim)
			reported = append(reported, map[string]any{"read_by": name, "status": witnessClaim.Status, "findings": nonNilStrings(witnessClaim.Findings)})
		}
	}
	for i, x := range listed {
		if !readsKind(x.Kind, x.Status) {
			continue
		}
		var entry map[string]any
		var readClaims []aacbundle.ClaimResult
		var read bool
		if !env.guard("Read("+x.Kind+")", func() { entry, readClaims, read = reader.Read(ctx, x.Kind) }) || !read {
			continue
		}
		if entry == nil {
			env.fail("%s read %s and returned no entry", name, x.Kind)
			continue
		}
		entry["kind"], entry["read_by"] = x.Kind, name
		entry["claims"] = add(readClaims)
		entries[i] = entry
	}
	var finished []map[string]any
	var finishClaims []aacbundle.ClaimResult
	if env.guard("Finish", func() { finished, finishClaims = reader.Finish(ctx) }) {
		present := map[string]bool{}
		for _, x := range listed {
			if !readsKind(x.Kind, x.Status) {
				present[x.Kind] = true
			}
		}
		for _, entry := range finished {
			kind, _ := entry["kind"].(string)
			if kind == "" || present[kind] || slices.Contains(coreOwnedKinds, kind) {
				env.fail("%s finished with an entry for kind %q, which the core reads or no kind", name, kind)
				continue
			}
			entry["read_by"] = name
			entries = append(entries, entry)
		}
		add(finishClaims)
	}
	return entries, claims, witnesses, reported
}

// deepCopy is v with every map, slice, pointer and interface value copied, so
// the copy shares no memory with v (unexported struct fields keep their
// shallow copy).
func deepCopy(v any) any {
	if v == nil {
		return nil
	}
	return deepCopyValue(reflect.ValueOf(v)).Interface()
}

func deepCopyValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(deepCopyValue(v.Elem()))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(deepCopyValue(v.Elem()))
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for _, k := range v.MapKeys() {
			out.SetMapIndex(k, deepCopyValue(v.MapIndex(k)))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(deepCopyValue(v.Index(i)))
		}
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if out.Field(i).CanSet() {
				out.Field(i).Set(deepCopyValue(v.Field(i)))
			}
		}
		return out
	default:
		return v
	}
}

// readerClaims folds a reader's claims into the verdict and reports them with
// the reader's name.
func readerClaims(name string, claims []aacbundle.ClaimResult, into *[]aacbundle.ClaimResult) []map[string]any {
	reported := make([]map[string]any, 0, len(claims))
	for _, c := range claims {
		*into = append(*into, c)
		reported = append(reported, map[string]any{"read_by": name, "status": c.Status, "findings": nonNilStrings(c.Findings)})
	}
	return reported
}
