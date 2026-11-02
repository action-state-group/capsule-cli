package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
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
