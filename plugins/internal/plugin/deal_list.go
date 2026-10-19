package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/spf13/cobra"
)

// dealListing is one deal in the profile's store, as `deal list` prints it:
// identifiers, timestamps and, with --json, a plain summary. A sale (`deal
// sale new`) is listed with kind "sale": its log is in the same store,
// though it is not a deal.
type dealListing struct {
	DealID       string `json:"deal_id"`
	Kind         string `json:"kind"`  // "deal" or "sale"
	State        string `json:"state"` // "open", or "closed" once a final close is indexed
	OpenedAt     string `json:"opened_at"`
	LastRecordAt string `json:"last_record_at"`
	// CounterpartyFingerprint is the payee fingerprint of the deal's latest
	// counterparty_profile step (profileKeyFor), or null when it has none.
	CounterpartyFingerprint *string `json:"counterparty_fingerprint"`
	LogID                   string  `json:"log_id"`
	// Summary is the deal in plain words, read from the steps the list
	// reads. It is the user's own view: what they asked, the counterparty's
	// name and the amount are private, and no shared copy carries them.
	Summary dealSummary `json:"summary"`
}

// dealSummary is a deal in plain words: what the user asked (the words
// `deal report` gives as asked), with whom, the amount and the outcome, and
// those four as one line. It is computed from the sealed steps alone: it
// seals nothing and cuts no checkpoint, unlike `deal report`.
type dealSummary struct {
	What string `json:"what"`
	// Counterparty is the name the opening step sealed as who.name, or
	// dealNotRecorded: a name is never read out of free text.
	Counterparty string `json:"counterparty"`
	// Amount is nil when no step records one.
	Amount  *dealSummaryAmount `json:"amount"`
	Outcome string             `json:"outcome"`
	Text    string             `json:"text"`
}

// dealSummaryAmount is an amount as recorded, in minor units. Basis says
// which: "paid" is the net of the sealed acts in the first act's currency
// (`deal report`'s money net_minor; acts in another currency are counted in
// OtherCurrencyActs, as the report counts them, never summed), and
// "opening_terms" the opening step's price when no act moved money (a later
// change of terms is not read).
type dealSummaryAmount struct {
	AmountMinor       int64  `json:"amount_minor"`
	Currency          string `json:"currency"`
	Basis             string `json:"basis"`
	OtherCurrencyActs int    `json:"other_currency_acts,omitempty"`
}

const dealNotRecorded = "not recorded"

func dealListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "list", Short: "One line per deal in the profile's store: deal id, kind, state, opened and last-record times, counterparty fingerprint, log id; --json adds a plain summary (what, with whom, amount, outcome); read-only", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, err := cli.Selected(c)
		if err != nil {
			return err
		}
		s, err := openDealStoreRead(p)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, s.close()) }()
		list, err := s.dealListings(c.Context())
		if err != nil {
			return err
		}
		if asJSON, _ := c.Flags().GetBool("json"); asJSON {
			return json.NewEncoder(c.OutOrStdout()).Encode(list)
		}
		for _, l := range list {
			fp := "-"
			if l.CounterpartyFingerprint != nil {
				fp = *l.CounterpartyFingerprint
			}
			if _, err = fmt.Fprintln(c.OutOrStdout(), strings.Join([]string{l.DealID, l.Kind, l.State, l.OpenedAt, l.LastRecordAt, fp, l.LogID}, "\t")); err != nil {
				return err
			}
		}
		return nil
	}}
	cmd.Flags().Bool("json", false, "Emit the deals as one JSON array, each with a plain summary, instead of tab-separated lines")
	return cmd
}

// dealSummaryOf reads the summary from a deal's steps. Its outcome is the
// last close step's, as `deal report` reads it, or "open".
func dealSummaryOf(events []sealedEvent) dealSummary {
	sum := dealSummary{What: dealNotRecorded, Counterparty: dealNotRecorded, Outcome: "open"}
	currency := ""
	if open := events[0].Event.Open; open != nil {
		if strings.TrimSpace(open.Intent.Verbatim) != "" {
			sum.What = open.Intent.Verbatim
		}
		if strings.TrimSpace(open.Who.Name) != "" {
			sum.Counterparty = open.Who.Name
		}
		currency = open.Terms.Currency
		if open.Terms.PriceMinor != nil {
			sum.Amount = &dealSummaryAmount{AmountMinor: *open.Terms.PriceMinor, Currency: strings.ToUpper(currency), Basis: "opening_terms"}
		}
	}
	if m := buildDealMoney(events, currency); m != nil {
		sum.Amount = &dealSummaryAmount{AmountMinor: m.NetMinor, Currency: m.Currency, Basis: "paid", OtherCurrencyActs: m.Other}
	}
	for _, se := range events {
		if se.Event.Kind == "close" && se.Event.Close != nil {
			sum.Outcome = se.Event.Close.Outcome
		}
	}
	amount := "amount not recorded"
	if a := sum.Amount; a != nil {
		amount = formatMoney(a.AmountMinor, a.Currency) + " in the opening terms"
		if a.Basis == "paid" {
			amount = "net " + formatMoney(a.AmountMinor, a.Currency) + " paid"
		}
		if a.OtherCurrencyActs > 0 {
			amount += fmt.Sprintf(", and %d act(s) in another currency", a.OtherCurrencyActs)
		}
	}
	with := "with " + sum.Counterparty
	if sum.Counterparty == dealNotRecorded {
		with = "counterparty not recorded"
	}
	sum.Text = strings.Join([]string{sum.What, with, amount, sum.Outcome}, " · ")
	return sum
}

// dealListings reads every deal's steps from the store's index, in deal id
// order, and checks each row the way `load` does (one strict record whose
// deal, step number and kind are the row's, chained to the step before by
// its digest), without re-verifying the steps against the deal's log
// (`deal export` does). A last step indexed but not yet in the log (a crash
// between the two writes, which the next write command recovers) is listed.
// A store with no deal yet lists none.
func (s *dealSession) dealListings(ctx context.Context) (_ []dealListing, err error) {
	list := []dealListing{}
	rows, err := s.db.QueryContext(ctx, `SELECT deal_id, n, kind, record_digest, local FROM deal_steps ORDER BY deal_id, n`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return list, nil
		}
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var id, prev string
	var events []sealedEvent
	flush := func() {
		if len(events) > 0 {
			list = append(list, dealListingOf(id, events))
		}
	}
	for rows.Next() {
		var rowID, kind, digest, local string
		var n int64
		if err = rows.Scan(&rowID, &n, &kind, &digest, &local); err != nil {
			return nil, err
		}
		if rowID != id {
			flush()
			id, prev, events = rowID, "", nil
		}
		var ev dealEvent
		if err = cli.DecodeJSON([]byte(local), &ev); err != nil {
			return nil, cli.ErrConflict
		}
		if ev.DealID != id || ev.N != n || n != int64(len(events)+1) || ev.Kind != kind || ev.Prev != prev {
			return nil, cli.ErrConflict
		}
		events = append(events, sealedEvent{Event: ev})
		prev = digest
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	flush()
	return list, nil
}

func dealListingOf(dealID string, events []sealedEvent) dealListing {
	l := dealListing{
		DealID: dealID, Kind: "deal", State: "open",
		OpenedAt: events[0].Event.At, LastRecordAt: events[len(events)-1].Event.At,
		LogID: cli.DealLogID(dealID), Summary: dealSummaryOf(events),
	}
	if cli.SaleIDPattern.MatchString(dealID) {
		l.Kind = "sale"
	}
	if dealFinallyClosed(events) {
		l.State = "closed"
	}
	for _, se := range events {
		if cp := se.Event.CounterpartyProfile; se.Event.Kind == "counterparty_profile" && cp != nil {
			l.CounterpartyFingerprint = &cp.Payee
		}
	}
	return l
}
