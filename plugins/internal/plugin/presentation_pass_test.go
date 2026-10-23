package plugin

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/action-state-group/capsule-viewer/go/emitter"
	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPresentationPass(t *testing.T) {
	passIsolate(t)
	chrome := passChrome(t)
	update := os.Getenv("CAPSULECTL_UPDATE_PRESENTATION_PASS") == "1"
	for _, f := range passFixtures {
		t.Run(f.dir, func(t *testing.T) {
			dir := filepath.Join(presentationDir, f.dir)
			bundlePath := filepath.Join(dir, "bundle.json")
			raw, err := os.ReadFile(bundlePath)
			require.NoError(t, err)
			value, err := cli.DecodeBundleJSON(raw)
			require.NoError(t, err)
			var meta presentationMeta
			readJSONFile(t, filepath.Join(dir, "meta.json"), &meta)
			listOther := f.listOther
			if listOther == "" {
				listOther = f.list
			}
			var want presentationSnapshot
			var seen *presentationSnapshot
			for _, audience := range passAudiences {
				// What list says, per format.
				for format, wantID := range map[string]string{"html": f.list, "fragment": listOther, "embedded": listOther} {
					id, err := passList(t, bundlePath, audience, format)
					assert.Equal(t, wantID, id, "presentation list --audience %s --format %s", audience, format)
					assert.NoError(t, err)
				}
				// Packagings this tool does not build are refusals, for every bundle.
				for _, format := range []string{"fragment", "embedded"} {
					_, _, err := passBuild(t, bundlePath, "--audience", audience, "--format", format)
					assert.ErrorContains(t, err, "--format "+format+" is not built by this tool yet")
				}
				_, _, err := passBuild(t, bundlePath, "--audience", audience, "--format", "static")
				assert.ErrorContains(t, err, "--format static is unavailable (no-document)")

				b, page, err := passBuild(t, bundlePath, "--audience", audience)
				require.NoError(t, err)
				require.Equal(t, "written", b.Page)
				require.Empty(t, b.Modules, "no plugin is installed: the page holds the built-ins only")

				// One builder: the page is agent-action-capsule's offline
				// packaging of the bundle as given, for this audience.
				packaged, err := emitter.BuildOfflineHTML(value, cli.EvidenceGraphIIFE, emitter.OfflineOptions{Audience: audience, CoreRuntimeSHA256: cli.EvidenceGraphIIFEDigest})
				require.NoError(t, err)
				require.Equal(t, packaged, page, "presentation build's page is the offline packaging of the bundle")

				if chrome == nil {
					continue
				}
				got := passSnapshots(t, chrome, page)
				assert.Contains(t, append(append([]string{}, got.Views.Page...), got.Views.Notice...), f.view,
					"the page renders with %s, the module list selects (%s)", f.view, f.list)
				if seen != nil {
					// Every built-in is for every audience: the page shows
					// a counterparty what it shows anyone.
					passSameSnapshot(t, *seen, got)
					continue
				}
				seen = &got
				golden := filepath.Join(dir, "snapshot.json")
				if f.differs != "" {
					golden = filepath.Join(dir, "build-snapshot.json")
					if update {
						writeSnapshotFile(t, golden, got)
					}
					// The golden's own snapshot must still differ, or the
					// fixture reproduces it now and differs is stale.
					var theirs presentationSnapshot
					readJSONFile(t, filepath.Join(dir, "snapshot.json"), &theirs)
					assert.NotEqual(t, passMarshal(t, theirs), passMarshal(t, got), "the build reproduces the golden now: clear differs")
				}
				readJSONFile(t, golden, &want)
				passSameSnapshot(t, want, got)
				if f.differs == passCardDiffers {
					passSameSnapshot(t, passWithoutCard(t, dir, value, meta), got)
				}
			}
			if f.dir == "6-generic-fallback-unknown-extension" && seen != nil {
				assert.Contains(t, strings.Join(seen.Text, " "), "x-example-unknown/v1 covered Covered by the file's fingerprint; not interpreted by this page's checks")
			}
		})
	}
}

// passWithoutCard is the golden's snapshot (a report build page) as it reads
// without what report build adds: the card's extension row, and the digest
// of the bundle with the card block in place of the bundle's own. Build's
// page must show exactly this.
func passWithoutCard(t *testing.T, dir string, value map[string]interface{}, meta presentationMeta) presentationSnapshot {
	t.Helper()
	var golden presentationSnapshot
	readJSONFile(t, filepath.Join(dir, "snapshot.json"), &golden)
	own, err := aacbundle.BundleDigest(value)
	require.NoError(t, err)
	carded, err := cli.DecodeBundleJSON([]byte(passMarshal(t, value)))
	require.NoError(t, err)
	require.NoError(t, setExtension(carded, cli.CardExtension, map[string]interface{}{"card": meta.Card}))
	withCard, err := aacbundle.BundleDigest(carded)
	require.NoError(t, err)
	words := strings.Join(strings.Fields(strings.Join(golden.Text, " ")), " ")
	require.Equal(t, 1, strings.Count(words, passCardRow), "the golden shows the card's extension row once")
	require.Contains(t, words, withCard, "the golden shows the carded bundle's digest")
	words = strings.ReplaceAll(strings.Replace(words, passCardRow+" ", "", 1), withCard, own)
	golden.Text = []string{words}
	for i, id := range golden.EvidenceIDs {
		if id == withCard {
			golden.EvidenceIDs[i] = own
		}
	}
	sort.Strings(golden.EvidenceIDs)
	return golden
}

// The presentation pass: the presentation goldens' bundles
// (testdata/presentation) driven through the one registry, shell and builder
// capsulectl has, `presentation list` and `presentation build`, as a reader
// would get them:
//
//   - each bundle's html page is agent-action-capsule's own offline
//     packaging of the bundle (emitter.BuildOfflineHTML), per audience, so
//     no bundle kind has a renderer of its own. The packagings this tool
//     does not build are refusals the test names: fragment and embedded are
//     not built and static is unavailable, for every bundle;
//   - flipping the wording pack, its locale or the default depth changes the
//     page's bytes and leaves the bundle it carries, and (in Chrome) its
//     evidence identifiers and verification state, byte-identical;
//   - a failing bundle gets no page, and a page that carries it
//     anyway shows the refusal and nothing else. A record signed by another
//     key than the one it names gets no page either; the page's
//     own gate does not check producer signatures (TestPresentationPassWrongSigner).
//
// Without CAPSULECTL_CHROME, every check that reads bytes runs. With it, each
// html page is also opened at 390px, 1280px and in print: its semantic
// snapshot (verification state, views, evidence identifiers and words, never
// layout) must be the same in all three, for both audiences, and equal the
// committed one: the golden's own snapshot.json where the build reproduces
// it, else build-snapshot.json (passFixture.differs says why).
// CAPSULECTL_UPDATE_PRESENTATION_PASS=1 rewrites build-snapshot.json and
// refusal-snapshot.json.

// passFixture is one golden bundle and what the pass expects of it.
type passFixture struct {
	dir string
	// list is the module `presentation list` selects for html, fragment and
	// embedded.
	list, listOther string
	// view is the page's own mark of the module it rendered: a data-page
	// or data-notice value.
	view string
	// differs is why build's page is not the golden's (built by
	// build-fixtures.sh through another command), or "" when the golden's
	// snapshot.json is reproduced.
	differs string
}

const (
	passNoAggregate = "aac.builtin.no-aggregate/v0"
	passDealView    = "capsulectl.deal-view/v0"
	// The deal view is a module `presentation list` offers and `presentation
	// build` does not carry (an open question; see TestPresentationPassDealViewNotCarried).
	passDealDiffers = "deal report --html built the golden with the deal view; presentation build carries no deal view, so its page is the fallback"
	passCardDiffers = "report build stamps the card extension into the bundle; presentation build packages the bundle as given, so the bundle digest differs"
)

var passFixtures = []passFixture{
	{dir: "1-rules-comparison", list: "aac.builtin.report-rows/v0", view: "report-rows"},
	{dir: "2-unilateral-receipt", list: passDealView, listOther: passNoAggregate, view: "no-aggregate", differs: passDealDiffers},
	{dir: "3-bilateral-composed", list: passNoAggregate, view: "no-aggregate"},
	{dir: "4-monthly-outcome", list: "aac.builtin.result-outcome-report/v0", view: "outcome-report", differs: passCardDiffers},
	{dir: "5-monthly-compliance", list: "aac.builtin.result-compliance/v0", view: "compliance", differs: passCardDiffers},
	{dir: "6-generic-fallback", list: passNoAggregate, view: "no-aggregate"},
	{dir: "6-generic-fallback-unknown-extension", list: passNoAggregate, view: "no-aggregate"},
	{dir: "7-receipt-declared-rules", list: passDealView, listOther: passNoAggregate, view: "no-aggregate", differs: passDealDiffers},
	{dir: "8-seller-buyer-copy", list: passDealView, listOther: passNoAggregate, view: "no-aggregate", differs: passDealDiffers},
	{dir: "9-seller-later-evidence", list: passDealView, listOther: passNoAggregate, view: "no-aggregate", differs: passDealDiffers},
}

var passAudiences = []string{"*", "counterparty"}

// passBuild runs `presentation build` and returns its report and what it
// wrote ("" when nothing).
func passBuild(t *testing.T, bundle string, args ...string) (buildOutput, string, error) {
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

// passList runs `presentation list` and returns the selected module's id, or
// "" with the command's error.
func passList(t *testing.T, bundle, audience, format string) (string, error) {
	t.Helper()
	stdout, err := invoke(t, "", "presentation", "list", "--bundle", bundle, "--audience", audience, "--format", format)
	var r struct {
		Resolution string `json:"resolution"`
		Selected   *struct {
			ID     string `json:"id"`
			Source string `json:"source"`
		} `json:"selected"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &r), stdout)
	if r.Selected == nil {
		return "", err
	}
	return r.Selected.ID, err
}

var passBundleScript = regexp.MustCompile(`(?m)^\s*<script>window\.__BUNDLE__ = (.*);</script>$`)

// passEmbeddedBundle is the bundle a page carries, exactly as it carries it.
func passEmbeddedBundle(t *testing.T, page string) string {
	t.Helper()
	m := passBundleScript.FindStringSubmatch(page)
	require.NotNil(t, m, "the page carries its bundle")
	return m[1]
}

// passSnapshots opens a page in every view and returns its snapshot, after
// checking that the views agree: what a reader is shown does not depend on
// the width or the medium.
func passSnapshots(t *testing.T, chrome *headlessChrome, page string) presentationSnapshot {
	t.Helper()
	path := filepath.Join(t.TempDir(), "page.html")
	require.NoError(t, os.WriteFile(path, []byte(page), 0o600))
	var first presentationSnapshot
	for i, view := range presentationViews {
		var got presentationSnapshot
		chrome.open(t, "file://"+path, view).eval(t, snapshotScript, &got)
		if i == 0 {
			first = got
			continue
		}
		assert.Equal(t, first.Verification, got.Verification, "verification state at %s", view.name)
		assert.Equal(t, first.Views, got.Views, "views at %s", view.name)
		assert.Equal(t, first.EvidenceIDs, got.EvidenceIDs, "evidence identifiers at %s", view.name)
		assertSameWords(t, first.Text, got.Text)
	}
	return first
}

// passSameSnapshot holds got to want: the same verification state, views and
// evidence identifiers, and the same words in order.
func passSameSnapshot(t *testing.T, want, got presentationSnapshot) {
	t.Helper()
	assert.Equal(t, want.Verification, got.Verification, "verification state")
	assert.Equal(t, want.Views, got.Views, "views")
	assert.Equal(t, want.EvidenceIDs, got.EvidenceIDs, "evidence identifiers")
	assertSameWords(t, want.Text, got.Text)
}

func passMarshal(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}

// passIsolate gives the commands no installed plugin and no configuration:
// the pages hold the built-ins only.
func passIsolate(t *testing.T) {
	t.Helper()
	pluginRoot(t)
	trustPresentationsIn(t, t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// passChrome is the Chrome the pages are opened in, or nil. With
// CAPSULECTL_PRESENTATION_PINNED=1 (CI) it must be set, and be the Chrome
// known-overflow.json names, so a missing browser cannot pass as a skip.
func passChrome(t *testing.T) *headlessChrome {
	t.Helper()
	pinned := os.Getenv("CAPSULECTL_PRESENTATION_PINNED") == "1"
	if binary := os.Getenv("CAPSULECTL_CHROME"); binary != "" {
		c := startHeadlessChrome(t, binary)
		if pinned {
			var baseline overflowBaseline
			readJSONFile(t, filepath.Join(presentationDir, "known-overflow.json"), &baseline)
			product := c.version(t)
			require.Equal(t, baseline.Environment.Chrome, product[strings.LastIndex(product, "/")+1:], "the pinned Chrome")
		}
		return c
	}
	require.False(t, pinned, "CAPSULECTL_PRESENTATION_PINNED needs CAPSULECTL_CHROME")
	t.Log("CAPSULECTL_CHROME is not set: the pages are checked as bytes only, and no page is opened")
	return nil
}

// passCardRow is the extension row the card block adds to a report build
// page: an extension the viewer does not interpret.
const passCardRow = cli.CardExtension + " covered Covered by the file's fingerprint; not interpreted by this page's checks"

// The deal view is offered by list and not carried by build (the open
// question of whether build should register it): list names it, build's page
// holds no module and falls back. When build starts carrying it, this test
// fails, and the deal fixtures' expectations above change with it.
func TestPresentationPassDealViewNotCarried(t *testing.T) {
	passIsolate(t)
	bundle := filepath.Join(presentationDir, "2-unilateral-receipt", "bundle.json")
	id, err := passList(t, bundle, "*", "html")
	require.NoError(t, err)
	assert.Equal(t, passDealView, id)
	b, page, err := passBuild(t, bundle)
	require.NoError(t, err)
	assert.Empty(t, b.Modules)
	assert.NotContains(t, page, strings.TrimSpace(cli.DealViewJSSHA256), "the page does not carry the deal view")
}

// passWording, passWordingOther and passWordingFR are the wording flips'
// packs: one pack, other words, and the first pack in another locale.
const (
	passWording      = `{"wording_pack_version":"aac.wording-pack/v0","id":"example.pass/v0","locale":"en","entries":{"page.title":"Example evidence page"}}`
	passWordingOther = `{"wording_pack_version":"aac.wording-pack/v0","id":"example.pass/v0","locale":"en","entries":{"page.title":"Another example page"}}`
	passWordingFR    = `{"wording_pack_version":"aac.wording-pack/v0","id":"example.pass/v0","locale":"fr","entries":{"page.title":"Example evidence page"}}`
)

// passWordingPlugin installs one plugin offering one module with the given
// wording pack, so the page carries that pack (a page holds a single
// module's pack). The module's script registers nothing, and its manifest
// forbids what the specific built-ins require, so the page resolves as it
// does without it.
func passWordingPlugin(t *testing.T, pack string) {
	t.Helper()
	root := pluginRoot(t)
	trustPresentationsIn(t, root)
	p := moduleSlotFixture()
	p.manifest = exampleDealManifest(hexSHA256(fixtureScript))
	p.manifest["id"] = "org.example.wording/v0"
	p.manifest["executable"].(map[string]any)["wording_sha256"] = hexSHA256([]byte(pack))
	p.files["wording.json"] = []byte(pack)
	p.entries[2]["sha256"] = hexSHA256([]byte(pack))
	presentationPlugin(t, root, "wording", p)
}

// Flipping the wording, its locale or the default depth changes the page's
// bytes (its bootstrap) and never the evidence it shows: its bundle bytes,
// and (in Chrome, at 1280px) its evidence identifiers and verification
// state, are byte-identical. The wording reaches the page the one way this
// tool has, an installed module's pack. This tool has no theme input, so
// the theme is not flipped. Whether a flip changes what a reader sees is
// not checked: the built-in modules do not read the wording pack, and the
// runtime does not read its locale.
func TestPresentationPassFlipsLeaveTheEvidence(t *testing.T) {
	passIsolate(t)
	chrome := passChrome(t)
	flips := []struct {
		name string
		// pack is the installed module's wording pack ("" for none) and
		// args the build's own; from is the flip's other side: the base
		// page, or another flip.
		pack string
		args []string
		from string
	}{
		{"wording", passWording, nil, "wording other"},
		{"wording other", passWordingOther, nil, "wording"},
		{"locale", passWordingFR, nil, "wording"},
		{"depth L0", "", []string{"--depth", "L0"}, ""},
		{"depth L1", "", []string{"--depth", "L1"}, "depth L0"},
		{"depth L2", "", []string{"--depth", "L2"}, "depth L1"},
	}
	for _, f := range passFixtures {
		t.Run(f.dir, func(t *testing.T) {
			bundle := filepath.Join(presentationDir, f.dir, "bundle.json")
			pages := map[string]string{}
			_, pages[""], _ = passBuild(t, bundle)
			require.NotEmpty(t, pages[""])
			for _, flip := range flips {
				if flip.pack != "" {
					passWordingPlugin(t, flip.pack)
				} else {
					passIsolate(t)
				}
				b, page, err := passBuild(t, bundle, flip.args...)
				require.NoError(t, err, flip.name)
				if flip.pack != "" {
					require.Len(t, b.Modules, 1)
					require.Equal(t, "passed", b.Modules[0]["wording"], "the module's pack is the page's")
				}
				pages[flip.name] = page
			}
			for _, flip := range flips {
				page := pages[flip.name]
				assert.NotEqual(t, pages[flip.from], page, "the %s flip changes the page", flip.name)
				assert.Equal(t, passEmbeddedBundle(t, pages[flip.from]), passEmbeddedBundle(t, page), "the %s flip leaves the bundle's bytes", flip.name)
			}
			if chrome == nil {
				return
			}
			evidence := func(page string) presentationSnapshot {
				path := filepath.Join(t.TempDir(), "page.html")
				require.NoError(t, os.WriteFile(path, []byte(page), 0o600))
				var s presentationSnapshot
				chrome.open(t, "file://"+path, presentationViews[1]).eval(t, snapshotScript, &s)
				require.NotEmpty(t, s.EvidenceIDs)
				return s
			}
			base := evidence(pages[""])
			for _, flip := range flips {
				got := evidence(pages[flip.name])
				if strings.HasPrefix(flip.name, "depth ") {
					// A deeper level opens more of the page (presentation
					// contract section 9.1): it may add identifiers and
					// checks, never drop one, and the verdict is the same.
					for _, id := range base.EvidenceIDs {
						assert.Contains(t, got.EvidenceIDs, id, "the %s flip keeps every evidence identifier", flip.name)
					}
					assert.Equal(t, base.Verification.Verify, got.Verification.Verify, "the %s flip leaves the verification state", flip.name)
					continue
				}
				assert.Equal(t, passMarshal(t, base.EvidenceIDs), passMarshal(t, got.EvidenceIDs), "the %s flip leaves every evidence identifier", flip.name)
				assert.Equal(t, passMarshal(t, base.Verification), passMarshal(t, got.Verification), "the %s flip leaves the verification state", flip.name)
			}
		})
	}
}

// signedByAnotherKey replaces a record's producer signature with one by a
// fixed example key, under the same protected header: the record still names its
// key (key_id, and the header's kid), and another key signed it.
func signedByAnotherKey(t *testing.T, record map[string]interface{}) {
	t.Helper()
	raw, err := hex.DecodeString(record["signature"].(string))
	require.NoError(t, err)
	var envelope cbor.Tag
	require.NoError(t, cbor.Unmarshal(raw, &envelope))
	parts, ok := envelope.Content.([]interface{})
	require.True(t, ok && len(parts) == 4, "a COSE_Sign1")
	toBeSigned, err := cbor.Marshal([]interface{}{"Signature1", parts[0], []byte{}, parts[2]})
	require.NoError(t, err)
	other := ed25519.NewKeyFromSeed([]byte(strings.Repeat("another signer, not the record's", 1)))
	parts[3] = ed25519.Sign(other, toBeSigned)
	out, err := cbor.Marshal(cbor.Tag{Number: envelope.Number, Content: parts})
	require.NoError(t, err)
	record["signature"] = hex.EncodeToString(out)
}

// passFailing is fixture 6's bundle made to fail: edit changes it, and
// verify --bundle must call it INVALID.
func passFailing(t *testing.T, edit func(record map[string]interface{})) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(presentationDir, "6-generic-fallback", "bundle.json"))
	require.NoError(t, err)
	value, err := cli.DecodeBundleJSON(raw)
	require.NoError(t, err)
	edit(value["records"].([]interface{})[0].(map[string]interface{}))
	verdict, _ := cli.BundleVerdict(value, nil)
	require.Equal(t, "INVALID", verdict, "verify --bundle calls the bundle INVALID")
	return value
}

// passRefusedByBuild: build writes no page for the bundle.
func passRefusedByBuild(t *testing.T, bundle string) {
	t.Helper()
	b, written, err := passBuild(t, bundle)
	require.ErrorContains(t, err, "no page written")
	assert.Equal(t, "not written", b.Page)
	assert.Empty(t, written)
}

// passPageOf is the page the bundle gets from the same packaging without
// capsulectl's gate: a page built elsewhere, carrying it anyway.
func passPageOf(t *testing.T, value map[string]interface{}) string {
	t.Helper()
	page, err := emitter.BuildOfflineHTML(value, cli.EvidenceGraphIIFE, emitter.OfflineOptions{Audience: "*", CoreRuntimeSHA256: cli.EvidenceGraphIIFEDigest})
	require.NoError(t, err)
	return page
}

// A failing bundle (a record whose sealed fields were edited after sealing):
// list resolves nothing, build writes no page, and a page that
// carries it anyway shows the refusal and nothing else: no module, no
// notice, no section, the words refusal-snapshot.json holds.
func TestPresentationPassFailingBundleShowsTheRefusalOnly(t *testing.T) {
	passIsolate(t)
	chrome := passChrome(t)
	value := passFailing(t, func(record map[string]interface{}) { record["operator"] = "another-operator" })
	bundle := writeBundle(t, value)
	id, err := passList(t, bundle, "*", "html")
	assert.Empty(t, id)
	assert.ErrorContains(t, err, "no presentation")
	passRefusedByBuild(t, bundle)
	if chrome == nil {
		return
	}
	got := passSnapshots(t, chrome, passPageOf(t, value))
	assert.Equal(t, []string{"failed"}, got.Verification.Verify, "the page says verification failed")
	assert.Empty(t, got.Views.Notice, "no presentation's notice: no module rendered")
	assert.Empty(t, got.Views.Section, "no module's section")
	assert.Equal(t, []string{"verification"}, got.Views.Page, "the verification page only")
	golden := filepath.Join(presentationDir, "refusal-snapshot.json")
	if os.Getenv("CAPSULECTL_UPDATE_PRESENTATION_PASS") == "1" {
		writeSnapshotFile(t, golden, got)
	}
	var want presentationSnapshot
	readJSONFile(t, golden, &want)
	passSameSnapshot(t, want, got)
}

// A record signed by another key than the one it names: verify --bundle
// calls the bundle INVALID (producer_signature_invalid), so build writes no
// page. The page's own gate (the vendored viewer, and
// agent-action-capsule's Go resolver that list uses) checks each record's
// identity, not its producer signature: list resolves the bundle, and a page
// that carries it anyway says it verified. Both are pinned here so that a
// viewer that checks producer signatures fails this test; then it asserts
// the refusal, as the test above does.
func TestPresentationPassWrongSigner(t *testing.T) {
	passIsolate(t)
	chrome := passChrome(t)
	value := passFailing(t, func(record map[string]interface{}) { signedByAnotherKey(t, record) })
	_, report := cli.BundleVerdict(value, nil)
	assert.Equal(t, map[string]any{"status": "fail", "findings": []string{"producer_signature_invalid:" + value["root"].(string)}},
		passJSONValue(t, report["producer_signatures"]), "the signature is what fails")
	bundle := writeBundle(t, value)
	passRefusedByBuild(t, bundle)
	id, err := passList(t, bundle, "*", "html")
	require.NoError(t, err)
	assert.Equal(t, passNoAggregate, id, "list's gate does not check producer signatures")
	if chrome == nil {
		return
	}
	got := passSnapshots(t, chrome, passPageOf(t, value))
	assert.Equal(t, []string{"verified"}, got.Verification.Verify, "the page's gate does not check producer signatures")
}

// passJSONValue is v as JSON decodes it, so a typed report member compares
// with a literal.
func passJSONValue(t *testing.T, v any) any {
	t.Helper()
	var out any
	require.NoError(t, json.Unmarshal([]byte(passMarshal(t, v)), &out))
	if m, ok := out.(map[string]any); ok {
		if f, ok := m["findings"].([]any); ok {
			strs := make([]string, len(f))
			for i, x := range f {
				strs[i], _ = x.(string)
			}
			m["findings"] = strs
		}
	}
	return out
}
