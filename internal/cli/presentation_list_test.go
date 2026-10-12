package cli

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	aacpresentation "github.com/action-state-group/agent-action-capsule/go/presentation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A VALID bundle no specific built-in or deal view matches: only the
// built-in fallback does.
const presentationListGenericBundle = "testdata/presentation/6-generic-fallback/bundle.json"

type listOutput struct {
	Resolution     string                     `json:"resolution"`
	Descriptor     aacpresentation.Descriptor `json:"descriptor"`
	Selected       map[string]any             `json:"selected"`
	Refused        []map[string]any           `json:"refused"`
	Ambiguity      map[string]any             `json:"ambiguity"`
	Modules        []map[string]any           `json:"modules"`
	PluginsRefused []map[string]any           `json:"plugins_refused"`
	Detail         string                     `json:"detail"`
}

func decodeList(t *testing.T, out string) listOutput {
	t.Helper()
	var l listOutput
	require.NoError(t, json.Unmarshal([]byte(out), &l), out)
	return l
}

func listedModule(t *testing.T, l listOutput, id string) map[string]any {
	t.Helper()
	for _, m := range l.Modules {
		if m["id"] == id {
			return m
		}
	}
	t.Fatalf("%s is not listed: %v", id, l.Modules)
	return nil
}

// genericPluginFixture is a plugin's module that stands beside the built-ins
// and the deal view: it forbids what each specific one requires, so it is
// selected for a bundle only the fallback matches.
func genericPluginFixture(t *testing.T) presentationFixture {
	p := exampleDealFixture(t)
	p.manifest["forbids"] = map[string]any{
		"profiles":   []any{"spec_version:report/v1", "result_version:evidence-result-v0", "spec_version:evaluation-summary/v1", "spec_version:org.example.rules/v1"},
		"extensions": []any{"x-deal-v0"},
	}
	return p
}

func declarativePluginFixture() presentationFixture {
	p := moduleSlotFixture()
	delete(p.manifest, "executable")
	p.manifest["id"], p.manifest["trust_class"] = "org.example.rules/v0", "declarative"
	p.manifest["requires"] = map[string]any{"bundle_kind": "evidence-bundle/v2", "profiles": []any{"spec_version:org.example.rules/v1"}}
	p.manifest["forbids"] = map[string]any{"profiles": []any{"result_version:evidence-result-v0"}, "extensions": []any{"x-deal-v0"}}
	p.manifest["declarative"] = map[string]any{"renderer": "aac.declarative-renderer/v0", "wording_sha256": hexSHA256(fixtureWording),
		"fields": []any{map[string]any{"level": "L0", "source": "/body/state", "kind": "identifier", "label_key": "heading.l0"}}}
	p.entries = p.entries[2:]
	delete(p.files, "view.js")
	delete(p.files, "view.css")
	return p
}

// The deal receipt resolves to this tool's deal view; the fallback also
// matches and loses to it.
func TestPresentationListResolvesTheDealViewForADeal(t *testing.T) {
	pluginRoot(t)
	stdout, err := invoke(t, "", "presentation", "list", "--bundle", presentationBuildBundle)
	require.NoError(t, err, stdout)
	l := decodeList(t, stdout)
	assert.Equal(t, "module", l.Resolution)
	assert.True(t, l.Descriptor.Verified)
	assert.Contains(t, l.Descriptor.Extensions, "x-deal-v0")
	require.NotNil(t, l.Selected)
	assert.Equal(t, "capsulectl.deal-view/v0", l.Selected["id"])
	assert.Equal(t, "capsulectl", l.Selected["source"])
	assert.Equal(t, "trusted-executable", l.Selected["trust_class"])
	assert.Equal(t, true, listedModule(t, l, "aac.builtin.no-aggregate/v0")["matches"])
	assert.Empty(t, l.Refused)
}

// A plugin's modules are listed with their trust class, and the resolver
// selects one that is the only specific match.
func TestPresentationListListsAPluginsModulesWithTheirTrustClass(t *testing.T) {
	root := pluginRoot(t)
	trustPresentationsIn(t, root)
	presentationPlugin(t, root, "dealview", genericPluginFixture(t))
	presentationPlugin(t, root, "rules", declarativePluginFixture())
	stdout, err := invoke(t, "", "presentation", "list", "--bundle", presentationListGenericBundle)
	require.NoError(t, err, stdout)
	l := decodeList(t, stdout)
	assert.Equal(t, "module", l.Resolution)
	require.NotNil(t, l.Selected)
	assert.Equal(t, map[string]any{"id": "org.example.deal-view/v0", "source": "plugin", "plugin": "dealview",
		"trust_class": "trusted-executable", "fallback": false, "matches": true}, l.Selected)
	rules := listedModule(t, l, "org.example.rules/v0")
	assert.Equal(t, "declarative", rules["trust_class"])
	assert.Equal(t, "rules", rules["plugin"])
	assert.Equal(t, false, rules["matches"])
}

// Two specific modules one descriptor can match is the resolver's named
// error, never first-wins: AAC #225's pair, the deal view and a unilateral
// receipt view, here offered by a plugin.
func TestPresentationListNamesAnAmbiguity(t *testing.T) {
	root := pluginRoot(t)
	trustPresentationsIn(t, root)
	p := exampleDealFixture(t)
	p.manifest["id"] = "capsuleviewer.receipt.unilateral/v0"
	p.manifest["requires"] = map[string]any{"bundle_kind": "evidence-bundle/v2", "extensions": map[string]any{"required": []any{"x-deal-v0"}}}
	p.manifest["forbids"] = map[string]any{
		"profiles":   []any{"spec_version:report/v1", "spec_version:evaluation-summary/v1", "result_version:evidence-result-v0"},
		"extensions": []any{"composed/v1"},
	}
	p.manifest["audiences"] = []any{"keep", "counterparty", "adjudicator"}
	presentationPlugin(t, root, "receipt", p)
	stdout, err := invoke(t, "", "presentation", "list", "--bundle", presentationBuildBundle, "--audience", "keep")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInput))
	assert.Equal(t, 2, ExitCode(err))
	l := decodeList(t, stdout)
	assert.Equal(t, "ambiguous", l.Resolution)
	assert.Nil(t, l.Selected)
	assert.Equal(t, map[string]any{
		"stage":   "registration",
		"modules": []any{"capsulectl.deal-view/v0", "capsuleviewer.receipt.unilateral/v0"},
		"detail":  "manifests capsulectl.deal-view/v0 and capsuleviewer.receipt.unilateral/v0 are both specific and one descriptor can match both; state precedence with forbids",
	}, l.Ambiguity)
	assert.Contains(t, err.Error(), "state precedence with forbids")
}

// A module the page's runtime refuses is listed with the refusal, and when it
// matches, the result names it as the page does.
func TestPresentationListShowsTheRuntimesRefusal(t *testing.T) {
	for name, tc := range map[string]struct{ api, min, reason, line string }{
		"api": {"aac.presentation-api/v1", "0.1.0", "presentation_api_unsupported",
			"Presentation module org.example.deal-view/v0 was not used: it needs presentation API aac.presentation-api/v1, which this viewer does not implement"},
		"runtime": {"aac.presentation-api/v0", "0.2.0", "runtime_too_old",
			"Presentation module org.example.deal-view/v0 was not used: it needs runtime 0.2.0 or later; this viewer is 0.1.0"},
	} {
		t.Run(name, func(t *testing.T) {
			root := pluginRoot(t)
			trustPresentationsIn(t, root)
			p := genericPluginFixture(t)
			p.manifest["presentation_api"], p.manifest["runtime_min"] = tc.api, tc.min
			presentationPlugin(t, root, "dealview", p)
			stdout, err := invoke(t, "", "presentation", "list", "--bundle", presentationListGenericBundle)
			require.NoError(t, err, stdout)
			l := decodeList(t, stdout)
			assert.Equal(t, "aac.builtin.no-aggregate/v0", l.Selected["id"])
			require.Len(t, l.Refused, 1)
			assert.Equal(t, tc.reason, l.Refused[0]["reason"])
			assert.Equal(t, tc.line, l.Refused[0]["line"])
			row := listedModule(t, l, "org.example.deal-view/v0")
			assert.Equal(t, tc.reason, row["refusal"].(map[string]any)["reason"])

			text, err := invoke(t, "", "presentation", "list", "--bundle", presentationListGenericBundle, "--text")
			require.NoError(t, err)
			assert.Contains(t, text, "refused: "+tc.line)
		})
	}
}

// A bundle the viewer's gate refuses matches nothing: the resolver's refusal,
// exit 1.
func TestPresentationListMatchesNothingForABundleThatDoesNotVerify(t *testing.T) {
	pluginRoot(t)
	raw, err := os.ReadFile(presentationBuildBundle)
	require.NoError(t, err)
	value, err := decodeBundleJSON(raw)
	require.NoError(t, err)
	value["root"] = strings.Repeat("0", 64)
	stdout, err := invoke(t, "", "presentation", "list", "--bundle", writeBundle(t, value))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBundleInvalid))
	assert.Equal(t, 1, ExitCode(err))
	l := decodeList(t, stdout)
	assert.Equal(t, "refusal", l.Resolution)
	assert.False(t, l.Descriptor.Verified)
	assert.Nil(t, l.Selected)
	for _, m := range l.Modules {
		assert.Equal(t, false, m["matches"], m["id"])
	}
}

// With no fallback in the registry, a VALID bundle no module matches is the
// resolver's no-presentation: listed as none, exit 0. (The page's built-in
// fallback matches every VALID evidence-bundle/v2, so only a registry without
// it reaches this.)
func TestPresentationListSaysNoPresentationWhenNothingMatches(t *testing.T) {
	pluginRoot(t)
	old := presentationListCandidates
	presentationListCandidates = func() ([]presentationCandidate, []presentationRefusedPlugin, error) {
		all, refused, err := old()
		var specific []presentationCandidate
		for _, c := range all {
			if !c.manifest.Fallback {
				specific = append(specific, c)
			}
		}
		return specific, refused, err
	}
	t.Cleanup(func() { presentationListCandidates = old })
	stdout, err := invoke(t, "", "presentation", "list", "--bundle", presentationListGenericBundle)
	require.NoError(t, err, stdout)
	l := decodeList(t, stdout)
	assert.Equal(t, "no-presentation", l.Resolution)
	assert.Nil(t, l.Selected)
	assert.Equal(t, `no presentation module matches the bundle for audience *, format html; a page shows its "no presentation" notice`, l.Detail)
}

func TestPresentationListRejectsAnUnknownFormat(t *testing.T) {
	pluginRoot(t)
	_, err := invoke(t, "", "presentation", "list", "--bundle", presentationBuildBundle, "--format", "static")
	assert.ErrorContains(t, err, "not a presentation format")
	_, err = invoke(t, "", "presentation", "list")
	assert.ErrorContains(t, err, "--bundle is required")
}

// The registry list builds is the page's: the runtime the page declares is
// the resolver's reference runtime, and the vendored built-ins are the
// resolver's own, byte for byte.
func TestPresentationListUsesThePagesRuntimeAndBuiltins(t *testing.T) {
	assert.Equal(t, aacpresentation.ReferenceRuntime(), presentationListRuntime())
	entries, err := builtinPresentationFS.ReadDir("assets/presentation/builtin")
	require.NoError(t, err)
	want, err := aacpresentation.BuiltinManifests()
	require.NoError(t, err)
	require.Len(t, entries, len(want))
	for _, e := range entries {
		vendored, err := builtinPresentationFS.ReadFile("assets/presentation/builtin/" + e.Name())
		require.NoError(t, err)
		m, err := aacpresentation.ParseManifest(vendored)
		require.NoError(t, err, e.Name())
		resolverCopy, ok := aacpresentation.BuiltinManifestJSON(m.ID)
		require.True(t, ok, m.ID)
		assert.Equal(t, string(resolverCopy), string(vendored), m.ID)
	}
}
