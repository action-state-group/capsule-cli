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

	"github.com/action-state-group/cll-go/checkpoint"
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
