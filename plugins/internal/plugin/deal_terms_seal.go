package plugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/capsule-cli/internal/cli"
	emit "github.com/action-state-group/capsule-emit/go"
	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/evidencebook"
	"github.com/spf13/cobra"
)

// Sealing a terms Result (note §2.10 v7). `deal terms-result` seals every
// Result it writes as an evidence_result record on the deal's results book,
// deal/<deal_id>/results: a book of its own on the shelf, never the deal's
// book, so no copy of the deal carries it (Decision 7.1). The record is
// sealed through the one path `result build` uses (sealResultRecord), whose
// only resolver here is the run's own two halves (7.3).

// Witness states of a sealed Result (7.2): signed and under the results
// book's local checkpoint, its time the producer's own clock; in a tick the
// witness has not yet answered for; or covered by a witnessed tick.
const (
	termsSealed         = "sealed"
	termsPendingWitness = "pending_witness"
	termsWitnessed      = "witnessed"
)

// dealTermsResultActionID is the action_id of a sealed terms Result's
// Capsule: every Result of every deal shares it, as reports do theirs.
const dealTermsResultActionID = "capsulectl-deal-terms-result"

// termsSeal is what sealing a Result did, as the verb reports it.
type termsSeal struct {
	RecordID string `json:"record_id"`
	Seq      uint64 `json:"seq"`
	LogID    string `json:"log_id"`
	// Retry is set when the run matched the newest sealed Result in every
	// member but generated_at and returned its record.
	Retry      bool   `json:"retry"`
	Checkpoint uint64 `json:"checkpoint,omitempty"`
	State      string `json:"state"`
}

// resultDocumentOf is a Result as the decoded JSON value the result checks
// and JCS read.
func resultDocumentOf(result evidencebook.DealReconcileResult) (map[string]interface{}, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return cli.DecodeBundleJSON(encoded)
}

// withoutGeneratedAt is a Result document's JCS with generated_at left out:
// a run's identity, since every run stamps a fresh generated_at.
func withoutGeneratedAt(doc map[string]interface{}) ([]byte, error) {
	rest := make(map[string]interface{}, len(doc))
	for k, v := range doc {
		if k != "generated_at" {
			rest[k] = v
		}
	}
	return canonical.JCS(rest)
}

// newestTermsResult is the results book's newest evidence_result record and
// its statement, when it holds one.
func newestTermsResult(ctx context.Context, book *evidencebook.Book) (evidencebook.Record, map[string]interface{}, bool, error) {
	records, err := book.Query(ctx, evidencebook.Filter{RecordType: resultRecordType})
	if err != nil || len(records) == 0 {
		return evidencebook.Record{}, nil, false, err
	}
	newest := records[0]
	for _, r := range records[1:] {
		if r.Seq > newest.Seq {
			newest = r
		}
	}
	doc, err := decodeResultDocument(newest.Header.Statement)
	if err != nil {
		return evidencebook.Record{}, nil, false, cli.ErrConflict
	}
	return newest, doc, true, nil
}

// sealTermsResult seals result on the deal's results book (7.2). halves are
// the digests of the halves this run admitted: the claims' evidence must be
// those and nothing else, which is the resolver 7.3 gives this verb. A run
// equal to the newest sealed Result in every member but generated_at is a
// retry: it takes that Result's generated_at and returns its record, so a
// run that sealed and failed before writing its file seals nothing new.
// Only the newest is a retry's match; any other difference is a new
// issuance, and the book keeps both. A failure after the record is sealed
// returns it with the error.
func (s *dealSession) sealTermsResult(ctx context.Context, dealID string, result *evidencebook.DealReconcileResult, halves map[string]bool) (termsSeal, error) {
	for _, claim := range result.Claims {
		for _, refs := range [][]evidencebook.EvidenceRef{claim.Evidence, claim.Presentation.Evidence} {
			for _, e := range refs {
				if !halves[e.Digest] {
					return termsSeal{}, fmt.Errorf("%w: claim %s cites %s, which is no half this run admitted", cli.ErrConflict, claim.ID, e.Digest)
				}
			}
		}
	}
	logID := dealResultsLogID(dealID)
	book, err := s.shelf.Open(ctx, logID)
	if err != nil {
		return termsSeal{}, err
	}
	newest, newestDoc, have, err := newestTermsResult(ctx, book)
	if err != nil {
		return termsSeal{}, err
	}
	doc, err := resultDocumentOf(*result)
	if err != nil {
		return termsSeal{}, err
	}
	retry := false
	if have {
		mine, err := withoutGeneratedAt(doc)
		if err != nil {
			return termsSeal{}, err
		}
		theirs, err := withoutGeneratedAt(newestDoc)
		if err != nil {
			return termsSeal{}, err
		}
		if at, ok := newestDoc["generated_at"].(string); ok && bytes.Equal(mine, theirs) {
			result.GeneratedAt, retry = at, true
			if doc, err = resultDocumentOf(*result); err != nil {
				return termsSeal{}, err
			}
		}
	}
	checked, err := checkResult(doc, evidencebook.DealReconcileContractRef)
	if err != nil {
		return termsSeal{}, err
	}
	encoded, digest, err := statementDigest(doc)
	if err != nil {
		return termsSeal{}, err
	}
	sealed, already, cut, err := sealResultRecord(ctx, book, encoded, checked.contractRef, nil, func() (evidencebook.Record, bool, error) {
		if !retry {
			return evidencebook.Record{}, false, nil
		}
		_, prior, err := statementDigest(newestDoc)
		return newest, err == nil && prior == digest, err
	}, func(entry evidencebook.Entry) (evidencebook.Record, error) {
		// The record is the profile's own Capsule, its payload the Result's
		// JCS, so a copy of the results book is bundled and verified as a
		// deal's is.
		at := cli.DealClock().UTC().Truncate(time.Second)
		return s.appendTo(ctx, book, at, cli.Request{
			Version: "capsule-seal-request/v1",
			Capsule: emit.Input{ActionID: dealTermsResultActionID, ActionType: emit.ActionTypeFYI, Operator: s.p.Name, Developer: "capsulectl-deal", Timestamp: at},
			Payload: encoded,
		}, entry)
	})
	seal := termsSeal{RecordID: sealed.RecordID, Seq: sealed.Seq, LogID: logID, Retry: already, Checkpoint: cut}
	if err != nil {
		return seal, err
	}
	_, cadence, err := s.resultsBundle(ctx, dealID, sealed.RecordID, nil)
	if err != nil {
		return seal, err
	}
	seal.State = termsWitnessState(cadence)
	return seal, nil
}

// termsWitnessState names a results book checkpoint's witness state for a
// sealed Result.
func termsWitnessState(cadence map[string]interface{}) string {
	switch {
	case cadence["state"] == "not_configured":
		return termsSealed
	case cadence["extent"] == "all":
		return termsWitnessed
	}
	return termsPendingWitness
}

// resultsBundle is a bundle of the deal's results book rooted on one Result
// record, at the book's current checkpointed tip (a checkpoint is cut when
// none covers the tip), with withhold's records' payloads withheld, and the
// book checkpoint's witness state through the cadence log (7.4).
func (s *dealSession) resultsBundle(ctx context.Context, dealID, root string, withhold map[string]bool) (_ map[string]interface{}, _ map[string]interface{}, err error) {
	lp := s.dp
	lp.LogID = dealResultsLogID(dealID)
	t, err := cli.OpenDealTarget(ctx, lp, cli.UseRead, s)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, t.Close()) }()
	b, err := cli.AssembleBundle(ctx, t.Artifacts, t.Log, lp.LogID, cli.BundleOptions{
		Root: root, ClosureDepth: 0, Payloads: "selected", WithDisclosure: true, Withhold: withhold,
	})
	if err != nil {
		return nil, nil, err
	}
	cp, _ := b["checkpoint"].(map[string]interface{})
	statement, err := base64.StdEncoding.DecodeString(fmt.Sprint(cp["statement"]))
	if err != nil {
		return nil, nil, err
	}
	cp["cose"] = base64.RawURLEncoding.EncodeToString(statement)
	cadence, err := s.logWitnessState(ctx, lp.LogID, statement)
	if err != nil {
		return nil, nil, err
	}
	return b, cadence, nil
}

// logWitnessState is where a checkpoint of one of the profile's logs stands
// with the witness, through the cadence log: dealWitnessState for a log that
// is not a deal, so with no step counts. not_configured, scheduled, pending,
// or the cadence chain a verifier needs; a chain whose extent is "part"
// covers an earlier checkpoint, and carries what is not yet witnessed as
// rest.
func (s *dealSession) logWitnessState(ctx context.Context, logID string, statement []byte) (map[string]interface{}, error) {
	service, err := cli.ServiceID(s.p)
	if err != nil || service == "" || s.p.LogID == "" {
		return map[string]interface{}{"state": "not_configured"}, err
	}
	current, err := checkpoint.ParseRecord(statement)
	if err != nil {
		return nil, err
	}
	c, p, err := s.cadenceOf(ctx)
	if err != nil {
		return nil, err
	}
	notYet := func() (map[string]interface{}, error) {
		rest := map[string]interface{}{"state": "scheduled", "cadence": s.cadenceWords()}
		last, ok, err := c.Last(ctx)
		if err != nil || !ok {
			return rest, err
		}
		rest["due"] = last.DueNext.Format("2006-01-02T15:04:05Z07:00")
		leaf, in := last.Leaves.Deals[logID]
		if !in || leaf.Size < current.MMRSize {
			return rest, nil
		}
		pending, err := s.tickPending(ctx, p, service, last.N, last.CheckpointSize)
		if err != nil || pending == nil {
			return rest, err
		}
		return pending, nil
	}
	raw, err := c.Chain(ctx, statement)
	if errors.Is(err, evidencebook.ErrNotFound) {
		return notYet()
	}
	if err != nil {
		return nil, err
	}
	chain, err := bundleValue(raw)
	if err != nil {
		return nil, err
	}
	if ok, err := chainVerifiesNow(ctx, p, service, chain); err != nil {
		return nil, err
	} else if !ok {
		return notYet()
	}
	if chain["extent"] == "part" {
		if chain["rest"], err = notYet(); err != nil {
			return nil, err
		}
	}
	return chain, nil
}

// tickPending is the pending state of a cadence tick the witness has not
// answered for, or nil when its receipt verifies.
func (s *dealSession) tickPending(ctx context.Context, p cli.Profile, service string, tick, size uint64) (_ map[string]interface{}, err error) {
	t, err := cli.OpenTarget(ctx, p, cli.UseCLLRead)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, t.Close()) }()
	state, err := t.Log.GetWitness(ctx, service, size)
	if err != nil && !errors.Is(err, cll.ErrNotFound) {
		return nil, err
	}
	if err == nil && state.Receipt != nil && cli.VerifyWitness(p, state) == nil {
		return nil, nil
	}
	reason, text := cli.WitnessPendingReason(state, p.Checkpoint.Endpoint)
	return map[string]interface{}{"state": "pending", "tick": cli.Integer(tick), "reason": reason, "text": text, "cadence": s.cadenceWords()}, nil
}

// shareTermsResult writes the bundle of one sealed Result for the
// counterparty (7.4): the deal's results book, with that Result's payload
// disclosed and every other run's withheld, the book checkpoint's witness
// chain when one covers it, through the counterparty share gate, and on
// record on the deal's disclosures log with the exact bytes written.
func (s *dealSession) shareTermsResult(c *cobra.Command, dealID string, events []sealedEvent, recordID, recipient, out string) error {
	ctx := c.Context()
	if !s.onBook {
		return cli.InputError("deal " + dealID + " is a legacy deal: it has no terms Results to share")
	}
	book, err := s.shelf.Open(ctx, dealResultsLogID(dealID))
	if err != nil {
		return err
	}
	records, err := book.Query(ctx, evidencebook.Filter{RecordType: resultRecordType})
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return cli.InputError("deal " + dealID + " has no sealed terms Result to share: run `deal terms-result` first")
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Seq < records[j].Seq })
	chosen := records[len(records)-1]
	if recordID != "" {
		found := false
		for _, r := range records {
			if r.RecordID == recordID {
				chosen, found = r, true
			}
		}
		if !found {
			return cli.InputError("--record " + recordID + " is no terms Result sealed for deal " + dealID)
		}
	}
	// The bundle is at the book's tip, which a checkpoint must cover.
	if covered, err := checkpointCovers(ctx, book, records[len(records)-1].Seq); err != nil {
		return err
	} else if !covered {
		if _, err := book.Checkpoint(ctx); err != nil {
			return err
		}
	}
	withhold := map[string]bool{}
	for _, r := range records {
		if r.RecordID != chosen.RecordID {
			withhold[r.RecordID] = true
		}
	}
	b, cadence, err := s.resultsBundle(ctx, dealID, chosen.RecordID, withhold)
	if err != nil {
		return err
	}
	b["extensions"] = map[string]interface{}{cli.DealCadenceExtension: cadence}
	if err = cli.VerifyProducedBundle(b, true); err != nil {
		return err
	}
	encoded, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if err = dealShareGate(encoded, events, cli.DealAudienceCounterparty); err != nil {
		return err
	}
	share, err := s.recordShare(ctx, dealID, b, cli.DealAudienceCounterparty, recipient, dealRole(events), encoded, "")
	if err != nil {
		return err
	}
	if err = cli.AtomicFile(out, encoded, false); err != nil {
		return err
	}
	return cli.Output(c, map[string]any{"deal_id": dealID, "record_id": chosen.RecordID, "bundle": out, "share": share, "state": termsWitnessState(cadence)})
}
