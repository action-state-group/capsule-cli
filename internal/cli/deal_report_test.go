package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDealReportIsOneVerifyingPage(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)
	check := dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(jetSkiDemo, "06-check-pay.json"))
	dealRun(t, "note", "--deal", dealID, "--kind", "approval", "--check", check["check_id"].(string), "--choice", "hold")
	page := filepath.Join(t.TempDir(), "receipt.html")
	report := dealRun(t, "report", "--deal", dealID, "--html", page)
	assert.Equal(t, page, report["html"])
	assert.Equal(t, "inline", report["fragment_kind"])

	// The fragment decodes to the bundle, and the authoritative verifier passes
	// every claim, authenticates the checkpoint, and withholds only messages.
	decoded, err := aacbundle.DecodeFragment(report["fragment"].(string))
	require.NoError(t, err)
	b := decoded.(map[string]interface{})
	digest, err := aacbundle.BundleDigest(b)
	require.NoError(t, err)
	assert.Equal(t, report["bundle_digest"], digest)
	v := aacbundle.VerifyBundle(b)
	assert.Equal(t, "pass", v.GraphClosure.Status)
	assert.Equal(t, "pass", v.IntervalCoverage.Status, v.IntervalCoverage.Findings)
	assert.Equal(t, "pass", v.PerRecordMembership.Status, v.PerRecordMembership.Findings)
	assert.Len(t, b["records"], 8, "exactly this deal's steps, nothing from any other deal")
	kinds := map[string]string{}
	for _, s := range report["steps"].([]any) {
		step := s.(map[string]any)
		kinds[step["capsule_id"].(string)] = step["kind"].(string)
	}
	for _, d := range v.Disclosures {
		if kinds[d.CapsuleID] == "message" {
			assert.Equal(t, "withheld", string(d.Status), "message text stays on the machine")
		} else {
			assert.Equal(t, "disclosure_match", string(d.Status), kinds[d.CapsuleID])
		}
	}

	// One self-contained page: no external scripts, styles or fetches.
	raw, err := os.ReadFile(page)
	require.NoError(t, err)
	html := string(raw)
	assert.NotRegexp(t, regexp.MustCompile(`(?i)<(script|link|img)[^>]+(src|href)=`), html)
	assert.Contains(t, html, "<title>Deal receipt</title>")
	assert.Contains(t, html, `<div id="deal"></div>`)
	assert.Contains(t, html, string(evidenceGraphIIFE))
	assert.Equal(t, 1, strings.Count(html, "EvidenceGraph.verifyBundle(bundle)"))
	assert.NotContains(t, html, "our office line is down", "withheld message text is not in the page")
}

func TestDealReportFragmentFallsBackToPointer(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(bookingFixture, "open.json"))["deal_id"].(string)
	old := maxInlineFragment
	maxInlineFragment = 64
	t.Cleanup(func() { maxInlineFragment = old })
	report := dealRun(t, "report", "--deal", dealID, "--location", "https://example.org/receipts/x.json")
	assert.Equal(t, "pointer (draft)", report["fragment_kind"])
	decoded, err := aacbundle.DecodeFragment(report["fragment"].(string))
	require.NoError(t, err)
	assert.Equal(t, map[string]interface{}{"bundle_ref": map[string]interface{}{
		"digest": report["bundle_digest"], "root": report["steps"].([]any)[0].(map[string]any)["capsule_id"],
		"locations": []interface{}{"https://example.org/receipts/x.json"},
	}}, decoded)
}

func TestVendoredVerifierMatchesRecordedDigest(t *testing.T) {
	sum := sha256.Sum256(evidenceGraphIIFE)
	assert.Equal(t, strings.TrimSpace(evidenceGraphIIFESHA256), hex.EncodeToString(sum[:]), "rebuild with scripts/build-evidence-graph-iife.sh; never hand-edit")
}
