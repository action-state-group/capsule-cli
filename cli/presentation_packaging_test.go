package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-viewer/go/emitter"
	"github.com/action-state-group/capsule-viewer/go/pageruntime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
)

// The fragment and embedded packagings of `presentation build`, and the
// availability `presentation list` reports (capsule-cli#254).

const monthlyOutcomeBundle = "testdata/presentation/4-monthly-outcome/bundle.json"

type packagingOutput struct {
	Page           string           `json:"page"`
	Out            string           `json:"out"`
	Format         string           `json:"format"`
	BundleDigest   string           `json:"bundle_digest"`
	FragmentLength int              `json:"fragment_length"`
	FragmentLimit  int              `json:"fragment_limit"`
	Unavailable    *packagingReason `json:"unavailable"`
	ScriptSources  []string         `json:"script_sources"`
	StyleSources   []string         `json:"style_sources"`
	Modules        []map[string]any `json:"modules"`
}

func decodePackaging(t *testing.T, out string) packagingOutput {
	t.Helper()
	var p packagingOutput
	require.NoError(t, json.Unmarshal([]byte(out), &p), out)
	return p
}

func readBundleValue(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	value, err := DecodeBundleJSON(raw)
	require.NoError(t, err)
	return value
}

func buildPage(t *testing.T, args ...string) (string, packagingOutput, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "page.html")
	stdout, err := invoke(t, "", append([]string{"presentation", "build", "--out", out}, args...)...)
	if stdout == "" {
		// Refused before any result was written (a flag error).
		require.Error(t, err)
		return out, packagingOutput{}, err
	}
	return out, decodePackaging(t, stdout), err
}

func verdictOf(t *testing.T, path string) string {
	t.Helper()
	out, err := invoke(t, "", "verify", "--bundle", path)
	var result struct {
		Verdict string `json:"verdict"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &result), out)
	if result.Verdict == "VALID" {
		require.NoError(t, err)
	}
	return result.Verdict
}

// A fragment is the offline page's settings and bundle, with code by pin:
// the hand-back rebuilds exactly the page --format html writes, which
// `verify --bundle` calls VALID. With a plugin's module, the fragment pins it
// and the hand-back needs it.
func TestPresentationBuildFragmentHandsBackTheOfflinePage(t *testing.T) {
	root := pluginRoot(t)
	for _, withModule := range []bool{false, true} {
		var modules []emitter.Module
		if withModule {
			trustPresentationsIn(t, root)
			fixture := exampleDealFixture(t)
			presentationPlugin(t, root, "dealview", fixture)
			script := fixture.files["view.js"]
			// The fragment pins the script; its stylesheet pins come with
			// the module, as the page's CSP needs them.
			modules = []emitter.Module{{Code: script, SHA256: hexSHA256(script), StyleSHA256: []string{hexSHA256(fixtureStyle)}}}
		}
		html, _, err := buildPage(t, "--bundle", presentationBuildBundle, "--format", "html", "--audience", "counterparty", "--depth", "L1")
		require.NoError(t, err)
		page, err := os.ReadFile(html)
		require.NoError(t, err)

		out, b, err := buildPage(t, "--bundle", presentationBuildBundle, "--format", "fragment", "--audience", "counterparty", "--depth", "L1")
		require.NoError(t, err)
		assert.Equal(t, "written", b.Page)
		assert.Equal(t, "fragment", b.Format)
		raw, err := os.ReadFile(out)
		require.NoError(t, err)
		token := strings.TrimSuffix(string(raw), "\n")
		assert.Equal(t, len(token), b.FragmentLength)
		assert.Equal(t, emitter.FragmentTokenDefaultBudget, b.FragmentLimit)
		assert.Len(t, b.Modules, len(modules))

		payload, err := emitter.DecodePresentationFragment(token, 0)
		require.NoError(t, err)
		assert.Equal(t, "counterparty", payload.Audience)
		assert.Equal(t, "auto", payload.Presentation)
		assert.Equal(t, pageruntime.SHA256, payload.CoreRuntimeSHA256)
		digest, err := aacbundle.BundleDigest(payload.Bundle)
		require.NoError(t, err)
		assert.Equal(t, b.BundleDigest, digest)

		rebuilt, err := emitter.OfflineHTMLFromFragment(token, pageruntime.IIFE, modules)
		require.NoError(t, err)
		require.True(t, rebuilt == string(page), "the hand-back is the offline page byte for byte (module: %v)", withModule)
		path := filepath.Join(t.TempDir(), "rebuilt.html")
		require.NoError(t, os.WriteFile(path, []byte(rebuilt), 0o600))
		assert.Equal(t, "VALID", verdictOf(t, path))
	}

	// With --viewer-url the page is the whole permalink.
	out, _, err := buildPage(t, "--bundle", presentationBuildBundle, "--format", "fragment", "--viewer-url", "https://viewer.example/v.html")
	require.NoError(t, err)
	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(raw), "https://viewer.example/v.html#"))
	_, err = emitter.DecodePresentationFragment(strings.TrimSpace(strings.SplitN(string(raw), "#", 2)[1]), 0)
	require.NoError(t, err)
}

// The monthly report is over the default budget, so its fragment is
// refused and nothing is written; raised to the ceiling it fits, and with a
// viewer address it is still refused once the whole URL is over 1 MiB.
func TestPresentationBuildFragmentRefusesTheMonthlyFixture(t *testing.T) {
	pluginRoot(t)
	out, b, err := buildPage(t, "--bundle", monthlyOutcomeBundle, "--format", "fragment")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInput)
	assert.ErrorContains(t, err, "--format fragment is unavailable (fragment-too-large)")
	assert.Equal(t, "not written", b.Page)
	require.NotNil(t, b.Unavailable)
	assert.Equal(t, "fragment-too-large", b.Unavailable.Code)
	assert.Equal(t, "fragment token", b.Unavailable.Subject)
	assert.Equal(t, emitter.FragmentTokenDefaultBudget, b.Unavailable.Limit)
	assert.Greater(t, b.Unavailable.Length, emitter.FragmentTokenDefaultBudget)
	assert.NoFileExists(t, out)

	_, b, err = buildPage(t, "--bundle", monthlyOutcomeBundle, "--format", "fragment", "--max-fragment-length", "1046528")
	require.NoError(t, err)
	assert.Equal(t, "written", b.Page)

	viewer := "https://viewer.example/" + strings.Repeat("v", emitter.FragmentURLMaxLength-b.FragmentLength)
	out, b, err = buildPage(t, "--bundle", monthlyOutcomeBundle, "--format", "fragment", "--max-fragment-length", "1046528", "--viewer-url", viewer)
	require.Error(t, err)
	require.NotNil(t, b.Unavailable)
	assert.Equal(t, "permalink URL", b.Unavailable.Subject)
	assert.Equal(t, emitter.FragmentURLMaxLength, b.Unavailable.Limit)
	assert.NoFileExists(t, out)
}

// embeddedBundleOf is the bundle an embedded snippet carries in its JSON
// data element.
func embeddedBundleOf(t *testing.T, snippet string) map[string]interface{} {
	t.Helper()
	const start = `<script type="application/json">`
	i := strings.Index(snippet, start)
	require.GreaterOrEqual(t, i, 0)
	rest := snippet[i+len(start):]
	j := strings.Index(rest, "</script>")
	require.GreaterOrEqual(t, j, 0)
	value, err := DecodeBundleJSON([]byte(rest[:j]))
	require.NoError(t, err)
	return value
}

// The embedded snippet is the Go builder's, carries the bundle the page
// gate passed, and reports the CSP sources its host must allow. In Chrome,
// under a host CSP of only those sources, it renders with no violation and
// draws the banner the offline page draws.
func TestPresentationBuildEmbeddedWritesTheSnippet(t *testing.T) {
	pluginRoot(t)
	out, b, err := buildPage(t, "--bundle", presentationBuildBundle, "--format", "embedded")
	require.NoError(t, err)
	assert.Equal(t, "written", b.Page)
	snippet, err := os.ReadFile(out)
	require.NoError(t, err)
	want, err := emitter.BuildEmbeddedHTML(readBundleValue(t, presentationBuildBundle), pageruntime.IIFE,
		emitter.OfflineOptions{Audience: "*", CoreRuntimeSHA256: pageruntime.SHA256})
	require.NoError(t, err)
	require.True(t, want.HTML == string(snippet), "the snippet is the Go builder's")
	assert.Equal(t, want.ScriptSources, b.ScriptSources)
	assert.Equal(t, want.StyleSources, b.StyleSources)

	carried := embeddedBundleOf(t, string(snippet))
	digest, err := aacbundle.BundleDigest(carried)
	require.NoError(t, err)
	assert.Equal(t, b.BundleDigest, digest)
	raw, err := json.Marshal(carried)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "carried.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	assert.Equal(t, "VALID", verdictOf(t, path))

	// Fragment-only flags do not apply here, malformed or not; for
	// --format fragment, and for list, a malformed one is refused.
	_, _, err = buildPage(t, "--bundle", presentationBuildBundle, "--format", "embedded", "--viewer-url", "https://v.example/#old", "--max-fragment-length", "-1")
	require.NoError(t, err)
	_, _, err = buildPage(t, "--bundle", presentationBuildBundle, "--format", "html", "--max-fragment-length", "-1")
	require.NoError(t, err)
	_, _, err = buildPage(t, "--bundle", presentationBuildBundle, "--format", "fragment", "--viewer-url", "https://v.example/#old")
	assert.ErrorContains(t, err, "--viewer-url must not already carry a fragment")
	_, _, err = buildPage(t, "--bundle", presentationBuildBundle, "--format", "fragment", "--max-fragment-length", "1046529")
	assert.ErrorContains(t, err, "--max-fragment-length must be 0 (the default budget, 65536) or from 1 to 1046528")
	_, err = invoke(t, "", "presentation", "list", "--bundle", presentationBuildBundle, "--max-fragment-length", "-1")
	assert.ErrorIs(t, err, ErrInput)
	// list resolves among every installed module, so it takes no --module.
	_, err = invoke(t, "", "presentation", "list", "--bundle", presentationBuildBundle, "--module", "org.example.deal-view/v0")
	assert.ErrorContains(t, err, "has no flag --module")

	// --title is the host page's for a snippet, as for TypeScript's mount.
	titled, _, err := buildPage(t, "--bundle", presentationBuildBundle, "--format", "embedded", "--title", "T")
	require.NoError(t, err)
	same, err := os.ReadFile(titled)
	require.NoError(t, err)
	assert.Equal(t, string(snippet), string(same))

	binary := os.Getenv("CAPSULECTL_CHROME")
	if binary == "" {
		t.Skip("CAPSULECTL_CHROME is not set")
	}
	csp := strings.Join([]string{"default-src 'none'", "script-src " + strings.Join(b.ScriptSources, " "),
		"style-src " + strings.Join(b.StyleSources, " "), "img-src data:", "connect-src 'none'", "base-uri 'none'", "form-action 'none'"}, "; ")
	host := filepath.Join(t.TempDir(), "host.html")
	require.NoError(t, os.WriteFile(host, []byte(`<!doctype html><html><head><meta charset="utf-8">`+
		`<meta http-equiv="Content-Security-Policy" content="`+csp+`"><title>host</title></head>`+
		`<body><p>host page</p>`+string(snippet)+`</body></html>`), 0o600))
	html, _, err := buildPage(t, "--bundle", presentationBuildBundle, "--format", "html")
	require.NoError(t, err)
	chrome := startHeadlessChrome(t, binary)
	banner := `(document.querySelector("[data-verify]") || {getAttribute(){return ""}}).getAttribute("data-verify")`
	var embedded, offline string
	chrome.open(t, "file://"+host, presentationViews[1]).eval(t, banner, &embedded)
	chrome.open(t, "file://"+html, presentationViews[1]).eval(t, banner, &offline)
	assert.NotEmpty(t, embedded)
	assert.Equal(t, offline, embedded, "the snippet draws the offline page's banner")
}

type listPackagings struct {
	Packagings        []packagingAvailability `json:"packagings"`
	PackagingsRefused string                  `json:"packagings_refused"`
}

func listPackagingsOf(t *testing.T, args ...string) (listPackagings, error) {
	t.Helper()
	stdout, err := invoke(t, "", append([]string{"presentation", "list"}, args...)...)
	var l listPackagings
	require.NoError(t, json.Unmarshal([]byte(stdout), &l), stdout)
	return l, err
}

// `presentation list` reports each packaging target for the (bundle,
// audience) and settings, and a target is available exactly when
// `presentation build` writes it with the same settings.
func TestPresentationListReportsPackagingAvailability(t *testing.T) {
	pluginRoot(t)
	for _, c := range []struct {
		bundle    string
		settings  []string
		available map[string]bool
	}{
		{presentationBuildBundle, nil, map[string]bool{"html": true, "fragment": true, "embedded": true, "static": false}},
		{monthlyOutcomeBundle, nil, map[string]bool{"html": true, "fragment": false, "embedded": true, "static": false}},
		{monthlyOutcomeBundle, []string{"--max-fragment-length", "1046528"}, map[string]bool{"html": true, "fragment": true, "embedded": true, "static": false}},
		{presentationBuildBundle, []string{"--audience", "counterparty", "--depth", "L2", "--max-fragment-length", "64"}, map[string]bool{"html": true, "fragment": false, "embedded": true, "static": false}},
	} {
		l, err := listPackagingsOf(t, append([]string{"--bundle", c.bundle}, c.settings...)...)
		require.NoError(t, err)
		require.Empty(t, l.PackagingsRefused)
		require.Len(t, l.Packagings, len(packagingTargets))
		for i, p := range l.Packagings {
			assert.Equal(t, packagingTargets[i], p.Target)
			assert.Equal(t, c.available[p.Target], p.Available, "%s %v %s", c.bundle, c.settings, p.Target)
			switch {
			case p.Target == "static":
				assert.Equal(t, "no-document", p.Reason.Code)
			case p.Target == "fragment" && !p.Available:
				assert.Equal(t, "fragment-too-large", p.Reason.Code)
				assert.Equal(t, "fragment token", p.Reason.Subject)
				assert.Greater(t, p.Reason.Length, p.Reason.Limit)
			default:
				assert.Nil(t, p.Reason)
			}
			_, _, err := buildPage(t, append([]string{"--bundle", c.bundle, "--format", p.Target}, c.settings...)...)
			assert.Equal(t, p.Available, err == nil, "build agrees with list: %s %v %s (%v)", c.bundle, c.settings, p.Target, err)
		}
	}

	// A bundle no page may be written for has no packaging, and says why.
	value := readBundleValue(t, presentationBuildBundle)
	records := value["records"].([]interface{})
	records[0].(map[string]interface{})["operator"] = "someone-else"
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	tampered := filepath.Join(t.TempDir(), "tampered.json")
	require.NoError(t, os.WriteFile(tampered, raw, 0o600))
	l, err := listPackagingsOf(t, "--bundle", tampered)
	require.Error(t, err)
	assert.Empty(t, l.Packagings)
	assert.Contains(t, l.PackagingsRefused, "no page written")
}
