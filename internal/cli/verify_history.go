package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/action-state-group/agent-action-capsule/go/personalhistory"
	"github.com/spf13/cobra"
)

// ErrHistoryNotAccepted is the exit of `verify --history` when a History
// fails a check: an envelope property or a History property is FAIL.
var ErrHistoryNotAccepted = errors.New("history not accepted")

// historyNotEstablished is what a History at the Basic level never
// establishes, shown before any count (PH §5, §6.3 item 1).
var historyNotEstablished = []string{
	"any outcome: sale, payment, delivery, receipt or return",
	"that the selection is all of the holder's deals (completeness not asserted)",
	"that anything happened at a claimed event time",
	"who holds the holder key",
}

// historyDisplayOrder is PH §6.3's order, by this report's members: a
// JSON object has no order of its own, so a page built from the report
// shows its members in this one.
var historyDisplayOrder = []string{"not_established", "coverage", "completeness", "selected_deals", "deal_states", "deals", "history_properties", "exchanges", "not_run", "limits"}

// historyNotRun are the checks a Basic level verifier cannot run (PH §2,
// §6.1, §6.3 item 4).
var historyNotRun = []string{
	"anchor_inclusion: no verification material kind exists for a derivation's artifact (PA-13)",
	"that the anchor is a witnessed checkpoint tick committing to each stream an artifact names (PA-14)",
	"responder_key_current: this verifier is offline",
}

// historyLimits are the limits a History carries whatever its checks say
// (PH §4.5, §9, and the deal profile's own, note on #264 L1 and L2).
var historyLimits = []string{
	"one holder key answered for every deal; a record's signature is outside what the Basic level reveals, so that is not evidence the key's holder made the deals",
	"deal references and record digests are the same in every History that includes the deal, so two recipients can tell they received the same deal, and the deal's other party knows its reference",
	"a record digest can confirm a guessed record when the record's content is enumerable",
	"gaps in leaf_index show how many records a stream holds beside the deal's steps, and where",
	"committed_at is the time the holder's stream assigned; event_time_claim is the holder's agent's claim; neither is a witnessed time",
	"a recipient can forward what it received",
	"a deal reference is chosen by whichever party opened the deal, and the file does not say which",
	"epistemic_type is the holder's own label for each record, never a corroboration by anyone else",
}

type historyRecordView struct {
	Digest         any `json:"digest"`
	LeafIndex      any `json:"leaf_index"`
	CommittedAt    any `json:"committed_at,omitempty"`
	EventTimeClaim any `json:"event_time_claim,omitempty"`
	RecordType     any `json:"record_type,omitempty"`
	EpistemicType  any `json:"epistemic_type,omitempty"`
}

type historyDealView struct {
	Deal           string              `json:"deal"`
	EvidenceStream any                 `json:"evidence_stream,omitempty"`
	State          string              `json:"state,omitempty"`
	Records        []historyRecordView `json:"records"`
}

type historyExchangeView struct {
	Subject    any               `json:"subject"`
	Accepted   bool              `json:"accepted"`
	Message    string            `json:"message_type"`
	Responder  any               `json:"responder,omitempty"`
	Properties map[string]string `json:"properties"`
}

// historyReport is what `verify --history` prints, in PH §6.3's order:
// what is not established and the coverage kind, then the counts, then
// each deal's records, then the checks run and not run, then the limits.
type historyReport struct {
	Kind           string                `json:"kind"`
	DisplayOrder   []string              `json:"display_order"`
	Verdict        string                `json:"verdict"`
	NotEstablished []string              `json:"not_established"`
	Coverage       string                `json:"coverage"`
	Completeness   string                `json:"completeness"`
	SelectedDeals  *int                  `json:"selected_deals,omitempty"`
	DealStates     map[string]int        `json:"deal_states,omitempty"`
	Deals          []historyDealView     `json:"deals"`
	Recipient      any                   `json:"recipient,omitempty"`
	HolderKeys     []string              `json:"holder_keys"`
	Mode           string                `json:"mode"`
	ModeNote       string                `json:"mode_note"`
	HistoryChecks  map[string]string     `json:"history_properties"`
	Exchanges      []historyExchangeView `json:"exchanges"`
	NotRun         []string              `json:"not_run"`
	TrustNote      string                `json:"trust_note,omitempty"`
	Limits         []string              `json:"limits"`
	Findings       []string              `json:"findings"`
}

// verifyHistoryFile is `verify --history`: a one-file History at the Basic
// level of the Personal-Agent History Profile (personal_history_basic/1, PH
// §8.2), checked with agent-action-capsule's evidence-interop/0 verifier
// and History evaluator from the file alone. holderKeys are the responder
// keys the relying party accepts; with none, responder_trust cannot pass
// and the History is not accepted. Exit 0 when the History is accepted,
// 3 (ErrPartial) when nothing failed but the holder key was not pinned,
// 1 (ErrHistoryNotAccepted) when a check failed.
func verifyHistoryFile(c *cobra.Command, path string, holderKeys []string) error {
	for _, k := range holderKeys {
		if !evidencerequest.IsDigest(k) {
			return InputError("--holder-key must be a raw Ed25519 public key as 64 lowercase hex characters")
		}
	}
	raw, err := ReadInput(path)
	if err != nil {
		return err
	}
	file, err := personalhistory.ParseFile(raw)
	if err != nil {
		return InputError("--history is not a one-file History (" + personalhistory.Name + ", PH §8.2): " + strings.TrimPrefix(err.Error(), personalhistory.ErrInvalid.Error()+": "))
	}
	report, err := HistoryVerdict(file, holderKeys)
	if err != nil {
		return err
	}
	if err := Output(c, report); err != nil {
		return err
	}
	switch {
	case report.Verdict == "ACCEPTED":
		return nil
	case len(report.Findings) == 0:
		return ErrPartial
	}
	return ErrHistoryNotAccepted
}

// HistoryVerdict evaluates a one-file History against the holder keys the
// relying party accepts and returns `verify --history`'s report. With no
// holder key, no envelope can pass responder_trust, so the History is
// evaluated a second time trusting every key it names, only to report its
// other findings; it is still not accepted.
func HistoryVerdict(file personalhistory.File, holderKeys []string) (historyReport, error) {
	r, err := historyEvaluate(file, holderKeys)
	if err != nil || len(holderKeys) > 0 {
		return r, err
	}
	asIfPinned, err := historyEvaluate(file, r.HolderKeys)
	if err != nil {
		return r, err
	}
	asIfPinned.Verdict = "NOT_ACCEPTED"
	for i := range asIfPinned.Exchanges {
		asIfPinned.Exchanges[i].Accepted = false
		asIfPinned.Exchanges[i].Properties["responder_trust"] = r.Exchanges[i].Properties["responder_trust"]
	}
	asIfPinned.SelectedDeals, asIfPinned.DealStates = nil, nil
	for i := range asIfPinned.Deals {
		asIfPinned.Deals[i].State = ""
	}
	asIfPinned.TrustNote = "no --holder-key was given, so responder_trust cannot pass and no envelope is accepted (IP §7.2). " +
		"The other checks were run as if the keys in holder_keys were trusted. Obtain the holder's key by another channel, pin it with --holder-key, and run again"
	return asIfPinned, nil
}

func historyEvaluate(file personalhistory.File, holderKeys []string) (historyReport, error) {
	v, err := evidencerequest.NewVerifier(evidencerequest.TrustPolicy{ResponderKeys: holderKeys},
		evidencerequest.VerificationPolicyFromPin(nil), evidencerequest.ProfileTime(DealClock()), nil)
	if err != nil {
		return historyReport{}, err
	}
	res, err := personalhistory.Evaluate(v, file.Definition, file.Exchanges, nil)
	if err != nil {
		return historyReport{}, err
	}
	r := historyReport{
		Kind: personalhistory.Name, DisplayOrder: historyDisplayOrder, NotEstablished: historyNotEstablished, Coverage: "holder_selected",
		Completeness: "not_asserted", Deals: []historyDealView{}, HolderKeys: []string{}, Mode: "handed_over",
		ModeNote:      "the holder formed the requests (PH §8.2): request_binding shows only that each answer matches the request octets in the file, not that the recipient asked",
		HistoryChecks: res.Properties, NotRun: historyNotRun, Limits: historyLimits, Findings: []string{},
		Exchanges: []historyExchangeView{},
	}
	if d, ok := file.Definition.(map[string]any); ok {
		r.Recipient = d["recipient"]
	}
	states := map[string]string{}
	if res.States != nil {
		n := res.States.SelectedDeals
		r.SelectedDeals = &n
		r.DealStates = res.States.Counts
		for _, s := range res.States.PerDeal {
			states[s.Deal] = s.State
		}
	}
	for i, x := range res.Exchanges {
		view := historyExchangeView{Subject: x.Subject, Accepted: x.Accepted, Message: x.Reply.MessageType, Properties: map[string]string{}}
		for _, p := range x.Reply.Properties {
			view.Properties[p.Property] = p.Status
			if p.Status == evidencerequest.StatusFail && (p.Property != "responder_trust" || len(holderKeys) > 0) {
				r.Findings = append(r.Findings, fmt.Sprintf("exchange %d: %s FAIL", i, p.Property))
			}
		}
		if x.Reply.MessageType != "envelope" {
			r.Findings = append(r.Findings, fmt.Sprintf("exchange %d: the reply is a %s, not an envelope", i, x.Reply.MessageType))
		}
		if x.Reply.Envelope != nil {
			view.Responder = x.Reply.Envelope["responder"]
			if key, ok := view.Responder.(string); ok && !slices.Contains(r.HolderKeys, key) {
				r.HolderKeys = append(r.HolderKeys, key)
			}
		}
		r.Exchanges = append(r.Exchanges, view)
		if deal := historyDeal(x, states); deal != nil {
			r.Deals = append(r.Deals, *deal)
		}
	}
	if len(holderKeys) > 0 {
		for _, p := range personalhistory.HistoryProperties {
			if res.Properties[p] != evidencerequest.StatusPass {
				r.Findings = append(r.Findings, p+" FAIL")
			}
		}
	}
	r.Verdict = "NOT_ACCEPTED"
	if res.Accepted {
		r.Verdict = "ACCEPTED"
	}
	return r, nil
}

// historyDeal is one answered deal's records as its artifact lists them.
func historyDeal(x personalhistory.ExchangeResult, states map[string]string) *historyDealView {
	subject, _ := x.Subject.(map[string]any)
	deal, ok := subject["correlation"].(string)
	artifact, isMap := x.Artifact.(map[string]any)
	if !ok || !isMap {
		return nil
	}
	view := &historyDealView{Deal: deal, EvidenceStream: artifact["evidence_stream"], State: states[deal], Records: []historyRecordView{}}
	records, _ := artifact["records"].([]any)
	for _, raw := range records {
		rec, _ := raw.(map[string]any)
		view.Records = append(view.Records, historyRecordView{
			Digest: rec["digest"], LeafIndex: rec["leaf_index"], CommittedAt: rec["committed_at"],
			EventTimeClaim: rec["event_time_claim"], RecordType: rec["record_type"], EpistemicType: rec["epistemic_type"],
		})
	}
	return view
}
