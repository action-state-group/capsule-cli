package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/personalhistory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// personalHistoryVectorsSHA256 pins
// testdata/personal-history-basic-v0/vectors.json to agent-action-capsule
// vectors/personal-history-basic-v0/vectors.json at 2cf9037 (its SHA256SUMS
// records this digest). The copy is byte for byte.
const personalHistoryVectorsSHA256 = "f1f0e1b95c7dd32f2ddc584d4c55bc9a643a58f550a941651bfd5d3e4c59a4b9"

type historyVectorCase struct {
	ID   string `json:"id"`
	Pins struct {
		TrustPolicy struct {
			ResponderKeys []string `json:"responder_keys"`
		} `json:"trust_policy"`
	} `json:"pins"`
	Inputs struct {
		Definition json.RawMessage `json:"definition"`
		Exchanges  []struct {
			Request historyVectorMessage `json:"request"`
			Reply   historyVectorMessage `json:"reply"`
		} `json:"exchanges"`
	} `json:"inputs"`
	Expected struct {
		Exchanges []struct {
			Properties map[string]string `json:"properties"`
			Accepted   bool              `json:"accepted"`
		} `json:"exchanges"`
		HistoryProperties map[string]string `json:"history_properties"`
		Accepted          bool              `json:"accepted"`
		DealStates        *struct {
			SelectedDeals int               `json:"selected_deals"`
			PerDeal       map[string]string `json:"per_deal"`
			Counts        map[string]int    `json:"counts"`
		} `json:"deal_states"`
	} `json:"expected"`
}

type historyVectorMessage struct {
	MediaType  string `json:"media_type"`
	OctetsText string `json:"octets_text"`
}

// historyVectors is V's 13 History cases (refused-other-requester is a lone
// refusal, not a History file; agent-action-capsule's own tests check it).
func historyVectors(t *testing.T) []historyVectorCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "personal-history-basic-v0", "vectors.json"))
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	require.Equal(t, personalHistoryVectorsSHA256, hex.EncodeToString(sum[:]))
	var file struct {
		Cases []historyVectorCase `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &file))
	var out []historyVectorCase
	for _, c := range file.Cases {
		if c.Inputs.Definition != nil {
			out = append(out, c)
		}
	}
	require.Len(t, out, 13)
	return out
}

// historyFileOf writes a vector case as the one-file History it holds
// (PH §8.2).
func historyFileOf(t *testing.T, c historyVectorCase) []byte {
	t.Helper()
	encoded := map[string]any{"history": personalhistory.Name, "definition": c.Inputs.Definition}
	var exchanges []any
	for _, x := range c.Inputs.Exchanges {
		exchanges = append(exchanges, map[string]any{"request": x.Request, "reply": x.Reply})
	}
	encoded["exchanges"] = exchanges
	data, err := json.Marshal(encoded)
	require.NoError(t, err)
	return data
}

func writeHistory(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

// Every History case of V, through `verify --history` with the case's
// trusted responder keys as --holder-key: each exchange's properties and
// acceptance, the seven History properties, the verdict and the deal
// states are V's.
func TestVerifyHistoryRunsThePersonalHistoryVectors(t *testing.T) {
	for _, c := range historyVectors(t) {
		t.Run(c.ID, func(t *testing.T) {
			args := []string{"verify", "--history", writeHistory(t, historyFileOf(t, c))}
			for _, k := range c.Pins.TrustPolicy.ResponderKeys {
				args = append(args, "--holder-key", k)
			}
			out, err := invoke(t, "", args...)
			var got struct {
				Verdict           string            `json:"verdict"`
				HistoryProperties map[string]string `json:"history_properties"`
				Exchanges         []struct {
					Properties map[string]string `json:"properties"`
					Accepted   bool              `json:"accepted"`
				} `json:"exchanges"`
				SelectedDeals *int           `json:"selected_deals"`
				DealStates    map[string]int `json:"deal_states"`
				Deals         []struct {
					Deal  string `json:"deal"`
					State string `json:"state"`
				} `json:"deals"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &got), out)
			assert.Equal(t, c.Expected.HistoryProperties, got.HistoryProperties)
			require.Len(t, got.Exchanges, len(c.Expected.Exchanges))
			for i, x := range c.Expected.Exchanges {
				assert.Equal(t, x.Properties, got.Exchanges[i].Properties, "exchange %d", i)
				assert.Equal(t, x.Accepted, got.Exchanges[i].Accepted, "exchange %d", i)
			}
			if c.Expected.Accepted {
				require.NoError(t, err)
				assert.Equal(t, "ACCEPTED", got.Verdict)
				require.NotNil(t, got.SelectedDeals)
				assert.Equal(t, c.Expected.DealStates.SelectedDeals, *got.SelectedDeals)
				assert.Equal(t, c.Expected.DealStates.Counts, got.DealStates)
				require.Len(t, got.Deals, len(c.Expected.DealStates.PerDeal))
				for _, d := range got.Deals {
					assert.Equal(t, c.Expected.DealStates.PerDeal[d.Deal], d.State, d.Deal)
				}
			} else {
				assert.ErrorIs(t, err, ErrHistoryNotAccepted)
				assert.Equal(t, 1, ExitCode(err))
				assert.Equal(t, "NOT_ACCEPTED", got.Verdict)
				assert.Nil(t, got.SelectedDeals, "no deal states for a History not accepted")
			}
		})
	}
}

func positiveHistory(t *testing.T) (historyVectorCase, string) {
	t.Helper()
	for _, c := range historyVectors(t) {
		if c.ID == "history-basic" {
			return c, writeHistory(t, historyFileOf(t, c))
		}
	}
	t.Fatal("no history-basic case")
	return historyVectorCase{}, ""
}

// An accepted History is shown in PH §6.3's order: what is not
// established first, then the counts, then each deal's records with their
// committed and claimed times apart, then the checks not run and the
// limits.
func TestVerifyHistoryShowsWhatPH63Asks(t *testing.T) {
	c, path := positiveHistory(t)
	out, err := invoke(t, "", "verify", "--history", path, "--holder-key", c.Pins.TrustPolicy.ResponderKeys[0])
	require.NoError(t, err, out)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, "ACCEPTED", got["verdict"])
	// The members come in PH §6.3's order in the output itself.
	at := func(member string) int {
		i := strings.Index(out, `"`+member+`":`)
		require.GreaterOrEqual(t, i, 0, member)
		return i
	}
	order := []string{"not_established", "coverage", "completeness", "selected_deals", "deal_states", "deals", "history_properties", "not_run", "limits"}
	for i := 1; i < len(order); i++ {
		assert.Less(t, at(order[i-1]), at(order[i]), "%s before %s", order[i-1], order[i])
	}
	assert.True(t, strings.HasPrefix(out, `{"spec_version":"capsule-cli-result/v1",`))
	assert.Equal(t, "holder_selected", got["coverage"])
	assert.Equal(t, "not_asserted", got["completeness"])
	assert.Contains(t, got["not_established"], "any outcome: sale, payment, delivery, receipt or return")
	assert.Equal(t, "handed_over", got["mode"])
	assert.Equal(t, "recipient-1", got["recipient"])
	assert.Equal(t, []any{c.Pins.TrustPolicy.ResponderKeys[0]}, got["signing_keys"])
	assert.Contains(t, strings.Join(toStrings(got["not_run"]), "\n"), "anchor_inclusion")
	deals := got["deals"].([]any)
	require.Len(t, deals, 3)
	record := deals[0].(map[string]any)["records"].([]any)[0].(map[string]any)
	for _, member := range []string{"digest", "leaf_index", "committed_at", "event_time_claim", "record_type", "epistemic_type"} {
		assert.Contains(t, record, member)
	}
	assert.NotEqual(t, record["committed_at"], record["event_time_claim"], "two times, shown apart")
	assert.NotEmpty(t, deals[0].(map[string]any)["state"])
}

func toStrings(v any) []string {
	var out []string
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

// With no --holder-key nothing is accepted (IP §7.2): the History is
// reported not accepted, with the key it is signed by, and exit 3; its
// other checks still run as if that key were trusted, so a real defect
// still exits 1.
func TestVerifyHistoryWithoutAHolderKey(t *testing.T) {
	c, path := positiveHistory(t)
	out, err := invoke(t, "", "verify", "--history", path)
	assert.ErrorIs(t, err, ErrPartial)
	assert.Equal(t, 3, ExitCode(err))
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.Equal(t, "NOT_ACCEPTED", got["verdict"])
	assert.Equal(t, []any{c.Pins.TrustPolicy.ResponderKeys[0]}, got["signing_keys"])
	assert.Contains(t, got["trust_note"], "--holder-key")
	assert.Empty(t, got["findings"])
	assert.Nil(t, got["deal_states"])
	for _, x := range got["exchanges"].([]any) {
		assert.Equal(t, false, x.(map[string]any)["accepted"])
		assert.Equal(t, "FAIL", x.(map[string]any)["properties"].(map[string]any)["responder_trust"])
	}

	for _, v := range historyVectors(t) {
		if v.ID != "mutant-second-trusted-key" && v.ID != "mutant-record-kind-outside-list" {
			continue
		}
		out, err := invoke(t, "", "verify", "--history", writeHistory(t, historyFileOf(t, v)))
		assert.ErrorIs(t, err, ErrHistoryNotAccepted, v.ID)
		require.NoError(t, json.Unmarshal([]byte(out), &got), out)
		assert.NotEmpty(t, got["findings"], v.ID)
	}
}

// A History no key could make acceptable is a failure, not a key left to
// pin: an empty History, or one whose replies name no holder key, exits 1
// without --holder-key.
func TestVerifyHistoryWithNothingToPin(t *testing.T) {
	c, _ := positiveHistory(t)
	empty := []byte(`{"history":"personal_history_basic/1","definition":` + string(c.Inputs.Definition) + `,"exchanges":[]}`)
	out, err := invoke(t, "", "verify", "--history", writeHistory(t, empty))
	assert.ErrorIs(t, err, ErrHistoryNotAccepted, out)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.Contains(t, got["findings"], "no reply is an envelope naming a holder key")

	noKey := c
	noKey.Inputs.Exchanges = append(noKey.Inputs.Exchanges[:0:0], c.Inputs.Exchanges...)
	for i := 0; i < len(noKey.Inputs.Exchanges); i++ {
		noKey.Inputs.Exchanges[i].Reply.OctetsText = strings.Replace(c.Inputs.Exchanges[i].Reply.OctetsText, `"responder":"`, `"responder_was":"`, 1)
	}
	out, err = invoke(t, "", "verify", "--history", writeHistory(t, historyFileOf(t, noKey)))
	assert.ErrorIs(t, err, ErrHistoryNotAccepted, "replies that name no holder key")
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.Contains(t, got["findings"], "no reply is an envelope naming a holder key")
}

// An answer PH §6.1 rejects without a FAIL still fails the History under a
// pinned key: here an extra envelope, re-signed by the holder's test key,
// that carries a verification member, which a Basic answer must not.
func TestVerifyHistoryFailsAnAnswerRejectedWithoutAFail(t *testing.T) {
	c, _ := positiveHistory(t)
	seed := sha256.Sum256([]byte("personal-agent-history-profile-v0 conformance vectors: holder key"))
	key := ed25519.NewKeyFromSeed(seed[:])
	var env map[string]any
	require.NoError(t, json.Unmarshal([]byte(c.Inputs.Exchanges[0].Reply.OctetsText), &env))
	material := []byte("material")
	env["verification"] = []any{map[string]any{"kind": "x-kind", "digest": hex.EncodeToString(sha256Of(material))}}
	env["verification_content"] = []any{base64.RawURLEncoding.EncodeToString(material)}
	signed := map[string]any{}
	for _, k := range []string{"profile", "request_digest", "subject", "derivation", "anchor", "artifact_digest", "verification", "responder", "issued_at"} {
		signed[k] = env[k]
	}
	body, err := canonical.JCS(signed)
	require.NoError(t, err)
	env["sig"] = hex.EncodeToString(ed25519.Sign(key, body))
	reply, err := canonical.JCS(env)
	require.NoError(t, err)
	extra := c
	extra.Inputs.Exchanges = append(append(extra.Inputs.Exchanges[:0:0], c.Inputs.Exchanges...), c.Inputs.Exchanges[0])
	extra.Inputs.Exchanges[len(extra.Inputs.Exchanges)-1].Reply.OctetsText = string(reply)
	out, err := invoke(t, "", "verify", "--history", writeHistory(t, historyFileOf(t, extra)), "--holder-key", c.Pins.TrustPolicy.ResponderKeys[0])
	assert.ErrorIs(t, err, ErrHistoryNotAccepted, out)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.Equal(t, []any{"exchange 3: not accepted (PH §6.1)"}, got["findings"], "the only finding: no property FAILs")
	for _, s := range got["history_properties"].(map[string]any) {
		assert.Equal(t, "PASS", s)
	}
	for _, s := range got["exchanges"].([]any)[3].(map[string]any)["properties"].(map[string]any) {
		assert.NotEqual(t, "FAIL", s)
	}
}

// A request in the file that is not a request map is an input error.
func TestVerifyHistoryRefusesARequestThatIsNotAMap(t *testing.T) {
	c, _ := positiveHistory(t)
	bad := c
	bad.Inputs.Exchanges = append(bad.Inputs.Exchanges[:0:0], c.Inputs.Exchanges...)
	bad.Inputs.Exchanges[0].Request.OctetsText = "[]"
	_, err := invoke(t, "", "verify", "--history", writeHistory(t, historyFileOf(t, bad)), "--holder-key", c.Pins.TrustPolicy.ResponderKeys[0])
	assert.ErrorIs(t, err, ErrInput)
	assert.Equal(t, 2, ExitCode(err))
}

func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func TestVerifyHistoryUnderAnotherKey(t *testing.T) {
	_, path := positiveHistory(t)
	out, err := invoke(t, "", "verify", "--history", path, "--holder-key", strings.Repeat("ab", 32))
	assert.ErrorIs(t, err, ErrHistoryNotAccepted)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.Contains(t, got["findings"], "exchange 0: responder_trust FAIL")
}

func TestVerifyHistoryRefusesWhatIsNotAHistoryFile(t *testing.T) {
	c, _ := positiveHistory(t)
	good := historyFileOf(t, c)
	var value map[string]any
	require.NoError(t, json.Unmarshal(good, &value))
	with := func(edit func(map[string]any)) []byte {
		clone := map[string]any{}
		for k, v := range value {
			clone[k] = v
		}
		edit(clone)
		data, err := json.Marshal(clone)
		require.NoError(t, err)
		return data
	}
	for name, data := range map[string][]byte{
		"not json":           []byte("not json"),
		"an array":           []byte("[]"),
		"duplicate member":   []byte(strings.Replace(string(good), `{"definition"`, `{"history":"x","definition"`, 1)),
		"an extra member":    with(func(m map[string]any) { m["carrier"] = "x-history-selection/v0" }),
		"no definition":      with(func(m map[string]any) { delete(m, "definition") }),
		"exchanges not list": with(func(m map[string]any) { m["exchanges"] = map[string]any{} }),
		"cbor given as text": with(func(m map[string]any) {
			m["exchanges"] = []any{map[string]any{"request": map[string]any{"media_type": "application/cbor", "octets_text": "{}"}, "reply": map[string]any{"media_type": "application/cbor", "octets_text": "{}"}}}
		}),
	} {
		require.NotEqual(t, string(good), string(data), name)
		_, err := invoke(t, "", "verify", "--history", writeHistory(t, data), "--holder-key", c.Pins.TrustPolicy.ResponderKeys[0])
		assert.ErrorIs(t, err, ErrInput, name)
		assert.Equal(t, 2, ExitCode(err), name)
	}
}

func TestVerifyHistoryFlags(t *testing.T) {
	_, path := positiveHistory(t)
	_, err := invoke(t, "", "verify", "--history", path, "--holder-key", "ABCD")
	assert.ErrorIs(t, err, ErrInput, "a holder key is 64 lowercase hex characters")
	_, err = invoke(t, "", "verify", "--holder-key", strings.Repeat("ab", 32), "--capsule", path)
	assert.ErrorIs(t, err, ErrInput, "--holder-key goes with --history")
	for _, other := range []string{"--bundle", "--capsule"} {
		_, err = invoke(t, "", "verify", "--history", path, other, path)
		assert.ErrorIs(t, err, ErrInput, "--history and %s are exclusive", other)
		assert.Equal(t, 2, ExitCode(err))
	}
	_, err = invoke(t, "", "verify", "--bundle", path, "--holder-key", strings.Repeat("ab", 32))
	assert.ErrorIs(t, err, ErrInput, "--holder-key is never ignored beside --bundle")
	_, err = invoke(t, "", "verify", "--history", path, "--witness-directory", path)
	assert.ErrorIs(t, err, ErrInput, "--witness-directory is never ignored beside --history")
}

// The limits are the file's, so they are printed from the file alone, on a
// machine with no store: who chose each reference and what epistemic_type
// is are among them.
func TestVerifyHistoryPrintsItsLimitsWithNoStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c, path := positiveHistory(t)
	out, err := invoke(t, "", "verify", "--history", path, "--holder-key", c.Pins.TrustPolicy.ResponderKeys[0])
	require.NoError(t, err, out)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	limits := strings.Join(toStrings(got["limits"]), "\n")
	assert.Contains(t, limits, "a deal reference is chosen by whichever party opened the deal, and the file does not say which")
	assert.Contains(t, limits, "epistemic_type is the holder's own label")
	assert.Contains(t, limits, "two recipients can tell they received the same deal")
}
