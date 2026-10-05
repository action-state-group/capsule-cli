package cli

import (
	"fmt"
	"regexp"
	"strings"
)

// The user's spending limit (intent.max_total_minor) is private from the
// counterparty: a seller who learns the buyer's maximum learns what to ask
// for. The adjudicator's copy may carry it (it is how "over your limit" is
// judged). A counterparty copy withholds any record that carries it, leaves
// it out of the merchant section, and the gate refuses a counterparty page or
// bundle that names it, as the field or as money, unless it equals an amount
// the copy may show anyway (the price the seller asked, an amount paid).

// dealBasisYourLimit is the merchant section's basis when what was approved
// is only the user's spending limit.
const dealBasisYourLimit = "your limit"

// dealCeiling is the ceiling as the copy could write it: as money with
// its currency, and as a bare decimal amount.
type dealCeiling struct {
	minor    int64
	currency string
}

func dealCeilings(events []sealedEvent) []dealCeiling {
	var out []dealCeiling
	currency := ""
	if len(events) > 0 && events[0].Event.Open != nil {
		currency = events[0].Event.Open.Terms.Currency
	}
	for _, se := range events {
		e := se.Event
		if e.Open != nil && e.Open.Intent.MaxTotalMinor != nil {
			out = append(out, dealCeiling{*e.Open.Intent.MaxTotalMinor, currency})
		}
		if e.Intent != nil && e.Intent.MaxTotalMinor != nil {
			out = append(out, dealCeiling{*e.Intent.MaxTotalMinor, currency})
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

// dealCeilingGate refuses a counterparty copy that carries the spending
// limit. It reads the same plain form as the share gate (foldText).
func dealCeilingGate(data []byte, events []sealedEvent, audience string) error {
	if audience != dealAudienceCounterparty {
		return nil
	}
	text := strings.ToLower(foldText(string(data)))
	refuse := inputError("refusing to write the shared copy: the counterparty's copy would carry your spending limit")
	if strings.Contains(text, "max_total_minor") {
		return refuse
	}
	shown := dealShownAmounts(events)
	for _, c := range dealCeilings(events) {
		if shown[c.minor] {
			continue
		}
		for _, form := range []string{formatMoney(c.minor, c.currency), fmt.Sprintf("%d.%02d", c.minor/100, c.minor%100)} {
			if regexp.MustCompile(`(^|[^0-9.])` + regexp.QuoteMeta(strings.ToLower(form)) + `([^0-9]|$)`).MatchString(text) {
				return refuse
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
