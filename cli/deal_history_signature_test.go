package cli

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
	"github.com/action-state-group/agent-action-capsule/go/envelope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The History's selection record is signed exactly when verify --bundle's
// per-record verdict for its capsule id (AAC strict, the last listing of a
// repeated id) is authored; the History check does not verify it again. So
// an unsigned record listed after a signed selection record under the same
// id leaves the selection unsigned, as the CLI read it before (#438).
func TestHistorySelectionRecordSignatureIsTheStrictVerdict(t *testing.T) {
	id := strings.Repeat("ab", 32)
	value := map[string]interface{}{
		"root":    id,
		"records": []interface{}{map[string]interface{}{"capsule_id": id, "action_id": HistoryActionID, "key_id": "holder"}},
		"disclosures": map[string]interface{}{id: map[string]interface{}{"agent_input": map[string]interface{}{
			"type": "history_selection", "profile": HistorySelectionExtension,
		}}},
		"extensions": map[string]interface{}{HistorySelectionExtension: map[string]interface{}{"selection_record": id}},
	}
	result := aacbundle.VerificationResult{Disclosures: []aacbundle.DisclosureResult{{CapsuleID: id, Member: "agent_input", Status: disclosure.Match}}}

	for verdict, unsigned := range map[string]bool{envelope.Authored: false, envelope.Unclaimed: true, envelope.Invalid: true} {
		_, claim := historyEntry(value, result, map[string]string{id: verdict}, nil)
		assert.Equal(t, unsigned, slices.Contains(claim.Findings, "selection_record_unsigned"), verdict)
		assert.NotContains(t, claim.Findings, "selection_record_unverified", verdict)
	}
}

// A carried deal whose checkpoint claim fails (here: no log_id of its own,
// as AAC strict requires) has no authenticated statement, so the History
// reports it unbound to its log (capsule-cli#438).
func TestHistoryReportsADealWhoseCheckpointClaimFailsAsUnbound(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const id = "deal-0123456789abcdef"
	findings := func(edit func(map[string]interface{})) []string {
		raw, err := os.ReadFile(producedBundle(t, edit))
		require.NoError(t, err)
		deal, err := DecodeBundleJSON(raw)
		require.NoError(t, err)
		return historyDealFindings(deal, id)
	}
	// With its own log_id the claim passes, and the log is compared: the
	// test fixture's log is not this deal's.
	control := findings(nil)
	assert.Contains(t, control, "deal_not_of_its_log:"+id)
	assert.NotContains(t, control, "deal_log_unbound:"+id)

	unbound := findings(func(b map[string]interface{}) { delete(b["checkpoint"].(map[string]interface{}), "log_id") })
	assert.Contains(t, unbound, "deal_log_unbound:"+id)
	assert.NotContains(t, unbound, "deal_not_of_its_log:"+id)
}

// historyDealFindings is historyEntry's findings for a History carrying deal
// as its one selected deal.
func historyDealFindings(deal map[string]interface{}, id string) []string {
	value := map[string]interface{}{
		"extensions": map[string]interface{}{HistorySelectionExtension: map[string]interface{}{
			"deals": map[string]interface{}{id: deal},
		}},
		"disclosures": map[string]interface{}{},
	}
	sealed := map[string]interface{}{"selection": []interface{}{map[string]interface{}{
		"deal_id": id, "bundle_digest": JCSDigest(deal), "steps_after_checkpoint": json.Number("0"),
	}}}
	value["extensions"].(map[string]interface{})[HistorySelectionExtension].(map[string]interface{})["selection_record"] = "r"
	value["disclosures"] = map[string]interface{}{"r": map[string]interface{}{"agent_input": sealed}}
	_, claim := historyEntry(value, aacbundle.VerificationResult{}, nil, nil)
	return claim.Findings
}
