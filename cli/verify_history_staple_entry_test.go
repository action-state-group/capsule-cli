package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// zeroKey is a well-formed key that signed nothing here.
var zeroKey = hex.EncodeToString(make([]byte, 32))

// A staple answers under one key: given several, StapleVerdict returns the
// result under the key the staple is answered with, whatever the order.
func TestStapleVerdictTakesSeveralCounterpartyKeys(t *testing.T) {
	path := filepath.Join("testdata", "goose-commitment-v1", "commitment.json")
	raw := checkedFile(t, "goose-commitment-v1/commitment.json", gooseCommitmentSHA256)
	key := checkpointKeyOf(t, path)
	for _, keys := range [][]string{{zeroKey, gooseNodeKey}, {gooseNodeKey, zeroKey}} {
		got, err := StapleVerdict(raw, keys, []string{key})
		require.NoError(t, err)
		require.ErrorIs(t, got.Outcome, ErrHistoryAnswersNoRequest, "a clean commitment answers no request")
		assert.Equal(t, "PASS", got.Report.Staples[0].Properties["responder_trust"])
		assert.Equal(t, "NOT_PRESENT", got.Report.Staples[0].Properties["request_binding"])
		assert.Equal(t, []string{"history_card/1"}, got.Derivations)
		assert.Empty(t, got.NotRun)
	}
	got, err := StapleVerdict(raw, []string{zeroKey}, []string{key})
	require.NoError(t, err)
	assert.ErrorIs(t, got.Outcome, ErrHistoryNotAccepted, "no listed key answered the staple")
	assert.NotErrorIs(t, got.Outcome, ErrPartial)
	assert.NotEmpty(t, got.Report.Findings)
}

// A check a missing key leaves unrun is named, so a caller can say which.
func TestStapleVerdictNamesTheChecksNotRun(t *testing.T) {
	raw := checkedFile(t, "goose-commitment-v1/commitment.json", gooseCommitmentSHA256)
	got, err := StapleVerdict(raw, []string{gooseNodeKey}, nil)
	require.NoError(t, err)
	require.ErrorIs(t, got.Outcome, ErrPartial)
	assert.NotErrorIs(t, got.Outcome, ErrHistoryAnswersNoRequest)
	assert.Equal(t, []string{"checkpoint_key"}, got.NotRun)

	got, err = StapleVerdict(raw, nil, nil)
	require.NoError(t, err)
	require.ErrorIs(t, got.Outcome, ErrPartial)
	assert.Contains(t, got.NotRun, "responder_trust", "no counterparty key: the responder is not checked")
}

// history_card/2, as a {history, request, reply} file and stapled in a
// commitment: the derivation is the card's, while the report's kind stays
// as `verify --history` prints it.
func TestStapleVerdictReadsHistoryCard2(t *testing.T) {
	for _, v := range card2Vectors(t) {
		if v.Name != "history_card/2" {
			continue
		}
		key := lastCheckpointKey(t, v.Reply)
		raw, err := os.ReadFile(card2File(t, v))
		require.NoError(t, err)
		got, err := StapleVerdict(raw, []string{zeroKey, v.Holder}, []string{key})
		require.NoError(t, err)
		require.NoError(t, got.Outcome, "%v", got.Report.Findings)
		assert.Equal(t, "ACCEPTED", got.Report.Verdict)
		assert.Equal(t, "history_card/2", got.Report.Kind)
		assert.Equal(t, []string{"history_card/2"}, got.Derivations)
		assert.Equal(t, "PASS", got.Report.Staples[0].Properties["request_binding"])

		sum := sha256.Sum256([]byte(v.Reply))
		commitment, err := json.Marshal(map[string]any{"profile": "evidence-commitment/1",
			"history_cards": []any{map[string]any{"stream": "book-sh-log", "derivation": "history_card/2",
				"bytes": base64.StdEncoding.EncodeToString([]byte(v.Reply)), "sha256": hex.EncodeToString(sum[:])}}})
		require.NoError(t, err)
		got, err = StapleVerdict(commitment, []string{v.Holder}, []string{key})
		require.NoError(t, err)
		assert.Equal(t, []string{"history_card/2"}, got.Derivations)
		assert.Equal(t, "history_card/1", got.Report.Kind, "the verb's kind for any commitment")
		return
	}
	t.Fatal("no history_card/2 vector")
}

// What is not a staple, and a key that is not one, are input errors.
func TestStapleVerdictRefusesWhatIsNotAStaple(t *testing.T) {
	for name, raw := range map[string]string{
		"a History":    `{"history":"personal_history_basic/1"}`,
		"not JSON":     `nope`,
		"another kind": `{"history":"standard_history/1","request":{},"reply":{}}`,
	} {
		_, err := StapleVerdict([]byte(raw), []string{gooseNodeKey}, nil)
		assert.ErrorIs(t, err, ErrInput, name)
	}
	raw := checkedFile(t, "goose-commitment-v1/commitment.json", gooseCommitmentSHA256)
	for _, keys := range [][]string{{""}, {strings.ToUpper(gooseNodeKey)}, {"abc"}} {
		_, err := StapleVerdict(raw, keys, nil)
		assert.ErrorIs(t, err, ErrInput, "counterparty %q", keys)
		_, err = StapleVerdict(raw, []string{gooseNodeKey}, keys)
		assert.ErrorIs(t, err, ErrInput, "checkpoint %q", keys)
	}
}
