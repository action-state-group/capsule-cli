package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testdata/contract/cases is the Evidence Contract edge-case library, copied
// verbatim from capsule-engine's tests/fixtures/contract-cases (generated
// there by scripts/generate_contract_cases.py, which also holds the
// reference implementation of the diff rules). Every case must come out the
// same here as it does there: that is what keeps the two implementations of
// `contract diff` and the contract digest from drifting. SOURCE.json pins the
// engine commit the copy was taken from and the digest of its SHA256SUMS;
// TestContractCaseLibraryMatchesPinnedSource fails if the copy is edited, or
// re-copied without updating the pin.
const casesDir = "testdata/contract/cases"

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestContractCaseLibraryMatchesPinnedSource(t *testing.T) {
	var source struct {
		Repo             string `json:"repo"`
		Commit           string `json:"commit"`
		SHA256SUMSDigest string `json:"sha256sums_digest"`
	}
	raw, e := os.ReadFile(filepath.Join(casesDir, "SOURCE.json"))
	require.NoError(t, e)
	require.NoError(t, json.Unmarshal(raw, &source))
	require.Len(t, source.Commit, 40, "SOURCE.json must pin a full engine commit")

	sums, e := os.ReadFile(filepath.Join(casesDir, "SHA256SUMS"))
	require.NoError(t, e)
	require.Equal(t, source.SHA256SUMSDigest, sha256Hex(sums), "SHA256SUMS is not the one pinned in SOURCE.json")

	listed := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(sums), "\n"), "\n") {
		digest, name, ok := strings.Cut(line, "  ")
		require.True(t, ok, line)
		listed[name] = digest
	}
	entries, e := os.ReadDir(casesDir)
	require.NoError(t, e)
	actual := map[string]string{}
	for _, entry := range entries {
		if name := entry.Name(); name != "SHA256SUMS" && name != "SOURCE.json" {
			b, e := os.ReadFile(filepath.Join(casesDir, name))
			require.NoError(t, e)
			actual[name] = sha256Hex(b)
		}
	}
	assert.Equal(t, listed, actual, "the copied library differs from the pinned SHA256SUMS")

	// Optional: compare directly against a capsule-engine checkout.
	if dir := os.Getenv("CAPSULE_ENGINE_CONTRACT_CASES"); dir != "" {
		upstream, e := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
		require.NoError(t, e)
		assert.Equal(t, string(upstream), string(sums), "the copy differs from %s", dir)
	}
}

type caseIndexEntry struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	File     string `json:"file"`
	Negative bool   `json:"negative"`
}

type contractCase struct {
	Kind     string          `json:"kind"`
	Contract json.RawMessage `json:"contract"`
	A        json.RawMessage `json:"a"`
	B        json.RawMessage `json:"b"`
	Expect   struct {
		Valid *bool `json:"valid"`
		Error any   `json:"error"`
		// diff
		Breaking *bool `json:"breaking"`
		Changes  []struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		} `json:"changes"`
		// pin
		ContractRef    string `json:"contract_ref"`
		ContractDigest struct {
			Digest string `json:"digest"`
		} `json:"contract_digest"`
	} `json:"expect"`
}

func loadCaseIndex(t *testing.T) []caseIndexEntry {
	t.Helper()
	raw, e := os.ReadFile(filepath.Join(casesDir, "index.json"))
	require.NoError(t, e)
	var index []caseIndexEntry
	require.NoError(t, json.Unmarshal(raw, &index))
	return index
}

func writeTemp(t *testing.T, name string, raw json.RawMessage) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

func decodeNumbers(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	require.NoError(t, dec.Decode(&doc))
	return doc
}

func TestContractCaseLibrary(t *testing.T) {
	index := loadCaseIndex(t)
	require.GreaterOrEqual(t, len(index), 100)
	for _, entry := range index {
		t.Run(entry.ID, func(t *testing.T) {
			raw, e := os.ReadFile(filepath.Join(casesDir, entry.File))
			require.NoError(t, e)
			var tc contractCase
			require.NoError(t, json.Unmarshal(raw, &tc))
			switch tc.Kind {
			case "validate":
				runValidateCase(t, tc)
			case "diff":
				runDiffCase(t, tc)
			case "pin":
				pin, e := computeContractPin(decodeNumbers(t, tc.Contract))
				require.NoError(t, e)
				assert.Equal(t, tc.Expect.ContractRef, pin.Ref)
				assert.Equal(t, tc.Expect.ContractDigest.Digest, pin.Digest)
			default:
				t.Fatalf("unknown case kind %q", tc.Kind)
			}
		})
	}
}

// runValidateCase checks the verdict, and for a negative case that the
// expected violation's location is reported -- as an issue's own path; or,
// where the profile union collapses a branch's failures into one issue,
// inside that issue's message; or, for an unknown profile value, which this
// validator reports at the requirement, by a message naming the field.
func runValidateCase(t *testing.T, tc contractCase) {
	path := writeTemp(t, "contract.json", tc.Contract)
	out, e := invoke(t, "", "contract", "validate", path, "--schema", testSchema, "--json")
	var report struct {
		Valid  bool `json:"valid"`
		Issues []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"issues"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.NotNil(t, tc.Expect.Valid)
	if *tc.Expect.Valid {
		require.NoError(t, e)
		assert.True(t, report.Valid)
		return
	}
	require.ErrorIs(t, e, ErrSchemaInvalid)
	assert.False(t, report.Valid)
	want := tc.Expect.Error.(map[string]any)["path"].(string)
	for _, issue := range report.Issues {
		if issue.Path == want || strings.Contains(issue.Message, "at "+want+":") || namesChild(issue.Path, issue.Message, want) {
			return
		}
	}
	t.Errorf("no issue at %s: %+v", want, report.Issues)
}

func namesChild(path, message, want string) bool {
	child, ok := strings.CutPrefix(want, path+"/")
	return ok && !strings.Contains(child, "/") && strings.Contains(message, child)
}

func runDiffCase(t *testing.T, tc contractCase) {
	a := writeTemp(t, "a.json", tc.A)
	b := writeTemp(t, "b.json", tc.B)
	out, e := invoke(t, "", "contract", "diff", a, b, "--schema", testSchema, "--json")
	if code, isErr := tc.Expect.Error.(string); isErr {
		require.ErrorIs(t, e, ErrInput)
		assert.Equal(t, 2, ExitCode(e))
		if code == "duplicate_requirement_id" {
			assert.ErrorIs(t, e, errDuplicateRequirementID)
		}
		return
	}
	require.NotNil(t, tc.Expect.Breaking)
	if *tc.Expect.Breaking {
		require.ErrorIs(t, e, ErrBreaking)
		assert.Equal(t, 1, ExitCode(e))
	} else {
		require.NoError(t, e)
	}
	var report struct {
		Breaking bool `json:"breaking"`
		Changes  []struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		} `json:"changes"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, *tc.Expect.Breaking, report.Breaking)
	if len(tc.Expect.Changes) == 0 {
		assert.Empty(t, report.Changes)
	} else {
		assert.Equal(t, tc.Expect.Changes, report.Changes)
	}
}

func caseFile(t *testing.T, id string) contractCase {
	t.Helper()
	raw, e := os.ReadFile(filepath.Join(casesDir, id+".json"))
	require.NoError(t, e)
	var tc contractCase
	require.NoError(t, json.Unmarshal(raw, &tc))
	return tc
}

func TestContractDiffHumanReadableOutput(t *testing.T) {
	tc := caseFile(t, "diff-source-changed")
	out, e := invoke(t, "", "contract", "diff", writeTemp(t, "a.json", tc.A), writeTemp(t, "b.json", tc.B))
	require.ErrorIs(t, e, ErrBreaking)
	assert.Equal(t, ErrBreaking.Error(), SafeError(e))
	assert.Contains(t, out, "A: ec:example-fixture:2026-09-30@1 sha256:")
	assert.Contains(t, out, "B: ec:example-fixture:2026-09-30@2 sha256:")
	assert.Contains(t, out, "requirements[req-a]/evidence_requirements/required_sources: required sources: source-one replaced by source-two; evidence for the old members may not count")
	assert.True(t, strings.HasSuffix(out, "BREAKING\n"))

	tc = caseFile(t, "diff-identical")
	out, e = invoke(t, "", "contract", "diff", writeTemp(t, "a.json", tc.A), writeTemp(t, "b.json", tc.B))
	require.NoError(t, e)
	assert.Contains(t, out, "identical\nnon-breaking\n")
}

func TestContractDiffJSONNamesBothContractsByRefAndDigest(t *testing.T) {
	tc := caseFile(t, "diff-version-bump-only")
	out, e := invoke(t, "", "contract", "diff", writeTemp(t, "a.json", tc.A), writeTemp(t, "b.json", tc.B), "--json")
	require.NoError(t, e)
	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, "capsule-cli-result/v1", report["spec_version"])
	pinA := caseFile(t, "pin-base").Expect
	assert.Equal(t, map[string]any{
		"contract_ref":    pinA.ContractRef,
		"contract_digest": map[string]any{"digest_alg": "SHA-256", "digest": pinA.ContractDigest.Digest},
	}, report["a"])
	assert.Equal(t, []any{map[string]any{
		"path": "version", "kind": "version_changed", "severity": "non_breaking", "reason": "version 1 -> 2",
	}}, report["changes"])
}

func TestContractDiffUsageErrors(t *testing.T) {
	tc := caseFile(t, "diff-identical")
	a := writeTemp(t, "a.json", tc.A)
	t.Run("one argument", func(t *testing.T) {
		_, e := invoke(t, "", "contract", "diff", a)
		assert.Equal(t, 2, ExitCode(e))
	})
	t.Run("file does not exist", func(t *testing.T) {
		_, e := invoke(t, "", "contract", "diff", a, "testdata/contract/does-not-exist.json")
		assert.Equal(t, 2, ExitCode(e))
	})
	t.Run("not JSON", func(t *testing.T) {
		bad := writeTemp(t, "bad.json", json.RawMessage("{not json"))
		_, e := invoke(t, "", "contract", "diff", a, bad)
		assert.Equal(t, 2, ExitCode(e))
		assert.Contains(t, SafeError(e), "malformed JSON")
	})
	t.Run("invalid without --schema is still refused when unmatchable", func(t *testing.T) {
		noReqs := writeTemp(t, "b.json", json.RawMessage(`{"id":"x","version":"1"}`))
		_, e := invoke(t, "", "contract", "diff", a, noReqs)
		assert.Equal(t, 2, ExitCode(e))
		assert.Contains(t, SafeError(e), "no requirements array")
	})
	t.Run("invalid under --schema names the file", func(t *testing.T) {
		bad := caseFile(t, "diff-bad-b-invalid")
		b := writeTemp(t, "b.json", bad.B)
		_, e := invoke(t, "", "contract", "diff", a, b, "--schema", testSchema)
		assert.Equal(t, 2, ExitCode(e))
		assert.Contains(t, SafeError(e), "is not a valid contract under --schema")
	})
}
