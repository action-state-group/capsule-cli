package cli

import (
	"bytes"
	"embed"
	"path"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The typed action records carry the check contract: one record per step of a
// consequential action, each naming its own type, in the same hash-linked
// chain as the x-deal-v0 evidence records (messages, claims, evidence, detail
// changes, proposals, unapproved disclosures and the close):
//
//	task-authority/v0     what the user asked for and the limits they set
//	proposed-action/v0    the normalized ProposedAction, exactly as checked
//	action-evaluation/v0  one evaluation of it (the check response)
//	action-approval/v0    one approval artifact, of a stated authority
//	action-record/v0      the action taken, with every authority it relied on
//	action-outcome/v0     what was observed, or an action taken without authority
//	action-report/v0      a report a person reads (defined; sealed later)
//
// A deal opened with --records typed is sealed in them (dealOpen.Records). Each
// typed record is derived from the step's x-deal-v0 record, so the two record
// sets commit to the same facts: only the header, the names and the check
// contract's fields differ.

const (
	typeTaskAuthority    = "task-authority/v0"
	typeProposedAction   = "proposed-action/v0"
	typeActionEvaluation = "action-evaluation/v0"
	typeActionApproval   = "action-approval/v0"
	typeActionRecord     = "action-record/v0"
	typeActionOutcome    = "action-outcome/v0"
	typeActionReport     = "action-report/v0"
	typeCheckRequest     = "check-request/v0"
	typeCheckResponse    = "check-response/v0"
	typedRecordRef       = "record"
	typedFPAlg           = "hmac-sha256-chain-key"
)

// The record schemas ship in skills/deal/profile/records/; these are
// byte-identical copies (a test keeps them equal).
//
//go:embed assets/records/*.schema.json
var recordSchemaFiles embed.FS

var (
	recordSchemasOnce sync.Once
	recordSchemas     map[string]*jsonschema.Schema
	recordSchemasErr  error
)

// recordSchema is the compiled schema of a typed record or contract type.
func recordSchema(typeName string) (*jsonschema.Schema, error) {
	recordSchemasOnce.Do(func() {
		c := jsonschema.NewCompiler()
		entries, err := recordSchemaFiles.ReadDir("assets/records")
		if err != nil {
			recordSchemasErr = err
			return
		}
		for _, e := range entries {
			raw, err := recordSchemaFiles.ReadFile(path.Join("assets/records", e.Name()))
			if err != nil {
				recordSchemasErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				recordSchemasErr = err
				return
			}
			if err = c.AddResource("https://agentactioncapsule.org/records/"+e.Name(), doc); err != nil {
				recordSchemasErr = err
				return
			}
		}
		recordSchemas = map[string]*jsonschema.Schema{}
		for _, t := range []string{typeTaskAuthority, typeProposedAction, typeActionEvaluation, typeActionApproval,
			typeActionRecord, typeActionOutcome, typeActionReport, typeCheckRequest, typeCheckResponse} {
			s, err := c.Compile("https://agentactioncapsule.org/records/" + schemaFile(t))
			if err != nil {
				recordSchemasErr = err
				return
			}
			recordSchemas[t] = s
		}
	})
	if recordSchemasErr != nil {
		return nil, recordSchemasErr
	}
	s, ok := recordSchemas[typeName]
	if !ok {
		return nil, inputError("unknown record type " + typeName)
	}
	return s, nil
}

func schemaFile(typeName string) string {
	return string(bytes.ReplaceAll([]byte(typeName), []byte("/"), []byte("-"))) + ".schema.json"
}

func typedRef(digest string) map[string]interface{} {
	return map[string]interface{}{"type": typedRecordRef, "digest_alg": "SHA-256", "digest": digest}
}

// typedRecord derives the typed record of a step from its x-deal-v0 record
// (v0), or returns nil when the step stays an x-deal-v0 evidence record.
func typedRecord(ev dealEvent, events []sealedEvent, v0 map[string]interface{}) (map[string]interface{}, error) {
	block := v0[dealProfile].(map[string]interface{})
	body := v0["body"].(map[string]interface{})
	var typeName string
	var out map[string]interface{}
	switch block["record_type"] {
	case "check":
		typeName, out = typeProposedAction, cloneBody(body)
		moveCounterparty(block, out)
	case "verdict":
		typeName = typeActionEvaluation
		ev, err := evaluationBody(ev, events, body)
		if err != nil {
			return nil, err
		}
		out = ev
	case "approval":
		a := ev.Approval
		if a.Choice == "confirm_limits" && !a.Proceed {
			return nil, nil // a late answer changes nothing: it stays an x-deal-v0 approval
		}
		if a.Choice == "confirm_limits" {
			typeName = typeTaskAuthority
			ta, err := confirmedAuthorityBody(ev, events, body)
			if err != nil {
				return nil, err
			}
			out = ta
			break
		}
		typeName = typeActionApproval
		out = map[string]interface{}{"choice": body["choice"], "proceed": body["proceed"]}
		switch a.Approver {
		case "user":
			out["authority"] = "user_approval"
			out["said_commitment"] = body["said_commitment"]
		case "agent_card":
			out["authority"] = "card_answer"
		default:
			return nil, inputError("a typed record has no " + a.Approver + " approval: a DO evaluation authorizes on the task authority")
		}
		if a.ShownCard != "" {
			c, err := shownCommitment(events, a.Check, a.ShownCard)
			if err != nil {
				return nil, err
			}
			out["rendering_commitment"] = c
		}
	case "action":
		typeName, out = typeActionRecord, cloneBody(body)
		moveCounterparty(block, out)
		if err := addAuthority(out, events, ev.Act.AuthorizedBy, ev.Act.AmountMinor); err != nil {
			return nil, err
		}
	case "outcome":
		typeName = typeActionOutcome
		out = map[string]interface{}{"status": body["status"], "outcome": body["outcome"], "findings": body["differences"]}
		for _, k := range []string{"delivered", "note_commitment"} {
			if v, ok := body[k]; ok {
				out[k] = v
			}
		}
		if u, ok := body["unchecked"]; ok {
			out["attempted"] = u
		}
	case "disclosure":
		if body["authority"] != "approval" {
			return nil, nil // told without approval: stays an x-deal-v0 disclosure
		}
		typeName = typeActionRecord
		d := ev.Disclosure
		out = map[string]interface{}{"action": d.action(), "disclosed": map[string]interface{}{"to": body["to"], "fields": body["fields"]}}
		for _, k := range []string{"action_class", "taxonomy_version"} {
			if v, ok := body[k]; ok {
				out[k] = v
			}
		}
		if ch, ok := block["channel"]; ok {
			out["channel"] = ch
		}
		moveCounterparty(block, out)
		if err := addAuthority(out, events, d.AuthorizedBy, nil); err != nil {
			return nil, err
		}
	default:
		return nil, nil
	}
	return typedHeader(typeName, block, out), nil
}

// typedHeader is the header every typed record carries, from the step's
// x-deal-v0 block, around body.
func typedHeader(typeName string, block, body map[string]interface{}) map[string]interface{} {
	r := map[string]interface{}{
		"type": typeName, "canonicalization": "jcs", "chain_id": block["deal_id"], "seq": block["seq"], "at": block["at"], "body": body,
	}
	if p, ok := block["prev"].(map[string]interface{}); ok {
		r["prev"] = typedRef(p["digest"].(string))
	}
	if b, ok := block["baseline_ref"].(map[string]interface{}); ok {
		r["chain_root"] = typedRef(b["digest"].(string))
	}
	if refs, ok := block["refs"].([]interface{}); ok {
		typed := make([]interface{}, len(refs))
		for i, x := range refs {
			ref := x.(map[string]interface{})
			typed[i] = map[string]interface{}{"rel": ref["rel"], "type": typedRecordRef, "digest_alg": "SHA-256", "digest": ref["digest"]}
		}
		r["refs"] = typed
	}
	if p, ok := block["producer"]; ok {
		r["producer"] = p
	}
	if a, ok := block["commit_alg"]; ok {
		r["commit_alg"] = a
	}
	return r
}

func cloneBody(body map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(body))
	for k, v := range body {
		out[k] = v
	}
	return out
}

// moveCounterparty carries the step's counterparty fingerprints into the
// typed body: the same keyed fingerprints, named for the chain.
func moveCounterparty(block, body map[string]interface{}) {
	if cp, ok := block["counterparty"].(map[string]interface{}); ok {
		body["counterparty"] = map[string]interface{}{"fp_alg": typedFPAlg, "ids": cp["ids"]}
	}
}

// taskAuthorityRecord is the task authority step sealed right after the
// baseline when a typed deal opens: the user's words and limits as asked.
func taskAuthorityRecord(ev dealEvent, events []sealedEvent, commit func(string) (string, error), block map[string]interface{}) (map[string]interface{}, error) {
	ta := ev.TaskAuthority
	verbatim, err := commit("verbatim")
	if err != nil {
		return nil, err
	}
	body := map[string]interface{}{"verbatim_commitment": verbatim}
	if ta.MaxTotalMinor != nil {
		body["max_total_minor"] = *ta.MaxTotalMinor
	}
	if ta.Allowed != nil {
		allowed := make([]interface{}, len(ta.Allowed))
		for i, a := range ta.Allowed {
			allowed[i] = a
		}
		body["allowed"] = allowed
	}
	block["refs"] = []interface{}{relRef("source", events[0].Digest)}
	return typedHeader(typeTaskAuthority, block, body), nil
}

// confirmedAuthorityBody is a new version of the task authority: the user's
// confirmation (in their words) of the limits a proposal asked for, naming
// the version it replaces.
func confirmedAuthorityBody(ev dealEvent, events []sealedEvent, body map[string]interface{}) (map[string]interface{}, error) {
	a := ev.Approval
	var proposal *sealedEvent
	for i := range events {
		if events[i].CapsuleID == a.Check {
			proposal = &events[i]
		}
	}
	if proposal == nil || proposal.Event.Intent == nil {
		return nil, inputError("confirm_limits answers no proposal of this deal")
	}
	verbatim, err := commitText(proposal.Event.Nonces["verbatim"], proposal.Event.Intent.Verbatim)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{
		"verbatim_commitment": verbatim, "previous_ref": typedRef(taskAuthorityAt(events, "")),
		"said_commitment": body["said_commitment"],
	}
	if l := a.Limits; l != nil {
		if l.New.MaxTotalMinor != nil {
			out["max_total_minor"] = *l.New.MaxTotalMinor
		}
		if l.New.Allowed != nil {
			allowed := make([]interface{}, len(l.New.Allowed))
			for i, x := range l.New.Allowed {
				allowed[i] = x
			}
			out["allowed"] = allowed
		}
	}
	return out, nil
}

// evaluationBody is the check contract's response on the evaluation: the
// verdict under the contract's names, with the ProposedAction, task
// authority, rule table, materiality predicate and validity it was made
// under, and its proposed-phase authority basis.
func evaluationBody(ev dealEvent, events []sealedEvent, v0 map[string]interface{}) (map[string]interface{}, error) {
	ck := ev.Check
	disposition := map[string]string{"pass": "DO", "pause": "ASK", "deny": "DENY"}[ck.Verdict]
	task := typedRef(taskAuthorityAt(events, ""))
	proposed := ""
	for _, se := range events {
		if se.CapsuleID == ck.Snapshot {
			proposed = se.Digest
		}
	}
	out := map[string]interface{}{
		"disposition": disposition, "findings": v0["differences"], "options": v0["options"],
		"proposed_action_digest": proposed, "task_authority_ref": task,
		"ruleset_digest": ck.RulesetDigest, "valid_until": ck.ValidUntil,
		"authority_basis": []interface{}{map[string]interface{}{"type": "task_authority", "ref": task}},
	}
	// The ruleset the profile's external checker ran, when it ran: the
	// ruleset_digest above stays the built-in rule table's.
	if r := ck.Rules; r != nil && r.Status == "evaluated" {
		out["rules_checks"] = []interface{}{map[string]interface{}{"ruleset_id": r.RulesetID, "definition_digest": r.DefinitionDigest, "verdict": r.Verdict}}
	}
	mode, digest := ck.Materiality.mode()
	out["materiality"], out["materiality_digest"] = mode, nil
	if digest != nil {
		out["materiality_digest"] = *digest
	}
	for _, k := range []string{"unverified", "notes", "judge"} {
		if v, ok := v0[k]; ok {
			out[k] = v
		}
	}
	if c, ok := v0["card_commitment"]; ok {
		out["rendering_commitment"] = c
	}
	return out, nil
}

// renderingCommitment is the one commitment to text a person was shown: the
// text under the nonce of its rendering. It has three call sites, one
// mechanism: an evaluation's card and the user's answer to it (equal exactly
// when what was shown is what was checked), a policy-change confirmation, and
// a platform approval observation (the text the platform displayed).
func renderingCommitment(nonce, text string) (string, error) {
	if nonce == "" {
		return "", inputError("a rendering commitment needs its rendering's nonce")
	}
	return commitText(nonce, text)
}

// shownCommitment commits to what the person was shown when they answered an
// evaluation, under that evaluation's own rendering nonce.
func shownCommitment(events []sealedEvent, check, shown string) (string, error) {
	verdict, _, ok := checkedCard(events, check)
	if !ok {
		return "", inputError("what was shown has no rendered check to commit against")
	}
	return renderingCommitment(verdict.Event.Nonces["card"], shown)
}

// taskAuthorityAt is the record digest of the task authority in force before
// the step named upTo ("" = after every step in events): the task authority
// sealed when the deal opened, or the latest confirmed new version of it.
func taskAuthorityAt(events []sealedEvent, upTo string) string {
	task := ""
	for _, se := range events {
		if upTo != "" && se.CapsuleID == upTo {
			break
		}
		if se.Event.Kind == "task_authority" {
			task = se.Digest
		}
		if ap := se.Event.Approval; ap != nil && ap.Choice == "confirm_limits" && ap.Proceed && ap.Limits != nil {
			task = se.Digest
		}
	}
	return task
}

// addAuthority adds the executed phase of the check contract to an action
// record: the evaluation it relied on and every authority layer, in order,
// each a ref to the record of that layer. Layers accumulate: none replaces
// another. A DO evaluation authorizes on the task authority alone; an ASK
// adds the user's own answer; every platform approval observed for the same
// evaluation is listed beside them (scope mismatch when it stated another
// amount) and never counts as the answer.
func addAuthority(body map[string]interface{}, events []sealedEvent, authorizedBy string, amount *int64) error {
	evaluation, approval := authorizedBy, ""
	for _, se := range events {
		if se.CapsuleID == authorizedBy && se.Event.Approval != nil {
			evaluation, approval = se.Event.Approval.Check, authorizedBy
		}
	}
	var evalDigest, approvalDigest string
	for _, se := range events {
		switch se.CapsuleID {
		case evaluation:
			evalDigest = se.Digest
		case approval:
			approvalDigest = se.Digest
		}
	}
	if evalDigest == "" {
		return inputError("an authorized step names no sealed evaluation")
	}
	basis := []interface{}{map[string]interface{}{"type": "task_authority", "ref": typedRef(taskAuthorityAt(events, evaluation))}}
	if approval != "" {
		basis = append(basis, map[string]interface{}{"type": "user_approval", "ref": typedRef(approvalDigest)})
	}
	for _, se := range events {
		p := se.Event.Platform
		if se.Event.Kind != "platform_approval" || p == nil || p.Check != evaluation {
			continue
		}
		entry := map[string]interface{}{"type": "platform_approval", "ref": typedRef(se.Digest)}
		if amount != nil && p.AmountMinor != nil && *amount != *p.AmountMinor {
			entry["scope"] = "mismatch"
		}
		basis = append(basis, entry)
	}
	body["evaluation_ref"] = typedRef(evalDigest)
	body["authority_basis"] = basis
	return nil
}

// Materiality modes an evaluation states, so that no predicate is told apart
// from a missing field.
const (
	materialityModePredicate = "predicate"
	materialityModeNone      = "none_fail_safe"
)

// predicateDigest is the digest of the predicate that applied, "" when none
// was configured (digest "none": every pick the agent made alone paused).
func (m dealMateriality) predicateDigest() string {
	if m.Digest == "none" {
		return ""
	}
	return m.Digest
}

// mode is how typed records state the materiality a check ran under, from
// the check's Materiality, the same value its output shows: the predicate's
// digest, or, with none configured, null and the fail-safe mode.
func (m dealMateriality) mode() (string, *string) {
	if d := m.predicateDigest(); d != "" {
		return materialityModePredicate, &d
	}
	return materialityModeNone, nil
}

// platformObservationKind names the platform approval observation shape.
const platformObservationKind = "platform-approval-observation"

// platformApprovalRecord is a platform approval observation: that another
// platform's approval interaction for the proposed action happened with
// these bytes (what it displayed, by rendering commitment; the user's text it
// returned, by commitment), and when. It claims nothing about whether the
// platform authorized anything or a rule is satisfied: it is listed beside
// the evaluation's authority, never as its answer.
func platformApprovalRecord(ev dealEvent, events []sealedEvent, commit func(string) (string, error), block map[string]interface{}, currency string) (map[string]interface{}, error) {
	p := ev.Platform
	displayed, err := renderingCommitment(ev.Nonces["displayed_text"], p.DisplayedText)
	if err != nil {
		return nil, err
	}
	proposed := ""
	if verdict, _, ok := checkedCard(events, p.Check); ok {
		for _, se := range events {
			if se.CapsuleID == verdict.Event.Check.Snapshot {
				proposed = se.Digest
			}
		}
	}
	if proposed == "" {
		return nil, inputError("a platform approval observation names the proposed action of a sealed check")
	}
	body := map[string]interface{}{"authority": "platform_approval", "kind": platformObservationKind,
		"platform": p.Platform, "mechanism": p.Mechanism, "displayed_text_digest": displayed,
		"proposed_action_ref": typedRef(proposed), "observed_at": p.ObservedAt}
	if p.UserText != "" {
		if body["returned_user_text_digest"], err = commit("returned_user_text"); err != nil {
			return nil, err
		}
	}
	if p.AmountMinor != nil {
		body["amount_minor"] = *p.AmountMinor
		cur := p.Currency
		if cur == "" {
			cur = currency
		}
		body["currency"] = cur
	}
	return typedHeader(typeActionApproval, block, body), nil
}

// policyChangeApproval is the body of the user's confirmation of a policy
// change: their words, what they were shown, the policy that takes effect and
// the semantic diff from the one it replaces. capsulectl seals none itself
// yet; the policy flow that does uses this shape.
func policyChangeApproval(saidCommitment, renderingCommitment, effectivePolicyDigest, semanticDiffDigest string) map[string]interface{} {
	return map[string]interface{}{
		"authority": "policy_change", "said_commitment": saidCommitment, "rendering_commitment": renderingCommitment,
		"effective_policy_digest": effectivePolicyDigest, "semantic_diff_digest": semanticDiffDigest,
	}
}

// CheckRef is a reference to a sealed record by its digest.
type CheckRef struct {
	Type      string `json:"type"`
	DigestAlg string `json:"digest_alg"`
	Digest    string `json:"digest"`
}

// CheckFinding is one rule a ProposedAction tripped.
type CheckFinding struct {
	Question string `json:"question"`
	Rule     string `json:"rule"`
	Field    string `json:"field,omitempty"`
}

// CheckAuthority is one authority layer an evaluation or action relies on.
type CheckAuthority struct {
	Type  string   `json:"type"`
	Ref   CheckRef `json:"ref"`
	Scope string   `json:"scope,omitempty"`
}

// CheckPlatform names the platform a request comes from (instance data).
type CheckPlatform struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
}

// CheckRequest is check-request/v0: a request to evaluate one ProposedAction.
// It is platform-neutral: the platform is instance data, and ruleset_ref names
// the RuleSet where the caller keeps it (the sealed evaluation binds only its
// digest).
type CheckRequest struct {
	Type                 string                 `json:"type"`
	Phase                string                 `json:"phase"`
	Platform             *CheckPlatform         `json:"platform,omitempty"`
	RulesetRef           string                 `json:"ruleset_ref,omitempty"`
	RulesetDigest        string                 `json:"ruleset_digest"`
	TaskAuthorityRef     *CheckRef              `json:"task_authority_ref"`
	RootIntentRef        *CheckRef              `json:"root_intent_ref,omitempty"`
	ParentDelegationRef  *CheckRef              `json:"parent_delegation_ref"`
	ProposedAction       map[string]interface{} `json:"proposed_action"`
	HistoryRefs          []CheckRef             `json:"history_refs,omitempty"`
	AgentAssessment      map[string]interface{} `json:"agent_assessment,omitempty"`
	PlatformApprovalRefs []CheckRef             `json:"platform_approval_refs,omitempty"`
}

// CheckResponse is check-response/v0: one evaluation as returned, the fields
// its action-evaluation/v0 record seals plus evaluation_ref (that record).
type CheckResponse struct {
	Type                 string           `json:"type"`
	Disposition          string           `json:"disposition"`
	ValidUntil           string           `json:"valid_until"`
	ProposedActionDigest string           `json:"proposed_action_digest"`
	Findings             []CheckFinding   `json:"findings"`
	AuthorityBasis       []CheckAuthority `json:"authority_basis"`
	EvaluationRef        CheckRef         `json:"evaluation_ref"`
	RulesetDigest        string           `json:"ruleset_digest"`
	TaskAuthorityRef     CheckRef         `json:"task_authority_ref"`
	MaterialityDigest    *string          `json:"materiality_digest"`
	Materiality          string           `json:"materiality"`
}

// checkResponseFor is the check response of a sealed evaluation step.
func checkResponseFor(ev dealEvent, events []sealedEvent, digest string) CheckResponse {
	ck := ev.Check
	task := CheckRef{Type: typedRecordRef, DigestAlg: "SHA-256", Digest: taskAuthorityAt(events, "")}
	resp := CheckResponse{
		Type: typeCheckResponse, Disposition: map[string]string{"pass": "DO", "pause": "ASK", "deny": "DENY"}[ck.Verdict],
		ValidUntil: ck.ValidUntil, Findings: []CheckFinding{},
		AuthorityBasis: []CheckAuthority{{Type: "task_authority", Ref: task}},
		EvaluationRef:  CheckRef{Type: typedRecordRef, DigestAlg: "SHA-256", Digest: digest},
		RulesetDigest:  ck.RulesetDigest, TaskAuthorityRef: task,
	}
	resp.Materiality, resp.MaterialityDigest = ck.Materiality.mode()
	for _, se := range events {
		if se.CapsuleID == ck.Snapshot {
			resp.ProposedActionDigest = se.Digest
		}
	}
	// The same findings the evaluation record seals.
	for _, x := range differencesBody(ck.Differences) {
		m := x.(map[string]interface{})
		f := CheckFinding{Question: m["question"].(string), Rule: m["rule"].(string)}
		if field, ok := m["field"].(string); ok {
			f.Field = field
		}
		resp.Findings = append(resp.Findings, f)
	}
	return resp
}
