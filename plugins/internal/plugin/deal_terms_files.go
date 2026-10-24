package plugin

import (
	"encoding/json"
	"strings"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/action-state-group/evidencebook"
	"github.com/spf13/cobra"
)

// The adjudicator's check (capsule-cli#278 Decision 2). An adjudicator has
// no deal profile and no book: it holds each party's own `disclose --share
// adjudicator` file, which carries that party's openings (Decision 1), and
// the keys it was given for each party, out of band. `deal terms-result
// --check RESULT --copy FILE --copy FILE --party-key SIGNER:CHECKPOINT
// --party-key SIGNER:CHECKPOINT` checks a terms Result from those alone: it
// opens no store and does not go through runDeal.

// partyKeys are the signing and checkpoint keys an adjudicator pins for one
// party, as hex.
type partyKeys struct{ signer, checkpoint string }

func parsePartyKeys(values []string) ([]partyKeys, error) {
	keys := make([]partyKeys, 0, len(values))
	for _, v := range values {
		signer, checkpoint, ok := strings.Cut(v, ":")
		if !ok || len(signer) != 64 || len(checkpoint) != 64 || !cli.IsLowerHex(signer) || !cli.IsLowerHex(checkpoint) {
			return nil, cli.InputError("--party-key is SIGNER:CHECKPOINT, each a public key as 64 lowercase hex characters")
		}
		keys = append(keys, partyKeys{signer: signer, checkpoint: checkpoint})
	}
	if len(keys) == 2 && (keys[0].signer == keys[1].signer || keys[0].checkpoint == keys[1].checkpoint) {
		return nil, cli.InputError("the two --party-key pairs must name two parties: their signing keys differ, and so do their checkpoint keys")
	}
	return keys, nil
}

// fileCopy is one party's file, verified and its half admitted.
type fileCopy struct {
	side     reconcileSide
	logID    string
	openings map[string]evidencebook.Opening
}

// admitFileCopy verifies one party's adjudicator file and admits its half:
// the bundle is VALID with its checkpoint signed by a pinned checkpoint key,
// the half is the one record its own field_openings names, and it passes
// §2.10's admission checks 1 to 5 under the signing key paired with that
// checkpoint key. A bundle check alone does not pin the signer, since each
// capsule verifies under the key it declares itself, so check 1 does. The
// pair is found by the copy's checkpoint key, then must sign the half:
// flags are never paired by order. ok is false when the file carries no
// openings, or not both fields: the check cannot reproduce from it.
func admitFileCopy(raw []byte, keys []partyKeys) (copyOf fileCopy, ok bool, err error) {
	bundle, err := cli.DecodeBundleJSON(raw)
	if err != nil || bundle["bundle_kind"] != "evidence-bundle/v2" {
		return fileCopy{}, false, cli.InputError("a --copy is not an evidence-bundle/v2 deal copy")
	}
	if verdict, _ := cli.BundleVerdict(bundle, nil); verdict != "VALID" {
		return fileCopy{}, false, cli.InputError("a --copy verifies " + verdict + ", not VALID")
	}
	_, statement := cli.CheckpointClaim(bundle)
	signed, err := cli.SignedCheckpoint(statement)
	if err != nil {
		return fileCopy{}, false, cli.InputError("a --copy's checkpoint does not verify")
	}
	var pinned *partyKeys
	for i := range keys {
		if signed["key_id"] == keys[i].checkpoint {
			pinned = &keys[i]
		}
	}
	if pinned == nil {
		return fileCopy{}, false, cli.InputError("a --copy's checkpoint is signed by no --party-key's checkpoint key")
	}
	logID, _ := signed["log_id"].(string)
	dealID, isDeal := strings.CutPrefix(logID, "deal/")
	if !isDeal || !cli.DealIDPattern.MatchString(dealID) {
		return fileCopy{}, false, cli.InputError("a --copy is not of a deal's log")
	}
	copyOf = fileCopy{logID: logID, side: reconcileSide{book: pinned.signer}}
	openings, err := dealOpeningsFile(raw)
	if err != nil {
		return fileCopy{}, false, err
	}
	digest, err := soleDigest(openings)
	if err != nil || digest == "" {
		return copyOf, false, err
	}
	half, err := admitPeerHalf(dealID, dealPeerCopy{bundle: bundle, pinned: pinned.signer}, digest, openings[digest])
	if err != nil {
		return fileCopy{}, false, err
	}
	copyOf.side.half, copyOf.side.openings, copyOf.openings = &half, openings[digest], openings[digest]
	return copyOf, opensEvery(openings[digest]), nil
}

// checkTermsFiles is the adjudicator's check, from files and pinned keys
// only.
func checkTermsFiles(c *cobra.Command, checkPath string, copies, partyKeyFlags []string) error {
	report := func(outcome, detail string) error {
		out := map[string]any{"check": outcome, "detail": detail}
		if err := cli.Output(c, out); err != nil {
			return err
		}
		switch outcome {
		case termsCheckDiffers:
			return cli.Hint(cli.ErrConflict, "the Result is not the recompute of the inputs given: "+detail)
		case termsCheckCannotReproduce:
			return cli.Hint(cli.ErrInput, "cannot reproduce the Result's inputs: "+detail)
		}
		return nil
	}
	keys, err := parsePartyKeys(partyKeyFlags)
	if err != nil {
		return err
	}
	switch {
	case len(copies) > 2 || len(keys) > 2:
		return cli.InputError("the adjudicator's check takes two --copy files and two --party-key pairs, one of each per party")
	case len(copies) == 2 && len(keys) != 2:
		return cli.InputError("two --copy files need two --party-key pairs, one per party")
	case len(copies) == 0:
		return cli.InputError("--copy is required: each party's own adjudicator file")
	case len(keys) == 0:
		return cli.InputError("--party-key is required: the keys you hold for each party, SIGNER:CHECKPOINT")
	}
	raw, err := cli.ReadInput(checkPath)
	if err != nil {
		return err
	}
	doc, err := decodeResultDocument(raw)
	if err != nil {
		return err
	}
	var result evidencebook.DealReconcileResult
	if err = json.Unmarshal(raw, &result); err != nil || len(result.Claims) == 0 {
		return report(termsCheckDiffers, "the Result holds no reconcile claims")
	}
	for _, claim := range result.Claims {
		if claim.ContractRef != evidencebook.DealReconcileContractRef {
			return report(termsCheckDiffers, "claim "+claim.ID+" is not under "+evidencebook.DealReconcileContractRef)
		}
		if claim.Reconcile.Peer != result.Claims[0].Reconcile.Peer || claim.Reconcile.Period != result.Claims[0].Reconcile.Period {
			return report(termsCheckDiffers, "the claims name different peers or periods")
		}
	}
	if len(copies) == 1 {
		return report(termsCheckCannotReproduce, "one side cannot be checked alone: a party that declines to disclose leaves the adjudicator one file")
	}
	var files [2]fileCopy
	reproducible := true
	for i, path := range copies {
		raw, err := cli.ReadInput(path)
		if err != nil {
			return err
		}
		f, ok, err := admitFileCopy(raw, keys)
		if err != nil {
			return err
		}
		files[i], reproducible = f, reproducible && ok
	}
	if files[0].logID != files[1].logID {
		return cli.InputError("the two --copy files are of different deals")
	}
	if files[0].side.book == files[1].side.book {
		return cli.InputError("the two --copy files are one party's: each party's own file is needed")
	}
	if !reproducible {
		return report(termsCheckCannotReproduce, "a --copy carries no openings of its party's close, or not both fields")
	}
	peerKey, period := result.Claims[0].Reconcile.Peer, result.Claims[0].Reconcile.Period
	var a, b reconcileSide
	switch peerKey {
	case files[0].side.book:
		a, b = files[1].side, files[0].side
	case files[1].side.book:
		a, b = files[0].side, files[1].side
	default:
		return report(termsCheckDiffers, "the Result's peer is neither party's signing key")
	}
	inputs := "the adjudicator files " + copies[0] + " and " + copies[1]
	if !sameHalves(result, period, a, b) {
		if onlyHalfOf(result, a) {
			return report(termsCheckDiffers, "the Result's evidence omits the other party's half supplied here ("+inputs+"); missing openings at production time can cause this")
		}
		return report(termsCheckDiffers, "the Result's evidence (DR §7.3) names halves other than the closes read here ("+inputs+")")
	}
	got, err := recompute(a, b, period, result.GeneratedAt)
	if err != nil {
		return report(termsCheckDiffers, "the inputs do not recompute: "+err.Error())
	}
	if member := firstDifference(doc, got.Result); member != "" {
		return report(termsCheckDiffers, member+" differs from the recompute of "+inputs)
	}
	return report(termsCheckEqual, "the Result is the recompute of "+inputs)
}

// onlyHalfOf is whether every claim of a Result names exactly one half, the
// producer's own: the case where the other party's half was left out. Any
// other mismatch, such as a lone peer half or a digest of no copy, is not
// that case.
func onlyHalfOf(result evidencebook.DealReconcileResult, producer reconcileSide) bool {
	if producer.half == nil {
		return false
	}
	for _, claim := range result.Claims {
		if len(claim.Evidence) != 1 || claim.Evidence[0].Digest != producer.half.digest {
			return false
		}
	}
	return true
}
