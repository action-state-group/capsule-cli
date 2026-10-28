package cli

import (
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	aacverify "github.com/action-state-group/agent-action-capsule/go/verify"
	"github.com/action-state-group/capsule-emit/go/artifact"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/spf13/cobra"
)

// storeScanPage is the CLL page size verify --profile reads with; tests
// shrink it to put a parent and its child on different pages.
var storeScanPage = cll.MaxScanLimit

// storeCheck is one check of `verify --profile` that failed or could not be
// made, named by the CLL entry it concerns.
type storeCheck struct {
	Sequence  uint64 `json:"sequence"`
	CapsuleID string `json:"capsule_id"`
	Check     string `json:"check"`
	Reason    string `json:"reason"`
}

// verifyStore verifies what a profile's store holds: every CLL entry in
// sequence order, the artifact record each names, and then every capsule
// together with AAC's VerifyStore. VerifyStore treats its list as the complete
// store, so it is called once on the whole log: called per page, a parent on an
// earlier page would read as missing.
//
// A check that failed (exit 1) and one that could not be made (exit 3) are
// kept in separate lists, so a missing original never reads as a forgery.
func verifyStore(c *cobra.Command, p Profile) (err error) {
	if p.HasPluginLog() {
		return InputError("verify --profile reads a log capsulectl keeps; profile " + p.Name + "'s log is kept by the capsulectl-" + p.LogOwner() + " plugin")
	}
	t, err := OpenTarget(c.Context(), p, UseRead)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, t.Close()) }()
	keys, err := ParseKeys(p.TrustedKeys)
	if err != nil {
		return err
	}
	failed, unchecked := []storeCheck{}, []storeCheck{}
	// capsules is the store VerifyStore checks, in CLL order. An entry whose
	// record could not be read joins it as its bare capsule_id, which
	// VerifyStore counts as present, so its children do not read as orphans;
	// only the entries with a read record (read) have results to report.
	var capsules []any
	var owners []storeCheck
	var read []bool
	entries, passed, unread := 0, 0, 0
	for after := uint64(0); ; {
		page, err := t.Log.ScanEntries(c.Context(), after, storeScanPage)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, entry := range page {
			after = entry.Seq
			entries++
			at := storeCheck{Sequence: entry.Seq, CapsuleID: hex.EncodeToString(entry.Value)}
			fail := func(check, reason string) {
				at.Check, at.Reason = check, reason
				failed = append(failed, at)
				capsules, owners, read = append(capsules, at.CapsuleID), append(owners, at), append(read, false)
				unread++
			}
			// The store authenticates a record as it reads it (artifact.Verify
			// with the profile's trusted keys, the record's capsule_id against
			// the one asked for), so a record it refuses is this entry's failure.
			r, err := t.Artifacts.Get(c.Context(), at.CapsuleID)
			switch {
			case errors.Is(err, artifact.ErrNotFound):
				fail("record", "the artifact store holds no record for this entry")
				continue
			case errors.Is(err, artifact.ErrInvalid), errors.Is(err, artifact.ErrUntrustedSigner), errors.Is(err, artifact.ErrCorrupt), errors.Is(err, artifact.ErrDigestMismatch):
				fail("record", err.Error())
				continue
			case err != nil:
				return err
			}
			// Verify again for its per-original results, which Get does not
			// return; it fails only if the store did not verify on read.
			checks, err := artifact.Verify(r, keys)
			if err != nil {
				fail("record", err.Error())
				continue
			}
			capsule, err := aacbundle.Decode(r.Capsule)
			if err != nil {
				fail("record", "the capsule is not valid JSON: "+err.Error())
				continue
			}
			capsules, owners, read = append(capsules, capsule), append(owners, at), append(read, true)
			ok := true
			for _, name := range MissingBindings(r) {
				at.Check, at.Reason = "originals", "original not retained: "+name
				unchecked = append(unchecked, at)
				ok = false
			}
			names := make([]string, 0, len(checks))
			for name := range checks {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if check := checks[name]; check.Bound && !check.Verified {
					at.Check, at.Reason = "originals", "bound original not verified: "+name
					unchecked = append(unchecked, at)
					ok = false
				}
			}
			if ok {
				passed++
			}
		}
	}
	storeChecks := "passed"
	if unread > 0 {
		storeChecks = "incomplete"
	}
	storeFailed := false
	for i, result := range aacverify.VerifyStore(capsules, nil) {
		if !read[i] || result.OK {
			continue
		}
		reason, internal := storeFindings(result)
		at := owners[i]
		at.Check, at.Reason = "store_checks", reason
		// The verifier itself could not run: nothing was checked, so
		// nothing failed.
		if internal {
			unchecked = append(unchecked, at)
			storeChecks = "incomplete"
			continue
		}
		failed = append(failed, at)
		storeFailed = true
	}
	if storeFailed {
		storeChecks = "failed"
	}
	out := map[string]any{
		"log_id": p.LogID, "entries": entries, "records_passed": passed,
		"store_checks": storeChecks, "failed": failed, "could_not_check": unchecked,
		"checkpoint": "not_performed: see `capsulectl cll verify`", "business_truth": "not_performed",
	}
	if err = Output(c, out); err != nil {
		return err
	}
	switch {
	case len(failed) > 0:
		return errors.New("store verification failed: " + failed[0].Check + " at sequence " + strconv.FormatUint(failed[0].Sequence, 10))
	case len(unchecked) > 0:
		return ErrPartial
	}
	return nil
}

// storeFindings joins a failed VerifyStore result's error codes, and reports
// whether every one is verifier_internal_error: the verifier could not run,
// so the capsule was not checked rather than found wanting.
func storeFindings(result aacverify.VerificationResult) (string, bool) {
	var codes []string
	internal := true
	for _, f := range result.Findings {
		if f.Severity == "error" {
			codes = append(codes, f.Code)
			internal = internal && f.Code == "verifier_internal_error"
		}
	}
	return strings.Join(codes, ", "), internal && len(codes) > 0
}
