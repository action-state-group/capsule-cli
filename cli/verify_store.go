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
// sequence order, the artifact record each names (artifact.Verify against the
// profile's trusted keys), and then every capsule together with AAC's
// VerifyStore. VerifyStore treats its list as the complete store, so it is
// called once on the whole log: called per page, a parent on an earlier page
// would read as missing.
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
	var capsules []any
	var owners []storeCheck // the entry each of capsules came from
	entries, passed := 0, 0
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
			}
			r, err := t.Artifacts.Get(c.Context(), at.CapsuleID)
			if errors.Is(err, artifact.ErrNotFound) {
				fail("record", "the artifact store holds no record for this entry")
				continue
			}
			// The store checks a record as it reads it; a record it refuses
			// is this entry's failure, not a reason to stop.
			if errors.Is(err, artifact.ErrInvalid) || errors.Is(err, artifact.ErrUntrustedSigner) || errors.Is(err, artifact.ErrCorrupt) || errors.Is(err, artifact.ErrDigestMismatch) {
				fail("record", err.Error())
				continue
			}
			if err != nil {
				return err
			}
			if r.CapsuleID != at.CapsuleID {
				fail("record", "the record's capsule_id is "+r.CapsuleID+", not the entry's")
				continue
			}
			// The capsule joins the store-wide checks even when its record
			// fails, so its children do not also read as orphans.
			if capsule, err := aacbundle.Decode(r.Capsule); err == nil {
				capsules = append(capsules, capsule)
				owners = append(owners, at)
			}
			checks, err := artifact.Verify(r, keys)
			if err != nil {
				fail("record", err.Error())
				continue
			}
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
	for i, result := range aacverify.VerifyStore(capsules, nil) {
		if result.OK {
			continue
		}
		var codes []string
		for _, f := range result.Findings {
			if f.Severity == "error" {
				codes = append(codes, f.Code)
			}
		}
		at := owners[i]
		at.Check, at.Reason = "store_checks", strings.Join(codes, ", ")
		failed = append(failed, at)
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
