package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/cll-go/checkpoint"
	"github.com/action-state-group/cll-go/cll"
	"github.com/action-state-group/cll-go/witness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An unreachable witness endpoint: milestones leave delivery pending, and the
// test then stores a receipt as a delivery would.
const dealTestWitness = "https://127.0.0.1:1"

// witnessedDeal runs the retail checkout on a profile with a witness, closes
// it, and stores a receipt signed by signer for the deal's last checkpoint.
func witnessedDeal(t *testing.T, pinned ed25519.PublicKey, signer ed25519.PrivateKey) string {
	t.Helper()
	dealFixture(t)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	p.Checkpoint.Endpoint, p.Checkpoint.PublicKey = dealTestWitness, hex.EncodeToString(pinned)
	require.NoError(t, saveProfile(p, true))
	dealID := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(retailDemo, "check-pay.json"))
	dealRun(t, "note", "--deal", dealID, "--kind", "act", "--input", filepath.Join(retailDemo, "act-pay.json"))
	closed := dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received","delivered":{"item":"cat sticker"}}`))
	assert.Equal(t, "pending", closed["checkpoint"].(map[string]any)["witness"])

	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoError(t, s.useDeal(t.Context(), dealID, false))
	service, err := serviceID(s.dp)
	require.NoError(t, err)
	tip, err := s.t.log.LoadCLL(t.Context())
	require.NoError(t, err)
	state, err := s.t.log.GetWitness(t.Context(), service, tip.Checkpoint.Size)
	require.NoError(t, err)
	record, err := checkpoint.ParseRecord(state.Checkpoint)
	require.NoError(t, err)
	entry, err := record.EntryHash()
	require.NoError(t, err)
	next := state
	next.Receipt = &cll.WitnessReceiptState{Bytes: mintReceipt(t, entry, signer), EntryHash: hex.EncodeToString(entry), EntryHashScheme: witness.EntryHashSchemeCheckpointDigest, LeafIndex: ptr(int64(0)), TreeSize: ptr(int64(1))}
	require.NoError(t, s.t.log.CommitWitness(t.Context(), state.Attempts, next))
	return dealID
}

func TestDealReportCarriesTheWitnessReceipt(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	dealID := witnessedDeal(t, public, key)
	dir := t.TempDir()
	bundlePath, page, eml := filepath.Join(dir, "bundle.json"), filepath.Join(dir, "receipt.html"), filepath.Join(dir, "receipt.eml")
	report := dealRun(t, "report", "--deal", dealID, "--bundle", bundlePath, "--html", page, "--email", eml)
	assurance := report["assurance"].(map[string]any)
	assert.Equal(t, "witnessed", assurance["rung"])
	assert.Equal(t, "127.0.0.1:1", assurance["witness"])

	raw, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	var bundle map[string]any
	require.NoError(t, json.Unmarshal(raw, &bundle))
	witnesses := bundle["checkpoint"].(map[string]any)["witnesses"].([]any)
	require.Len(t, witnesses, 1)
	assert.Equal(t, dealTestWitness, witnesses[0].(map[string]any)["ts_url"])

	// A reader checks it against a witness directory they choose.
	result, err := verifyWithDirectory(t, bundlePath, writeDirectory(t, rawKeyRow(dealTestWitness, public)))
	require.NoError(t, err)
	status, _ := bundleWitnesses(result)
	assert.Equal(t, "pass", status)
	other, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	result, _ = verifyWithDirectory(t, bundlePath, writeDirectory(t, rawKeyRow(dealTestWitness, other)))
	status, _ = bundleWitnesses(result)
	assert.Equal(t, "fail", status, "a directory key that did not sign it fails")

	html, err := os.ReadFile(page)
	require.NoError(t, err)
	assert.Contains(t, string(html), "this page does not check it")
	_, bodies, _ := emailParts(t, mustRead(t, eml))
	for _, body := range bodies {
		assert.Contains(t, body, "Witnessed: 127.0.0.1:1, an independent log")
		assert.Contains(t, body, "--witness-directory")
		assert.NotContains(t, strings.ToLower(body), "verified")
	}
}

func TestDealReportLeavesOutAReceiptThePinnedKeyDidNotSign(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, stranger, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	dealID := witnessedDeal(t, public, stranger)
	bundlePath := filepath.Join(t.TempDir(), "bundle.json")
	report := dealRun(t, "report", "--deal", dealID, "--bundle", bundlePath)
	assert.Equal(t, "sealed", report["assurance"].(map[string]any)["rung"])
	var bundle map[string]any
	require.NoError(t, json.Unmarshal(mustRead(t, bundlePath), &bundle))
	assert.NotContains(t, bundle["checkpoint"].(map[string]any), "witnesses")
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

// Every receipt states its own scope on its face, and nothing claims to be a
// complete record.
func TestDealReceiptsStateTheirScope(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	dealRun(t, "check", "--deal", dealID, "--input", filepath.Join(retailDemo, "check-pay.json"))
	dir := t.TempDir()
	page, eml := filepath.Join(dir, "receipt.html"), filepath.Join(dir, "receipt.eml")
	dealRun(t, "report", "--deal", dealID, "--html", page, "--email", eml)

	const scope = "This receipt covers this one deal. It is not a record of everything the agent did."
	assert.Equal(t, scope, dealScopeLine)
	assert.Contains(t, string(mustRead(t, page)), scope, "the receipt page header")
	_, bodies, _ := emailParts(t, mustRead(t, eml))
	require.Len(t, bodies, 2)
	for kind, body := range bodies {
		assert.Contains(t, strings.ReplaceAll(body, "\r\n", "\n"), scope, kind)
		assert.Contains(t, html.UnescapeString(body), "What the agent did is the agent's own report", kind)
	}

	path := writeJSON(t, `{"id":"c1","at":"2026-09-27T18:05:00Z","tool":"make_payment","action":"pay","amount_minor":627,"currency":"USD","status":"succeeded"}`)
	result, err := reconcileRun(t, "--executions", path, "--to", "2026-09-28T00:00:00Z")
	require.NoError(t, err)
	assert.Equal(t, "This pass covers the execution records it was given, for this period. It is not a record of everything the agent did.", result["scope"])

	for _, text := range append([]string{string(mustRead(t, page)), fmt.Sprint(result)}, bodies["text/plain"], bodies["text/html"]) {
		lower := strings.ToLower(text)
		for _, claim := range []string{"all activity", "complete history", "complete record", "full history", "everything the agent did."} {
			if claim == "everything the agent did." {
				assert.NotContains(t, strings.ReplaceAll(lower, "not a record of everything the agent did.", ""), claim)
				continue
			}
			assert.NotContains(t, lower, claim)
		}
	}
}
