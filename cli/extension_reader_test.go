package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
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
	value := bundleWithExtensions(t, "x-vendor/1", "x-vendor/2", aacbundle.ParentLogWitnessKind)
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
	assert.Equal(t, "uninterpreted", byKind[aacbundle.ParentLogWitnessKind]["status"], "a core-read kind stays the core's")
	assert.NotContains(t, byKind[aacbundle.ParentLogWitnessKind], "read_by")
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
	verdict, _, report, err := assessWith(t, value, reader)
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
	_ = report
	// A withheld reader witness claim does not gate; a failing one does.
	for status, want := range map[string]string{"withheld": "VALID", "fail": "INVALID"} {
		_, _, _, err := assessWith(t, plain, &fakeReader{name: "r", witness: &aacbundle.ClaimResult{Status: status}})
		require.NoError(t, err)
		v, _, _, _ := assessWith(t, plain, &fakeReader{name: "r", witness: &aacbundle.ClaimResult{Status: status}})
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
	// A claim status the core does not know is INCOMPLETE, as for any claim.
	verdict, _, _, err = assessWith(t, value, &fakeReader{name: "r", finishClaim: []aacbundle.ClaimResult{{Status: "odd"}}})
	require.NoError(t, err)
	assert.Equal(t, "INCOMPLETE", verdict)
}

// Each composed member gets a reader of its own, with the run's directory;
// its claims fail the parent, and a member with no extensions still gets Finish.
func TestAssessBundleReaderReachesComposedMembers(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	container := composedVectors(t)[0].Container
	block := container["extensions"].(map[string]interface{})["composed/v1"].(map[string]interface{})
	members := block["members"].([]interface{})
	require.Len(t, members, 2)
	withExtension := bundleWithExtensions(t, "x-vendor/1")
	without := bundleWithExtensions(t)
	for i, bundle := range []map[string]interface{}{withExtension, without} {
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

	var built []*fakeReader
	newFactory := func() ExtensionReaderFactory {
		built = nil
		return func() ExtensionReader {
			r := &fakeReader{name: "member-reader", read: map[string]fakeRead{"x-vendor/1": {entry: map[string]any{"status": "pass"}}}}
			// Only the third reader built (the without-extension member's) fails in Finish.
			if len(built) == 2 {
				r.finishClaim = []aacbundle.ClaimResult{{Status: "fail", Findings: []string{"member_section_lost"}}}
			}
			built = append(built, r)
			return r
		}
	}
	_, _, report, err := AssessBundle(container, nil, newFactory())
	require.NoError(t, err)
	require.Len(t, built, 3, "one reader for the container and one for each member")
	assert.Equal(t, 1, built[0].finishes, "the container's reader finished once")
	assert.Equal(t, []string{"x-vendor/1"}, built[1].reads, "the member with the extension was read")
	assert.Empty(t, built[2].reads)
	assert.Equal(t, 1, built[2].finishes, "a member with no extensions still gets Finish")
	var composed map[string]any
	for _, x := range report["extensions"].([]map[string]any) {
		if x["kind"] == "composed/v1" {
			composed = x["composed"].(map[string]any)
		}
	}
	memberReport := composed["members"].([]map[string]any)[0]["bundle"].(map[string]any)
	assert.Equal(t, "member-reader", memberReport["extensions"].([]map[string]any)[0]["read_by"])
	verdict, page, _, _ := AssessBundle(container, nil, newFactory())
	assert.Equal(t, "INVALID", verdict, "a failing member claim fails the container")
	assert.Equal(t, "INVALID", page)
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
