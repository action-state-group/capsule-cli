package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-viewer/go/pageruntime"

	aacpresentation "github.com/action-state-group/capsule-viewer/go/presentation"
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
	Registration   map[string]any             `json:"registration_refused"`
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

// genericPluginFixture is a plugin's module that stands beside the built-ins:
// it forbids what each specific one requires, and x-deal-v0, so it is
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

// The core has no deal view of its own (the deal plugin offers it,
// capsule-cli#428): with no plugin installed, a deal receipt resolves through
// the generic path, to the built-in fallback, and the list says so.
func TestPresentationListResolvesADealToTheFallbackWithoutAPlugin(t *testing.T) {
	pluginRoot(t)
	stdout, err := invoke(t, "", "presentation", "list", "--bundle", presentationBuildBundle)
	require.NoError(t, err, stdout)
	l := decodeList(t, stdout)
	assert.Equal(t, "module", l.Resolution)
	assert.True(t, l.Descriptor.Verified)
	assert.Contains(t, l.Descriptor.Extensions, "x-deal-v0")
	require.NotNil(t, l.Selected)
	assert.Equal(t, "aac.builtin.no-aggregate/v0", l.Selected["id"])
	assert.Equal(t, "builtin", l.Selected["source"])
	assert.Equal(t, true, l.Selected["fallback"])
	for _, m := range l.Modules {
		assert.Equal(t, "builtin", m["source"], "only the page's built-ins are candidates")
	}
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

// viewerModuleFixture is a plugin's module that declares what capsule-viewer's
// manifest of that id declares (capsule-viewer db3db22e,
// src/capsule_viewer/{receipt/module-slot,history}/manifest*.json): its id,
// requires, forbids and audiences.
func viewerModuleFixture(t *testing.T, id string) presentationFixture {
	notReports := []any{"spec_version:report/v1", "spec_version:evaluation-summary/v1", "result_version:evidence-result-v0"}
	extensions := func(kinds ...any) map[string]any { return map[string]any{"required": kinds} }
	receipt := []any{"keep", "counterparty", "adjudicator"}
	declared := map[string]struct {
		requires  map[string]any
		forbids   []any
		audiences []any
	}{
		"capsuleviewer.receipt.unilateral/v0": {extensions("x-deal-v0"), []any{"composed/v1", "x-history-selection/v0", "x-deal-seller/v0"}, receipt},
		"capsuleviewer.receipt.seller/v0":     {extensions("x-deal-v0", "x-deal-seller/v0"), []any{"composed/v1", "x-history-selection/v0"}, receipt},
		"capsuleviewer.receipt.bilateral/v0":  {extensions("composed/v1"), []any{"x-deal-v0", "x-history-selection/v0"}, receipt},
		"capsuleviewer.history/v0":            {extensions("x-history-selection/v0"), nil, append([]any{"history"}, receipt...)},
	}[id]
	require.NotNil(t, declared.requires, id)
	p := exampleDealFixture(t)
	p.manifest["id"] = id
	p.manifest["requires"] = map[string]any{"bundle_kind": "evidence-bundle/v2", "extensions": declared.requires}
	forbids := map[string]any{"profiles": notReports}
	if declared.forbids != nil {
		forbids["extensions"] = declared.forbids
	}
	p.manifest["forbids"] = forbids
	p.manifest["audiences"] = declared.audiences
	return p
}

var viewerModules = []string{"capsuleviewer.receipt.unilateral/v0", "capsuleviewer.receipt.seller/v0",
	"capsuleviewer.receipt.bilateral/v0", "capsuleviewer.history/v0"}

func installViewerModules(t *testing.T, ids ...string) {
	t.Helper()
	root := pluginRoot(t)
	trustPresentationsIn(t, root)
	for i, id := range ids {
		presentationPlugin(t, root, fmt.Sprintf("viewer%d", i), viewerModuleFixture(t, id))
	}
}

// With capsule-viewer's three receipt modules and its history module
// installed, a buyer's copy resolves to the unilateral receipt and a seller's
// copy (golden 8, which carries x-deal-seller/v0) to the seller receipt: no
// ambiguity. (testdata has no composed bundle, so the bilateral receipt is
// shown registering, not selected.)
func TestPresentationListResolvesTheReceiptModules(t *testing.T) {
	for name, tc := range map[string]struct{ bundle, selected string }{
		"user buys":  {presentationBuildBundle, "capsuleviewer.receipt.unilateral/v0"},
		"user sells": {"testdata/presentation/8-seller-buyer-copy/bundle.json", "capsuleviewer.receipt.seller/v0"},
	} {
		bundle := tc.bundle
		t.Run(name, func(t *testing.T) {
			installViewerModules(t, viewerModules...)
			stdout, err := invoke(t, "", "presentation", "list", "--bundle", bundle, "--audience", "keep")
			require.NoError(t, err, stdout)
			l := decodeList(t, stdout)
			assert.Equal(t, "module", l.Resolution)
			assert.Nil(t, l.Ambiguity)
			require.NotNil(t, l.Selected)
			assert.Equal(t, tc.selected, l.Selected["id"])
			assert.Equal(t, "plugin", l.Selected["source"])
			for _, id := range viewerModules {
				listedModule(t, l, id)
			}
		})
	}
}

// Two specific modules one descriptor can match is the resolver's named
// error, never first-wins: two plugins that each offer a unilateral receipt
// view.
func TestPresentationListNamesAnAmbiguity(t *testing.T) {
	root := pluginRoot(t)
	trustPresentationsIn(t, root)
	presentationPlugin(t, root, "receipt", viewerModuleFixture(t, "capsuleviewer.receipt.unilateral/v0"))
	other := viewerModuleFixture(t, "capsuleviewer.receipt.unilateral/v0")
	other.manifest["id"] = "org.example.receipt/v0"
	presentationPlugin(t, root, "other", other)
	stdout, err := invoke(t, "", "presentation", "list", "--bundle", presentationBuildBundle, "--audience", "keep")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInput))
	assert.Equal(t, 2, ExitCode(err))
	l := decodeList(t, stdout)
	assert.Equal(t, "ambiguous", l.Resolution)
	assert.Nil(t, l.Selected)
	assert.Equal(t, map[string]any{
		"stage":   "registration",
		"modules": []any{"capsuleviewer.receipt.unilateral/v0", "org.example.receipt/v0"},
		"detail":  "manifests org.example.receipt/v0 and capsuleviewer.receipt.unilateral/v0 are both specific and one descriptor can match both; state precedence with forbids",
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
	value, err := DecodeBundleJSON(raw)
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

// A plugin's manifest the registry refuses to hold is shown, with the
// resolver's reason, and nothing is resolved: a duplicate id (refused by
// Register) and a dead manifest (refused by ParseManifest, which the plugin
// loader's schema check lets through).
func TestPresentationListShowsARegistrationRefusal(t *testing.T) {
	for name, tc := range map[string]struct {
		edit   func(m map[string]any)
		module string
		detail string
	}{
		"duplicate id": {func(m map[string]any) { m["id"] = "aac.builtin.no-aggregate/v0" },
			"aac.builtin.no-aggregate/v0", "manifest aac.builtin.no-aggregate/v0 is already registered"},
		"dead manifest": {func(m map[string]any) {
			m["requires"] = map[string]any{"bundle_kind": "evidence-bundle/v2", "profiles": []any{"spec_version:org.example.rules/v1"}}
			m["forbids"] = map[string]any{"profiles": []any{"spec_version:org.example.rules/v1"}}
		}, "org.example.deal-view/v0", "manifest \"org.example.deal-view/v0\" refused: "},
	} {
		t.Run(name, func(t *testing.T) {
			root := pluginRoot(t)
			trustPresentationsIn(t, root)
			p := genericPluginFixture(t)
			tc.edit(p.manifest)
			presentationPlugin(t, root, "dealview", p)
			stdout, err := invoke(t, "", "presentation", "list", "--bundle", presentationListGenericBundle)
			require.Error(t, err)
			assert.Equal(t, 2, ExitCode(err))
			assert.Contains(t, err.Error(), tc.detail)
			l := decodeList(t, stdout)
			assert.Equal(t, "registration_refused", l.Resolution)
			assert.Nil(t, l.Selected)
			assert.Equal(t, map[string]any{"module": tc.module, "plugin": "dealview"}, l.Registration)
			assert.Contains(t, l.Detail, tc.detail)

			text, err := invoke(t, "", "presentation", "list", "--bundle", presentationListGenericBundle, "--text")
			require.Error(t, err)
			assert.Contains(t, text, "registration_refused: "+tc.detail)
		})
	}
}

// A module the page's runtime refuses is held, refused and never selected,
// and the page falls back.
func TestPresentationListARefusedModuleIsNeverSelected(t *testing.T) {
	root := pluginRoot(t)
	trustPresentationsIn(t, root)
	p := viewerModuleFixture(t, "capsuleviewer.receipt.unilateral/v0")
	p.manifest["presentation_api"] = "aac.presentation-api/v1"
	presentationPlugin(t, root, "receipt", p)
	stdout, err := invoke(t, "", "presentation", "list", "--bundle", presentationBuildBundle, "--audience", "keep")
	require.NoError(t, err, stdout)
	l := decodeList(t, stdout)
	assert.Equal(t, "module", l.Resolution)
	assert.Nil(t, l.Ambiguity)
	require.NotNil(t, l.Selected)
	assert.Equal(t, "aac.builtin.no-aggregate/v0", l.Selected["id"])
	require.Len(t, l.Refused, 1)
	assert.Equal(t, "capsuleviewer.receipt.unilateral/v0", l.Refused[0]["id"])
	assert.Equal(t, "presentation_api_unsupported", l.Refused[0]["reason"])
	row := listedModule(t, l, "capsuleviewer.receipt.unilateral/v0")
	assert.Equal(t, true, row["matches"])
	assert.Equal(t, "presentation_api_unsupported", row["refusal"].(map[string]any)["reason"])
}

// A Result opted into contract-report/v1 is drawn by the module the page
// registers in place of agent-action-capsule's built-in for it
// (spec/contract-report-v1.md section 4), and list names that module.
func TestPresentationListNamesThePagesContractReportModule(t *testing.T) {
	pluginRoot(t)
	for _, fixture := range []string{"4-monthly-outcome", "5-monthly-compliance"} {
		t.Run(fixture, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata/presentation", fixture, "bundle.json"))
			require.NoError(t, err)
			var bundle map[string]any
			require.NoError(t, json.Unmarshal(raw, &bundle))
			bundle["extensions"].(map[string]any)["contract-report/v1"] = map[string]any{}
			opted, err := json.Marshal(bundle)
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "bundle.json")
			require.NoError(t, os.WriteFile(path, opted, 0o600))
			stdout, err := invoke(t, "", "presentation", "list", "--bundle", path)
			require.NoError(t, err, stdout)
			l := decodeList(t, stdout)
			require.NotNil(t, l.Selected)
			assert.Equal(t, contractReportPageID, l.Selected["id"])
			assert.Equal(t, "builtin", l.Selected["source"])
			assert.Contains(t, string(pageruntime.IIFE), `"`+contractReportPageID+`"`, "the page registers it")
		})
	}
}
