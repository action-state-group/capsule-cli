package cli

import (
	"fmt"
	"strings"
)

// A claim's words, and the free-form note of where it was read, are sealed
// as salted commitments like every other text a deal record carries; only
// whose it is (source_kind, a closed set) is in the clear. The user's own
// copy, and an adjudicator's when the words carry none of the user's private
// details, open them; a counterparty's copy shows that the claim exists and
// whose it is, never what it says.
//
// A step sealed before this has no ClaimCommit and re-derives its claims in
// the clear, unchanged.
const dealClaimCommitVersion = "1"

// dealSourceKinds is whose a claim is.
var dealSourceKinds = map[string]bool{"merchant": true, "agent": true, "platform": true, "user": true, "external": true}

// merchantSources are the source notes that can only mean the counterparty
// said it. Any other note (a page, a photo, a snapshot: the merchant's own
// or a marketplace's) does not say whose it is, so its kind must be stated.
var merchantSources = map[string]bool{
	"counterparty": true, "merchant": true, "seller": true, "merchant_email": true,
	"seller_message": true, "merchant_message": true, "hotel_message": true, "host_message": true,
}

// normalizeClaim makes a claim's source a token and settles its kind: as
// stated, or for a note that can only mean the counterparty, merchant. A
// claim whose note does not say whose it is must state source_kind.
func normalizeClaim(c *dealClaim) error {
	if strings.TrimSpace(c.Source) != "" {
		t, err := sourceToken(c.Source)
		if err != nil {
			return err
		}
		c.Source = t
	}
	c.SourceKind = strings.ToLower(strings.TrimSpace(c.SourceKind))
	switch {
	case c.SourceKind != "":
		if !dealSourceKinds[c.SourceKind] {
			return inputError("a claim's source_kind must be one of merchant, agent, platform, user or external")
		}
	case merchantSources[c.Source]:
		c.SourceKind = "merchant"
	default:
		return inputError(fmt.Sprintf("state the claim's source_kind (merchant, agent, platform, user or external): its source %q does not say whose it is", c.Source))
	}
	return c.validate()
}

// claimTexts names a claim's committed texts for its step's nonces.
func claimTexts(t map[string]string, c dealClaim, textName, sourceName string) {
	t[textName] = c.Text
	if c.Source != "" {
		t[sourceName] = c.Source
	}
}

// claimBody is a claim as a record carries it: its words and its source note
// as commitments, whose it is in the clear.
func claimBody(c dealClaim, textName, sourceName string, commit func(string) (string, error)) (map[string]interface{}, error) {
	text, err := commit(textName)
	if err != nil {
		return nil, err
	}
	m := map[string]interface{}{"text_commitment": text, "source_kind": c.SourceKind}
	if c.Source != "" {
		if m["source_ref_commitment"], err = commit(sourceName); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// claimOpenings are the openings of the claims a deal's records commit to,
// by record digest (and, for the baseline's claims, index): each claim's
// nonce and words, and its source note's. keep says whether a claim's
// openings go in this copy.
func claimOpenings(events []sealedEvent, keep func(dealClaim) bool) []interface{} {
	out := []interface{}{}
	add := func(se sealedEvent, c dealClaim, index int, textName, sourceName string) {
		if !keep(c) {
			return
		}
		m := map[string]interface{}{
			"record_digest": se.Digest,
			"text":          map[string]interface{}{"nonce": se.Event.Nonces[textName], "text": c.Text},
		}
		if index >= 0 {
			m["index"] = index
		}
		if c.Source != "" {
			m["source"] = map[string]interface{}{"nonce": se.Event.Nonces[sourceName], "text": c.Source}
		}
		out = append(out, m)
	}
	for _, se := range events {
		if se.Event.ClaimCommit == "" {
			continue
		}
		switch {
		case se.Event.Open != nil:
			for i, c := range se.Event.Open.Claims {
				add(se, c, i, fmt.Sprintf("claim_text_%d", i), fmt.Sprintf("claim_source_%d", i))
			}
		case se.Event.Claim != nil:
			add(se, *se.Event.Claim, -1, "claim_text", "claim_source")
		}
	}
	return out
}

// unverifiedClaimRefs names, by reference, the claims a check found
// unverified: the record each was sealed in (the claim's own step, or the
// baseline with the claim's index), the first that says it.
func unverifiedClaimRefs(events []sealedEvent, texts []string) []interface{} {
	out := []interface{}{}
	for _, text := range texts {
		found := false
		for _, se := range events {
			switch {
			case se.Event.Open != nil:
				for i, c := range se.Event.Open.Claims {
					if !found && c.Text == text {
						out, found = append(out, map[string]interface{}{"claim": digestRef(se.Digest), "index": i}), true
					}
				}
			case se.Event.Claim != nil:
				if !found && se.Event.Claim.Text == text {
					out, found = append(out, map[string]interface{}{"claim": digestRef(se.Digest)}), true
				}
			}
		}
	}
	return out
}
