package cli

import (
	"slices"
	"strings"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
	"github.com/action-state-group/agent-action-capsule/go/envelope"
	"github.com/stretchr/testify/assert"
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
