package cli

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"

	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/action-state-group/agent-action-capsule/go/inferencehistory"
	"github.com/action-state-group/agent-action-capsule/go/personalhistoryoutcome"
	"github.com/spf13/cobra"
)

// The two History instances AAC reads since go/v0.20.0 (agent-action-
// capsule#557). The file holds the instance's own inputs, as the
// instance's vectors do: the definition the recipient holds and the
// request and reply octets as carried. Keys and policy are the reader's,
// given by flag, never read from the file.
const (
	inferenceInstance = "inference-service-history/0"
	outcomeInstance   = "personal_history_outcome/1"
)

// outcomeExchange is one request and reply of an Outcome History.
type outcomeExchange struct {
	Request carried `json:"request"`
	Reply   carried `json:"reply"`
}

func (x outcomeExchange) messages() (evidencerequest.Message, evidencerequest.Message, error) {
	req, err := x.Request.message()
	if err != nil {
		return req, req, errors.New("request: " + err.Error())
	}
	rep, err := x.Reply.message()
	if err != nil {
		return req, rep, errors.New("reply: " + err.Error())
	}
	return req, rep, nil
}

// serviceVerifier is the verifier both readers run each exchange through:
// the responders the reader accepts (--holder-key), the default
// verification policy and the profile time.
func serviceVerifier(holderKeys []string) (*evidencerequest.Verifier, error) {
	return evidencerequest.NewVerifier(evidencerequest.TrustPolicy{ResponderKeys: holderKeys},
		evidencerequest.VerificationPolicyFromPin(nil), evidencerequest.ProfileTime(DealClock()), nil)
}

func decodeDefinition(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, errors.New("the file holds no definition")
	}
	return evidencerequest.DecodeJSON(raw)
}

// verifyInferenceHistory evaluates an inference-service-history/0 answer
// (ISH §8): the exchange under the interop profile and the History
// properties of the answer against the definition. The log, witness and
// peer keys are the reader's pins (ISH §9).
func verifyInferenceHistory(c *cobra.Command, raw []byte, holderKeys []string) (frameReport, error) {
	r := frameReport{Kind: inferenceInstance, Findings: []string{}}
	var f struct {
		Definition json.RawMessage `json:"definition"`
		Request    carried         `json:"request"`
		Reply      carried         `json:"reply"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return r, InputError("--history is not an inference-service-history/0 file {history, definition, request, reply}: " + err.Error())
	}
	def, err := decodeDefinition(f.Definition)
	if err != nil {
		return r, InputError("--history definition: " + err.Error())
	}
	req, err1 := f.Request.message()
	rep, err2 := f.Reply.message()
	if err := errors.Join(err1, err2); err != nil {
		return r, InputError("--history request or reply: " + err.Error())
	}
	logKeys, _ := c.Flags().GetStringArray("log-key")
	witnessKeys, _ := c.Flags().GetStringArray("witness-key")
	peerKeys, _ := c.Flags().GetStringArray("peer-key")
	v, err := serviceVerifier(holderKeys)
	if err != nil {
		return r, err
	}
	res, err := inferencehistory.Evaluate(v, inferencehistory.Pins{LogKeys: logKeys, WitnessKeys: witnessKeys, PeerKeys: peerKeys}, def,
		inferencehistory.Exchange{Request: req, Reply: rep}, nil)
	if errors.Is(err, evidencerequest.ErrInvalid) {
		return r, InputError("--history holds a request that is not an evidence request map")
	}
	if err != nil {
		return r, err
	}
	r.Properties = res.Properties
	r.Envelope = envelopeStatuses(res.Exchange.Reply)
	r.Detail = map[string]any{"coverage": res.Coverage, "witness": res.Witness, "exchanges": res.Exchanges}
	r.Verdict = acceptedVerdict(res.Accepted)
	return r, nil
}

// outcomePolicy is the reader's own Outcome policy (PO §8.4): the
// predicate table whose digest the definition must name, the keys it
// trusts for each role, and the requirements it holds.
type outcomePolicy struct {
	PredicateTable json.RawMessage `json:"predicate_table"`
	Roles          struct {
		Counterparty []string `json:"counterparty"`
		Judge        []string `json:"judge"`
	} `json:"outcome_roles"`
	Requirements []struct {
		ID        string   `json:"id"`
		Predicate string   `json:"predicate"`
		Sources   []string `json:"sources"`
	} `json:"outcome_requirements"`
}

// verifyOutcomeHistory evaluates a personal_history_outcome/1 History (PO
// §8): each exchange, the History properties, each outcome claim and the
// requirements the reader holds, with no adapter: a native object's check
// is NOT_CHECKED.
func verifyOutcomeHistory(c *cobra.Command, raw []byte, holderKeys []string) (frameReport, error) {
	r := frameReport{Kind: outcomeInstance, Findings: []string{}, NotRun: []string{
		"native objects (adapter checks) are NOT_CHECKED: this verifier loads no adapter",
	}}
	var f struct {
		Definition json.RawMessage   `json:"definition"`
		Exchanges  []outcomeExchange `json:"exchanges"`
		Evidence   []carried         `json:"evidence"`
		Openings   json.RawMessage   `json:"openings"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return r, InputError("--history is not a personal_history_outcome/1 file {history, definition, exchanges, evidence, openings}: " + err.Error())
	}
	def, err := decodeDefinition(f.Definition)
	if err != nil {
		return r, InputError("--history definition: " + err.Error())
	}
	in := personalhistoryoutcome.Inputs{Definition: def}
	for i, x := range f.Exchanges {
		req, rep, err := x.messages()
		if err != nil {
			return r, InputError("--history exchange " + strconv.Itoa(i) + " " + err.Error())
		}
		in.Exchanges = append(in.Exchanges, personalhistoryoutcome.Exchange{Request: req, Reply: rep})
	}
	for i, e := range f.Evidence {
		m, err := e.message()
		if err != nil {
			return r, InputError("--history evidence " + strconv.Itoa(i) + ": " + err.Error())
		}
		in.Evidence = append(in.Evidence, m.Octets)
	}
	if len(f.Openings) > 0 {
		o, err := evidencerequest.DecodeJSON(f.Openings)
		openings, ok := o.(map[string]any)
		if err != nil || !ok {
			return r, InputError("--history openings is not an object by deal")
		}
		in.Openings = openings
	}
	var policy outcomePolicy
	var table any
	if path, _ := c.Flags().GetString("outcome-policy"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return r, InputError("--outcome-policy: " + err.Error())
		}
		if err := json.Unmarshal(b, &policy); err != nil {
			return r, InputError("--outcome-policy is not {predicate_table, outcome_roles, outcome_requirements}: " + err.Error())
		}
		if len(policy.PredicateTable) > 0 {
			if table, err = evidencerequest.DecodeJSON(policy.PredicateTable); err != nil {
				return r, InputError("--outcome-policy predicate_table: " + err.Error())
			}
		}
	}
	cfg := personalhistoryoutcome.Config{PredicateTable: table,
		Roles: personalhistoryoutcome.Roles{Counterparty: policy.Roles.Counterparty, Judge: policy.Roles.Judge}}
	for _, q := range policy.Requirements {
		cfg.Requirements = append(cfg.Requirements, personalhistoryoutcome.Requirement{ID: q.ID, Predicate: q.Predicate, Sources: q.Sources})
	}
	v, err := serviceVerifier(holderKeys)
	if err != nil {
		return r, err
	}
	res, err := personalhistoryoutcome.Evaluate(v, cfg, in, nil)
	if errors.Is(err, evidencerequest.ErrInvalid) {
		return r, InputError("--history holds a request that is not an evidence request map")
	}
	if err != nil {
		return r, err
	}
	r.Properties = res.Properties
	r.Detail = map[string]any{"claims": res.Claims, "requirements": res.Requirements, "deal_commitment_opening": res.Openings}
	r.Verdict = acceptedVerdict(res.Accepted)
	return r, nil
}

func acceptedVerdict(accepted bool) string {
	if accepted {
		return "ACCEPTED"
	}
	return "NOT_ACCEPTED"
}

// envelopeStatuses is an exchange's property statuses by name.
func envelopeStatuses(r *evidencerequest.ReplyResult) map[string]string {
	if r == nil {
		return nil
	}
	out := map[string]string{}
	for _, p := range r.Properties {
		out[p.Property] = p.Status
	}
	return out
}
