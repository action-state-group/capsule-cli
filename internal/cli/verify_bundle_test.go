package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// producedBundle is an Evidence Bundle this CLI's own producer assembles
// over a checkpointed log, written to a file.
func producedBundle(t *testing.T, edit func(map[string]interface{})) string {
	t.Helper()
	profile, key := profileFixture(t)
	store, log, root := checkpointedStore(t, key, profile.LogID)
	bundle, err := AssembleBundle(t.Context(), store, log, profile.LogID, BundleOptions{Root: root, ClosureDepth: 2, Payloads: "none"})
	require.NoError(t, err)
	encoded, err := json.Marshal(bundle)
	require.NoError(t, err)
	value, err := DecodeBundleJSON(encoded)
	require.NoError(t, err)
	if edit != nil {
		edit(value)
	}
	out, err := json.Marshal(value)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "bundle.json")
	require.NoError(t, os.WriteFile(path, out, 0o600))
	return path
}

func verifyBundleOutput(t *testing.T, path string) (map[string]interface{}, error) {
	t.Helper()
	out, err := invoke(t, "", "verify", "--bundle", path)
	var result map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &result), out)
	return result, err
}

func status(result map[string]interface{}, claim string) string {
	return result[claim].(map[string]interface{})["status"].(string)
}

func TestVerifyBundleAcceptsAProducedBundleOffline(t *testing.T) {
	// No profile is selected or needed: the file alone is verified.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	result, err := verifyBundleOutput(t, producedBundle(t, nil))
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])
	for _, claim := range []string{"graph_closure", "interval_coverage", "per_record_membership"} {
		assert.Equal(t, "pass", status(result, claim), claim)
	}
	assert.NotEmpty(t, result["bundle_digest"])
}

func TestVerifyBundleWithoutACheckpointSignatureIsIncomplete(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := producedBundle(t, func(b map[string]interface{}) {
		delete(b["checkpoint"].(map[string]interface{}), "cose")
	})
	result, err := verifyBundleOutput(t, path)
	assert.ErrorIs(t, err, ErrPartial)
	assert.Equal(t, 3, ExitCode(err))
	assert.Equal(t, "INCOMPLETE", result["verdict"])
	assert.Contains(t, result["interval_coverage"].(map[string]interface{})["findings"], "checkpoint_unverified")
}

func TestVerifyBundleRejectsAnEditedRecord(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := producedBundle(t, func(b map[string]interface{}) {
		b["records"].([]interface{})[0].(map[string]interface{})["operator"] = "someone-else"
	})
	result, err := verifyBundleOutput(t, path)
	assert.ErrorIs(t, err, ErrBundleInvalid)
	assert.Equal(t, 1, ExitCode(err))
	assert.Equal(t, "INVALID", result["verdict"])
}

func TestVerifyBundleRejectsAForgedCheckpointSignature(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := producedBundle(t, func(b map[string]interface{}) {
		checkpoint := b["checkpoint"].(map[string]interface{})
		cose := checkpoint["cose"].(string)
		swap := "A"
		if cose[len(cose)-2:len(cose)-1] == "A" {
			swap = "B"
		}
		checkpoint["cose"] = cose[:len(cose)-2] + swap + cose[len(cose)-1:]
	})
	result, err := verifyBundleOutput(t, path)
	assert.ErrorIs(t, err, ErrBundleInvalid)
	assert.Equal(t, "fail", status(result, "interval_coverage"))
}

func TestVerifyBundleRefusesAFileThatIsNotABundle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "x.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"schema":"some-pane-export/1"}`), 0o600))
	_, err := invoke(t, "", "verify", "--bundle", path)
	assert.Equal(t, 2, ExitCode(err))
}

func TestProducedBundleCarriesTheSignedCheckpointFieldsAndRecordSignatures(t *testing.T) {
	raw, err := os.ReadFile(producedBundle(t, nil))
	require.NoError(t, err)
	bundle, err := DecodeBundleJSON(raw)
	require.NoError(t, err)
	cp := bundle["checkpoint"].(map[string]interface{})
	for _, field := range []string{"log_id", "key_id", "timestamp", "prev_size", "prev_root", "cose"} {
		assert.Contains(t, cp, field)
	}
	record := bundle["records"].([]interface{})[0].(map[string]interface{})
	assert.Contains(t, record, "signature")
	assert.Contains(t, record, "key_id")
}

func TestVerifyBundleHoldsEveryCheckpointFieldToItsSignature(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, field := range []string{"key_id", "timestamp", "log_id", "prev_root", "prev_size"} {
		path := producedBundle(t, func(b map[string]interface{}) {
			cp := b["checkpoint"].(map[string]interface{})
			if field == "prev_size" {
				cp[field] = json.Number("99")
			} else {
				cp[field] = "edited"
			}
		})
		result, err := verifyBundleOutput(t, path)
		assert.ErrorIs(t, err, ErrBundleInvalid, field)
		assert.Equal(t, "INVALID", result["verdict"], field)
		assert.Contains(t, result["checkpoint"].(map[string]interface{})["findings"], "checkpoint_field_mismatch:"+field)
	}
}

// The checkpoint claim is AAC strict's (bundle.VerifyCheckpoint): the
// checkpoint's JSON copy must carry the log id, and every copy stated, the
// checkpoint's and the completeness certificate's, must equal the signed one.
// The bundle draft has the verifier "require its log identifier, MMR size,
// and root to equal the certificate and checkpoint values".
func TestVerifyBundleHoldsBothLogIDCopiesToTheSignedOne(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	checkpoint := func(result map[string]interface{}) map[string]interface{} {
		return result["checkpoint"].(map[string]interface{})
	}

	t.Run("both copies, equal to the signed one", func(t *testing.T) {
		result, err := verifyBundleOutput(t, producedBundle(t, nil))
		require.NoError(t, err)
		assert.Equal(t, "pass", checkpoint(result)["status"])
	})

	// capsule-cli#438: a copy without its own log id was VALID here while
	// AAC strict failed it.
	t.Run("only the certificate's", func(t *testing.T) {
		result, err := verifyBundleOutput(t, producedBundle(t, func(b map[string]interface{}) {
			delete(b["checkpoint"].(map[string]interface{}), "log_id")
		}))
		assert.ErrorIs(t, err, ErrBundleInvalid)
		assert.Equal(t, "INVALID", result["verdict"])
		assert.Equal(t, []interface{}{"checkpoint_field_missing:log_id"}, checkpoint(result)["findings"])
	})

	t.Run("the certificate's differs from the signed one", func(t *testing.T) {
		for name, edit := range map[string]func(map[string]interface{}){
			"checkpoint copy absent": func(b map[string]interface{}) {
				delete(b["checkpoint"].(map[string]interface{}), "log_id")
				b["completeness_certificate"].(map[string]interface{})["log_id"] = "another-log"
			},
			"checkpoint copy equal to the signed one": func(b map[string]interface{}) {
				b["completeness_certificate"].(map[string]interface{})["log_id"] = "another-log"
			},
		} {
			result, err := verifyBundleOutput(t, producedBundle(t, edit))
			assert.ErrorIs(t, err, ErrBundleInvalid, name)
			assert.Contains(t, checkpoint(result)["findings"], "checkpoint_field_mismatch:log_id", name)
		}
		// Mismatches come before missing fields (AAC strict's order).
		result, _ := verifyBundleOutput(t, producedBundle(t, func(b map[string]interface{}) {
			delete(b["checkpoint"].(map[string]interface{}), "log_id")
			b["completeness_certificate"].(map[string]interface{})["log_id"] = "another-log"
		}))
		assert.Equal(t, []interface{}{"checkpoint_field_mismatch:log_id", "checkpoint_field_missing:log_id"}, checkpoint(result)["findings"])
	})

	t.Run("neither", func(t *testing.T) {
		result, err := verifyBundleOutput(t, producedBundle(t, func(b map[string]interface{}) {
			delete(b["checkpoint"].(map[string]interface{}), "log_id")
			delete(b["completeness_certificate"].(map[string]interface{}), "log_id")
		}))
		assert.ErrorIs(t, err, ErrBundleInvalid)
		assert.Contains(t, checkpoint(result)["findings"], "checkpoint_field_missing:log_id")
	})
}

// An integer written as a float ("0e0", "1.0") is not the signed integer,
// and a checkpoint.cose that is not a string is malformed, not absent
// (capsule-cli#438: both read as AAC strict reads them).
func TestVerifyBundleCheckpointIntegersAndCOSEKeepTheirType(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for name, c := range map[string]struct {
		edit    func(map[string]interface{})
		finding string
	}{
		"prev_size as 0e0": {func(cp map[string]interface{}) {
			cp["prev_size"] = json.Number(cp["prev_size"].(json.Number).String() + "e0")
		}, "checkpoint_field_mismatch:prev_size"},
		"mmr_size as N.0": {func(cp map[string]interface{}) {
			cp["mmr_size"] = json.Number(cp["mmr_size"].(json.Number).String() + ".0")
		}, "checkpoint_field_mismatch:mmr_size"},
		"cose a number": {func(cp map[string]interface{}) { cp["cose"] = json.Number("5") }, "checkpoint_signature_malformed"},
		// Strict unpadded base64url only: a line break inside a cose whose
		// signature verifies is malformed, as AAC strict reads it.
		"cose with a line break": {func(cp map[string]interface{}) {
			encoded := cp["cose"].(string)
			cp["cose"] = encoded[:16] + "\n" + encoded[16:]
		}, "checkpoint_signature_malformed"},
	} {
		result, err := verifyBundleOutput(t, producedBundle(t, func(b map[string]interface{}) {
			c.edit(b["checkpoint"].(map[string]interface{}))
		}))
		assert.ErrorIs(t, err, ErrBundleInvalid, name)
		assert.Equal(t, "INVALID", result["verdict"], name)
		assert.Equal(t, []interface{}{c.finding}, result["checkpoint"].(map[string]interface{})["findings"], name)
	}
}

// verify --bundle's checkpoint claim is AAC strict's, on every bundle, and so
// is the verdict wherever the CLI adds no claim of its own.
func TestVerifyBundleCheckpointIsAACStrict(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	produced := func(edit func(map[string]interface{})) map[string]interface{} {
		raw, err := os.ReadFile(producedBundle(t, edit))
		require.NoError(t, err)
		value, err := DecodeBundleJSON(raw)
		require.NoError(t, err)
		return value
	}
	for name, value := range map[string]map[string]interface{}{
		"produced":                       produced(nil),
		"produced, no checkpoint log_id": produced(func(b map[string]interface{}) { delete(b["checkpoint"].(map[string]interface{}), "log_id") }),
		"produced, no cose":              produced(func(b map[string]interface{}) { delete(b["checkpoint"].(map[string]interface{}), "cose") }),
		"selected":                       selectedBundle(t, nil),
		"selected, certificate log_id": selectedBundle(t, func(b map[string]interface{}) {
			b["completeness_certificate"].(map[string]interface{})["log_id"] = "another-log"
		}),
		"selected, no checkpoint log_id": selectedBundle(t, func(b map[string]interface{}) {
			delete(b["checkpoint"].(map[string]interface{}), "log_id")
		}),
	} {
		strict := aacbundle.VerifyStrict(value, aacbundle.StrictOptions{Options: bundleOptions})
		result, err := verifyBundleOutput(t, writeBundle(t, value))
		cp := result["checkpoint"].(map[string]interface{})
		assert.Equal(t, strict.Claim("checkpoint").Status, cp["status"], name)
		assert.ElementsMatch(t, strict.Claim("checkpoint").Findings, cp["findings"], name)
		assert.Equal(t, strict.Verdict, result["verdict"], name)
		if want := verdictError(strict.Verdict); want == nil {
			assert.NoError(t, err, name)
		} else {
			assert.ErrorIs(t, err, want, name)
		}
	}
}

func TestVerifyBundleChecksProducerSignatures(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	record := func(b map[string]interface{}) map[string]interface{} {
		return b["records"].([]interface{})[0].(map[string]interface{})
	}
	unsigned := producedBundle(t, func(b map[string]interface{}) {
		delete(record(b), "signature")
		delete(record(b), "key_id")
	})
	result, err := verifyBundleOutput(t, unsigned)
	assert.ErrorIs(t, err, ErrPartial)
	assert.Equal(t, "INCOMPLETE", result["verdict"])
	assert.Equal(t, "withheld", status(result, "producer_signatures"))

	for name, edit := range map[string]func(map[string]interface{}){
		"a signature byte flipped": func(b map[string]interface{}) {
			sig, err := hex.DecodeString(record(b)["signature"].(string))
			require.NoError(t, err)
			sig[len(sig)-1] ^= 0x01
			record(b)["signature"] = hex.EncodeToString(sig)
		},
		"only a signature": func(b map[string]interface{}) { delete(record(b), "key_id") },
		// Present as null is present (AAC strict vector invalid-null-members),
		// so this is a malformed signature, not an unsigned record.
		"signature and key_id null": func(b map[string]interface{}) {
			record(b)["signature"], record(b)["key_id"] = nil, nil
		},
		// The envelope signs the Capsule ID recomputed from the content, not
		// the capsule_id the record carries.
		"content edited under a kept signature": func(b map[string]interface{}) { record(b)["operator"] = "x" },
	} {
		result, err := verifyBundleOutput(t, producedBundle(t, edit))
		assert.ErrorIs(t, err, ErrBundleInvalid, name)
		assert.Equal(t, "fail", status(result, "producer_signatures"), name)
		for _, state := range result["record_signatures"].(map[string]interface{}) {
			assert.Equal(t, "invalid", state, name)
		}
	}
}

// verify --bundle's producer_signatures is AAC strict's signatures claim and
// record_signatures its per-record verdicts, so the CLI and AAC never give
// two answers for one bundle.
func TestVerifyBundleProducerSignaturesAreAACStrict(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	record := func(b map[string]interface{}) map[string]interface{} {
		return b["records"].([]interface{})[0].(map[string]interface{})
	}
	for name, edit := range map[string]func(map[string]interface{}){
		"produced":         nil,
		"unsigned":         func(b map[string]interface{}) { delete(record(b), "signature"); delete(record(b), "key_id") },
		"signature number": func(b map[string]interface{}) { record(b)["signature"] = json.Number("5") },
		"a non-object record": func(b map[string]interface{}) {
			b["records"] = append(b["records"].([]interface{}), "junk")
		},
	} {
		path := producedBundle(t, edit)
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		value, err := DecodeBundleJSON(raw)
		require.NoError(t, err)
		strict := aacbundle.VerifyStrict(value, aacbundle.StrictOptions{Options: bundleOptions})
		want := map[string]interface{}{}
		for _, r := range strict.Records {
			want[r.CapsuleID] = r.Signature
		}
		result, _ := verifyBundleOutput(t, path)
		signatures := result["producer_signatures"].(map[string]interface{})
		assert.Equal(t, strict.Claim("signatures").Status, signatures["status"], name)
		assert.ElementsMatch(t, strict.Claim("signatures").Findings, signatures["findings"], name)
		assert.Equal(t, want, result["record_signatures"], name)
	}
}

// The claim lists invalid records before unsigned ones, whatever their order
// in the bundle (the AAC strict README), and record_signatures keeps the
// last verdict for a repeated capsule id, as the History check reads it.
func TestVerifyBundleListsInvalidSignaturesFirst(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := producedBundle(t, func(b map[string]interface{}) {
		signed := b["records"].([]interface{})[0].(map[string]interface{})
		unsigned, forged := map[string]interface{}{}, map[string]interface{}{}
		for k, v := range signed {
			unsigned[k], forged[k] = v, v
		}
		delete(unsigned, "signature")
		delete(unsigned, "key_id")
		forged["signature"] = strings.Repeat("00", 64)
		b["records"] = []interface{}{unsigned, forged}
	})
	result, err := verifyBundleOutput(t, path)
	assert.ErrorIs(t, err, ErrBundleInvalid)
	signatures := result["producer_signatures"].(map[string]interface{})
	findings := signatures["findings"].([]interface{})
	require.Len(t, findings, 2)
	assert.True(t, strings.HasPrefix(findings[0].(string), "producer_signature_invalid:"), findings)
	assert.True(t, strings.HasPrefix(findings[1].(string), "producer_signature_unclaimed:"), findings)
	for _, state := range result["record_signatures"].(map[string]interface{}) {
		assert.Equal(t, "invalid", state)
	}
}

// AAC verifies the envelope over the Capsule ID recomputed from the record,
// so editing only the carried capsule_id leaves the signature authored; the
// record's identity fails it instead.
func TestVerifyBundleCarriedCapsuleIDEditFailsIdentityNotSignature(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	result, err := verifyBundleOutput(t, producedBundle(t, func(b map[string]interface{}) {
		b["records"].([]interface{})[0].(map[string]interface{})["capsule_id"] = strings.Repeat("ab", 32)
	}))
	assert.ErrorIs(t, err, ErrBundleInvalid)
	assert.Equal(t, "INVALID", result["verdict"])
	assert.Equal(t, "pass", status(result, "producer_signatures"))
	assert.Equal(t, map[string]interface{}{strings.Repeat("ab", 32): "authored"}, result["record_signatures"])
}

// A value AAC strict refuses before reading its records reports that
// refusal as its producer-signature claim.
func TestSignaturesClaimCarriesAStrictRefusal(t *testing.T) {
	strict := aacbundle.VerifyStrict(map[string]interface{}{"bundle_kind": "evidence-bundle/v1"}, aacbundle.StrictOptions{})
	claim, states := signaturesClaim(strict)
	assert.Equal(t, aacbundle.ClaimResult{Status: "fail", Findings: []string{"not_an_evidence_bundle_v2"}}, claim)
	assert.Empty(t, states)
}

// TestVerifyBundleAgreesWithThePythonVerifier: a bundle this CLI produces,
// and the same bundle unsigned, unanchored or edited, get the same verdict
// from capsule-emit's offline verifier (capsule_emit.evidence_file) as from
// verify --bundle. Runs where that verifier is importable
// (CAPSULECTL_TEST_PYTHON, else python3 on PATH); skipped otherwise.
func TestVerifyBundleAgreesWithThePythonVerifier(t *testing.T) {
	pythonPath := os.Getenv("CAPSULECTL_TEST_PYTHON")
	if pythonPath == "" {
		var err error
		if pythonPath, err = exec.LookPath("python3"); err != nil {
			t.Skip("python3 not on PATH; skipping the Python cross-check")
		}
	}
	if err := exec.Command(pythonPath, "-c", "import capsule_emit.evidence_file").Run(); err != nil {
		t.Skip("capsule_emit.evidence_file not importable; skipping the Python cross-check")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	script := `import json, sys
from capsule_emit.evidence_file import check_evidence_file
print(check_evidence_file(json.load(open(sys.argv[1]))).verdict)`
	cases := map[string]func(map[string]interface{}){
		"produced": nil,
		"unsigned": func(b map[string]interface{}) {
			r := b["records"].([]interface{})[0].(map[string]interface{})
			delete(r, "signature")
			delete(r, "key_id")
		},
		"unanchored": func(b map[string]interface{}) { delete(b["checkpoint"].(map[string]interface{}), "cose") },
		"edited record": func(b map[string]interface{}) {
			b["records"].([]interface{})[0].(map[string]interface{})["operator"] = "x"
		},
		"edited key_id": func(b map[string]interface{}) { b["checkpoint"].(map[string]interface{})["key_id"] = "edited" },
		"mmr_size as a string": func(b map[string]interface{}) {
			cp := b["checkpoint"].(map[string]interface{})
			cp["mmr_size"] = cp["mmr_size"].(json.Number).String()
		},
	}
	for name, edit := range cases {
		path := producedBundle(t, edit)
		goResult, _ := verifyBundleOutput(t, path)
		out, err := exec.Command(pythonPath, "-c", script, path).Output()
		require.NoError(t, err, name)
		assert.Equal(t, goResult["verdict"], strings.TrimSpace(string(out)), name)
	}
	fractional := filepath.Join("testdata", "bundle-fractional-issued-at.json")
	goResult, _ := verifyBundleOutput(t, fractional)
	out, err := exec.Command(pythonPath, "-c", script, fractional).Output()
	require.NoError(t, err, "fractional issued_at")
	assert.Equal(t, goResult["verdict"], strings.TrimSpace(string(out)), "fractional issued_at")
}

// TestVerifyBundleComparesTheTimestampAsSigned: a checkpoint signed with a
// fractional issued_at ("…:00.000Z", as another log implementation writes
// it) states that exact text as its timestamp. The file is one such
// implementation's bundle, unchanged; it is VALID, and a timestamp naming the
// same instant in other text is not what was signed.
func TestVerifyBundleComparesTheTimestampAsSigned(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := filepath.Join("testdata", "bundle-fractional-issued-at.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	bundle, err := DecodeBundleJSON(raw)
	require.NoError(t, err)
	stated := bundle["checkpoint"].(map[string]interface{})["timestamp"].(string)
	require.True(t, strings.HasSuffix(stated, ".000Z"), "the fixture is signed with a fractional issued_at")

	result, err := verifyBundleOutput(t, path)
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])

	renormalized := strings.TrimSuffix(stated, ".000Z") + "Z"
	edited := filepath.Join(t.TempDir(), "bundle.json")
	bundle["checkpoint"].(map[string]interface{})["timestamp"] = renormalized
	data, err := json.Marshal(bundle)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(edited, data, 0o600))
	result, err = verifyBundleOutput(t, edited)
	assert.ErrorIs(t, err, ErrBundleInvalid)
	assert.Contains(t, result["checkpoint"].(map[string]interface{})["findings"], "checkpoint_field_mismatch:timestamp")
}

// TestVerifyBundleCheckpointFieldsKeepTheirJSONType: a number stated as a
// string (or a string stated as a number) is a different value, as it is
// to capsule-emit's verifier.
func TestVerifyBundleCheckpointFieldsKeepTheirJSONType(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for field, retype := range map[string]func(interface{}) interface{}{
		"mmr_size":  func(v interface{}) interface{} { return v.(json.Number).String() },
		"prev_size": func(v interface{}) interface{} { return v.(json.Number).String() },
		"root":      func(v interface{}) interface{} { return json.Number("5") },
	} {
		path := producedBundle(t, func(b map[string]interface{}) {
			cp := b["checkpoint"].(map[string]interface{})
			cp[field] = retype(cp[field])
		})
		result, err := verifyBundleOutput(t, path)
		assert.ErrorIs(t, err, ErrBundleInvalid, field)
		assert.Contains(t, result["checkpoint"].(map[string]interface{})["findings"], "checkpoint_field_mismatch:"+field)
	}
}

func TestSameJSONValue(t *testing.T) {
	assert.True(t, sameJSONValue(json.Number("5"), json.Number("5")))
	assert.True(t, sameJSONValue(json.Number("5"), json.Number("5.0")))
	assert.False(t, sameJSONValue("5", json.Number("5")))
	assert.False(t, sameJSONValue(json.Number("5"), "5"))
	assert.True(t, sameJSONValue("a", "a"))
	assert.False(t, sameJSONValue("a", "b"))
	assert.False(t, sameJSONValue(nil, ""))
}

// A disclosed payload that does not match what its record sealed fails the
// bundle: the verdict is INVALID, not VALID with the mismatch listed. Only a
// matching disclosure, or a member withheld, leaves the verdict as it was.
func TestVerifyBundleFailsADisclosureThatDoesNotMatch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	disclose := func(members map[string]interface{}) func(map[string]interface{}) {
		return func(b map[string]interface{}) {
			b["disclosures"] = map[string]interface{}{b["root"].(string): members}
		}
	}
	for name, members := range map[string]map[string]interface{}{
		"a payload the record did not seal": {"agent_input": map[string]interface{}{"result": "edited"}},
		"a member that cannot be disclosed": {"capsule_id": "edited"},
	} {
		result, err := verifyBundleOutput(t, producedBundle(t, disclose(members)))
		assert.ErrorIs(t, err, ErrBundleInvalid, name)
		assert.Equal(t, "INVALID", result["verdict"], name)
		var statuses []string
		for _, d := range result["disclosures"].([]interface{}) {
			statuses = append(statuses, d.(map[string]interface{})["status"].(string))
		}
		assert.NotContains(t, statuses, "disclosure_match", name)
	}

	result, err := verifyBundleOutput(t, producedBundle(t, nil))
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"], "nothing disclosed: the verdict is as before")
}

// More disclosures a bundle must not carry: an edited agent_output (the
// other disclosable member), and an overlay naming a record the bundle does
// not hold. Each fails the bundle.
func TestVerifyBundleFailsAnEditedOutputOrADisclosureForNoRecord(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for name, edit := range map[string]func(map[string]interface{}){
		"an agent_output the record did not seal": func(b map[string]interface{}) {
			b["disclosures"] = map[string]interface{}{b["root"].(string): map[string]interface{}{"agent_output": map[string]interface{}{"result": "edited"}}}
		},
		"a disclosure for a record the bundle does not hold": func(b map[string]interface{}) {
			b["disclosures"] = map[string]interface{}{strings.Repeat("ab", 32): map[string]interface{}{"agent_input": map[string]interface{}{"result": "edited"}}}
		},
	} {
		result, err := verifyBundleOutput(t, producedBundle(t, edit))
		assert.ErrorIs(t, err, ErrBundleInvalid, name)
		assert.Equal(t, "INVALID", result["verdict"], name)
		assert.Equal(t, 1, ExitCode(err), name)
		var statuses []string
		for _, d := range result["disclosures"].([]interface{}) {
			statuses = append(statuses, d.(map[string]interface{})["status"].(string))
		}
		assert.Contains(t, statuses, "disclosure_mismatch", "%s: failed for its disclosure", name)
		assert.Equal(t, "pass", status(result, "graph_closure"), name)
	}
}

// selectedRecords is a RecordSource over the test artifact store.
type selectedRecords mapArtifactStore

func (s selectedRecords) GetRecord(ctx context.Context, id string) ([]byte, []byte, error) {
	record, err := mapArtifactStore(s).Get(ctx, id)
	if err != nil {
		return nil, nil, aacbundle.ErrRecordNotFound
	}
	return record.Capsule, record.ProducerEnvelope, nil
}

// selectedBundle is a producer-selected Evidence Bundle (completeness
// selection "producer-selected", no range) over a checkpointed log, as AAC's
// producer builds one for a scoped export: each record carries its own
// inclusion proof under the signed checkpoint.
func selectedBundle(t *testing.T, edit func(map[string]interface{})) map[string]interface{} {
	t.Helper()
	profile, key := profileFixture(t)
	store, log, root := checkpointedStore(t, key, profile.LogID)
	// Leaves after the record, checkpointed, so its inclusion proof has
	// sibling hashes to tamper with.
	for i := byte(1); i <= 3; i++ {
		_, err := log.Append(t.Context(), cll.AppendInput{Value: bytes.Repeat([]byte{i}, 32), AppendedAt: time.Now().UTC()})
		require.NoError(t, err)
	}
	signer, err := checkpoint.NewEd25519Signer(key)
	require.NoError(t, err)
	config := checkpoint.DefaultRunnerConfig(profile.LogID)
	config.Cadence.CadenceEntries = 1
	runner, err := checkpoint.NewRunner(config, log, signer)
	require.NoError(t, err)
	_, err = runner.RunOnce(t.Context(), time.Now().UTC())
	require.NoError(t, err)
	built, err := aacbundle.BuildEvidenceBundle(t.Context(), aacbundle.ProofSourceFromCLL(log, selectedRecords(store)), aacbundle.BuildOptions{Root: root, NoRange: true})
	require.NoError(t, err)
	encoded, err := json.Marshal(built)
	require.NoError(t, err)
	value, err := DecodeBundleJSON(encoded)
	require.NoError(t, err)
	require.Equal(t, aacbundle.SelectionProducerSelected, value["completeness"].(map[string]interface{})["selection"])
	if edit != nil {
		edit(value)
	}
	return value
}

// tamperInclusionProof flips one byte of the first sibling hash of the
// selected bundle's one inclusion proof, keeping it lowercase hex.
func tamperInclusionProof(t *testing.T, b map[string]interface{}) {
	t.Helper()
	memberships := b["completeness_certificate"].(map[string]interface{})["memberships"].(map[string]interface{})
	require.Len(t, memberships, 1)
	for _, membership := range memberships {
		witness := membership.(map[string]interface{})["inclusion_proof"].(map[string]interface{})["witness"].([]interface{})
		require.NotEmpty(t, witness)
		hash, err := hex.DecodeString(witness[0].(string))
		require.NoError(t, err)
		hash[0] ^= 1
		witness[0] = hex.EncodeToString(hash)
	}
}

// A producer-selected bundle reads as AAC strict reads it (capsule-cli#301):
// INCOMPLETE, its interval not checkable and each record's membership
// checked, and INVALID once a record's inclusion proof is tampered with.
func TestVerifyBundleReadsAProducerSelectedBundleAsIncomplete(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	value := selectedBundle(t, nil)
	result, err := verifyBundleOutput(t, writeBundle(t, value))
	assert.ErrorIs(t, err, ErrPartial)
	assert.Equal(t, "INCOMPLETE", result["verdict"])
	assert.Equal(t, map[string]interface{}{"status": "withheld", "findings": []interface{}{"selection_not_checkable"}}, result["interval_coverage"])
	assert.Equal(t, "pass", status(result, "per_record_membership"))
	assert.Equal(t, "pass", status(result, "checkpoint"))

	strict := aacbundle.VerifyStrict(value, aacbundle.StrictOptions{Options: bundleOptions})
	assert.Equal(t, strict.Verdict, result["verdict"], "the same verdict as AAC strict")

	tampered := selectedBundle(t, func(b map[string]interface{}) { tamperInclusionProof(t, b) })
	result, err = verifyBundleOutput(t, writeBundle(t, tampered))
	assert.ErrorIs(t, err, ErrBundleInvalid)
	assert.Equal(t, "fail", status(result, "per_record_membership"))
	assert.Equal(t, aacbundle.VerdictInvalid, aacbundle.VerifyStrict(tampered, aacbundle.StrictOptions{Options: bundleOptions}).Verdict)
}

// AAC strict checks a producer-selected bundle's proofs only against a
// checkpoint that carries its own log_id. One that states it only in the
// certificate fails the checkpoint claim, as strict reads it, so a tampered
// proof there cannot read INCOMPLETE.
func TestVerifyBundleProducerSelectedNeedsTheCheckpointLogID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	value := selectedBundle(t, func(b map[string]interface{}) {
		delete(b["checkpoint"].(map[string]interface{}), "log_id")
		tamperInclusionProof(t, b)
	})
	result, err := verifyBundleOutput(t, writeBundle(t, value))
	assert.ErrorIs(t, err, ErrBundleInvalid)
	assert.Equal(t, "fail", status(result, "checkpoint"))
	assert.Contains(t, result["checkpoint"].(map[string]interface{})["findings"], "checkpoint_field_missing:log_id")
	assert.Equal(t, aacbundle.VerdictInvalid, aacbundle.VerifyStrict(value, aacbundle.StrictOptions{Options: bundleOptions}).Verdict)
}

// Without the selection extension a certificate missing its range is
// still malformed: the Bundle draft requires first_seq and last_seq.
func TestVerifyBundleStillRejectsARangelessCertificateWithoutSelection(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	value := selectedBundle(t, func(b map[string]interface{}) {
		delete(b["completeness"].(map[string]interface{}), "selection")
	})
	result, err := verifyBundleOutput(t, writeBundle(t, value))
	assert.ErrorIs(t, err, ErrBundleInvalid)
	assert.Equal(t, "fail", status(result, "interval_coverage"))
}
