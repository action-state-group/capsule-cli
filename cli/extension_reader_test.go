package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeReader is a scripted ExtensionReader that records how the core called it.
type fakeReader struct {
	name        string
	witness     *aacbundle.ClaimResult
	read        map[string]fakeRead
	finish      []map[string]any
	finishClaim []aacbundle.ClaimResult
	reads       []string
	finishes    int
	order       []string
	sawAnchored bool
}

type fakeRead struct {
	entry  map[string]any
	claims []aacbundle.ClaimResult
	nilOK  bool // read true with no entry
}

func (f *fakeReader) Name() string { return f.name }
func (f *fakeReader) Witnesses(ctx *ExtensionContext) (aacbundle.ClaimResult, []map[string]any, bool) {
	f.order = append(f.order, "witnesses")
	if f.witness == nil {
		return aacbundle.ClaimResult{}, nil, false
	}
	return *f.witness, []map[string]any{}, true
}
func (f *fakeReader) Read(ctx *ExtensionContext, kind string) (map[string]any, []aacbundle.ClaimResult, bool) {
	f.order = append(f.order, "read:"+kind)
	f.reads = append(f.reads, kind)
	f.sawAnchored = ctx.Anchored
	r, ok := f.read[kind]
	if !ok {
		return nil, nil, false
	}
	if r.nilOK {
		return nil, nil, true
	}
	entry := map[string]any{}
	for k, v := range r.entry {
		entry[k] = v
	}
	return entry, r.claims, true
}
func (f *fakeReader) Finish(ctx *ExtensionContext) ([]map[string]any, []aacbundle.ClaimResult) {
	f.order = append(f.order, "finish")
	f.finishes++
	return f.finish, f.finishClaim
}

func bundleWithExtensions(t *testing.T, kinds ...string) map[string]interface{} {
	t.Helper()
	path := producedBundle(t, func(v map[string]interface{}) {
		exts := map[string]interface{}{}
		for _, k := range kinds {
			exts[k] = map[string]interface{}{}
		}
		v["extensions"] = exts
	})
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	value, err := DecodeBundleJSON(raw)
	require.NoError(t, err)
	return value
}

func assessWith(t *testing.T, value map[string]interface{}, readers ...*fakeReader) (string, string, map[string]any, error) {
	t.Helper()
	i := 0
	return AssessBundle(value, nil, func() ExtensionReader {
		r := readers[min(i, len(readers)-1)]
		i++
		return r
	})
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// With no reader, or one that reads nothing and adds nothing, the assessment is
// the core's own, byte for byte, on a plain bundle, one with unread extensions
// and a composed one (capsule-cli#428 step 5).
func TestAssessBundleWithNoReaderIsTheCoresOwn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	composed := composedVectors(t)[0].Container
	for name, value := range map[string]map[string]interface{}{
		"plain":       bundleWithExtensions(t),
		"unread":      bundleWithExtensions(t, "x-vendor/1", "x-vendor/2"),
		"composed":    composed,
		"parent kind": bundleWithExtensions(t, aacbundle.ParentLogWitnessKind),
	} {
		t.Run(name, func(t *testing.T) {
			verdict, page, report := bundleAssessment(value, &assessEnv{})
			v2, p2, r2, err := AssessBundle(value, nil, nil)
			require.NoError(t, err)
			assert.Equal(t, verdict, v2)
			assert.Equal(t, page, p2)
			assert.Equal(t, marshal(t, report), marshal(t, r2))
			v3, p3, r3, err := assessWith(t, value, &fakeReader{name: "noop"})
			require.NoError(t, err)
			assert.Equal(t, verdict, v3)
			assert.Equal(t, page, p3)
			assert.Equal(t, marshal(t, report), marshal(t, r3), "a reader that reads nothing changes nothing")
		})
	}
}

// The reader is asked for extensions in report order, only those AAC lists
// uninterpreted that the core does not read, after Witnesses and before Finish;
// its entry replaces the listing with the core's kind and its own name, and
// its claims gate.
func TestAssessBundleCallsAReaderForTheExtensionsTheCoreLeaves(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	value := bundleWithExtensions(t, "x-vendor/1", "x-vendor/2")
	fail := aacbundle.ClaimResult{Status: "fail", Findings: []string{"vendor_rule_failed"}}
	reader := &fakeReader{name: "vendor-reader", read: map[string]fakeRead{
		"x-vendor/1": {entry: map[string]any{"status": "pass", "kind": "ignored"}},
		"x-vendor/2": {entry: map[string]any{"status": "fail"}, claims: []aacbundle.ClaimResult{fail}},
	}}
	nilVerdict, _, _ := bundleAssessment(value, &assessEnv{})
	verdict, _, report, err := assessWith(t, value, reader)
	require.NoError(t, err)
	assert.Equal(t, "VALID", nilVerdict)
	assert.Equal(t, "INVALID", verdict, "a reader's failing claim fails the verdict")
	assert.Equal(t, []string{"x-vendor/1", "x-vendor/2"}, reader.reads, "report order; the core's own kind is never offered")
	assert.Equal(t, []string{"witnesses", "read:x-vendor/1", "read:x-vendor/2", "finish"}, reader.order)
	byKind := map[string]map[string]any{}
	for _, x := range report["extensions"].([]map[string]any) {
		byKind[x["kind"].(string)] = x
	}
	assert.Equal(t, "pass", byKind["x-vendor/1"]["status"])
	assert.Equal(t, "vendor-reader", byKind["x-vendor/1"]["read_by"])
	assert.Equal(t, "x-vendor/1", byKind["x-vendor/1"]["kind"], "the core sets the kind")
}

// An extension the core reads itself is never offered to a reader.
func TestAssessBundleNeverOffersACoreReadKind(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	value := bundleWithExtensions(t, "x-vendor/1", aacbundle.ParentLogWitnessKind)
	reader := &fakeReader{name: "vendor-reader", read: map[string]fakeRead{
		aacbundle.ParentLogWitnessKind: {entry: map[string]any{"status": "pass"}},
		"x-vendor/1":                   {entry: map[string]any{"status": "pass"}},
	}}
	_, _, report, err := assessWith(t, value, reader)
	require.NoError(t, err)
	assert.Equal(t, []string{"x-vendor/1"}, reader.reads)
	for _, x := range report["extensions"].([]map[string]any) {
		if x["kind"] == aacbundle.ParentLogWitnessKind {
			assert.Contains(t, x, "parent_witness", "the core's own reading stays")
			assert.NotContains(t, x, "read_by")
		}
	}
}

// A reader adds to the core's result and never weakens it.
func TestAssessBundleAReaderNeverWeakensTheCoresResult(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pass := aacbundle.ClaimResult{Status: "pass"}
	// A bundle the core finds INVALID stays INVALID whatever the reader passes.
	value := bundleWithExtensions(t, "x-vendor/1")
	value["records"].([]interface{})[0].(map[string]interface{})["capsule_id"] = strings.Repeat("0", 64)
	coreVerdict, _, _ := bundleAssessment(value, &assessEnv{})
	require.NotEqual(t, "VALID", coreVerdict)
	reader := &fakeReader{name: "r", witness: &pass, read: map[string]fakeRead{"x-vendor/1": {entry: map[string]any{"status": "pass"}, claims: []aacbundle.ClaimResult{pass}}}, finishClaim: []aacbundle.ClaimResult{pass}}
	verdict, _, _, err := assessWith(t, value, reader)
	require.NoError(t, err)
	assert.Equal(t, coreVerdict, verdict)
	// Its witness claim is reported beside the core's, which is untouched.
	plain := bundleWithExtensions(t)
	_, _, coreReport := bundleAssessment(plain, &assessEnv{})
	reader = &fakeReader{name: "r", witness: &pass}
	_, _, withReader, err := assessWith(t, plain, reader)
	require.NoError(t, err)
	assert.Equal(t, marshal(t, coreReport["witnesses"]), marshal(t, withReader["witnesses"]), "the core's witness claim is untouched")
	rw := withReader["reader_witnesses"].(map[string]any)
	assert.Equal(t, "r", rw["read_by"])
	assert.Equal(t, "pass", rw["status"])
	assert.NotContains(t, coreReport, "reader_witnesses", "no hook, no member")
	// A withheld reader witness claim does not gate; a failing one does.
	for status, want := range map[string]string{"withheld": "VALID", "fail": "INVALID"} {
		v, _, _, err := assessWith(t, plain, &fakeReader{name: "r", witness: &aacbundle.ClaimResult{Status: status}})
		require.NoError(t, err)
		assert.Equal(t, want, v, status)
	}
	// Anchored is true for Read once Witnesses returned ok.
	anchoredReader := &fakeReader{name: "r", witness: &pass}
	_, _, _, err = assessWith(t, bundleWithExtensions(t, "x-vendor/1"), anchoredReader)
	require.NoError(t, err)
	assert.True(t, anchoredReader.sawAnchored)
}

// Reader bugs are errors to the caller, never claims in a verdict.
func TestAssessBundleRefusesReaderBugs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	value := bundleWithExtensions(t, "x-vendor/1")
	_, _, _, err := assessWith(t, value, &fakeReader{name: "r", read: map[string]fakeRead{"x-vendor/1": {nilOK: true}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "returned no entry")
	// Finish may not use the kind of an extension the core reads, or no kind.
	core := bundleWithExtensions(t, aacbundle.ParentLogWitnessKind)
	_, _, _, err = assessWith(t, core, &fakeReader{name: "r", finish: []map[string]any{{"kind": aacbundle.ParentLogWitnessKind, "status": "pass"}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "which the core reads")
	// Whether or not the bundle carries the kind.
	for _, kind := range []string{aacbundle.ParentLogWitnessKind, aacbundle.ComposedKind} {
		_, _, _, err = assessWith(t, value, &fakeReader{name: "r", finish: []map[string]any{{"kind": kind, "status": "pass"}}})
		require.Error(t, err, kind)
	}
	_, _, _, err = assessWith(t, value, &fakeReader{name: "r", finish: []map[string]any{{"status": "pass"}}})
	require.Error(t, err)
	// A Finish entry of a new kind is appended after the extensions with the reader's name.
	reader := &fakeReader{name: "r", finish: []map[string]any{{"kind": "x-lost/1", "status": "fail"}}, finishClaim: []aacbundle.ClaimResult{{Status: "fail", Findings: []string{"section_lost"}}}}
	verdict, _, report, err := assessWith(t, value, reader)
	require.NoError(t, err)
	assert.Equal(t, "INVALID", verdict)
	extensions := report["extensions"].([]map[string]any)
	last := extensions[len(extensions)-1]
	assert.Equal(t, "x-lost/1", last["kind"])
	assert.Equal(t, "r", last["read_by"])
	// Finish claims are reported with the reader's name even with no entry.
	_, _, report, err = assessWith(t, value, &fakeReader{name: "r", finishClaim: []aacbundle.ClaimResult{{Status: "fail", Findings: []string{"section_lost"}}}})
	require.NoError(t, err)
	assert.Equal(t, []map[string]any{{"read_by": "r", "status": "fail", "findings": []string{"section_lost"}}}, report["reader_claims"])
	assert.Equal(t, []string{"r fail section_lost"}, UnmetClaims(report)[len(UnmetClaims(report))-1:], "a refusal names the reader's claim")
	// A claim status the core does not know is INCOMPLETE, as for any claim.
	verdict, _, _, err = assessWith(t, value, &fakeReader{name: "r", finishClaim: []aacbundle.ClaimResult{{Status: "odd"}}})
	require.NoError(t, err)
	assert.Equal(t, "INCOMPLETE", verdict)
}

// Each composed member gets a reader of its own, built before the container's
// (the core reads members while it reads the container's extensions), with the
// run's directory; a failing member claim fails the container, a withheld one
// does not, and a member with no extensions still gets Finish.
func TestAssessBundleReaderReachesComposedMembers(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	directory, err := LoadWitnessDirectory(writeDirectory(t, rawKeyRow(testWitness, public)))
	require.NoError(t, err)

	build := func(first, second map[string]interface{}) map[string]interface{} {
		container := composedVectors(t)[0].Container
		block := container["extensions"].(map[string]interface{})["composed/v1"].(map[string]interface{})
		members := block["members"].([]interface{})
		require.Len(t, members, 2)
		for i, bundle := range []map[string]interface{}{first, second} {
			m := members[i].(map[string]interface{})
			m["bundle"] = bundle
			var err error
			m["digest"], err = aacbundle.BundleDigest(bundle)
			require.NoError(t, err)
		}
		block["joins"] = []interface{}{}
		var err error
		block["composed_digest"], err = aacbundle.ComposedDigest(block)
		require.NoError(t, err)
		return container
	}
	run := func(container map[string]interface{}, memberClaim func(index int) []aacbundle.ClaimResult) (string, string, []*fakeReader, []any, map[string]any) {
		var built []*fakeReader
		var dirs []any
		verdict, page, report, err := AssessBundle(container, &directory, func() ExtensionReader {
			index := len(built)
			r := &fakeReader{name: "member-reader", read: map[string]fakeRead{"x-vendor/1": {entry: map[string]any{"status": "pass"}, claims: memberClaim(index)}}}
			built = append(built, r)
			return &dirCapture{fakeReader: r, dirs: &dirs}
		})
		require.NoError(t, err)
		return verdict, page, built, dirs, report
	}
	two := build(bundleWithExtensions(t, "x-vendor/1"), bundleWithExtensions(t, "x-vendor/1"))
	baseline, basePage, _ := bundleAssessment(two, &assessEnv{})
	require.NotEqual(t, "INVALID", baseline)

	// Passing claims: the verdicts are the core's own; two members, two
	// independent readers, plus the container's, each with the run's directory.
	verdict, page, built, dirs, report := run(two, func(int) []aacbundle.ClaimResult { return []aacbundle.ClaimResult{{Status: "pass"}} })
	assert.Equal(t, baseline, verdict)
	assert.Equal(t, basePage, page)
	require.Len(t, built, 3, "one reader per member and one for the container")
	assert.Equal(t, []string{"x-vendor/1"}, built[0].reads)
	assert.Equal(t, []string{"x-vendor/1"}, built[1].reads, "the same kind, read independently")
	assert.Empty(t, built[2].reads, "the container itself carries only composed/v1")
	for _, d := range dirs {
		assert.Same(t, &directory, d, "the run's directory reaches every reader")
	}
	for _, r := range built {
		assert.Equal(t, 1, r.finishes)
	}
	for _, m := range composedMembers(t, report) {
		extensions := m["bundle"].(map[string]any)["extensions"].([]map[string]any)
		assert.Equal(t, "member-reader", extensions[0]["read_by"])
	}

	// A withheld member claim does not fail the container; a failing one in
	// the second member alone does, in both verdicts.
	verdict, page, _, _, withheldReport := run(two, func(int) []aacbundle.ClaimResult { return []aacbundle.ClaimResult{{Status: "withheld"}} })
	assert.NotEqual(t, "INVALID", verdict)
	assert.NotEqual(t, "INVALID", page)
	for i, m := range composedMembers(t, withheldReport) {
		memberReport := m["bundle"].(map[string]any)
		assert.Equal(t, "INCOMPLETE", memberReport["verdict"], "member %d: a withheld reader claim is not a pass", i)
		assert.Equal(t, []map[string]any{{"read_by": "member-reader", "status": "withheld", "findings": []string{}}}, memberReport["reader_claims"])
	}
	for i, m := range composedMembers(t, report) {
		assert.Equal(t, "VALID", m["bundle"].(map[string]any)["verdict"], "member %d with passing claims keeps the core's verdict", i)
	}
	verdict, page, _, _, _ = run(two, func(i int) []aacbundle.ClaimResult {
		if i == 1 {
			return []aacbundle.ClaimResult{{Status: "fail", Findings: []string{"member_rule_failed"}}}
		}
		return nil
	})
	assert.Equal(t, "INVALID", verdict)
	assert.Equal(t, "INVALID", page)

	// A member with no extensions still gets Finish, and its failure fails the parent.
	mixed := build(bundleWithExtensions(t, "x-vendor/1"), bundleWithExtensions(t))
	var finishing []*fakeReader
	verdict, _, _, err = AssessBundle(mixed, &directory, func() ExtensionReader {
		r := &fakeReader{name: "member-reader"}
		if len(finishing) == 1 {
			r.finishClaim = []aacbundle.ClaimResult{{Status: "fail", Findings: []string{"member_section_lost"}}}
		}
		finishing = append(finishing, r)
		return r
	})
	require.NoError(t, err)
	require.Len(t, finishing, 3)
	assert.Equal(t, 1, finishing[1].finishes, "the member with no extensions got Finish")
	assert.Equal(t, "INVALID", verdict)
}

// dirCapture records the directory each hook was given.
type dirCapture struct {
	*fakeReader
	dirs *[]any
}

func (d *dirCapture) Read(ctx *ExtensionContext, kind string) (map[string]any, []aacbundle.ClaimResult, bool) {
	*d.dirs = append(*d.dirs, ctx.Directory)
	return d.fakeReader.Read(ctx, kind)
}

// The core's own paths never pass a reader: every non-test call of the seam's
// entry points outside their definitions passes the nil literal, so there is no
// registration, discovery or hook that installs one.
func TestCoreNeverPassesAReader(t *testing.T) {
	calls := regexp.MustCompile(`\b(AssessBundle|PageGateWith|VerifyBundleFileWith)\(([^)]*)\)`)
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		for i, line := range strings.Split(string(raw), "\n") {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "//") || strings.HasPrefix(trim, "func ") {
				continue
			}
			for _, m := range calls.FindAllStringSubmatch(line, -1) {
				checked++
				args := strings.Split(m[2], ",")
				last := strings.TrimSpace(args[len(args)-1])
				// nil, or the forwarding parameter of the seam's own entry points.
				assert.Contains(t, []string{"nil", "newReader"}, last, "%s:%d passes a reader", f, i+1)
			}
		}
	}
	assert.GreaterOrEqual(t, checked, 3, "the scan found the core's calls")
	// Nothing outside the seam builds a factory or a reader.
	built := regexp.MustCompile(`ExtensionReaderFactory\(|func\(\) ExtensionReader`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "extension_reader.go" {
			continue
		}
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		assert.False(t, built.Match(raw), "%s builds a reader factory", f)
	}
}

// A reader that changes what it was handed cannot change what the core found:
// the whole report is the core's own, on every kind of bundle, whatever the
// reader edits in its context (the bundle value, the result's digest and
// findings, the receipts, the extensions).
func TestAssessBundleAReaderCannotChangeTheCoresFindings(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	values := map[string]map[string]interface{}{"plain": bundleWithExtensions(t, "x-vendor/1")}
	for i, v := range composedVectors(t) {
		values["composed "+string(rune('0'+i))] = v.Container
	}
	for name, value := range values {
		t.Run(name, func(t *testing.T) {
			coreVerdict, corePage, coreReport := bundleAssessment(value, &assessEnv{})
			coreJSON := marshal(t, coreReport)
			meddler := &meddlingReader{}
			verdict, page, report, err := AssessBundle(value, nil, func() ExtensionReader { return meddler })
			require.NoError(t, err)
			assert.True(t, meddler.ran)
			assert.Equal(t, coreVerdict, verdict)
			assert.Equal(t, corePage, page)
			assert.Equal(t, coreJSON, marshal(t, report), "the whole report is the core's own")
			assert.Equal(t, coreJSON, marshal(t, func() map[string]any { _, _, r := bundleAssessment(value, &assessEnv{}); return r }()), "and the caller's value is untouched")
		})
	}
}

type meddlingReader struct {
	fakeReader
	ran bool
}

func (m *meddlingReader) Name() string { return "meddler" }
func (m *meddlingReader) Witnesses(ctx *ExtensionContext) (aacbundle.ClaimResult, []map[string]any, bool) {
	m.ran = true
	for i := range ctx.Result.Extensions {
		ctx.Result.Extensions[i].Composed = nil
		ctx.Result.Extensions[i].Status = "pass"
	}
	if ctx.Result.BundleDigest != nil {
		*ctx.Result.BundleDigest = "replacement"
	}
	for _, c := range []*aacbundle.ClaimResult{&ctx.Result.GraphClosure, &ctx.Result.IntervalCoverage, &ctx.Result.PerRecordMembership, &ctx.Witnesses} {
		c.Status = "pass"
		for i := range c.Findings {
			c.Findings[i] = "edited"
		}
	}
	for _, r := range ctx.Receipts {
		r["status"] = "pass"
	}
	delete(ctx.Value, "checkpoint")
	delete(ctx.Value, "records")
	if exts, ok := ctx.Value["extensions"].(map[string]interface{}); ok {
		delete(exts, "composed/v1")
	}
	for k := range ctx.SignatureStates {
		ctx.SignatureStates[k] = "authored"
	}
	return aacbundle.ClaimResult{}, nil, false
}

type panicReader struct {
	fakeReader
	at string
}

func (p *panicReader) Name() string {
	if p.at == "Name" {
		panic("name boom")
	}
	return "panicky"
}
func (p *panicReader) Witnesses(ctx *ExtensionContext) (aacbundle.ClaimResult, []map[string]any, bool) {
	if p.at == "Witnesses" {
		panic("witnesses boom")
	}
	return aacbundle.ClaimResult{}, nil, false
}
func (p *panicReader) Read(ctx *ExtensionContext, kind string) (map[string]any, []aacbundle.ClaimResult, bool) {
	if p.at == "Read" {
		panic("read boom")
	}
	return nil, nil, false
}
func (p *panicReader) Finish(ctx *ExtensionContext) ([]map[string]any, []aacbundle.ClaimResult) {
	if p.at == "Finish" {
		panic("finish boom")
	}
	return nil, nil
}

// A panic in the factory or a hook is a reader bug returned to the caller on
// every entry point, not a crash.
func TestAssessBundleReaderPanicsAreCallerErrors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	value := bundleWithExtensions(t, "x-vendor/1")
	path := producedBundle(t, func(v map[string]interface{}) {
		v["extensions"] = map[string]interface{}{"x-vendor/1": map[string]interface{}{}}
	})
	for _, at := range []string{"factory", "Name", "Witnesses", "Read", "Finish"} {
		factory := func() ExtensionReader {
			if at == "factory" {
				panic("factory boom")
			}
			return &panicReader{at: at}
		}
		_, _, _, err := AssessBundle(value, nil, factory)
		require.Error(t, err, at)
		assert.Contains(t, err.Error(), "boom", at)
		assert.Error(t, PageGateWith(value, factory), at)
		cmd := &cobra.Command{}
		cmd.Flags().String("witness-directory", "", "")
		var out strings.Builder
		cmd.SetOut(&out)
		require.Error(t, VerifyBundleFileWith(cmd, path, factory), at)
		assert.Empty(t, out.String(), "a reader bug prints no report")
	}
}

// A page is refused for a failing reader claim, and the refusal names it.
func TestPageGateWithNamesAFailingReaderClaim(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	value := bundleWithExtensions(t)
	require.NoError(t, PageGate(value))
	err := PageGateWith(value, func() ExtensionReader {
		return &fakeReader{name: "vendor-reader", finishClaim: []aacbundle.ClaimResult{{Status: "fail", Findings: []string{"report_rule_failed"}}}}
	})
	require.ErrorIs(t, err, ErrBundleInvalid)
	assert.Contains(t, err.Error(), "vendor-reader fail report_rule_failed")
}

// The exported narration is verify's own, so a plugin's verb says the same.
func TestVerifyNarrationIsExportedForAPluginsVerb(t *testing.T) {
	result := map[string]any{"extensions": []any{map[string]any{"kind": "x-vendor/1", "status": "uninterpreted"}}}
	for _, err := range []error{nil, ErrPartial, ErrBundleInvalid} {
		assert.Equal(t, verifyNarration(&cobra.Command{}, result, err), VerifyNarration(&cobra.Command{}, result, err))
	}
	assert.Contains(t, VerifyNarration(&cobra.Command{}, result, nil).Say, "Not checked here: x-vendor/1")
}

// composedMembers are the member reports of a container report.
func composedMembers(t *testing.T, report map[string]any) []map[string]any {
	t.Helper()
	for _, x := range report["extensions"].([]map[string]any) {
		if x["kind"] == "composed/v1" {
			return x["composed"].(map[string]any)["members"].([]map[string]any)
		}
	}
	t.Fatal("no composed/v1 entry")
	return nil
}
