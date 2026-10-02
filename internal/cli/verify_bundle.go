package cli

import (
	"errors"
	"sort"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/spf13/cobra"
)

// ErrBundleInvalid is a failed Evidence Bundle check: the result was printed,
// and at least one claim failed.
var ErrBundleInvalid = errors.New("evidence bundle verification failed")

// claimOutput is one claim's status, as printed by `verify --bundle`.
type claimOutput struct {
	Status   string   `json:"status"`
	Findings []string `json:"findings"`
}

func claim(result aacbundle.ClaimResult) claimOutput {
	findings := result.Findings
	if findings == nil {
		findings = []string{}
	}
	return claimOutput{Status: result.Status, Findings: findings}
}

// verifyBundleFile checks an Evidence Bundle (evidence-bundle/v2) offline, from
// the file alone: every record's identity, citation closure, interval
// coverage and per-record membership under the checkpoint (authenticated
// when the bundle carries checkpoint.cose), disclosures, and which extensions
// and countersignatures were carried but not verified here. No profile, no
// network. Exit 0 when every claim passes, 3 (ErrPartial) when nothing failed
// but something is not shown, 1 (ErrBundleInvalid) when a claim failed.
func verifyBundleFile(c *cobra.Command, path string) error {
	raw, err := readInput(path)
	if err != nil {
		return err
	}
	value, err := decodeBundleJSON(raw)
	if err != nil {
		return err
	}
	if value["bundle_kind"] != "evidence-bundle/v2" || value["bundle_version"] != "2" {
		return inputError("not an evidence-bundle/v2 file")
	}
	result := aacbundle.VerifyBundle(value)

	records := map[string]string{}
	ids := make([]string, 0, len(result.CapsuleResults))
	for id := range result.CapsuleResults {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	identityOK := len(ids) > 0
	for _, id := range ids {
		if result.CapsuleResults[id].OK {
			records[id] = "passed"
		} else {
			records[id] = "failed"
			identityOK = false
		}
	}
	disclosures := make([]map[string]string, 0, len(result.Disclosures))
	for _, d := range result.Disclosures {
		disclosures = append(disclosures, map[string]string{"capsule_id": d.CapsuleID, "member": d.Member, "status": d.Status})
	}
	extensions := make([]map[string]string, 0, len(result.Extensions))
	for _, x := range result.Extensions {
		extensions = append(extensions, map[string]string{"kind": x.Kind, "status": x.Status})
	}
	countersignatures := make([]string, 0, len(result.Countersignatures))
	for _, s := range result.Countersignatures {
		countersignatures = append(countersignatures, s.Status)
	}

	claims := []aacbundle.ClaimResult{result.GraphClosure, result.IntervalCoverage, result.PerRecordMembership}
	verdict := "VALID"
	for _, r := range claims {
		if r.Status == "fail" {
			verdict = "INVALID"
		} else if (r.Status != "pass" || len(r.Findings) != 0) && verdict == "VALID" {
			verdict = "INCOMPLETE"
		}
	}
	if !identityOK {
		verdict = "INVALID"
	}

	digest := ""
	if result.BundleDigest != nil {
		digest = *result.BundleDigest
	}
	if err := output(c, map[string]any{
		"verdict":               verdict,
		"bundle_digest":         digest,
		"record_identity":       records,
		"graph_closure":         claim(result.GraphClosure),
		"interval_coverage":     claim(result.IntervalCoverage),
		"per_record_membership": claim(result.PerRecordMembership),
		"disclosures":           disclosures,
		"extensions":            extensions,
		"countersignatures":     countersignatures,
		"producer_signatures":   "not_performed: records' producer signatures are not checked by verify --bundle",
	}); err != nil {
		return err
	}
	switch verdict {
	case "VALID":
		return nil
	case "INCOMPLETE":
		return ErrPartial
	default:
		return ErrBundleInvalid
	}
}
