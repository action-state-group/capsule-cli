package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The deal view is pinned by digest like the runtime it runs beside.
func TestDealViewIsPinned(t *testing.T) {
	sum := sha256.Sum256([]byte(dealViewJS))
	assert.Equal(t, strings.TrimSpace(dealViewJSSHA256), hex.EncodeToString(sum[:]), "update assets/deal-view.js.sha256 with the file")
}

func cspSource(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

var (
	pageCSP     = regexp.MustCompile(`<meta http-equiv="Content-Security-Policy" content="([^"]*)"`)
	pageScripts = regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	pageStyles  = regexp.MustCompile(`(?s)<style>(.*?)</style>`)
)

// A deal page is built through the emitter's slots: its CSP pins every
// inline script and style the page holds and allows no network, the deal
// view runs as the one digest-pinned module, and nothing is spliced in.
func TestTheDealPageIsBuiltThroughTheEmittersSlots(t *testing.T) {
	dealFixture(t)
	id := retailDeal(t)
	path := filepath.Join(t.TempDir(), "page.html")
	dealRun(t, "report", "--deal", id, "--html", path)
	page := string(mustRead(t, path))

	m := pageCSP.FindStringSubmatch(page)
	require.NotNil(t, m, "the page carries a CSP")
	directives := map[string]string{}
	for _, d := range strings.Split(m[1], ";") {
		d = strings.TrimSpace(d)
		name, value, _ := strings.Cut(d, " ")
		directives[name] = value
	}
	assert.Equal(t, "'none'", directives["default-src"])
	assert.Equal(t, "'none'", directives["connect-src"], "no network")
	scripts := pageScripts.FindAllStringSubmatch(page, -1)
	require.NotEmpty(t, scripts)
	for _, s := range scripts {
		assert.Contains(t, directives["script-src"], cspSource(s[1]), "every inline script is pinned: %.60s", s[1])
	}
	for _, s := range pageStyles.FindAllStringSubmatch(page, -1) {
		assert.Contains(t, directives["style-src"], cspSource(s[1]), "every inline style is pinned")
	}

	module := 0
	readers := 0
	for _, s := range scripts {
		if s[1] == dealViewJS {
			module++
		}
		if strings.Contains(s[1], "__BUNDLE__") {
			readers++
			assert.True(t, strings.HasPrefix(s[1], "window.__BUNDLE__ = ") || strings.Contains(s[1], "buildVerifiedBundleContext(window.__BUNDLE__)"),
				"only the bundle slot and the bootstrap name the bundle: %.80s", s[1])
		}
	}
	assert.Equal(t, 1, module, "the deal view is one module")
	assert.Equal(t, 2, readers, "the bundle slot sets it, the bootstrap reads it")
	assert.Contains(t, directives["script-src"], cspSource(dealViewJS))
	assert.NotContains(t, page, `id="deal-countersign"`, "nothing beside the emitter's slots")
	assert.Regexp(t, `<title>Deal report: “[^<]+”</title>`, page, "the user's own copy is titled with the words they asked")
}

// The deal view never verifies, never reads the bundle itself and never
// recomputes a commitment: it reads the verified context it is handed.
func TestTheDealViewReadsOnlyTheVerifiedContext(t *testing.T) {
	for _, forbidden := range []string{"__BUNDLE__", "verifyBundle", "crypto.subtle", "bundle.disclosures", "bundle.records"} {
		assert.NotContains(t, dealViewJS, forbidden)
	}
	assert.Contains(t, dealViewJS, "EvidenceGraph.verifiedPayload(context")
}

type pageOpenings struct {
	Asked           bool   `json:"asked"`
	Representations []bool `json:"representations"`
}

func pageOpeningsOf(t *testing.T, path string) pageOpenings {
	t.Helper()
	var data struct {
		Openings pageOpenings `json:"openings"`
	}
	require.NoError(t, json.Unmarshal(pageData(t, mustRead(t, path)), &data))
	return data.Openings
}

func sellerWithRepresentations(t *testing.T) string {
	t.Helper()
	dealFixture(t)
	id := openTyped(t, sellerTyped)
	for _, body := range []string{
		`{"text": "The frame has one small scratch by the seat", "source_kind": "agent", "class": "condition"}`,
		`{"text": "Returns accepted within 7 days if unridden", "source_kind": "agent", "class": "refund_terms"}`,
		`{"text": "I paid 2400 for it new", "source_kind": "user"}`,
	} {
		dealRun(t, "note", "--deal", id, "--kind", "claim", "--input", writeJSON(t, body))
	}
	return id
}

// capsulectl checks the words a page shows against their sealed commitments
// when it writes the page, and the page says what it found: the user's words
// on their own copy, and each statement the agent made on every copy.
func TestTheDealPageChecksItsWordsWhenItIsBuilt(t *testing.T) {
	id := sellerWithRepresentations(t)
	dir := t.TempDir()
	own := filepath.Join(dir, "own.html")
	dealRun(t, "report", "--deal", id, "--html", own)
	got := pageOpeningsOf(t, own)
	assert.True(t, got.Asked, "the user's words, checked")
	assert.Equal(t, []bool{true, true}, got.Representations, "each statement the agent made, checked")

	shared := filepath.Join(dir, "buyer.html")
	dealRun(t, "report", "--deal", id, "--share", dealAudienceCounterparty, "--to", "the buyer", "--html", shared)
	got = pageOpeningsOf(t, shared)
	assert.False(t, got.Asked, "the user's words are withheld from the buyer")
	assert.Equal(t, []bool{true, true}, got.Representations)
}

// A page whose words do not recompute to their sealed commitment is never
// written, and the refusal names the step, never the words.
func TestADealPageWhoseWordsDoNotMatchIsNotWritten(t *testing.T) {
	id := sellerWithRepresentations(t)
	path := filepath.Join(t.TempDir(), "bundle.json")
	dealRun(t, "report", "--deal", id, "--bundle", path)
	fresh := func() map[string]interface{} {
		b, err := decodeBundleJSON(mustRead(t, path))
		require.NoError(t, err)
		return b
	}
	report := func(b map[string]interface{}) map[string]interface{} {
		id := b["extensions"].(map[string]interface{})[dealProfile].(map[string]interface{})["sealed_report"].(string)
		return b["disclosures"].(map[string]interface{})[id].(map[string]interface{})["agent_input"].(map[string]interface{})["report"].(map[string]interface{})
	}
	_, err := dealPageOpenings(fresh())
	require.NoError(t, err, "the deal as sealed")

	asked := fresh()
	report(asked)["asked_opening"].(map[string]interface{})["text"] = "Sell it for 100"
	_, err = dealPageOpenings(asked)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the words you asked do not match")
	assert.NotContains(t, err.Error(), "Sell it for 100")

	said := fresh()
	report(said)["representations"].([]interface{})[0].(map[string]interface{})["text"] = "No scratches at all"
	_, err = dealPageOpenings(said)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a statement the agent made does not match")
	assert.NotContains(t, err.Error(), "No scratches at all")
}

// The deal view is a presentation module: a schema-valid manifest that pins
// its script and its one stylesheet, registered by the script itself at
// load with the same manifest (but for the script's own digest, which a
// script cannot carry), and its stylesheet is the committed file's bytes.
func TestTheDealViewIsAPresentationModule(t *testing.T) {
	schema, err := presentationSchema("manifest")
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(dealViewManifestJSON))
	require.NoError(t, err)
	require.NoError(t, schema.Validate(doc))

	var manifest map[string]interface{}
	require.NoError(t, json.Unmarshal(dealViewManifestJSON, &manifest))
	assert.Equal(t, "capsulectl.deal-view/v0", manifest["id"])
	css := mustRead(t, "assets/deal-view.css")
	executable := manifest["executable"].(map[string]interface{})
	assert.Equal(t, "module-slot", executable["carrier"])
	assert.Equal(t, strings.TrimSpace(dealViewJSSHA256), executable["script_sha256"])
	assert.Equal(t, []interface{}{hexSHA256(css)}, executable["style_sha256"])

	m := regexp.MustCompile("(?s)const DEAL_VIEW_CSS = `(.*?)`;").FindStringSubmatch(dealViewJS)
	require.NotNil(t, m, "the script carries its stylesheet")
	assert.Equal(t, string(css), m[1], "assets/deal-view.css and the script's stylesheet are the same bytes")

	code := regexp.MustCompile(`EvidenceGraph\.registerPresentation\(\{\n  manifest: (\{.*\}),\n`).FindStringSubmatch(dealViewJS)
	require.NotNil(t, code, "the script registers its module at load")
	assert.Equal(t, strings.Repeat("0", 64), regexp.MustCompile(`"script_sha256": "([0-9a-f]{64})"`).FindStringSubmatch(code[1])[1])
	var inCode map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(code[1]), &inCode))
	inCode["executable"].(map[string]interface{})["script_sha256"] = executable["script_sha256"]
	assert.Equal(t, manifest, inCode)
	assert.Equal(t, 1, strings.Count(dealViewJS, "registerPresentation("), "one module")
	assert.NotContains(t, dealViewJS, "capsulectlDealView =", "no entry point beside the module")

	module, err := dealViewModule()
	require.NoError(t, err)
	assert.Equal(t, []string{hexSHA256(css)}, module.StyleSHA256)
}

// The page's theme is tokens only; the deal view's layout is its own,
// pinned, stylesheet, and the page's style-src lists it.
func TestTheDealPageThemeIsTokensOnly(t *testing.T) {
	for _, line := range strings.Split(strings.TrimSpace(dealThemeCSS), "\n") {
		assert.Regexp(t, `^(@media \(prefers-color-scheme: dark\) \{ )?:root \{( --aac-[a-z-]+: [^;{}]+;)+ \}( \})?$`, line)
	}
	dealFixture(t)
	id := retailDeal(t)
	path := filepath.Join(t.TempDir(), "page.html")
	dealRun(t, "report", "--deal", id, "--html", path)
	page := string(mustRead(t, path))
	for _, s := range pageStyles.FindAllStringSubmatch(page, -1) {
		assert.NotContains(t, s[1], "#deal", "the deal view's rules are not in the page's own styles")
	}
	assert.Contains(t, pageCSP.FindStringSubmatch(page)[1], cspSource(string(mustRead(t, "assets/deal-view.css"))))
}

// A deal view the page's runtime would refuse, one ambiguous with a
// built-in, or one whose manifest does not pin its script writes no page.
func TestADealViewThePageWouldRefuseWritesNoPage(t *testing.T) {
	for name, edit := range map[string]func(m map[string]interface{}){
		"runtime":   func(m map[string]interface{}) { m["runtime_min"] = "0.2.0" },
		"api":       func(m map[string]interface{}) { m["presentation_api"] = "aac.presentation-api/v1" },
		"ambiguous": func(m map[string]interface{}) { delete(m, "forbids") },
		"unpinned": func(m map[string]interface{}) {
			m["executable"].(map[string]interface{})["script_sha256"] = strings.Repeat("0", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var copied map[string]interface{}
			require.NoError(t, json.Unmarshal(dealViewManifestJSON, &copied))
			edit(copied)
			raw, err := json.Marshal(copied)
			require.NoError(t, err)
			old := dealViewManifestJSON
			dealViewManifestJSON = raw
			t.Cleanup(func() { dealViewManifestJSON = old })
			dealFixture(t)
			id := retailDeal(t)
			path := filepath.Join(t.TempDir(), "page.html")
			_, err = invoke(t, "", "--profile", "deal", "deal", "report", "--deal", id, "--html", path)
			require.Error(t, err)
			assert.NoFileExists(t, path)
		})
	}
}

// The page's title, what a tab or a host's preview shows before any script
// runs, says only what that copy already shows in the clear: the user's own
// copy quotes the words they asked (checked when the page was built); a
// shared copy, from which those words are withheld, says only whom it is
// for.
func TestTheDealPageTitleSaysOnlyWhatTheCopyShows(t *testing.T) {
	dealFixture(t)
	id := retailDeal(t)
	asked := "Order the cat sticker in my cart at Example Stickers"
	title := func(page string) string {
		m := regexp.MustCompile(`<title>([^<]*)</title>`).FindStringSubmatch(page)
		require.NotNil(t, m)
		return m[1]
	}
	dir := t.TempDir()
	own := filepath.Join(dir, "own.html")
	dealRun(t, "report", "--deal", id, "--html", own)
	assert.Equal(t, "Deal report: “"+asked+"”", title(string(mustRead(t, own))))
	for audience, want := range map[string]string{
		"counterparty": "Deal report: a shared copy for the other party",
		"adjudicator":  "Deal report: a shared copy for an adjudicator",
	} {
		path := filepath.Join(dir, audience+".html")
		dealRun(t, "report", "--deal", id, "--html", path, "--share", audience, "--to", "the shop's support desk")
		assert.Equal(t, want, title(string(mustRead(t, path))), audience)
	}

	report := func(audience string, text string) map[string]interface{} {
		return map[string]interface{}{"extensions": map[string]interface{}{dealProfile: map[string]interface{}{
			"audience": audience, "asked_opening": map[string]interface{}{"nonce": "n", "text": text}}}}
	}
	assert.Equal(t, "Deal report", dealPageTitle(report("keep", asked), map[string]interface{}{"asked": false}), "words not checked when the page was built are not quoted")
	assert.Equal(t, "Deal report: “a b”", dealPageTitle(report("keep", " a\n\tb "), map[string]interface{}{"asked": true}))
	long := dealPageTitle(report("keep", strings.Repeat("x", 500)), map[string]interface{}{"asked": true})
	assert.Equal(t, "Deal report: “"+strings.Repeat("x", dealPageTitleWords-1)+"…”", long)
	assert.Equal(t, "Deal report: a shared copy for the other party", dealPageTitle(report(dealAudienceCounterparty, asked), map[string]interface{}{"asked": true}), "a shared copy never quotes them")
}
