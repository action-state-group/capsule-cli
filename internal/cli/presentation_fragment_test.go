package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/emitter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The page inputs these tests give `presentation build`: a theme
// stylesheet, a wording pack, and the same pack in another locale.
const (
	fragmentTheme     = ":root { --aac-page-width: 640px; --aac-fg: #202020; }\n"
	fragmentWording   = `{"wording_pack_version":"aac.wording-pack/v0","id":"example.pass/v0","locale":"en","entries":{"page.title":"Example evidence page"}}`
	fragmentWordingFR = `{"wording_pack_version":"aac.wording-pack/v0","id":"example.pass/v0","locale":"fr","entries":{"page.title":"Example evidence page"}}`
)

// fragmentBuild runs `presentation build` and returns its report and what it
// wrote ("" when nothing).
func fragmentBuild(t *testing.T, bundle string, args ...string) (buildOutput, string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	stdout, err := invoke(t, "", append([]string{"presentation", "build", "--bundle", bundle, "--out", out}, args...)...)
	var b buildOutput
	if strings.HasPrefix(stdout, "{") {
		require.NoError(t, json.Unmarshal([]byte(stdout), &b), stdout)
	}
	written, readErr := os.ReadFile(out)
	if err != nil {
		assert.True(t, os.IsNotExist(readErr), "a refused build writes nothing")
		return b, "", err
	}
	require.NoError(t, readErr)
	return b, string(written), nil
}

// --format fragment writes the page's fragment token: with every page input
// set, it hands back the html page those inputs build, byte for byte. A token
// over --max-fragment-length is refused and nothing is written.
func TestPresentationBuildWritesTheFragmentOfThePage(t *testing.T) {
	root := pluginRoot(t)
	trustPresentationsIn(t, root)
	presentationPlugin(t, root, "dealview", exampleDealFixture(t))
	dir := t.TempDir()
	theme, wording := filepath.Join(dir, "theme.css"), filepath.Join(dir, "wording.json")
	require.NoError(t, os.WriteFile(theme, []byte(fragmentTheme), 0o600))
	require.NoError(t, os.WriteFile(wording, []byte(fragmentWording), 0o600))
	inputs := []string{"--audience", "counterparty", "--depth", "L1", "--title", "An example title", "--theme-css", theme, "--wording", wording}

	b, page, err := fragmentBuild(t, presentationBuildBundle, inputs...)
	require.NoError(t, err)
	require.Len(t, b.Modules, 1)
	assert.Equal(t, "not passed: the page carries one wording pack, and --wording gives it", b.Modules[0]["wording"])
	assert.Contains(t, page, `"sha256":"`+hexSHA256([]byte(fragmentWording))+`"`, "--wording's pack is the page's")
	assert.Contains(t, page, fragmentTheme)

	stdout, err := invoke(t, "", append([]string{"presentation", "build", "--bundle", presentationBuildBundle, "--out", filepath.Join(dir, "token"), "--format", "fragment"}, inputs...)...)
	require.NoError(t, err)
	var fb struct {
		Page           string `json:"page"`
		Format         string `json:"format"`
		FragmentLength int    `json:"fragment_length"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &fb))
	assert.Equal(t, "written", fb.Page)
	assert.Equal(t, "fragment", fb.Format)
	token, err := os.ReadFile(filepath.Join(dir, "token"))
	require.NoError(t, err)
	assert.Equal(t, len(token), fb.FragmentLength)
	payload, err := emitter.DecodePresentationFragment(string(token), 0)
	require.NoError(t, err)
	assert.Equal(t, "counterparty", payload.Audience)
	assert.Equal(t, "aac.builtin.no-aggregate/v0", payload.Presentation, "the module the resolver selects for a fragment: the example module and the deal view are html only")
	assert.Equal(t, evidenceGraphIIFEDigest, payload.CoreRuntimeSHA256)
	module := exampleDealFixture(t)
	back, err := emitter.OfflineHTMLFromFragment(string(token), evidenceGraphIIFE, []emitter.Module{{Code: module.files["view.js"],
		SHA256: hexSHA256(module.files["view.js"]), StyleSHA256: []string{hexSHA256(fixtureStyle)}}})
	require.NoError(t, err)
	assert.Equal(t, page, back)

	out := filepath.Join(dir, "short")
	stdout, err = invoke(t, "", "presentation", "build", "--bundle", presentationBuildBundle, "--out", out, "--format", "fragment", "--max-fragment-length", "10")
	require.ErrorContains(t, err, "over the 10-character link budget")
	refused := decodeBuild(t, stdout)
	assert.Equal(t, "not written", refused.Page)
	assert.Equal(t, "fragment_too_large", refused.Refusal["reason"])
	assert.EqualValues(t, 10, refused.Refusal["limit"])
	assert.Greater(t, refused.Refusal["length"], refused.Refusal["limit"])
	assert.NoFileExists(t, out)
}

// The page inputs are checked before anything is written.
func TestPresentationBuildRefusesBadPageInputs(t *testing.T) {
	pluginRoot(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"entries": {"page.title": "x"}}`), 0o600))
	closing := filepath.Join(dir, "closing.css")
	require.NoError(t, os.WriteFile(closing, []byte("p {} </style><script>x()</script>"), 0o600))
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"a budget of 0":                         {[]string{"--format", "fragment", "--max-fragment-length", "0"}, "--max-fragment-length must be from 1 to 1046528"},
		"a budget over the maximum":             {[]string{"--format", "fragment", "--max-fragment-length", "1046529"}, "--max-fragment-length must be from 1 to 1046528"},
		"a wording that is no pack":             {[]string{"--wording", bad}, "--wording " + bad + ": wording pack is not an aac.wording-pack/v0 object"},
		"a theme that closes its style element": {[]string{"--theme-css", closing}, "no page written"},
	} {
		t.Run(name, func(t *testing.T) {
			// fragmentBuild also holds that nothing was written.
			_, _, err := fragmentBuild(t, presentationBuildBundle, c.args...)
			assert.ErrorContains(t, err, c.want)
		})
	}
}

// A fragment that does not hand back the html page byte for byte is never
// written: the build fails and --out is not created.
func TestPresentationBuildWritesNoFragmentThatDoesNotHandBackThePage(t *testing.T) {
	pluginRoot(t)
	old := presentationHandBack
	t.Cleanup(func() { presentationHandBack = old })
	for name, handBack := range map[string]func(string, []byte, []emitter.Module) (string, error){
		"another page": func(token string, runtime []byte, modules []emitter.Module) (string, error) {
			page, err := old(token, runtime, modules)
			return page + "<!-- not the page -->", err
		},
		"an error": func(string, []byte, []emitter.Module) (string, error) {
			return "", errors.New("example hand-back failure")
		},
	} {
		t.Run(name, func(t *testing.T) {
			presentationHandBack = handBack
			_, written, err := fragmentBuild(t, presentationBuildBundle, "--format", "fragment")
			require.ErrorContains(t, err, "no fragment written: the fragment does not hand back the html page")
			assert.Empty(t, written)
		})
	}
	presentationHandBack = old
	_, written, err := fragmentBuild(t, presentationBuildBundle, "--format", "fragment")
	require.NoError(t, err)
	assert.NotEmpty(t, written, "the real hand-back writes the fragment")
}

var fragmentBundleScript = regexp.MustCompile(`(?m)^\s*<script>window\.__BUNDLE__ = (.*);</script>$`)

// --theme-css and --wording (and the pack's locale) change the page's bytes
// and never the bundle it carries.
func TestPresentationBuildThemeAndWordingLeaveTheBundle(t *testing.T) {
	pluginRoot(t)
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}
	bundleOf := func(page string) string {
		m := fragmentBundleScript.FindStringSubmatch(page)
		require.NotNil(t, m, "the page carries its bundle")
		return m[1]
	}
	_, base, err := fragmentBuild(t, presentationBuildBundle)
	require.NoError(t, err)
	_, themed, err := fragmentBuild(t, presentationBuildBundle, "--theme-css", write("theme.css", fragmentTheme))
	require.NoError(t, err)
	_, worded, err := fragmentBuild(t, presentationBuildBundle, "--wording", write("en.json", fragmentWording))
	require.NoError(t, err)
	_, localed, err := fragmentBuild(t, presentationBuildBundle, "--wording", write("fr.json", fragmentWordingFR))
	require.NoError(t, err)
	for name, pair := range map[string][2]string{"theme": {base, themed}, "wording": {base, worded}, "locale": {worded, localed}} {
		assert.NotEqual(t, pair[0], pair[1], "the %s flip changes the page", name)
		assert.Equal(t, bundleOf(pair[0]), bundleOf(pair[1]), "the %s flip leaves the bundle's bytes", name)
	}
}
