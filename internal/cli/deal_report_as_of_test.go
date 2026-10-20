package cli

import (
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/stretchr/testify/assert"
)

// asOfBundle is the part of a copy dealReportAsOf reads: its checkpoint, its
// steps (the last by log seq stamped stepAt) and its cadence chain.
func asOfBundle(stepAt, extent string) map[string]interface{} {
	return map[string]interface{}{
		"checkpoint": map[string]interface{}{"timestamp": "2026-09-27T18:00:00Z"},
		"records": []interface{}{
			map[string]interface{}{"capsule_id": "a", "evidence_record_header": map[string]interface{}{"record_type": "deal_step", "committed_at": "2026-09-27T17:00:00Z"}},
			map[string]interface{}{"capsule_id": "b", "evidence_record_header": map[string]interface{}{"record_type": "step_completed", "committed_at": stepAt}},
		},
		"completeness_certificate": map[string]interface{}{"memberships": map[string]interface{}{
			"a": map[string]interface{}{"log_coordinates": map[string]interface{}{"seq": float64(1)}},
			"b": map[string]interface{}{"log_coordinates": map[string]interface{}{"seq": float64(2)}},
		}},
		// A cadence tick's own time is never the report's.
		"extensions": map[string]interface{}{DealCadenceExtension: map[string]interface{}{"extent": extent, "checkpoint_at": "2026-09-28T09:00:00Z"}},
	}
}

// verify --bundle's as_of line for a standalone report: the checkpoint's
// signed time, named the last step's only when that step states the same
// time, and a witness only for a passing claim anchored at that checkpoint
// covering all of it; a failing claim is never shown as merely unwitnessed.
func TestDealReportAsOfSaysWhatBacksTheTime(t *testing.T) {
	pass, fail := aacbundle.ClaimResult{Status: "pass"}, aacbundle.ClaimResult{Status: "fail"}
	for name, c := range map[string]struct {
		value     map[string]interface{}
		witnesses aacbundle.ClaimResult
		anchored  bool
		want      string
	}{
		"the last step's time, unwitnessed": {asOfBundle("2026-09-27T18:00:00Z", "all"), aacbundle.ClaimResult{}, false, "as of the deal's last step at 2026-09-27T18:00:00Z (the producer's time); not witnessed"},
		"another time is the checkpoint's":  {asOfBundle("2026-09-27T17:30:00Z", "all"), aacbundle.ClaimResult{}, false, "as of the deal's checkpoint at 2026-09-27T18:00:00Z (the producer's time); not witnessed"},
		"witnessed":                         {asOfBundle("2026-09-27T18:00:00Z", "all"), pass, true, "as of the deal's last step at 2026-09-27T18:00:00Z (the producer's time); witnessed"},
		"a witness of part of it":           {asOfBundle("2026-09-27T18:00:00Z", "part"), pass, true, "as of the deal's last step at 2026-09-27T18:00:00Z (the producer's time); not witnessed"},
		"a later checkpoint's witness":      {asOfBundle("2026-09-27T18:00:00Z", "all"), pass, false, "as of the deal's last step at 2026-09-27T18:00:00Z (the producer's time); not witnessed"},
		"a failing claim":                   {asOfBundle("2026-09-27T18:00:00Z", "all"), fail, true, "as of the deal's last step at 2026-09-27T18:00:00Z (the producer's time); its witness claim failed"},
		"an unreadable checkpoint time":     {map[string]interface{}{"checkpoint": map[string]interface{}{"timestamp": "soon"}}, aacbundle.ClaimResult{}, false, "as of an unreadable checkpoint time"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, c.want, dealReportAsOf(c.value, c.witnesses, c.anchored))
		})
	}
}
