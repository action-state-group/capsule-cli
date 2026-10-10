package cli

import (
	"fmt"
	"regexp"
	"strings"
)

// The user's bounds are private from the counterparty, whichever side the
// user is on: a seller who learns the buyer's maximum (intent.max_total_minor)
// learns what to ask for, and a buyer who learns the seller's floor
// (intent.min_total_minor) learns what to offer. The floor is never sealed at
// all (only its commitment). The adjudicator's copy may carry the maximum (it
// is how "over your limit" is judged). A counterparty copy withholds any
// record that carries either bound, leaves them out of the merchant section,
// and the gate refuses a counterparty page or bundle that names either, as
// the field or as money, unless it equals an amount the copy may show anyway
// (the price asked, an amount paid). The party role changes only the words.

// dealPrivateBoundKeys are the bound fields no counterparty copy carries.
var dealPrivateBoundKeys = map[string]bool{"max_total_minor": true, "min_total_minor": true}

// dealBasisYourLimit is the merchant section's basis when what was approved
// is only the user's spending limit.
const dealBasisYourLimit = "your limit"

// dealCeiling is a bound as the copy could write it: as money with its
// currency, and as a bare decimal amount. floor marks the user's floor
// (min_total_minor), which no shared copy may carry; the limit
// (max_total_minor) is kept from the counterparty only.
type dealCeiling struct {
	minor    int64
	currency string
	floor    bool
}

// dealFloorKeys name the floor and its openings: no shared copy, for any
// audience, carries them.
var dealFloorKeys = []string{"min_total_minor", "bounds_openings", "commercial_bounds_opening"}

func dealCeilings(events []sealedEvent) []dealCeiling {
	var out []dealCeiling
	currency := ""
	if len(events) > 0 && events[0].Event.Open != nil {
		currency = events[0].Event.Open.Terms.Currency
	}
	for _, se := range events {
		e := se.Event
		var intents []dealIntent
		if e.Open != nil {
			intents = append(intents, e.Open.Intent)
		}
		if e.Intent != nil {
			intents = append(intents, *e.Intent)
		}
		for _, i := range intents {
			if i.MaxTotalMinor != nil {
				out = append(out, dealCeiling{*i.MaxTotalMinor, currency, false})
			}
			if i.MinTotalMinor != nil {
				out = append(out, dealCeiling{*i.MinTotalMinor, currency, true})
			}
		}
	}
	return out
}

// dealShownAmounts are the amounts a counterparty copy may carry: what the
// seller asked and what was paid, which the seller knows already.
func dealShownAmounts(events []sealedEvent) map[int64]bool {
	shown := map[int64]bool{}
	terms := func(t *dealTerms) {
		if t == nil {
			return
		}
		for _, v := range []*int64{t.PriceMinor, t.DepositMinor} {
			if v != nil {
				shown[*v] = true
			}
		}
	}
	for _, se := range events {
		e := se.Event
		if e.Open != nil {
			terms(&e.Open.Terms)
		}
		if e.Change != nil {
			terms(e.Change.Terms)
		}
		if e.Snapshot != nil {
			terms(e.Snapshot.Terms)
			if e.Snapshot.AmountMinor != nil {
				shown[*e.Snapshot.AmountMinor] = true
			}
		}
		if e.Act != nil && e.Act.AmountMinor != nil {
			shown[*e.Act.AmountMinor] = true
		}
	}
	return shown
}

// hexRun is a run of 16 or more hex digits.
var hexRun = regexp.MustCompile(`(?i)[0-9a-f]{16,}`)

// removeHexTokens blanks every whole token that is a run of 16 or more hex
// digits (a capsule id, a digest, a key, a signature): one with no letter or
// digit right before or after it.
func removeHexTokens(text []byte) []byte {
	out := append([]byte(nil), text...)
	alnum := func(i int) bool {
		if i < 0 || i >= len(text) {
			return false
		}
		b := text[i] | 0x20
		return (b >= 'a' && b <= 'z') || (text[i] >= '0' && text[i] <= '9')
	}
	for _, loc := range hexRun.FindAllIndex(text, -1) {
		if alnum(loc[0]-1) || alnum(loc[1]) {
			continue
		}
		for i := loc[0]; i < loc[1]; i++ {
			out[i] = ' '
		}
	}
	return out
}

// dealCeilingGate refuses a shared copy that carries a bound it may not: the
// floor (by name, by its openings, or as money) in any shared copy, and the
// spending limit in the counterparty's. It reads the same plain form as the
// share gate (foldText). An amount the copy shows anyway (the price asked, an
// amount paid) is not a leak.
func dealCeilingGate(data []byte, events []sealedEvent, audience string) error {
	if audience == dealAudienceKeep {
		return nil
	}
	counterparty := audience == dealAudienceCounterparty
	// The deal's own ids and every digest, key and signature are taken out
	// first, as the page gate takes out the ids it allows: a run of hex is
	// never prose, and one that holds "3cad8" is not 8 Canadian dollars.
	folded := []byte(foldText(string(data)))
	for _, id := range dealOwnIDs(events) {
		folded = removeWholeToken(folded, []byte(foldText(id)))
	}
	data = removeHexTokens(folded)
	text := strings.ToLower(string(data))
	refuseLimit := inputError("refusing to write the shared copy: the counterparty's copy would carry your spending limit")
	if dealRole(events) == dealRoleSeller {
		refuseLimit = inputError("refusing to write the shared copy: the counterparty's copy would carry the lowest price you will take, or a limit you set")
	}
	refuseFloor := inputError("refusing to write the shared copy: it would carry the lowest price you will take")
	for _, k := range dealFloorKeys {
		if strings.Contains(text, k) {
			return refuseFloor
		}
	}
	if counterparty && strings.Contains(text, "max_total_minor") {
		return refuseLimit
	}
	// The bound as money in any of the ways prose writes it ("$1,700.00",
	// "1.700,00 €", "USD 1 700", "lowest I'll take is 1700"), compared by
	// value.
	switch statesBound(string(data), events, counterparty) {
	case "intent.min_total_minor":
		return refuseFloor
	case "intent.max_total_minor":
		return refuseLimit
	}
	shown := dealShownAmounts(events)
	for _, c := range dealCeilings(events) {
		if shown[c.minor] || (!c.floor && !counterparty) {
			continue
		}
		for _, form := range []string{formatMoney(c.minor, c.currency), fmt.Sprintf("%d.%02d", c.minor/100, c.minor%100)} {
			if regexp.MustCompile(`(^|[^0-9.])` + regexp.QuoteMeta(strings.ToLower(form)) + `([^0-9]|$)`).MatchString(text) {
				if c.floor {
					return refuseFloor
				}
				return refuseLimit
			}
		}
	}
	return nil
}

// dealOwnIDs are the deal's own identifiers, which every shared copy carries (the
// x-deal-v0 deal_id and the log id deal/<deal_id>): random hex, never a private
// value. The gate takes them out before it checks, or a deal id whose hex happens to
// hold a Luhn-valid digit run reads as a card number and the deal can never be
// shared.
func dealOwnIDs(events []sealedEvent) []string {
	if len(events) == 0 || events[0].Event.DealID == "" {
		return nil
	}
	return []string{events[0].Event.DealID}
}

// dealShareGate is the last check before a shared copy is written or a share
// is put on record: the share gate, then the spending-limit gate for the
// counterparty.
func dealShareGate(data []byte, events []sealedEvent, audience string) error {
	allowed := append(dealShareableIDs(events, audience), dealOwnIDs(events)...)
	if err := dealPageGate(data, events, allowed...); err != nil {
		return err
	}
	return dealCeilingGate(data, events, audience)
}
