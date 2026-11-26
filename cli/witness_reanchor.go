package cli

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/checkpointed-local-log/go/mmr"
	"github.com/action-state-group/checkpointed-local-log/go/witness"
)

// A continuity refusal (HTTP 409) that persists after its one retry
// (RearmRefused) would leave every later checkpoint refused too: each chains
// on the refused one, which the witness never took. The 409 names the
// checkpoint the witness last accepted, "so an honest client that skipped a
// cadence can re-prove from the witness's view" (capsule-anchor router.py,
// POST /checkpoints). capsulectl does that once: it signs the refused
// checkpoint again with the witness's last accepted checkpoint as its
// predecessor and a consistency proof from it, and sends that. The stored row
// keeps its own signed bytes (cll keeps them immutable), so the receipt for
// the re-anchor is not kept; later checkpoints chain on the re-anchored one,
// now the witness's tip, and get their own receipts (capsule-cli#647).

// witnessReanchored marks a refused delivery whose checkpoint was re-anchored.
const witnessReanchored = "; re-anchored from the checkpoint the witness last accepted, receipt not kept"

// witnessNotReanchored starts the mark of a refused delivery that is not
// re-anchored; a short reason follows it, ending with a period.
const witnessNotReanchored = "; not re-anchored: "

// witnessTip reads the last-accepted checkpoint classifySubmitError keeps from
// a continuity refusal.
var witnessTip = regexp.MustCompile(`; the witness last accepted mmr_size (\d+) root ([0-9a-f]{64})`)

// witnessTipOf is the checkpoint the witness last accepted, as its latest
// continuity refusal of this delivery named it.
func witnessTipOf(state cll.WitnessState) (uint64, string, bool) {
	m := witnessTip.FindAllStringSubmatch(state.LastError, -1)
	if len(m) == 0 {
		return 0, "", false
	}
	last := m[len(m)-1]
	size, e := strconv.ParseUint(last[1], 10, 64)
	if e != nil {
		return 0, "", false
	}
	return size, last[2], true
}

// reanchorDue reports a delivery refused again after its retry, whose
// refusal named the witness's tip, that has been neither re-anchored nor
// marked as not to be. Until then it holds later rows (selectedWitness), like
// a row due for its retry.
func reanchorDue(state cll.WitnessState) bool {
	if !state.Permanent || state.Receipt != nil || answeredStatus(state) != "409" {
		return false
	}
	if !strings.Contains(state.LastError, witnessRearmed) || strings.Contains(state.LastError, witnessReanchored) || strings.Contains(state.LastError, witnessNotReanchored) {
		return false
	}
	_, _, ok := witnessTipOf(state)
	return ok
}

// reanchored reports a delivery whose checkpoint was re-anchored.
func reanchored(state cll.WitnessState) bool {
	return state.Receipt == nil && strings.Contains(state.LastError, witnessReanchored)
}

// withMark is a delivery error carrying mark, within the stored error's
// bound. An error too long to take it is replaced by the continuity
// refusal's own marks, which are all the readers of a marked row need.
func withMark(lastError, mark string) string {
	if strings.Contains(lastError, mark) {
		return lastError
	}
	if len(lastError)+len(mark) > cll.MaxReasonBytes {
		lastError = withRearmed(WitnessFailureBody(409))
	}
	if len(lastError)+len(mark) > cll.MaxReasonBytes {
		mark = mark[:cll.MaxReasonBytes-len(lastError)]
		for !utf8.ValidString(mark) {
			mark = mark[:len(mark)-1]
		}
	}
	return lastError + mark
}

// notReanchored is a definite reason a refused checkpoint is not re-anchored:
// the row is marked with it and holds later rows no longer.
type notReanchored string

func (n notReanchored) Error() string { return string(n) }

// reprover signs a re-anchor of the refused checkpoint in row from the
// witness's tip (size, root): the same checkpoint, with that tip as its
// predecessor. A notReanchored error says it never will be.
type reprover func(ctx context.Context, row cll.WitnessState, size uint64, root string) ([]byte, error)

// newReprover is the profile's reprover for a log whose tree it can rebuild,
// or nil for a store that keeps no tree here (a plugin's).
func newReprover(p Profile, store cll.WitnessStateStore) reprover {
	log, ok := store.(cll.CheckpointStateStore)
	if !ok {
		return nil
	}
	return func(ctx context.Context, row cll.WitnessState, tipSize uint64, tipRoot string) ([]byte, error) {
		return reproveCheckpoint(ctx, p, log, row, tipSize, tipRoot)
	}
}

// reproveCheckpoint re-signs the checkpoint in row with the witness's tip as
// its predecessor. The stored checkpoint is authenticated first, as the
// profile's own, and the log's tree must give its exact peaks and root and,
// at the tip's size, the witness's root: a witness whose view is not this
// log's (a fork) is never re-proved to. The re-anchor keeps the checkpoint's
// log id, key, size, root and time; it signs the way cll's checkpoint runner
// does.
func reproveCheckpoint(ctx context.Context, p Profile, log cll.CheckpointStateStore, row cll.WitnessState, tipSize uint64, tipRoot string) ([]byte, error) {
	record, e := VerifyCheckpoint(p, row.Checkpoint)
	if e != nil {
		return nil, notReanchored("its stored checkpoint does not verify as this profile's")
	}
	if record.MMRSize != row.CheckpointSize {
		return nil, notReanchored("its stored checkpoint names another size")
	}
	if tipSize == 0 || tipSize >= record.MMRSize {
		return nil, notReanchored("the witness's last accepted checkpoint is not an earlier one")
	}
	key, e := PrivateKey(p.Checkpoint.Signing)
	if e != nil {
		return nil, notReanchored("the profile's checkpoint signing key is not usable")
	}
	signer, e := checkpoint.NewEd25519Signer(key)
	if e != nil {
		return nil, notReanchored("the profile's checkpoint signing key is not usable")
	}
	if !strings.EqualFold(signer.KeyID(), record.KeyID) {
		return nil, notReanchored("it was signed with another key than the profile's checkpoint key")
	}
	state, e := log.LoadCLL(ctx)
	if e != nil {
		return nil, e
	}
	tree, e := mmr.New(state.Nodes)
	if e != nil {
		return nil, fmt.Errorf("%w: restore MMR: %v", cll.ErrCorrupt, e)
	}
	if record.MMRSize > tree.Size() {
		return nil, notReanchored("the log is shorter than the checkpoint")
	}
	peaks, e := tree.PeakHashesAt(record.MMRSize)
	if e != nil || !slices.EqualFunc(peaks, record.NewPeaks, func(a, b []byte) bool { return string(a) == string(b) }) || hex.EncodeToString(mmr.RootFromPeaks(peaks)) != record.Root {
		return nil, notReanchored("the log's tree does not give the checkpoint's root")
	}
	prevPeaks, e := tree.PeakHashesAt(tipSize)
	if e != nil {
		return nil, notReanchored("the witness's last accepted size is not a size of this log")
	}
	if hex.EncodeToString(mmr.RootFromPeaks(prevPeaks)) != tipRoot {
		return nil, notReanchored("the witness's last accepted checkpoint is not this log's")
	}
	proof, e := tree.ConsistencyProofAt(tipSize, record.MMRSize)
	if e != nil {
		return nil, notReanchored("the log's tree gives no consistency proof from the witness's last accepted checkpoint")
	}
	payload := checkpoint.Payload{LogID: record.LogID, KeyID: record.KeyID, MMRSize: record.MMRSize, Root: record.Root, PrevSize: tipSize, PrevRoot: tipRoot, Timestamp: record.Timestamp}
	body, e := payload.CanonicalJSON()
	if e != nil {
		return nil, e
	}
	if record.Cadence != nil {
		return signer.SignCheckpointWithCadence(ctx, body, peaks, prevPeaks, &proof, *record.Cadence)
	}
	return signer.SignCheckpoint(ctx, body, peaks, prevPeaks, &proof)
}

// reanchorRefused re-anchors the oldest refused row of service, up to the
// selection at size, that is due for it (reanchorDue), and reports whether
// it did. It sends at most one re-anchor. A row that cannot be re-anchored,
// or that the witness refuses, is marked so and holds later rows no longer;
// a re-anchor that did not get an answer (a timeout, an unreached witness, a
// spent budget) leaves the row as it was, to be tried at a later call. With
// no reprover (a store that keeps no tree here), each due row is marked.
func reanchorRefused(ctx context.Context, store cll.WitnessStateStore, service string, size uint64, prove reprover, submitter witness.Submitter, verifier witness.Verifier) (bool, error) {
	rows, e := witnessRows(ctx, store, service, size)
	if e != nil {
		return false, e
	}
	slices.SortFunc(rows, func(a, b cll.WitnessState) int { return cmp.Compare(a.CheckpointSize, b.CheckpointSize) })
	for i, row := range rows {
		if row.WitnessID != service || row.CheckpointSize > size || !reanchorDue(row) {
			continue
		}
		reason := ""
		var statement []byte
		switch {
		case prove == nil:
			reason = "this log's store keeps no tree that capsulectl can prove it from"
		case slices.ContainsFunc(rows[i+1:], func(r cll.WitnessState) bool { return r.WitnessID == service && (r.Receipt != nil || reanchored(r)) }):
			reason = "a later checkpoint was delivered"
		default:
			tipSize, tipRoot, _ := witnessTipOf(row)
			statement, e = prove(ctx, row, tipSize, tipRoot)
			var no notReanchored
			if errors.As(e, &no) {
				reason = string(no)
			} else if e != nil {
				return false, e
			}
		}
		if reason != "" {
			if e = markReanchor(ctx, store, row, witnessNotReanchored+reason+"."); e != nil {
				return false, e
			}
			continue
		}
		receipt, e := submitter.Submit(ctx, statement)
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		var answered *witness.HTTPError
		switch {
		case e != nil && errors.As(e, &answered) && !witness.IsRetryable(e):
			status := answeredStatusOf(e.Error())
			return false, markReanchor(ctx, store, row, witnessNotReanchored+"the witness refused the re-anchor (HTTP "+status+").")
		case e != nil:
			return false, nil
		}
		if e = verifier.Verify(statement, receipt); e != nil {
			return false, markReanchor(ctx, store, row, witnessNotReanchored+"the witness's receipt for the re-anchor did not verify.")
		}
		return true, markReanchor(ctx, store, row, witnessReanchored)
	}
	return false, nil
}

// markReanchor records a re-anchor's end on its row: an attempt, still
// failed for good and without a receipt, with mark.
func markReanchor(ctx context.Context, store cll.WitnessStateStore, row cll.WitnessState, mark string) error {
	next := row
	next.Attempts++
	next.LastError = withMark(row.LastError, mark)
	return store.CommitWitness(ctx, row.Attempts, next)
}

// answeredStatusOf is the HTTP status a classified submit error says came
// back, or "unknown".
func answeredStatusOf(text string) string {
	if m := witnessAnswered.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return "unknown"
}

// witnessRows is every delivery row of a log that lists them, or the
// selection's prev chain on a store that cannot (witnessChain).
func witnessRows(ctx context.Context, store cll.WitnessStateStore, service string, size uint64) ([]cll.WitnessState, error) {
	if log, ok := store.(cll.CheckpointStateStore); ok {
		state, e := log.LoadCLL(ctx)
		if e != nil {
			return nil, e
		}
		return state.Witnesses, nil
	}
	return witnessChain(ctx, store, service, size)
}
