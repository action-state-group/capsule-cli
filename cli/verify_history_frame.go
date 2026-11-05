package cli

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/action-state-group/agent-action-capsule/go/standardhistory"
	"github.com/action-state-group/agent-action-capsule/go/witness"
	"github.com/spf13/cobra"
)

// historyInstance is the History frame instance a file names (spec/history-
// frame-v0.md §6.1). It is read from the file's `history` member, never
// guessed from its shape.
type historyInstance struct {
	// staple marks a stapled history_card/1 (verify_history_staple.go).
	staple bool
	// reader evaluates an instance whose inputs and pins are its own flags.
	reader func(c *cobra.Command, raw []byte, holderKeys []string) (frameReport, error)
}

var historyInstances = map[string]historyInstance{
	"personal_history_basic/1": {},
	"standard_history/1":       {},
	outcomeInstance:            {reader: verifyOutcomeHistory},
	// A staple is a derivation row, not a History instance (Sherlock2, Buzz
	// 71efdf5a): the reader is chosen by the file's own id.
	stapleCommitment:  {staple: true},
	stapleCard:        {staple: true},
	inferenceInstance: {reader: verifyInferenceHistory},
}

// historyInstanceOf reads the instance id of a History file. It is the
// `history` member, or `profile` for a document that names its instance
// that way. ok is false when neither is a string.
func historyInstanceOf(raw []byte) (string, bool) {
	var head struct {
		History any `json:"history"`
		Profile any `json:"profile"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return "", false
	}
	for _, v := range []any{head.History, head.Profile} {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	return "", false
}

// anchorWitnessedView is `anchor_witnessed` as a report member: the status
// (one of IP v1 §7.1's five values) and, when receipts were given, AAC's per-
// receipt report.
type anchorWitnessedView struct {
	Status        string                        `json:"status"`
	Reason        string                        `json:"reason,omitempty"`
	Witness       *string                       `json:"witness,omitempty"`
	WitnessedTime *int64                        `json:"witnessed_time,omitempty"`
	Receipts      []witness.AnchorReceiptReport `json:"receipts,omitempty"`
}

// loadWitnessInputs reads --receipt files and --anchor-witness-policy.
func loadWitnessInputs(c *cobra.Command) ([][]byte, witness.AnchorWitnessPolicy, error) {
	var policy witness.AnchorWitnessPolicy
	paths, _ := c.Flags().GetStringArray("receipt")
	policyPath, _ := c.Flags().GetString("anchor-witness-policy")
	var receipts [][]byte
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, policy, InputError("--receipt: " + err.Error())
		}
		receipts = append(receipts, b)
	}
	if policyPath != "" {
		b, err := os.ReadFile(policyPath)
		if err != nil {
			return nil, policy, InputError("--anchor-witness-policy: " + err.Error())
		}
		if err := json.Unmarshal(b, &policy); err != nil {
			return nil, policy, InputError("--anchor-witness-policy is not a witness policy ({\"witnesses\":[{\"name\",\"keys\"}]}): " + err.Error())
		}
	}
	return receipts, policy, nil
}

// anchorWitnessed is IP v1 §6.2.2 over the receipts beside the card. With
// no receipt there is nothing to check: NOT_PRESENT (§6.2.2 step 2), never
// PASS. No witness list is built in: an empty policy gives NOT_CHECKED, as
// IP leaves the choice of witnesses to the relying party.
func anchorWitnessed(anchor string, receipts [][]byte, policy witness.AnchorWitnessPolicy) anchorWitnessedView {
	if len(receipts) == 0 {
		return anchorWitnessedView{Status: witness.AnchorNotPresent, Reason: "no --receipt was given"}
	}
	r := witness.CheckAnchorWitnessed(anchor, receipts, policy)
	return anchorWitnessedView{Status: r.Status, Witness: r.Witness, WitnessedTime: r.WitnessedTime, Receipts: r.Receipts}
}

// frameReport is `verify --history` for an instance other than the Basic
// level: that instance's own properties, never summed with another's, and
// no score (frame §4.6).
type frameReport struct {
	Kind            string               `json:"kind"`
	Verdict         string               `json:"verdict"`
	Reason          string               `json:"reason,omitempty"`
	Properties      map[string]string    `json:"history_properties,omitempty"`
	Envelope        map[string]string    `json:"envelope_properties,omitempty"`
	Responder       any                  `json:"responder,omitempty"`
	AnchorWitnessed *anchorWitnessedView `json:"anchor_witnessed,omitempty"`
	Findings        []string             `json:"findings"`
	Staples         []stapleRow          `json:"staples,omitempty"`
	// Detail is the instance's own report members (coverage, claims, ...).
	Detail map[string]any `json:"detail,omitempty"`
	NotRun []string       `json:"not_run,omitempty"`
}

// envelopeMustPass are the envelope properties a History answer needs
// (frame §4.3); anchor_inclusion is NOT_CHECKED.
var envelopeMustPass = []string{"profile_version", "envelope_signature", "responder_trust", "issued_at_form", "request_binding", "subject_binding", "artifact_digest", "anchor_match"}

type carried struct {
	MediaType  string  `json:"media_type"`
	OctetsText *string `json:"octets_text"`
	OctetsHex  *string `json:"octets_hex"`
}

func (m carried) message() (evidencerequest.Message, error) {
	switch {
	case m.MediaType == evidencerequest.MediaTypeJSON && m.OctetsText != nil:
		return evidencerequest.Message{Octets: []byte(*m.OctetsText), MediaType: m.MediaType}, nil
	case m.MediaType == evidencerequest.MediaTypeCBOR && m.OctetsHex != nil:
		b, err := hex.DecodeString(*m.OctetsHex)
		return evidencerequest.Message{Octets: b, MediaType: m.MediaType}, err
	}
	return evidencerequest.Message{}, errors.New("not {media_type, octets_text} for JSON or {media_type, octets_hex} for CBOR")
}

// readPrintedStaple rewrites a staple as `deal standard-history staple`
// prints it, {derivation, staple: {request, reply}} with the request and the
// reply as embedded JSON values, into the carried form
// {history, request, reply} the readers take. An embedded value is the whole
// JSON octets (printedOctets: compacted, the writer's HTML escapes read
// back); a changed byte fails request_binding. Any other file is returned
// unchanged.
func readPrintedStaple(raw []byte) []byte {
	var f struct {
		Derivation string `json:"derivation"`
		History    any    `json:"history"`
		Profile    any    `json:"profile"`
		Staple     *struct {
			Request json.RawMessage `json:"request"`
			Reply   json.RawMessage `json:"reply"`
		} `json:"staple"`
	}
	if json.Unmarshal(raw, &f) != nil || f.Derivation == "" || f.History != nil || f.Profile != nil || f.Staple == nil {
		return raw
	}
	// Always the whole embedded value: a request that happens to have
	// members named media_type and octets_text is still the octets checked.
	wrap := func(v json.RawMessage) any {
		text, ok := printedOctets(v)
		if !ok {
			return v
		}
		return carried{MediaType: evidencerequest.MediaTypeJSON, OctetsText: &text}
	}
	out, err := json.Marshal(map[string]any{"history": f.Derivation, "request": wrap(f.Staple.Request), "reply": wrap(f.Staple.Reply)})
	if err != nil {
		return raw
	}
	return out
}

// htmlEscapes are what Go's JSON encoder writes for <, >, & and the two line
// separators when it prints a RawMessage with its default HTML escaping, as
// the plugin's result writer does. Canonical request and reply octets (JCS)
// never contain them as escapes, so reading them back is exact; a value that
// did carry one stays unequal to its signed digest and fails request_binding.
var htmlEscapes = map[string]string{`\u003c`: "<", `\u003e`: ">", `\u0026`: "&", `\u2028`: "\u2028", `\u2029`: "\u2029"}

// printedOctets is an embedded JSON value as the octets it was printed from:
// compacted, with the printer's HTML escapes read back. It walks the
// escapes, so the text \\u003c (an escaped backslash, then u003c) is left alone.
func printedOctets(v json.RawMessage) (string, bool) {
	var buf bytes.Buffer
	if json.Compact(&buf, v) != nil {
		return "", false
	}
	in := buf.String()
	var out strings.Builder
	for i := 0; i < len(in); i++ {
		if in[i] != '\\' || i+1 >= len(in) {
			out.WriteByte(in[i])
			continue
		}
		if in[i+1] == 'u' && i+6 <= len(in) {
			if r, ok := htmlEscapes[in[i:i+6]]; ok {
				out.WriteString(r)
				i += 5
				continue
			}
		}
		out.WriteString(in[i : i+2])
		i++
	}
	return out.String(), true
}

// verifyStandardHistory evaluates a served standard_history/1 answer: the
// file holds the request and the reply octets as carried. The card is
// checked with AAC's standardhistory.Check; the policy, anchor and deal
// entries are verifier inputs this file does not carry, so the properties
// that need them stay NOT_CHECKED.
func verifyStandardHistory(c *cobra.Command, raw []byte, holderKeys []string) (frameReport, error) {
	r := frameReport{Kind: standardhistory.Derivation, Findings: []string{}, NotRun: []string{
		"window_from_policy, as_of and counts_recompute need the policy, the anchor time and the deal entries, which a served answer does not carry",
		"anchor_inclusion: no verification material kind exists for a derivation's artifact",
	}}
	var f struct {
		Request carried `json:"request"`
		Reply   carried `json:"reply"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return r, InputError("--history is not a served standard_history/1 answer {history, request, reply}: " + err.Error())
	}
	req, err := f.Request.message()
	if err != nil {
		return r, InputError("--history request: " + err.Error())
	}
	rep, err := f.Reply.message()
	if err != nil {
		return r, InputError("--history reply: " + err.Error())
	}
	v, err := evidencerequest.NewVerifier(evidencerequest.TrustPolicy{ResponderKeys: holderKeys},
		evidencerequest.VerificationPolicyFromPin(nil), evidencerequest.ProfileTime(DealClock()), nil)
	if err != nil {
		return r, err
	}
	res, err := v.Verify([]evidencerequest.Exchange{{Request: req, Replies: []evidencerequest.Message{rep}}}, evidencerequest.VerifyOptions{})
	if errors.Is(err, evidencerequest.ErrInvalid) {
		return r, InputError("--history holds a request that is not an evidence request map")
	}
	if err != nil {
		return r, err
	}
	reply := res.Exchanges[0].Replies[0]
	if reply.MessageType != "envelope" {
		r.Verdict, r.Reason = "NOT_ACCEPTED", fmt.Sprintf("the reply is a %s, not an envelope", reply.MessageType)
		return r, nil
	}
	r.Envelope = map[string]string{}
	accepted := true
	for _, p := range reply.Properties {
		r.Envelope[p.Property] = p.Status
	}
	for _, p := range envelopeMustPass {
		if reply.Status(p) != evidencerequest.StatusPass {
			accepted = false
			r.Findings = append(r.Findings, p+" "+reply.Status(p))
		}
	}
	if reply.Status("anchor_inclusion") != evidencerequest.StatusNotChecked {
		accepted = false
		r.Findings = append(r.Findings, "anchor_inclusion is not NOT_CHECKED")
	}
	r.Responder = reply.Envelope["responder"]
	props, err := standardhistory.Check(reply.Artifact, nil, nil, nil)
	if err != nil {
		return r, err
	}
	r.Properties = props
	if !standardhistory.IsStandard(props) {
		accepted = false
		r.Findings = append(r.Findings, "not a standard card (SH §6)")
	}
	receipts, policy, err := loadWitnessInputs(c)
	if err != nil {
		return r, err
	}
	anchor, _ := reply.Envelope["anchor"].(map[string]any)
	digest, _ := anchor["digest"].(string)
	w := anchorWitnessed(digest, receipts, policy)
	r.AnchorWitnessed = &w
	r.Verdict = "NOT_ACCEPTED"
	if accepted {
		r.Verdict = "ACCEPTED"
	}
	return r, nil
}

// notEvaluable is the report for an instance this verifier cannot check.
func notEvaluable(kind, reason string) frameReport {
	return frameReport{Kind: kind, Verdict: "not_evaluable", Reason: reason, Findings: []string{}}
}
