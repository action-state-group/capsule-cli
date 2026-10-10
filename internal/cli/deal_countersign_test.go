package cli

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dealReportFiles writes the deal's report bundle, page and email and returns
// the command output and the three paths.
func dealReportFiles(t *testing.T, dealID string, extra ...string) (map[string]any, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	html, eml := filepath.Join(dir, "receipt.html"), filepath.Join(dir, "receipt.eml")
	args := append([]string{"report", "--deal", dealID, "--html", html, "--email", eml}, extra...)
	bundle := ""
	if !strings.Contains(strings.Join(extra, " "), "--from-bundle") {
		bundle = filepath.Join(dir, "bundle.json")
		args = append(args, "--bundle", bundle)
	}
	return dealRun(t, args...), bundle, html, eml
}

func readBundleFile(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	b, err := decodeBundleJSON(raw)
	require.NoError(t, err)
	return b
}

func writeEntry(t *testing.T, entry CountersignatureEntry) string {
	t.Helper()
	raw, err := json.Marshal(entry)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "entry.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

func countersignerDirectoryFile(t *testing.T, name, keyHex string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "countersigners.json")
	require.NoError(t, os.WriteFile(path, []byte(`[{"name":"`+name+`","key_ids":["`+keyHex+`"]}]`), 0o600))
	return path
}

func pageCountersign(t *testing.T, htmlPath string) dealCountersignView {
	t.Helper()
	page, err := os.ReadFile(htmlPath)
	require.NoError(t, err)
	var data struct {
		Countersign dealCountersignView `json:"countersign"`
	}
	require.NoError(t, json.Unmarshal(pageData(t, page), &data))
	return data.Countersign
}

// pageData is the data the page's bootstrap hands the deal view: what
// capsulectl checked when it wrote the page.
func pageData(t *testing.T, page []byte) []byte {
	t.Helper()
	m := regexp.MustCompile(`globalThis\.capsulectlDealPage = Object\.freeze\((.*)\);\n`).FindSubmatch(page)
	require.NotNil(t, m, "the page's bootstrap carries the deal view's data")
	return m[1]
}

func asView(t *testing.T, v any) dealCountersignView {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var view dealCountersignView
	require.NoError(t, json.Unmarshal(raw, &view))
	return view
}

// TestDealReceiptIsNotCountersignedByDefault: every receipt names its
// countersign rung, and with no countersignature it says so: in the report
// JSON (with or without files), on the page and in the email.
func TestDealReceiptIsNotCountersignedByDefault(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)
	plain := dealRun(t, "report", "--deal", dealID)
	assert.Equal(t, dealNotCountersigned(), asView(t, plain["countersign"]))

	out, _, html, eml := dealReportFiles(t, dealID)
	assert.Equal(t, "not_countersigned", asView(t, out["countersign"]).Finding)
	assert.Equal(t, dealNotCountersigned(), pageCountersign(t, html))
	email, err := os.ReadFile(eml)
	require.NoError(t, err)
	assert.Contains(t, string(email), "Not countersigned: no other party has signed this record.")
	assert.Contains(t, out["email"].(map[string]any)["text"], dealNotCountersignedText)
}

// TestDealCountersignAttachesAndRendersTheSigner: an existing countersignature
// over the deal's own bundle is verified, attached, and resolved in the
// directory the operator chose; the receipt rendered from that same bundle
// names the signer, and `countersign verify` agrees.
func TestDealCountersignAttachesAndRendersTheSigner(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)
	_, bundlePath, _, _ := dealReportFiles(t, dealID)
	digest, err := aacbundle.BundleDigest(readBundleFile(t, bundlePath))
	require.NoError(t, err)
	entry, signerKey := countersignerEntry(t, digest, []CountersignCheck{{Name: "range membership", Result: "established"}})
	directory := countersignerDirectoryFile(t, "Example Countersigner", signerKey)

	attached := dealRun(t, "countersign", "--deal", dealID, "--bundle", bundlePath, "--entry", writeEntry(t, entry), "--directory", directory)
	assert.Equal(t, true, attached["attached"])
	view := asView(t, attached["countersign"])
	assert.Equal(t, "countersigned", view.Rung)
	assert.Contains(t, view.Text, "Countersigned by Example Countersigner, a signer listed in the directory you chose: it signed this exact bundle, with its own results (range membership: established)")
	stored := readBundleFile(t, bundlePath)
	require.Len(t, stored["countersignatures"], 1)
	again, err := aacbundle.BundleDigest(stored)
	require.NoError(t, err)
	assert.Equal(t, digest, again, "attaching never changes what was signed")

	out, _, html, eml := dealReportFiles(t, dealID, "--from-bundle", bundlePath, "--directory", directory)
	assert.Equal(t, "countersigned", asView(t, out["countersign"]).Rung)
	assert.Equal(t, "countersigned", pageCountersign(t, html).Rung)
	assert.Contains(t, pageCountersign(t, html).Text, "Example Countersigner")
	email, err := os.ReadFile(eml)
	require.NoError(t, err)
	assert.Contains(t, string(email), "Countersigned by Example Countersigner")

	verified, err := invoke(t, "", "countersign", "verify", "--profile", "deal", "--directory", directory, bundlePath)
	require.NoError(t, err, verified)
	assert.Contains(t, verified, `"state":"resolved"`)
}

// TestDealSelfCountersignatureIsNotIndependent: a countersignature made with
// the deal profile's own signing key is shown as NOT INDEPENDENT on every
// surface, never as a countersignature.
func TestDealSelfCountersignatureIsNotIndependent(t *testing.T) {
	dir := dealFixture(t)
	dealID := openJetSki(t)
	_, bundlePath, _, _ := dealReportFiles(t, dealID)
	digest, err := aacbundle.BundleDigest(readBundleFile(t, bundlePath))
	require.NoError(t, err)
	seed, err := os.ReadFile(filepath.Join(dir, "signing.seed"))
	require.NoError(t, err)
	own, err := privateKey(Secret{Value: strings.TrimSpace(string(seed))})
	require.NoError(t, err)
	entry := CountersignatureEntry{
		Type: countersignAPI, Over: digest,
		Signer:    CountersignSigner{ID: "me", KeyID: hex.EncodeToString(own.Public().(ed25519.PublicKey))},
		Statement: CountersignStatement{Checks: []CountersignCheck{{Name: "everything", Result: "established"}}, RecomputedAt: "2026-10-04T00:00:00Z", Scope: CountersignScope{LedgerID: "deal"}},
	}
	signCountersignEntry(t, own, &entry)
	// Even a directory that lists the producer's key does not make it independent.
	directory := countersignerDirectoryFile(t, "Looks Independent", entry.Signer.KeyID)

	attached := dealRun(t, "countersign", "--deal", dealID, "--bundle", bundlePath, "--entry", writeEntry(t, entry), "--directory", directory)
	view := asView(t, attached["countersign"])
	assert.Equal(t, "self_countersigned", view.Finding)
	assert.True(t, strings.HasPrefix(view.Text, "NOT INDEPENDENT: countersigned by the producer's own key."), view.Text)
	assert.NotContains(t, view.Text, "Looks Independent")

	out, _, html, eml := dealReportFiles(t, dealID, "--from-bundle", bundlePath, "--directory", directory)
	assert.Equal(t, "self_countersigned", asView(t, out["countersign"]).Finding)
	assert.Equal(t, "self_countersigned", pageCountersign(t, html).Finding)
	email, err := os.ReadFile(eml)
	require.NoError(t, err)
	assert.Contains(t, string(email), "NOT INDEPENDENT")
}

// TestDealCountersignRefusesWhatItCannotVouchFor: a countersignature that
// does not verify is refused and never written; a bundle that is not this
// deal's is refused; a bundle with countersignatures needs a directory.
func TestDealCountersignRefusesWhatItCannotVouchFor(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)
	_, bundlePath, _, _ := dealReportFiles(t, dealID)
	before, err := os.ReadFile(bundlePath)
	require.NoError(t, err)

	entry, signerKey := countersignerEntry(t, strings.Repeat("ab", 32), nil) // over another digest
	directory := countersignerDirectoryFile(t, "Example Countersigner", signerKey)
	_, err = invoke(t, "", "--profile", "deal", "deal", "countersign", "--deal", dealID, "--bundle", bundlePath, "--entry", writeEntry(t, entry), "--directory", directory)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "refusing to show a countersignature that does not verify")
	after, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "nothing was written")

	other := dealRun(t, "open", "--input", filepath.Join(jetSkiDemo, "01-open.json"))["deal_id"].(string)
	_, err = invoke(t, "", "--profile", "deal", "deal", "countersign", "--deal", other, "--bundle", bundlePath, "--directory", directory)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "is not this deal's report bundle")

	digest, err := aacbundle.BundleDigest(readBundleFile(t, bundlePath))
	require.NoError(t, err)
	good, signer := countersignerEntry(t, digest, nil)
	dealRun(t, "countersign", "--deal", dealID, "--bundle", bundlePath, "--entry", writeEntry(t, good), "--directory", countersignerDirectoryFile(t, "X", signer))
	_, err = invoke(t, "", "--profile", "deal", "deal", "report", "--deal", dealID, "--from-bundle", bundlePath)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "--directory is required")
}

// TestDealCountersignStaysOnTheNeutralSide: the hook carries no commercial
// or evaluator language; it verifies signatures and names signers, nothing
// else.
func TestDealCountersignStaysOnTheNeutralSide(t *testing.T) {
	banned := regexp.MustCompile(`(?i)pricing|\bprice tier|\btier(ed|ing)?\b|subscription|\bpaid\b|evaluator`)
	for _, path := range []string{"deal_countersign.go", "assets/deal-view.js", "../../skills/deal/COUNTERSIGN.md"} {
		raw, err := os.ReadFile(path)
		require.NoError(t, err, path)
		assert.Empty(t, banned.FindAllString(string(raw), -1), path)
	}
}

// TestDealRemoteCheckerOnlyAddsAndFallsBack pins the optional second reader's
// three properties: a remote pass never clears a local pause, a slow checker
// is cut off at 2 s and the local rules decide alone, and nothing it says
// removes a local difference.
func TestDealRemoteCheckerOnlyAddsAndFallsBack(t *testing.T) {
	require.Equal(t, 2*time.Second, dealRemoteTimeout)
	dealFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"verdict":"pass","differences":[]}`))
	}))
	defer srv.Close()
	t.Setenv(dealCheckURLEnv, srv.URL)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	paused := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(bookingFixture, "check-commit.json"))
	assert.Equal(t, "used", paused["remote"])
	assert.Equal(t, "pause", paused["verdict"], "a remote pass never clears a local pause")
	assert.Contains(t, paused["card"], "Over your limit of $400.00")

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		_, _ = w.Write([]byte(`{"verdict":"pause","differences":[{"rule":"late","text":"too late to count"}]}`))
	}))
	defer slow.Close()
	defer close(release)
	t.Setenv(dealCheckURLEnv, slow.URL)
	start := time.Now()
	local := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(bookingFixture, "check-commit-asked.json"))
	elapsed := time.Since(start)
	assert.Equal(t, "unavailable", local["remote"])
	assert.Equal(t, "pass", local["verdict"], "the local rules decide alone")
	assert.NotContains(t, local["card"], "too late to count")
	assert.Less(t, elapsed, 4*time.Second, "cut off near 2 s, never waits for the slow reply")
}

// A countersignature covers your own copy only: a shared copy withholds
// records, so it is a different bundle. --from-bundle refuses --share, and a
// shared copy says it is not countersigned.
func TestDealSharedCopyIsNotCountersigned(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)
	_, bundlePath, _, _ := dealReportFiles(t, dealID)
	_, err := invoke(t, "", "--profile", "deal", "deal", "report", "--deal", dealID, "--from-bundle", bundlePath, "--html", filepath.Join(t.TempDir(), "s.html"), "--share", "counterparty", "--to", "x")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, err.Error(), "a shared copy (--share) withholds records, so it is a different bundle")

	html := filepath.Join(t.TempDir(), "shared.html")
	out := dealRun(t, "report", "--deal", dealID, "--html", html, "--share", "counterparty", "--to", "the shop")
	assert.Equal(t, "not_countersigned", asView(t, out["countersign"]).Finding)
	assert.Equal(t, dealNotCountersigned(), pageCountersign(t, html))
}
