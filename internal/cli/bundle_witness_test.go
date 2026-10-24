package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/veraison/go-cose"
)

const testWitness = "https://witness.example"

// mintReceipt is an RFC 9162 COSE receipt from a one-entry log over the
// signed checkpoint's entry hash, signed by key (as the witness protocol's
// own tests mint them).
func mintReceipt(t *testing.T, entry []byte, key ed25519.PrivateKey) []byte {
	t.Helper()
	proof, err := cbor.Marshal([]any{int64(1), int64(0), [][]byte{}})
	require.NoError(t, err)
	leaf := sha256.Sum256(append([]byte{0}, entry...))
	message := cose.NewSign1Message()
	message.Headers.Protected.SetAlgorithm(cose.AlgorithmEdDSA)
	message.Headers.Protected[int64(395)] = int64(1)
	message.Headers.Unprotected[int64(396)] = map[any]any{int64(-1): []any{proof}}
	message.Payload = leaf[:]
	signer, err := cose.NewSigner(cose.AlgorithmEdDSA, key)
	require.NoError(t, err)
	require.NoError(t, message.Sign(rand.Reader, nil, signer))
	message.Payload = nil
	encoded, err := message.MarshalCBOR()
	require.NoError(t, err)
	return encoded
}

// witnessed adds one receipt from testWitness, signed by key, over the
// bundle's signed checkpoint.
func witnessed(t *testing.T, key ed25519.PrivateKey) func(map[string]interface{}) {
	return func(b map[string]interface{}) {
		cp := b["checkpoint"].(map[string]interface{})
		statement, err := base64.RawURLEncoding.DecodeString(cp["cose"].(string))
		require.NoError(t, err)
		record, err := checkpoint.ParseRecord(statement)
		require.NoError(t, err)
		entry, err := record.EntryHash()
		require.NoError(t, err)
		cp["witnesses"] = []interface{}{map[string]interface{}{
			"ts_url":      testWitness,
			"entry_hash":  hex.EncodeToString(entry),
			"receipt_b64": base64.StdEncoding.EncodeToString(mintReceipt(t, entry, key)),
			"leaf_index":  "0",
			"tree_size":   json.Number("1"),
		}}
	}
}

func writeDirectory(t *testing.T, rows ...map[string]interface{}) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "witnesses.json")
	data, err := json.Marshal(map[string]interface{}{"directory_version": "1", "witnesses": rows})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func rawKeyRow(endpoint string, public ed25519.PublicKey) map[string]interface{} {
	return map[string]interface{}{"name": "test witness", "endpoint": endpoint, "since": "2026-01-01", "key_ids": []string{hex.EncodeToString(public)}}
}

func verifyWithDirectory(t *testing.T, path, directory string) (map[string]interface{}, error) {
	t.Helper()
	args := []string{"verify", "--bundle", path}
	if directory != "" {
		args = append(args, "--witness-directory", directory)
	}
	out, err := invoke(t, "", args...)
	var result map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &result), out)
	return result, err
}

func bundleWitnesses(result map[string]interface{}) (string, []interface{}) {
	w := result["witnesses"].(map[string]interface{})
	return w["status"].(string), w["findings"].([]interface{})
}

func TestVerifyBundleChecksAWitnessReceiptUnderADirectoryKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	path := producedBundle(t, witnessed(t, private))

	// A raw-key row, and the same key as a DER public_keys entry.
	der, err := x509.MarshalPKIXPublicKey(public)
	require.NoError(t, err)
	sum := sha256.Sum256(der)
	derRow := map[string]interface{}{"name": "test witness", "endpoint": testWitness + "/", "key_ids": []string{hex.EncodeToString(sum[:])}, "public_keys": []string{base64.StdEncoding.EncodeToString(der)}}
	for name, directory := range map[string]string{"raw key": writeDirectory(t, rawKeyRow(testWitness, public)), "DER key": writeDirectory(t, derRow)} {
		result, err := verifyWithDirectory(t, path, directory)
		require.NoError(t, err, name)
		assert.Equal(t, "VALID", result["verdict"], name)
		state, findings := bundleWitnesses(result)
		assert.Equal(t, "pass", state, name)
		assert.Empty(t, findings, name)
		receipts := result["witnesses"].(map[string]interface{})["receipts"].([]interface{})
		assert.Equal(t, "pass", receipts[0].(map[string]interface{})["status"], name)
	}
}

func TestVerifyBundleWithholdsAReceiptItHasNoKeyFor(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	path := producedBundle(t, witnessed(t, private))
	for name, directory := range map[string]string{
		"no directory":         "",
		"another witness only": writeDirectory(t, rawKeyRow("https://other.example", public)),
		"a rekor row":          writeDirectory(t, map[string]interface{}{"name": "r", "endpoint": testWitness, "binding": "rekor", "key_ids": []string{hex.EncodeToString(public)}}),
	} {
		result, err := verifyWithDirectory(t, path, directory)
		require.NoError(t, err, name)
		assert.Equal(t, "VALID", result["verdict"], name) // withheld never gates
		state, findings := bundleWitnesses(result)
		assert.Equal(t, "withheld", state, name)
		assert.Equal(t, []interface{}{"witness_unverified:" + testWitness}, findings, name)
	}
}

func TestVerifyBundleWithoutReceiptsIsJudgedAsBefore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	result, err := verifyWithDirectory(t, producedBundle(t, nil), "")
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])
	state, findings := bundleWitnesses(result)
	assert.Equal(t, "withheld", state)
	assert.Equal(t, []interface{}{"witness_receipt_absent"}, findings)
}

func TestVerifyBundleFailsAReceiptThatDoesNotVerify(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	directory := writeDirectory(t, rawKeyRow(testWitness, public))
	receipt := func(b map[string]interface{}) map[string]interface{} {
		return b["checkpoint"].(map[string]interface{})["witnesses"].([]interface{})[0].(map[string]interface{})
	}
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	for name, edit := range map[string]func(map[string]interface{}){
		"for another checkpoint": func(b map[string]interface{}) {
			witnessed(t, private)(b)
			other := make([]byte, 32)
			receipt(b)["entry_hash"] = hex.EncodeToString(other)
			receipt(b)["receipt_b64"] = base64.StdEncoding.EncodeToString(mintReceipt(t, other, private))
		},
		"signed by another key": witnessed(t, otherKey),
		"a receipt byte flipped": func(b map[string]interface{}) {
			witnessed(t, private)(b)
			raw, _ := base64.StdEncoding.DecodeString(receipt(b)["receipt_b64"].(string))
			raw[len(raw)-1] ^= 0x01
			receipt(b)["receipt_b64"] = base64.StdEncoding.EncodeToString(raw)
		},
	} {
		result, err := verifyWithDirectory(t, producedBundle(t, edit), directory)
		assert.ErrorIs(t, err, ErrBundleInvalid, name)
		assert.Equal(t, "INVALID", result["verdict"], name)
		state, findings := bundleWitnesses(result)
		assert.Equal(t, "fail", state, name)
		assert.Equal(t, []interface{}{"witness_receipt_invalid:" + testWitness}, findings, name)
	}

	malformed := producedBundle(t, func(b map[string]interface{}) {
		witnessed(t, private)(b)
		cp := b["checkpoint"].(map[string]interface{})
		cp["witnesses"] = append(cp["witnesses"].([]interface{}), "not a receipt")
	})
	result, err := verifyWithDirectory(t, malformed, directory)
	assert.ErrorIs(t, err, ErrBundleInvalid)
	state, findings := bundleWitnesses(result)
	assert.Equal(t, "fail", state)
	assert.Contains(t, findings, "witness_receipt_malformed:1")
}

func TestVerifyBundleDoesNotCheckReceiptsAgainstAnUnsignedCheckpoint(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	path := producedBundle(t, func(b map[string]interface{}) {
		witnessed(t, private)(b)
		delete(b["checkpoint"].(map[string]interface{}), "cose")
	})
	result, err := verifyWithDirectory(t, path, writeDirectory(t, rawKeyRow(testWitness, public)))
	assert.ErrorIs(t, err, ErrPartial)
	state, findings := bundleWitnesses(result)
	assert.Equal(t, "withheld", state)
	assert.Equal(t, []interface{}{"checkpoint_unverified"}, findings)
}

func TestVerifyBundleRefusesAWitnessDirectoryItCannotRead(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bad := filepath.Join(t.TempDir(), "witnesses.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"directory_version": "1"}`), 0o600))
	_, err := invoke(t, "", "verify", "--bundle", producedBundle(t, nil), "--witness-directory", bad)
	assert.Equal(t, 2, ExitCode(err))
	assert.Contains(t, SafeError(err), "no witnesses array")
}

// TestVerifyBundleWitnessVerdictsAgreeWithThePythonVerifier: given the same
// witness directory, capsule-emit's offline verifier reaches the same
// witness status and verdict. Runs where that verifier checks witnesses.
func TestVerifyBundleWitnessVerdictsAgreeWithThePythonVerifier(t *testing.T) {
	pythonPath := os.Getenv("CAPSULECTL_TEST_PYTHON")
	if pythonPath == "" {
		var err error
		if pythonPath, err = exec.LookPath("python3"); err != nil {
			t.Skip("python3 not on PATH; skipping the Python cross-check")
		}
	}
	probe := "import inspect\nfrom capsule_emit.evidence_file import check_evidence_file\nassert 'witness_directory' in inspect.signature(check_evidence_file).parameters"
	if err := exec.Command(pythonPath, "-c", probe).Run(); err != nil {
		t.Skip("capsule_emit.evidence_file does not check witnesses here; skipping the Python cross-check")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	directory := writeDirectory(t, rawKeyRow(testWitness, public))
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	script := `import json, sys
from capsule_emit.evidence_file import check_evidence_file
c = check_evidence_file(json.load(open(sys.argv[1])), witness_directory=json.load(open(sys.argv[2])))
print(c.verdict, c.witness.status)`
	emptyKeyRow := rawKeyRow(testWitness, public)
	emptyKeyRow["key_ids"] = []string{}
	type crossCase struct {
		edit      func(map[string]interface{})
		directory string
	}
	for name, tc := range map[string]crossCase{
		"witnessed":         {witnessed(t, private), directory},
		"another key":       {witnessed(t, otherKey), directory},
		"no receipt":        {nil, directory},
		"no row for it":     {witnessed(t, private), writeDirectory(t, rawKeyRow("https://other.example", public))},
		"a row with no key": {witnessed(t, private), writeDirectory(t, emptyKeyRow)},
	} {
		edit, directory := tc.edit, tc.directory
		path := producedBundle(t, edit)
		goResult, _ := verifyWithDirectory(t, path, directory)
		goState, _ := bundleWitnesses(goResult)
		out, err := exec.Command(pythonPath, "-c", script, path, directory).Output()
		require.NoError(t, err, name)
		assert.Equal(t, goResult["verdict"].(string)+" "+goState, strings.TrimSpace(string(out)), name)
	}
}

// The receipt checks are agent-action-capsule's (witness.VerifyReceipts).
// These tests pin where they differ from the checks this CLI carried before.

func witnessEntry(b map[string]interface{}) map[string]interface{} {
	return b["checkpoint"].(map[string]interface{})["witnesses"].([]interface{})[0].(map[string]interface{})
}

// A scrapi receipt is checked, not withheld: a SCITT COSE Receipt whose leaf
// is SHA-256 of the checkpoint's COSE statement.
func TestVerifyBundleChecksAScrapiReceipt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	scrapi := func(key ed25519.PrivateKey) func(map[string]interface{}) {
		return func(b map[string]interface{}) {
			witnessed(t, private)(b)
			statement, err := base64.RawURLEncoding.DecodeString(b["checkpoint"].(map[string]interface{})["cose"].(string))
			require.NoError(t, err)
			leaf := sha256.Sum256(statement)
			witnessEntry(b)["ts_url"] = "scrapi+" + testWitness
			witnessEntry(b)["receipt_b64"] = base64.StdEncoding.EncodeToString(mintReceipt(t, leaf[:], key))
		}
	}
	row := rawKeyRow(testWitness, public)
	row["binding"] = "scrapi"
	directory := writeDirectory(t, row)

	result, err := verifyWithDirectory(t, producedBundle(t, scrapi(private)), directory)
	require.NoError(t, err)
	state, findings := bundleWitnesses(result)
	assert.Equal(t, "pass", state)
	assert.Empty(t, findings)
	receipt := result["witnesses"].(map[string]interface{})["receipts"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "scrapi", receipt["binding"])
	assert.Equal(t, "ok", receipt["reason"], "the reason is AAC's (capsule-cli#438)")

	result, err = verifyWithDirectory(t, producedBundle(t, scrapi(otherKey)), directory)
	assert.ErrorIs(t, err, ErrBundleInvalid)
	state, findings = bundleWitnesses(result)
	assert.Equal(t, "fail", state)
	assert.Equal(t, []interface{}{"witness_receipt_invalid:scrapi+" + testWitness}, findings)
}

// A row with a key id that is not 64 lowercase hex characters is unusable as
// a whole: its receipts are withheld, not checked under its other keys.
func TestVerifyBundleWithholdsAReceiptWhoseRowHasAnUnusableKeyID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	row := rawKeyRow(testWitness, public)
	row["key_ids"] = []string{hex.EncodeToString(public), "not-a-key"}
	result, err := verifyWithDirectory(t, producedBundle(t, witnessed(t, private)), writeDirectory(t, row))
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])
	state, findings := bundleWitnesses(result)
	assert.Equal(t, "withheld", state)
	assert.Equal(t, []interface{}{"witness_unverified:" + testWitness}, findings)
}

// A stub receipt never reached a witness: it is withheld, never checked.
func TestVerifyBundleWithholdsAStubReceipt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	path := producedBundle(t, func(b map[string]interface{}) {
		witnessed(t, private)(b)
		witnessEntry(b)["is_stub"] = true
	})
	result, err := verifyWithDirectory(t, path, writeDirectory(t, rawKeyRow(testWitness, public)))
	require.NoError(t, err)
	state, _ := bundleWitnesses(result)
	assert.Equal(t, "withheld", state)
}

// leaf_index and tree_size must be present, and are not read: the receipt's
// own inclusion proof states the position it verifies at.
func TestVerifyBundleRequiresButDoesNotReadTheReceiptPosition(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	directory := writeDirectory(t, rawKeyRow(testWitness, public))

	result, err := verifyWithDirectory(t, producedBundle(t, func(b map[string]interface{}) {
		witnessed(t, private)(b)
		witnessEntry(b)["leaf_index"] = "not a number"
	}), directory)
	require.NoError(t, err)
	state, _ := bundleWitnesses(result)
	assert.Equal(t, "pass", state)

	result, err = verifyWithDirectory(t, producedBundle(t, func(b map[string]interface{}) {
		witnessed(t, private)(b)
		delete(witnessEntry(b), "tree_size")
	}), directory)
	assert.ErrorIs(t, err, ErrBundleInvalid)
	state, findings := bundleWitnesses(result)
	assert.Equal(t, "fail", state)
	assert.Equal(t, []interface{}{"witness_receipt_malformed:0"}, findings)
}

// An entry that is not a receipt this CLI reads is malformed, and fails the
// file, even where its witness has no directory row.
func TestVerifyBundleFailsAMalformedReceiptWithoutARow(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	directory := writeDirectory(t, rawKeyRow("https://other.example", public))
	for name, edit := range map[string]func(map[string]interface{}){
		"receipt_b64 not base64":   func(r map[string]interface{}) { r["receipt_b64"] = "!" },
		"receipt_b64 empty":        func(r map[string]interface{}) { r["receipt_b64"] = "" },
		"receipt_b64 not a string": func(r map[string]interface{}) { r["receipt_b64"] = json.Number("42") },
		"entry_hash missing":       func(r map[string]interface{}) { delete(r, "entry_hash") },
		"entry_hash not a string":  func(r map[string]interface{}) { r["entry_hash"] = true },
		"ts_url empty":             func(r map[string]interface{}) { r["ts_url"] = "" },
		"leaf_index missing":       func(r map[string]interface{}) { delete(r, "leaf_index") },
		"receipt is not an object": nil,
	} {
		path := producedBundle(t, func(b map[string]interface{}) {
			witnessed(t, private)(b)
			if edit == nil {
				b["checkpoint"].(map[string]interface{})["witnesses"] = []interface{}{"not a receipt"}
				return
			}
			edit(witnessEntry(b))
		})
		for _, dir := range []string{"", directory} {
			result, err := verifyWithDirectory(t, path, dir)
			assert.ErrorIs(t, err, ErrBundleInvalid, name)
			assert.Equal(t, 1, ExitCode(err), name)
			assert.Equal(t, "INVALID", result["verdict"], name)
			state, findings := bundleWitnesses(result)
			assert.Equal(t, "fail", state, name)
			assert.Equal(t, []interface{}{"witness_receipt_malformed:0"}, findings, name)
		}
	}
}

// Findings are AAC's: malformed entries first, in entry order, then one per
// receipt that did not pass (capsule-cli#438).
func TestVerifyBundleListsWitnessFindingsInEntryOrder(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	path := producedBundle(t, func(b map[string]interface{}) {
		witnessed(t, private)(b)
		cp := b["checkpoint"].(map[string]interface{})
		cp["witnesses"] = append(cp["witnesses"].([]interface{}), "not a receipt", witnessEntry(b), map[string]interface{}{"ts_url": testWitness})
	})
	result, err := verifyWithDirectory(t, path, "")
	assert.ErrorIs(t, err, ErrBundleInvalid)
	_, findings := bundleWitnesses(result)
	assert.Equal(t, []interface{}{
		"witness_receipt_malformed:1",
		"witness_receipt_malformed:3",
		"witness_unverified:" + testWitness,
		"witness_unverified:" + testWitness,
	}, findings)
}

// A directory row of another shape than witnesses.json's is an input error.
func TestVerifyBundleRefusesAWitnessDirectoryRowOfAnotherShape(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bundle := producedBundle(t, nil)
	for name, data := range map[string]string{
		"a row that is not an object":   `{"witnesses": [42]}`,
		"key_ids not a list":            `{"witnesses": [{"name": "w", "endpoint": "https://w.example", "key_ids": "abc"}]}`,
		"a public key that is a number": `{"witnesses": [{"name": "w", "endpoint": "https://w.example", "key_ids": [], "public_keys": [1]}]}`,
		"a bare list with a bad row":    `[{"endpoint": 7}]`,
	} {
		path := filepath.Join(t.TempDir(), "witnesses.json")
		require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
		_, err := invoke(t, "", "verify", "--bundle", bundle, "--witness-directory", path)
		assert.Equal(t, 2, ExitCode(err), name)
		assert.Contains(t, SafeError(err), "not a witnesses.json file", name)
	}
}

// aacStrictWitness is testdata/aac-strict-witness/vectors.json: agent-action-
// capsule's strict Bundle vectors for the rekor and ES256 receipts.
type aacStrictWitness struct {
	WitnessDirectory json.RawMessage `json:"witness_directory"`
	Cases            []struct {
		ID     string          `json:"id"`
		Bundle json.RawMessage `json:"bundle"`
	} `json:"cases"`
}

// The rekor binding and a P-256 cll witness, at the CLI boundary, on
// agent-action-capsule's own vectors.
func TestVerifyBundleChecksRekorAndES256ReceiptsFromTheAACVectors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	raw, err := os.ReadFile("testdata/aac-strict-witness/vectors.json")
	require.NoError(t, err)
	var vectors aacStrictWitness
	require.NoError(t, json.Unmarshal(raw, &vectors))
	dir := t.TempDir()
	directory := filepath.Join(dir, "witnesses.json")
	require.NoError(t, os.WriteFile(directory, vectors.WitnessDirectory, 0o600))
	want := map[string]struct{ status, binding, finding string }{
		"pos-witness-cll-es256":           {"pass", "cll", ""},
		"pos-witness-rekor":               {"pass", "rekor", ""},
		"neg-witness-rekor-other-payload": {"fail", "rekor", "witness_receipt_invalid:rekor+https://rekor.example"},
	}
	require.Len(t, vectors.Cases, len(want))
	for _, c := range vectors.Cases {
		expect, listed := want[c.ID]
		require.True(t, listed, c.ID)
		path := filepath.Join(dir, c.ID+".json")
		require.NoError(t, os.WriteFile(path, c.Bundle, 0o600))
		result, err := verifyWithDirectory(t, path, directory)
		state, findings := bundleWitnesses(result)
		assert.Equal(t, expect.status, state, c.ID)
		receipts := result["witnesses"].(map[string]interface{})["receipts"].([]interface{})
		require.Len(t, receipts, 1, c.ID)
		assert.Equal(t, expect.binding, receipts[0].(map[string]interface{})["binding"], c.ID)
		assert.Equal(t, expect.status, receipts[0].(map[string]interface{})["status"], c.ID)
		if expect.status == "fail" {
			assert.ErrorIs(t, err, ErrBundleInvalid, c.ID)
			assert.Equal(t, "INVALID", result["verdict"], c.ID)
			assert.Equal(t, []interface{}{expect.finding}, findings, c.ID)
		} else {
			require.NoError(t, err, c.ID)
			assert.Equal(t, "VALID", result["verdict"], c.ID)
			assert.Empty(t, findings, c.ID)
		}
	}
}

// agent-action-capsule reads a witness directory strictly, and capsule-cli
// hands it the file as written (it writes no directories itself and ships
// none in these lax forms). A row whose binding is empty or null, whose
// public_keys is null, or whose field names are cased otherwise is no row
// AAC can use, so the receipt it would have checked is withheld. A
// public_keys entry wrapped across lines is skipped, so the row's key id is
// read as a raw Ed25519 key and the receipt fails.
func TestVerifyBundleReadsLaxDirectoryRowsAsAACDoes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(public)
	require.NoError(t, err)
	digest := sha256.Sum256(der)
	encoded := base64.StdEncoding.EncodeToString(der)

	withheld := map[string]map[string]interface{}{
		"empty binding":    {"binding": ""},
		"null binding":     {"binding": nil},
		"null public_keys": {"public_keys": nil},
	}
	for name, extra := range withheld {
		t.Run(name, func(t *testing.T) {
			row := rawKeyRow(testWitness, public)
			for k, v := range extra {
				row[k] = v
			}
			result, err := verifyWithDirectory(t, producedBundle(t, witnessed(t, private)), writeDirectory(t, row))
			require.NoError(t, err)
			state, findings := bundleWitnesses(result)
			assert.Equal(t, "withheld", state)
			assert.Equal(t, []interface{}{"witness_unverified:" + testWitness}, findings)
		})
	}

	t.Run("field names cased otherwise", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "witnesses.json")
		data, err := json.Marshal(map[string]interface{}{"directory_version": "1", "witnesses": []interface{}{
			map[string]interface{}{"name": "test witness", "Endpoint": testWitness, "since": "2026-01-01", "key_ids": []string{hex.EncodeToString(public)}},
		}})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o600))
		result, err := verifyWithDirectory(t, producedBundle(t, witnessed(t, private)), path)
		require.NoError(t, err)
		state, _ := bundleWitnesses(result)
		assert.Equal(t, "withheld", state)
	})

	t.Run("public_keys entry wrapped across lines", func(t *testing.T) {
		row := rawKeyRow(testWitness, public)
		row["key_ids"] = []string{hex.EncodeToString(digest[:])}
		row["public_keys"] = []string{encoded[:20] + "\n" + encoded[20:]}
		result, _ := verifyWithDirectory(t, producedBundle(t, witnessed(t, private)), writeDirectory(t, row))
		state, findings := bundleWitnesses(result)
		assert.Equal(t, "fail", state)
		assert.Equal(t, []interface{}{"witness_receipt_invalid:" + testWitness}, findings)
		assert.Equal(t, "INVALID", result["verdict"])
	})
}

// A receipt_b64 wrapped across lines is not canonical base64, which
// agent-action-capsule requires, so the entry is malformed and fails.
// (capsule-cli writes its receipts unwrapped.)
func TestVerifyBundleFailsAWrappedReceipt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	path := producedBundle(t, func(b map[string]interface{}) {
		witnessed(t, private)(b)
		entry := witnessEntry(b)
		encoded := entry["receipt_b64"].(string)
		entry["receipt_b64"] = encoded[:16] + "\n" + encoded[16:]
	})
	result, _ := verifyWithDirectory(t, path, writeDirectory(t, rawKeyRow(testWitness, public)))
	state, findings := bundleWitnesses(result)
	assert.Equal(t, "fail", state)
	assert.Equal(t, []interface{}{"witness_receipt_malformed:0"}, findings)
	assert.Equal(t, "INVALID", result["verdict"])
}

// Receipts are checked only against a checkpoint whose claim passed, as AAC
// strict checks them: a checkpoint without its own log_id fails the claim,
// so a receipt that would verify is not checked (capsule-cli#438).
func TestVerifyBundleChecksNoReceiptAgainstAFailedCheckpointClaim(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	path := producedBundle(t, func(b map[string]interface{}) {
		witnessed(t, private)(b)
		delete(b["checkpoint"].(map[string]interface{}), "log_id")
	})
	result, err := verifyWithDirectory(t, path, writeDirectory(t, rawKeyRow(testWitness, public)))
	assert.ErrorIs(t, err, ErrBundleInvalid)
	assert.Equal(t, "fail", status(result, "checkpoint"))
	state, findings := bundleWitnesses(result)
	assert.Equal(t, "withheld", state)
	assert.Equal(t, []interface{}{"checkpoint_unverified"}, findings)
	assert.Empty(t, result["witnesses"].(map[string]interface{})["receipts"])
}
