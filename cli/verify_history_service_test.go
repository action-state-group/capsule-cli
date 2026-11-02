package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The vector files are byte copies of agent-action-capsule vectors at
// df8a31f7 (go/v0.20.0).
const (
	inferenceVectorsSHA = "e79ad9030399aafc67e547650b1a8ef8f65365961ed9b5195a85be4681da8db9"
	outcomeVectorsSHA   = "780e3ad5ca167ea443a888b352eb6330be56764121afacba97716200a4e87fb4"
)

type serviceOut struct {
	Kind       string            `json:"kind"`
	Verdict    string            `json:"verdict"`
	Reason     string            `json:"reason"`
	Properties map[string]string `json:"history_properties"`
}

func runService(t *testing.T, file any, args ...string) serviceOut {
	t.Helper()
	data, err := json.Marshal(file)
	require.NoError(t, err)
	out, _ := invoke(t, "", append([]string{"verify", "--history", writeHistory(t, data)}, args...)...)
	var got serviceOut
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	return got
}

// Every inference-service-history/0 case of AAC's vectors, through the CLI
// with the case's responder and service-history pins given as flags.
func TestVerifyHistoryRunsTheInferenceServiceHistoryVectors(t *testing.T) {
	var corpus struct {
		Cases []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
			Pins struct {
				TrustPolicy struct {
					ResponderKeys []string `json:"responder_keys"`
				} `json:"trust_policy"`
				ServiceHistory struct {
					LogKeys     []string `json:"log_keys"`
					WitnessKeys []string `json:"witness_keys"`
					PeerKeys    []string `json:"peer_keys"`
				} `json:"service_history"`
			} `json:"pins"`
			Inputs   json.RawMessage `json:"inputs"`
			Expected struct {
				Accepted bool              `json:"accepted"`
				Props    map[string]string `json:"history_properties"`
			} `json:"expected"`
		} `json:"cases"`
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "inference-service-history-v0", "vectors.json"))
	require.NoError(t, err)
	require.Equal(t, inferenceVectorsSHA, sha256Hex(raw))
	require.NoError(t, json.Unmarshal(raw, &corpus))
	ran := 0
	for _, tc := range corpus.Cases {
		var in map[string]any
		require.NoError(t, json.Unmarshal(tc.Inputs, &in))
		if _, single := in["request"]; !single {
			continue // a pair of Histories (period_fixed) is not a one-file History
		}
		ran++
		t.Run(tc.ID, func(t *testing.T) {
			in["history"] = inferenceInstance
			args := []string{}
			for _, k := range tc.Pins.TrustPolicy.ResponderKeys {
				args = append(args, "--holder-key", k)
			}
			for flag, keys := range map[string][]string{"--log-key": tc.Pins.ServiceHistory.LogKeys, "--witness-key": tc.Pins.ServiceHistory.WitnessKeys, "--peer-key": tc.Pins.ServiceHistory.PeerKeys} {
				for _, k := range keys {
					args = append(args, flag, k)
				}
			}
			got := runService(t, in, args...)
			assert.Equal(t, inferenceInstance, got.Kind)
			if tc.Expected.Accepted {
				assert.Equal(t, "ACCEPTED", got.Verdict)
			} else {
				assert.Equal(t, "NOT_ACCEPTED", got.Verdict)
			}
			for p, want := range tc.Expected.Props {
				assert.Equal(t, want, got.Properties[p], p)
			}
		})
	}
	assert.Positive(t, ran)
}

// Every personal_history_outcome/1 case of AAC's vectors, through the CLI
// with the case's responder keys as flags and its outcome pins as the
// --outcome-policy file. The cases that need an adapter's check are skipped
// below: this verifier loads no adapter, so a native object is NOT_CHECKED.
func TestVerifyHistoryRunsThePersonalHistoryOutcomeVectors(t *testing.T) {
	var corpus struct {
		Predicates struct {
			Document json.RawMessage `json:"document"`
		} `json:"predicates"`
		Cases []struct {
			ID   string `json:"id"`
			Pins struct {
				TrustPolicy struct {
					ResponderKeys []string `json:"responder_keys"`
				} `json:"trust_policy"`
				OutcomeRoles        json.RawMessage `json:"outcome_roles"`
				OutcomeRequirements json.RawMessage `json:"outcome_requirements"`
				NativeObservations  map[string]any  `json:"native_observations"`
			} `json:"pins"`
			Inputs   map[string]json.RawMessage `json:"inputs"`
			Expected struct {
				Accepted bool              `json:"accepted"`
				Props    map[string]string `json:"history_properties"`
			} `json:"expected"`
		} `json:"cases"`
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "personal-history-outcome-v0", "vectors.json"))
	require.NoError(t, err)
	require.Equal(t, outcomeVectorsSHA, sha256Hex(raw))
	require.NoError(t, json.Unmarshal(raw, &corpus))
	ran := 0
	for _, tc := range corpus.Cases {
		if len(tc.Pins.NativeObservations) > 0 {
			continue
		}
		ran++
		t.Run(tc.ID, func(t *testing.T) {
			in := map[string]any{"history": outcomeInstance}
			for k, v := range tc.Inputs {
				if k != "history" {
					in[k] = v
				}
			}
			policy, err := json.Marshal(map[string]any{"predicate_table": corpus.Predicates.Document,
				"outcome_roles": tc.Pins.OutcomeRoles, "outcome_requirements": tc.Pins.OutcomeRequirements})
			require.NoError(t, err)
			args := []string{"--outcome-policy", writeHistory(t, policy)}
			for _, k := range tc.Pins.TrustPolicy.ResponderKeys {
				args = append(args, "--holder-key", k)
			}
			got := runService(t, in, args...)
			assert.Equal(t, outcomeInstance, got.Kind)
			if tc.Expected.Accepted {
				assert.Equal(t, "ACCEPTED", got.Verdict)
			} else {
				assert.Equal(t, "NOT_ACCEPTED", got.Verdict)
			}
			for p, want := range tc.Expected.Props {
				assert.Equal(t, want, got.Properties[p], p)
			}
		})
	}
	assert.Positive(t, ran)
}

func TestVerifyHistoryServiceFlagsGoWithTheirOwnFile(t *testing.T) {
	key := "677edd9c7a6fbaa5d609dc7005ce8011d54e0314acf89ca172057bc140ede62a"
	for name, tc := range map[string]struct {
		history string
		args    []string
	}{
		"log key with an outcome file":        {outcomeInstance, []string{"--log-key", key}},
		"outcome policy with an inference":    {inferenceInstance, []string{"--outcome-policy", "x.json"}},
		"a log key that is not 64 hex digits": {inferenceInstance, []string{"--log-key", "abc"}},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{"history": tc.history})
			require.NoError(t, err)
			_, err = invoke(t, "", append([]string{"verify", "--history", writeHistory(t, data)}, tc.args...)...)
			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrHistoryNotAccepted)
		})
	}
}

// A refusal and a rejected exchange say why, and the report's members are
// snake_case (AAC's result structs have no json tags).
func TestVerifyHistoryServiceReportsWhyAndSnakeCase(t *testing.T) {
	var inference struct {
		Cases []struct {
			ID   string `json:"id"`
			Pins struct {
				TrustPolicy struct {
					ResponderKeys []string `json:"responder_keys"`
				} `json:"trust_policy"`
			} `json:"pins"`
			Inputs map[string]any `json:"inputs"`
		} `json:"cases"`
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "inference-service-history-v0", "vectors.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &inference))
	run := func(t *testing.T, id string, withKey bool) map[string]any {
		for _, tc := range inference.Cases {
			if tc.ID != id {
				continue
			}
			tc.Inputs["history"] = inferenceInstance
			data, err := json.Marshal(tc.Inputs)
			require.NoError(t, err)
			args := []string{"verify", "--history", writeHistory(t, data)}
			if withKey {
				for _, k := range tc.Pins.TrustPolicy.ResponderKeys {
					args = append(args, "--holder-key", k)
				}
			}
			out, _ := invoke(t, "", args...)
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &got), out)
			return got
		}
		t.Fatalf("no case %s", id)
		return nil
	}
	t.Run("a refusal", func(t *testing.T) {
		got := run(t, "refused-range-interval", true)
		assert.Equal(t, "NOT_ACCEPTED", got["verdict"])
		assert.Contains(t, got["reason"], "not an envelope")
		assert.NotContains(t, got, "detail", "no zero-value coverage for a refusal")
	})
	t.Run("an answer from an unaccepted responder", func(t *testing.T) {
		got := run(t, "standard-served-only", false)
		assert.Equal(t, "NOT_ACCEPTED", got["verdict"])
		assert.Contains(t, got["findings"], "envelope responder_trust FAIL")
		detail, ok := got["detail"].(map[string]any)
		require.True(t, ok, got)
		for key := range detail["coverage"].(map[string]any) {
			assert.Equal(t, strings.ToLower(key), key, "snake_case member names")
			assert.NotContains(t, key, "A", "no Go field names")
		}
	})
}

func TestVerifyHistoryOutcomePolicyIsStrict(t *testing.T) {
	file, err := json.Marshal(map[string]any{"history": outcomeInstance, "definition": map[string]any{}})
	require.NoError(t, err)
	for name, policy := range map[string]string{
		"a misspelled member":   `{"roles":{}}`,
		"an uppercase role key": `{"outcome_roles":{"judge":["` + strings.Repeat("A", 64) + `"]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := invoke(t, "", "verify", "--history", writeHistory(t, file), "--outcome-policy", writeHistory(t, []byte(policy)))
			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrHistoryNotAccepted)
		})
	}
}
