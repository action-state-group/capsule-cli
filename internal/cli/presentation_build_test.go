package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/emitter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The bundle every build here starts from: a deal's VALID receipt (a
// presentation golden), whose root matches none of the specific built-ins.
const presentationBuildBundle = "testdata/presentation/2-unilateral-receipt/bundle.json"

// exampleDealManifest is a specific module that can stand beside the
// built-ins: it forbids the profile tokens the specific built-ins require,
// so no descriptor matches it and one of them (section 4.5).
func exampleDealManifest(scriptSHA256 string) map[string]any {
	return map[string]any{
		"spec_version": "aac.presentation-manifest/v0", "id": "org.example.deal-view/v0",
		"presentation_api": "aac.presentation-api/v0", "runtime_min": "0.1.0",
		"trust_class": "trusted-executable", "requires": map[string]any{"bundle_kind": "evidence-bundle/v2"},
		"forbids":   map[string]any{"profiles": []any{"spec_version:report/v1", "result_version:evidence-result-v0", "spec_version:evaluation-summary/v1"}},
		"audiences": []any{"*"}, "formats": []any{"html"}, "fallback": false, "priority": 1,
		"executable": map[string]any{"carrier": "module-slot", "script_sha256": scriptSHA256,
			"style_sha256": []any{hexSHA256(fixtureStyle)}, "wording_sha256": hexSHA256(fixtureWording)},
	}
}

// exampleDealScript registers the module at load, as a module-slot script
// must, and renders one marked paragraph. Its in-code manifest cannot carry
// its own script's digest, so that one member is a stand-in.
func exampleDealScript(t *testing.T) []byte {
	t.Helper()
	manifest, err := json.Marshal(exampleDealManifest(strings.Repeat("0", 64)))
	require.NoError(t, err)
	return []byte(`EvidenceGraph.registerPresentation({
  manifest: ` + string(manifest) + `,
  canRender: () => true,
  buildModel: (context) => ({ records: context.records.length }),
  render(model, host) {
    const p = document.createElement("p");
    p.id = "example-deal-view";
    p.textContent = "Rendered by the example module over " + model.records + " records";
    host.L0.append(p);
  },
});
`)
}

func exampleDealFixture(t *testing.T) presentationFixture {
	script := exampleDealScript(t)
	p := moduleSlotFixture()
	p.manifest = exampleDealManifest(hexSHA256(script))
	p.files["view.js"] = script
	p.entries[0]["sha256"] = hexSHA256(script)
	return p
}

// sha256CSPSource is the base64 SHA-256 a page's CSP lists for an inline
// script or stylesheet.
func sha256CSPSource(b []byte) string {
	sum := sha256.Sum256(b)
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

func presentationOut(t *testing.T) string {
	return filepath.Join(t.TempDir(), "page.html")
}

type buildOutput struct {
	Page           string           `json:"page"`
	Out            string           `json:"out"`
	Format         string           `json:"format"`
	Audience       string           `json:"audience"`
	BundleDigest   string           `json:"bundle_digest"`
	Runtime        map[string]any   `json:"runtime"`
	Modules        []map[string]any `json:"modules"`
	NotIncluded    []map[string]any `json:"not_included"`
	PluginsRefused []map[string]any `json:"plugins_refused"`
	Refusal        map[string]any   `json:"refusal"`
}

func decodeBuild(t *testing.T, out string) buildOutput {
	t.Helper()
	var b buildOutput
	require.NoError(t, json.Unmarshal([]byte(out), &b), out)
	return b
}

// With no plugin module the page is exactly the default evidence page.
func TestPresentationBuildWithoutModulesWritesTheDefaultPage(t *testing.T) {
	pluginRoot(t)
	out := presentationOut(t)
	stdout, err := invoke(t, "", "presentation", "build", "--bundle", presentationBuildBundle, "--out", out)
	require.NoError(t, err)
	b := decodeBuild(t, stdout)
	assert.Equal(t, "written", b.Page)
	assert.Empty(t, b.Modules)
	raw, err := os.ReadFile(presentationBuildBundle)
	require.NoError(t, err)
	value, err := decodeBundleJSON(raw)
	require.NoError(t, err)
	want, err := emitter.EmitEvidenceGraphHTML(value, evidenceGraphIIFE)
	require.NoError(t, err)
	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, want, string(got))
}

// A plugin's module goes into the page's module slots, pinned in its CSP,
// and the result names it; in Chrome the page's resolver selects it.
func TestPresentationBuildCarriesAPluginsModule(t *testing.T) {
	root := pluginRoot(t)
	trustPresentationsIn(t, root)
	fixture := exampleDealFixture(t)
	presentationPlugin(t, root, "dealview", fixture)
	out := presentationOut(t)
	stdout, err := invoke(t, "", "presentation", "build", "--bundle", presentationBuildBundle, "--out", out)
	require.NoError(t, err)
	b := decodeBuild(t, stdout)
	assert.Equal(t, "written", b.Page)
	assert.Equal(t, out, b.Out)
	assert.Equal(t, map[string]any{"presentation_apis": []any{"aac.presentation-api/v0"}, "version": "0.1.0"}, b.Runtime)
	script := fixture.files["view.js"]
	assert.Equal(t, []map[string]any{{"id": "org.example.deal-view/v0", "plugin": "dealview",
		"script_sha256": hexSHA256(script), "style_sha256": []any{hexSHA256(fixtureStyle)}, "wording": "passed"}}, b.Modules)
	page, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Contains(t, string(page), string(script))
	assert.Contains(t, string(page), sha256CSPSource(script))
	assert.Contains(t, string(page), sha256CSPSource(fixtureStyle))
	assert.Contains(t, string(page), `"sha256":"`+hexSHA256(fixtureWording)+`"`, "the module's wording pack is the page's")

	binary := os.Getenv("CAPSULECTL_CHROME")
	if binary == "" {
		t.Skip("CAPSULECTL_CHROME is not set")
	}
	opened := startHeadlessChrome(t, binary).open(t, "file://"+out, presentationViews[1])
	var text string
	opened.eval(t, `(document.getElementById("example-deal-view") || {}).textContent || ""`, &text)
	assert.Regexp(t, `^Rendered by the example module over [1-9][0-9]* records$`, text)
}

// The page's runtime would refuse the module, so no page is written and
// the refusal names the module, its API and runtime_min, and the runtime.
func TestPresentationBuildRefusesAModuleThePagesRuntimeWouldRefuse(t *testing.T) {
	for name, tc := range map[string]struct{ api, min, reason string }{
		"api":     {"aac.presentation-api/v1", "0.1.0", "presentation_api_unsupported"},
		"runtime": {"aac.presentation-api/v0", "0.2.0", "runtime_too_old"},
	} {
		t.Run(name, func(t *testing.T) {
			root := pluginRoot(t)
			trustPresentationsIn(t, root)
			fixture := exampleDealFixture(t)
			fixture.manifest["presentation_api"], fixture.manifest["runtime_min"] = tc.api, tc.min
			presentationPlugin(t, root, "dealview", fixture)
			out := presentationOut(t)
			stdout, err := invoke(t, "", "presentation", "build", "--bundle", presentationBuildBundle, "--out", out)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInput)
			b := decodeBuild(t, stdout)
			assert.Equal(t, "not written", b.Page)
			assert.Equal(t, tc.reason, b.Refusal["reason"])
			assert.Equal(t, "org.example.deal-view/v0", b.Refusal["module"])
			assert.Equal(t, tc.api, b.Refusal["presentation_api"])
			assert.Equal(t, tc.min, b.Refusal["runtime_min"])
			assert.Equal(t, b.Runtime, b.Refusal["runtime"])
			assert.Contains(t, err.Error(), "aac.presentation-api/v0 and is version 0.1.0")
			assert.NoFileExists(t, out)
		})
	}
}

// A module that a built-in's descriptor could also match is ambiguous: the
// page could not choose, so no page is written.
func TestPresentationBuildRefusesAnAmbiguousModule(t *testing.T) {
	root := pluginRoot(t)
	trustPresentationsIn(t, root)
	fixture := exampleDealFixture(t)
	delete(fixture.manifest, "forbids")
	presentationPlugin(t, root, "dealview", fixture)
	out := presentationOut(t)
	stdout, err := invoke(t, "", "presentation", "build", "--bundle", presentationBuildBundle, "--out", out)
	require.ErrorIs(t, err, ErrInput)
	b := decodeBuild(t, stdout)
	assert.Equal(t, "ambiguous", b.Refusal["reason"])
	assert.Equal(t, "specific", b.Refusal["tier"])
	assert.Contains(t, b.Refusal["modules"], "org.example.deal-view/v0")
	assert.NoFileExists(t, out)
}

// Two plugins offering one id are refused; so is a manifest that can never
// match.
func TestPresentationBuildRefusesADuplicateIdAndADeadManifest(t *testing.T) {
	root := pluginRoot(t)
	trustPresentationsIn(t, root)
	presentationPlugin(t, filepath.Join(root), "dealview", exampleDealFixture(t))
	other := t.TempDir()
	require.NoError(t, os.Chmod(other, 0o755))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root+string(os.PathListSeparator)+other)
	trustPresentationsIn(t, filepath.Dir(root))
	presentationPlugin(t, other, "dealview2", exampleDealFixture(t))
	stdout, err := invoke(t, "", "presentation", "build", "--bundle", presentationBuildBundle, "--out", presentationOut(t))
	require.ErrorIs(t, err, ErrInput)
	assert.Equal(t, "duplicate_id", decodeBuild(t, stdout).Refusal["reason"])

	dead := exampleDealManifest(strings.Repeat("0", 64))
	dead["requires"] = map[string]any{"bundle_kind": "evidence-bundle/v2", "profiles": []any{"spec_version:report/v1"}}
	err = staticPresentationCheck([]map[string]interface{}{dead})
	var r *presentationBuildRefusal
	require.ErrorAs(t, err, &r)
	assert.Equal(t, "dead_manifest", r.Reason)
}

// The two checks over AAC's own vectors (schemas/examples/
// presentation-manifest-v0 at the pinned commit): the built-ins and the
// examples are each unambiguous; the negative pair is ambiguous though
// their priorities differ; removing any one of the four forbids that keep
// the built-ins apart makes a pair; the unsupported-API example is refused
// as presentation_api_unsupported, and a copy needing a later runtime as
// runtime_too_old.
func TestPresentationChecksAgreeWithTheContractsVectors(t *testing.T) {
	load := func(name string) map[string]interface{} {
		raw, err := os.ReadFile(filepath.Join("testdata/presentation-manifests", name+".json"))
		require.NoError(t, err)
		var m map[string]interface{}
		require.NoError(t, json.Unmarshal(raw, &m))
		return m
	}
	builtins, err := builtinPresentationManifests()
	require.NoError(t, err)
	require.Len(t, builtins, 6)
	assert.NoError(t, staticPresentationCheck(builtins))
	var examples []map[string]interface{}
	for _, name := range []string{"example-composition-aware", "example-declarative-rules", "example-generic-fallback", "example-unilateral"} {
		examples = append(examples, load(name))
	}
	assert.NoError(t, staticPresentationCheck(examples))

	err = staticPresentationCheck([]map[string]interface{}{load("neg-ambiguous-pair/a"), load("neg-ambiguous-pair/b")})
	var r *presentationBuildRefusal
	require.ErrorAs(t, err, &r)
	assert.Equal(t, "ambiguous", r.Reason)

	for _, drop := range []struct{ id, field, token string }{
		{"aac.builtin.result-compliance/v0", "extensions", "outcome-report/v1"},
		{"aac.builtin.result/v0", "extensions", "outcome-report/v1"},
		{"aac.builtin.result/v0", "extensions", "eu-ai-act-compliance/v1"},
		{"aac.builtin.evaluation-summary-graph/v0", "profiles", "result_version:evidence-result-v0"},
		{"aac.builtin.result-outcome-report/v0", "profiles", "spec_version:report/v1"},
	} {
		mutated, err := builtinPresentationManifests()
		require.NoError(t, err)
		removed := false
		for _, m := range mutated {
			if m["id"] != drop.id {
				continue
			}
			forbids := m["forbids"].(map[string]interface{})
			var kept []interface{}
			for _, token := range asList(forbids[drop.field]) {
				if token == drop.token {
					removed = true
					continue
				}
				kept = append(kept, token)
			}
			forbids[drop.field] = kept
		}
		require.True(t, removed, "%s forbids %s", drop.id, drop.token)
		r = nil
		require.ErrorAs(t, staticPresentationCheck(mutated), &r, "without %s's forbid of %s", drop.id, drop.token)
		assert.Equal(t, "ambiguous", r.Reason)
	}

	unsupported := load("neg-unsupported-presentation-api")
	assert.NoError(t, staticPresentationCheck(append(examples, unsupported)), "a refused module takes part in the test and is co-matchable with none of them")
	require.NotNil(t, runtimeRefusal(unsupported))
	assert.Equal(t, "presentation_api_unsupported", runtimeRefusal(unsupported).Reason)
	tooNew := load("example-unilateral")
	tooNew["runtime_min"] = "0.1.1"
	require.NotNil(t, runtimeRefusal(tooNew))
	assert.Equal(t, "runtime_too_old", runtimeRefusal(tooNew).Reason)
	tooNew["runtime_min"] = "0.0.9"
	assert.Nil(t, runtimeRefusal(tooNew))
	tooNew["runtime_min"] = "0.10.0"
	assert.Equal(t, "runtime_too_old", runtimeRefusal(tooNew).Reason, "versions compare numerically")
	for _, m := range append(builtins, examples...) {
		assert.Nil(t, runtimeRefusal(m), "%s", m["id"])
	}
}

// The declaration is the vendored runtime's own, and the built-ins are the
// pinned commit's files.
func TestThePresentationRuntimeDeclarationIsTheVendoredOne(t *testing.T) {
	assert.Contains(t, string(evidenceGraphIIFE), `var PRESENTATION_API_V0 = "aac.presentation-api/v0";
  var REFERENCE_PRESENTATION_RUNTIME = Object.freeze({
    presentationApis: Object.freeze([PRESENTATION_API_V0]),
    runtimeVersion: "0.1.0"
  });`)
	assert.Equal(t, []string{"aac.presentation-api/v0"}, presentationRuntime.PresentationAPIs)
	assert.Equal(t, "0.1.0", presentationRuntime.Version)
	want := map[string]string{
		"builtin-evaluation-summary-graph.json": "0747d86864c685d90e7dd9456bc8b378ff17dd5d2be9fa2f03a939f1ed00489b",
		"builtin-no-aggregate.json":             "0ea7853f0bdc957c772cd019d3de78b525352c3ec61a427e1f0238c7495f0ee6",
		"builtin-report-rows.json":              "b224a61b5494ba50e7b07b01d752972f1b46173deef17ffc46e608e286a81358",
		"builtin-result-compliance.json":        "e5d8e8212bfe2478b39bf4a7d78ae13e399d0e1b16eed4cd2235967f1a5c3289",
		"builtin-result-outcome-report.json":    "6e9399278f9f4c211bc495e3392499fc55f51f7e1a974532fe4250fd9ff4d398",
		"builtin-result.json":                   "52aaf2434c0b583afb47e1e896b8a5047fc1cf7494d0230286fc95a354b57314",
	}
	entries, err := builtinPresentationFS.ReadDir("assets/presentation/builtin")
	require.NoError(t, err)
	got := map[string]string{}
	for _, e := range entries {
		raw, err := builtinPresentationFS.ReadFile("assets/presentation/builtin/" + e.Name())
		require.NoError(t, err)
		got[e.Name()] = hexSHA256(raw)
	}
	assert.Equal(t, want, got)
	builtins, err := builtinPresentationManifests()
	require.NoError(t, err)
	for _, m := range builtins {
		assert.Contains(t, string(evidenceGraphIIFE), `"`+m["id"].(string)+`"`, "the runtime carries %s", m["id"])
	}
}

// What is not put in the page is said, and so is a packaging this tool
// cannot write.
func TestPresentationBuildSaysWhatItLeavesOut(t *testing.T) {
	root := pluginRoot(t)
	trustPresentationsIn(t, root)
	declarative := moduleSlotFixture()
	delete(declarative.manifest, "executable")
	declarative.manifest["id"], declarative.manifest["trust_class"] = "org.example.rules/v0", "declarative"
	declarative.manifest["requires"] = map[string]any{"bundle_kind": "evidence-bundle/v2", "profiles": []any{"spec_version:org.example.rules/v1"}}
	declarative.manifest["declarative"] = map[string]any{"renderer": "aac.declarative-renderer/v0", "wording_sha256": hexSHA256(fixtureWording),
		"fields": []any{map[string]any{"level": "L0", "source": "/body/state", "kind": "identifier", "label_key": "heading.l0"}}}
	declarative.entries = declarative.entries[2:]
	presentationPlugin(t, root, "rules", declarative)
	refused := exampleDealFixture(t)
	refused.entries[0]["sha256"] = strings.Repeat("0", 64)
	presentationPlugin(t, root, "broken", refused)

	stdout, err := invoke(t, "", "presentation", "build", "--bundle", presentationBuildBundle, "--out", presentationOut(t))
	require.NoError(t, err, stdout)
	b := decodeBuild(t, stdout)
	assert.Empty(t, b.Modules)
	require.Len(t, b.NotIncluded, 1)
	assert.Equal(t, "org.example.rules/v0", b.NotIncluded[0]["id"])
	require.Len(t, b.PluginsRefused, 1)
	assert.Equal(t, "broken", b.PluginsRefused[0]["plugin"])
	assert.Equal(t, "digest_mismatch", b.PluginsRefused[0]["reason"])

	_, err = invoke(t, "", "presentation", "build", "--bundle", presentationBuildBundle, "--out", presentationOut(t), "--module", "org.example.absent/v0")
	assert.ErrorContains(t, err, "org.example.absent/v0")
	_, err = invoke(t, "", "presentation", "build", "--bundle", presentationBuildBundle, "--out", presentationOut(t), "--format", "static")
	assert.ErrorContains(t, err, "unavailable (no-document)")
	_, err = invoke(t, "", "presentation", "build", "--bundle", presentationBuildBundle, "--out", presentationOut(t), "--format", "fragment")
	assert.ErrorContains(t, err, "not built by this tool yet")
}

// A bundle `verify --bundle` does not call VALID gets no page.
func TestPresentationBuildWritesNoPageForABundleThatDoesNotVerify(t *testing.T) {
	pluginRoot(t)
	raw, err := os.ReadFile(presentationBuildBundle)
	require.NoError(t, err)
	value, err := decodeBundleJSON(raw)
	require.NoError(t, err)
	value["root"] = strings.Repeat("0", 64)
	bundle := writeBundle(t, value)
	out := presentationOut(t)
	stdout, err := invoke(t, "", "presentation", "build", "--bundle", bundle, "--out", out)
	require.Error(t, err)
	assert.Equal(t, "not written", decodeBuild(t, stdout).Page)
	assert.NoFileExists(t, out)
}

// Section 4.5's first three conditions: one bundle kind, a common audience
// ("*" meets every one) and a common format.
func TestCoMatchableNeedsOneKindAudienceAndFormat(t *testing.T) {
	pair := func(edit func(b map[string]any)) bool {
		a, b := exampleDealManifest(strings.Repeat("0", 64)), exampleDealManifest(strings.Repeat("0", 64))
		a["audiences"], b["audiences"] = []any{"buyer"}, []any{"buyer"}
		edit(b)
		return coMatchable(a, b)
	}
	assert.True(t, pair(func(map[string]any) {}))
	assert.False(t, pair(func(b map[string]any) { b["requires"] = map[string]any{"bundle_kind": "evidence-bundle/v3"} }))
	assert.False(t, pair(func(b map[string]any) { b["audiences"] = []any{"seller"} }))
	assert.True(t, pair(func(b map[string]any) { b["audiences"] = []any{"*"} }))
	assert.False(t, pair(func(b map[string]any) { b["formats"] = []any{"fragment"} }))
}
