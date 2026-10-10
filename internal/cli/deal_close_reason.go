package cli

import (
	"strings"
)

// Who ended a deal, and why, from closed sets: sealed in the clear on the
// close's outcome and close records, never free text.
//
// The actor is whose decision ended the deal: the user's (they ended it, or
// told the agent to), the agent's (it could not go on), or another
// platform's. The reason follows from the close's status where the status
// says it (received, pending, not_selected); a deal closed with nothing
// received must say why, since the status alone cannot.
const (
	closeActorUser     = "user"
	closeActorAgent    = "agent"
	closeActorPlatform = "platform"
)

var closeActors = map[string]bool{closeActorUser: true, closeActorAgent: true, closeActorPlatform: true}

// closeReasonsNotReceived are the reasons a deal closed with nothing
// received may give.
var closeReasonsNotReceived = []string{
	"user_closed", "agent_could_not_complete", "merchant_rejected", "payment_failed",
	"cancelled_in_window", "not_delivered", "other",
}

// derivedCloseReason is the one reason a status (and, when received,
// whether what was delivered matched) allows; "" for not_received, which
// must be stated.
func derivedCloseReason(status, outcome string) string {
	switch status {
	case "received":
		if outcome == "mismatch" {
			return "delivered_mismatch"
		}
		return "completed"
	case "pending", "":
		return "awaiting_delivery"
	case "not_selected":
		return "not_selected"
	}
	return ""
}

// settleCloseActorReason fixes a close's actor and reason: as stated, or
// derived where the status says it, and checked against each other. The
// actor defaults to the agent, which runs the close, except where the
// reason is the user's own (user_closed).
func settleCloseActorReason(in *dealCloseInput, outcome string) error {
	in.Actor = strings.ToLower(strings.TrimSpace(in.Actor))
	in.Reason = strings.ToLower(strings.TrimSpace(in.Reason))
	if in.Actor != "" && !closeActors[in.Actor] {
		return inputError("actor must be user, agent or platform: whose decision ended the deal")
	}
	if derived := derivedCloseReason(in.Status, outcome); derived != "" {
		if in.Reason != "" && in.Reason != derived {
			return inputError("a close with status " + in.Status + " has reason " + derived + ", not " + in.Reason)
		}
		in.Reason = derived
	} else {
		known := false
		for _, r := range closeReasonsNotReceived {
			known = known || r == in.Reason
		}
		if !known {
			return inputError("a close with nothing received must say why: reason is one of " + strings.Join(closeReasonsNotReceived, ", "))
		}
	}
	if in.Actor == "" {
		in.Actor = closeActorAgent
		if in.Reason == "user_closed" {
			in.Actor = closeActorUser
		}
	}
	switch {
	case in.Reason == "user_closed" && in.Actor != closeActorUser:
		return inputError("user_closed is the user's own close: its actor is user")
	case in.Reason == "agent_could_not_complete" && in.Actor != closeActorAgent:
		return inputError("agent_could_not_complete is the agent's: its actor is agent")
	}
	return nil
}

// closeLine is the actor-marked line a close reads as: who ended the deal,
// and why. A close sealed before closes carried them reads as before.
func closeLine(c dealCloseResult) string {
	if c.Actor == "" {
		return "Closed: " + c.Outcome
	}
	who := map[string]string{closeActorUser: "You", closeActorAgent: "The agent", closeActorPlatform: "The platform"}[c.Actor]
	switch c.Reason {
	case "user_closed":
		return "You closed this deal"
	case "agent_could_not_complete":
		return "The agent could not complete this deal"
	}
	why := map[string]string{
		"completed":           "completed as agreed",
		"delivered_mismatch":  "what was delivered differs from what was agreed",
		"awaiting_delivery":   "still waiting for delivery",
		"not_selected":        "the other side was not chosen",
		"merchant_rejected":   "the merchant rejected it",
		"payment_failed":      "the payment failed",
		"cancelled_in_window": "cancelled within its cancellation window",
		"not_delivered":       "nothing was delivered",
		"other":               "for another reason",
	}[c.Reason]
	return who + " closed this deal: " + why
}

// closeActorReasonBody seals a close's actor and reason on its outcome or
// close record body, when the close carries them.
func closeActorReasonBody(body map[string]interface{}, in dealCloseInput) {
	if in.Actor != "" {
		body["actor"], body["reason"] = in.Actor, in.Reason
	}
}
