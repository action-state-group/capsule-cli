package cli

import (
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/spf13/cobra"
)

// `request` and `respond` carry draft-mih-agent-evidence-request-00 for a
// profile's own log: capsulectl-book over an evidence book, capsulectl-deal
// over a deal store. Each plugin does its own work with what was asked; the
// verbs' flags, the checks on what they read, and the shape of what they
// print are here, once, so the two cannot drift apart.

// EvidenceRequestInput is what `request` read, checked. Exactly one mode is
// set: Request (ask), Response (record what came back), or a non-zero
// WindowEnd (record that nothing came back); For names the request record
// for the last two.
type EvidenceRequestInput struct {
	Request, Response []byte
	For               string
	WindowEnd         time.Time
	// ResponderSigner and ResponderCheckpoint are set with Response: the
	// responder's two Ed25519 public keys as lowercase hex, which the caller
	// obtained independently and pins. A response that does not verify under
	// them is refused and never recorded, so it cannot foreclose the request.
	ResponderSigner, ResponderCheckpoint string
	// WitnessDirectory is --witness-directory as given, set only with
	// Response. The plugin reads it, after its own checks: a book refuses
	// the flag without opening the file.
	WitnessDirectory string
	// Responder, Output and Route are the flags as given.
	Responder, Output, Route string
}

// EvidenceRequestResult is what `request` prints.
type EvidenceRequestResult struct {
	// Request carries the canonical request bytes only when --output could
	// not be written.
	Request       json.RawMessage `json:"request,omitempty"`
	RecordID      string          `json:"record_id"`
	RequestDigest string          `json:"request_digest,omitempty"`
	Output        string          `json:"output,omitempty"`
	Outcome       string          `json:"outcome,omitempty"`
	Reason        string          `json:"reason,omitempty"`
}

// EvidenceRequestCommand is `request`: its flags and the checks on what it
// reads, then run on the selected profile.
func EvidenceRequestCommand(run func(c *cobra.Command, p Profile, in EvidenceRequestInput) error) *cobra.Command {
	cmd := &cobra.Command{Use: "request", Short: "Ask a party for evidence, or record its answer or its absence (each is a signed book record)", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		p, e := Selected(c)
		if e != nil {
			return e
		}
		ask, _ := c.Flags().GetString("request")
		answer, _ := c.Flags().GetString("response")
		absentUntil, _ := c.Flags().GetString("absent-until")
		var in EvidenceRequestInput
		in.For, _ = c.Flags().GetString("for")
		in.Responder, _ = c.Flags().GetString("responder")
		in.Output, _ = c.Flags().GetString("output")
		in.Route, _ = c.Flags().GetString("route")
		modes := 0
		for _, v := range []string{ask, answer, absentUntil} {
			if v != "" {
				modes++
			}
		}
		if modes != 1 || (ask == "") == (in.For == "") {
			return InputError("give exactly one of --request, --response or --absent-until; --for names the request record for the last two")
		}
		if absentUntil != "" {
			if in.WindowEnd, e = time.Parse(time.RFC3339Nano, absentUntil); e != nil {
				return InputError("--absent-until must be an RFC 3339 time")
			}
		}
		if ask != "" {
			if in.Request, e = ReadInput(ask); e != nil {
				return e
			}
		}
		witnesses, _ := c.Flags().GetString("witness-directory")
		if answer == "" {
			if witnesses != "" {
				return InputError("--witness-directory goes with --response: it checks the witness receipts an answer carries")
			}
			return run(c, p, in)
		}
		signer, _ := c.Flags().GetString("responder-key")
		checkpoint, _ := c.Flags().GetString("responder-checkpoint-key")
		keys, e := ParseKeys([]string{signer, checkpoint})
		if e != nil || len(keys) != 2 {
			return InputError("--response needs --responder-key and --responder-checkpoint-key, each a 32-byte Ed25519 public key in hex")
		}
		// Key ids are compared as lowercase hex strings.
		in.ResponderSigner, in.ResponderCheckpoint = hex.EncodeToString(keys[0]), hex.EncodeToString(keys[1])
		if in.Response, e = ReadInput(answer); e != nil {
			return e
		}
		in.WitnessDirectory = witnesses
		return run(c, p, in)
	}}
	cmd.Flags().String("request", "", "Evidence request JSON to ask with: the request map of draft-mih-agent-evidence-request-00 (a one-member subject such as {\"full_history\":null}, and coverage with exactly one of expected_pin or min_freshness); on a deal profile, the deal copy request")
	cmd.Flags().String("responder", "", "Who is being asked (recorded in the request record)")
	cmd.Flags().String("output", "", "Write the canonical request bytes to transmit")
	cmd.Flags().String("for", "", "The request record id an answer or absence belongs to")
	cmd.Flags().String("response", "", "The response JSON that came back")
	cmd.Flags().String("responder-key", "", "Required with --response: the responder's signing public key in hex; a response under any other key is refused")
	cmd.Flags().String("responder-checkpoint-key", "", "Required with --response: the responder's checkpoint public key in hex; an artifact anchored under any other key is refused")
	cmd.Flags().String("witness-directory", "", "With --response on a deal profile: a witness directory (witnesses.json format) to verify the witness receipts a deal copy carries; any receipt that does not verify under it, or any receipt at all without it, records the answer as failed")
	cmd.Flags().String("absent-until", "", "RFC 3339 end of the window in which nothing arrived")
	cmd.Flags().String("route", "", "Route the request was sent over (recorded with an absence)")
	return cmd
}

// EvidenceRespondInput is what `respond` read, checked.
type EvidenceRespondInput struct {
	// Request is the request's bytes as received.
	Request []byte
	// Requester is the identity the operator authenticated the request's
	// sender as, on the channel it came over.
	Requester string
	// Output is where the reply goes. PolicyPath is --policy as given: the
	// evidence book's share policy, which only a book reads.
	Output, PolicyPath string
}

// EvidenceRespondResult is what `respond` prints.
type EvidenceRespondResult struct {
	// Response carries the signed response only when --output could not be
	// written.
	Response json.RawMessage `json:"response,omitempty"`
	// RecordID and Relationship are empty when nothing was recorded: a
	// malformed or unauthenticated request is answered unrecorded.
	RecordID      string `json:"record_id,omitempty"`
	RequestDigest string `json:"request_digest"`
	Outcome       string `json:"outcome"`
	Reason        string `json:"reason,omitempty"`
	Relationship  string `json:"relationship,omitempty"`
	Output        string `json:"output"`
}

// EvidenceRespondCommand is `respond`: its flags and the checks on what it
// reads, then run on the selected profile.
func EvidenceRespondCommand(run func(c *cobra.Command, p Profile, in EvidenceRespondInput) error) *cobra.Command {
	cmd := &cobra.Command{Use: "respond", Short: "Answer a received evidence request with a signed artifact or refusal, and record it", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		p, e := Selected(c)
		if e != nil {
			return e
		}
		var in EvidenceRespondInput
		requestPath, _ := c.Flags().GetString("request")
		in.Output, _ = c.Flags().GetString("output")
		if in.Output == "" {
			return InputError("--output is required: the file to write the result to")
		}
		if in.Request, e = ReadInput(requestPath); e != nil {
			return e
		}
		in.Requester, _ = c.Flags().GetString("requester")
		in.PolicyPath, _ = c.Flags().GetString("policy")
		return run(c, p, in)
	}}
	cmd.Flags().String("request", "", "The request bytes as received")
	cmd.Flags().String("requester", "", "The requester's identity as you authenticated it on the channel the request came over; without it only public subjects (checkpoints, history_card/1) are answered. On a deal profile, who the copy is shared with")
	cmd.Flags().String("policy", "", "Share policy JSON (record_at_completion, history_segments, adjudications, witness); default: the documented defaults")
	cmd.Flags().String("output", "", "Write the signed reply (envelope or refusal) to send back")
	return cmd
}
